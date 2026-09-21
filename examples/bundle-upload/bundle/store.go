// Package bundle implements a multi-file bundle upload store with
// staging-then-publish semantics: a bundle becomes visible to readers
// only after every file in the request has been fully written and
// validated, and the whole staging directory is atomically renamed
// into the published root.
package bundle

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// stagingDirName is the dedicated staging area inside the root. Startup
// cleanup only ever touches this directory, never published bundles.
const stagingDirName = ".staging"

const manifestName = "manifest.json"

// idPattern matches server-generated bundle IDs and file keys. Anything
// else (including ".", "..", separators, or traversal sequences) is
// rejected before any path is built.
var idPattern = regexp.MustCompile("^[0-9a-f]{32}$")

// IsValidID reports whether s is a well-formed server-generated ID.
func IsValidID(s string) bool { return idPattern.MatchString(s) }

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Config controls upload limits and injectable behavior.
type Config struct {
	MaxFiles      int           // maximum number of files per bundle
	MaxFileBytes  int64         // maximum bytes per single file
	MaxTotalBytes int64         // maximum bytes per bundle, counted as actually read
	StagingMaxAge time.Duration // staging dirs older than this are removed by CleanupStaging

	// Now supplies the current time; defaults to time.Now. Tests inject a fake clock.
	Now func() time.Time
	// Rename publishes a staging directory; defaults to os.Rename. Tests inject failures.
	Rename func(oldpath, newpath string) error
}

func (c *Config) withDefaults() {
	if c.MaxFiles <= 0 {
		c.MaxFiles = 20
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 32 << 20
	}
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = 64 << 20
	}
	if c.StagingMaxAge <= 0 {
		c.StagingMaxAge = time.Hour
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Rename == nil {
		c.Rename = os.Rename
	}
}

// FileEntry describes one stored file. Name is the client-supplied
// display name only; the object on disk is named by Key.
type FileEntry struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the published record of a bundle.
type Manifest struct {
	ID        string      `json:"id"`
	CreatedAt time.Time   `json:"created_at"`
	Files     []FileEntry `json:"files"`
}

// Entry returns the manifest entry for key, or nil.
func (m *Manifest) Entry(key string) *FileEntry {
	for i := range m.Files {
		if m.Files[i].Key == key {
			return &m.Files[i]
		}
	}
	return nil
}

// Store manages staged and published bundles under a single root.
type Store struct {
	root    string // published bundles live at root/<bundleID>
	staging string // in-flight uploads live at root/.staging/<bundleID>
	cfg     Config
}

// NewStore creates the root and staging directories if needed.
func NewStore(root string, cfg Config) (*Store, error) {
	cfg.withDefaults()
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	s := &Store{root: abs, staging: filepath.Join(abs, stagingDirName), cfg: cfg}
	if err := os.MkdirAll(s.staging, 0o750); err != nil {
		return nil, err
	}
	return s, nil
}

// Root returns the published root directory.
func (s *Store) Root() string { return s.root }

// Staging returns the dedicated staging directory.
func (s *Store) Staging() string { return s.staging }

// Errors returned by the store.
var (
	ErrTooManyFiles  = errors.New("bundle: too many files")
	ErrFileTooLarge  = errors.New("bundle: file exceeds per-file limit")
	ErrTotalTooLarge = errors.New("bundle: total bytes exceed limit")
	ErrNotFound      = errors.New("bundle: not found")
	ErrInvalidID     = errors.New("bundle: invalid id or key")
)

// Session is one in-flight upload. It is not safe for concurrent use.
type Session struct {
	store    *Store
	dir      string
	manifest Manifest
	total    int64
	done     bool // true once finished; Abort becomes a no-op after Commit
}

// Begin creates a fresh staging directory for a new bundle.
func (s *Store) Begin() (*Session, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.staging, id)
	if err := os.Mkdir(dir, 0o750); err != nil {
		return nil, err
	}
	return &Session{
		store: s,
		dir:   dir,
		manifest: Manifest{
			ID:        id,
			CreatedAt: s.cfg.Now(),
		},
	}, nil
}

// ID returns the server-generated bundle ID.
func (se *Session) ID() string { return se.manifest.ID }

// AddFile streams one file into the staging directory under a fresh
// server-generated key. Byte limits are enforced on bytes actually read
// from r, never on client-supplied lengths. Duplicate display names are
// allowed; keys never collide. Empty files are recorded too.
func (se *Session) AddFile(name string, r io.Reader) (*FileEntry, error) {
	if len(se.manifest.Files) >= se.store.cfg.MaxFiles {
		return nil, ErrTooManyFiles
	}
	key, err := newID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(se.dir, key)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}

	// Read at most MaxFileBytes+1 so an oversize file is detected by
	// what was actually read, not by any advertised length.
	limited := &io.LimitedReader{R: r, N: se.store.cfg.MaxFileBytes + 1}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), limited)
	closeErr := f.Close()

	fail := func(err error) (*FileEntry, error) {
		_ = os.Remove(path)
		return nil, err
	}
	if copyErr != nil {
		return fail(copyErr)
	}
	if closeErr != nil {
		return fail(closeErr)
	}
	if n > se.store.cfg.MaxFileBytes {
		return fail(ErrFileTooLarge)
	}
	if se.total+n > se.store.cfg.MaxTotalBytes {
		return fail(ErrTotalTooLarge)
	}
	se.total += n

	entry := FileEntry{
		Key:    key,
		Name:   name,
		Size:   n,
		SHA256: hex.EncodeToString(hash.Sum(nil)),
	}
	se.manifest.Files = append(se.manifest.Files, entry)
	return &se.manifest.Files[len(se.manifest.Files)-1], nil
}

// Commit writes the manifest into the staging directory and publishes
// the bundle by renaming the directory into the root on the same
// filesystem. On any failure the staging directory is removed and the
// bundle never becomes visible.
func (se *Session) Commit() (*Manifest, error) {
	if se.done {
		return nil, errors.New("bundle: session already finished")
	}
	data, err := json.MarshalIndent(se.manifest, "", "  ")
	if err != nil {
		_ = se.Abort()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(se.dir, manifestName), data, 0o600); err != nil {
		_ = se.Abort()
		return nil, err
	}
	target := filepath.Join(se.store.root, se.manifest.ID)
	if err := se.store.cfg.Rename(se.dir, target); err != nil {
		_ = se.Abort()
		return nil, fmt.Errorf("bundle: publish failed: %w", err)
	}
	se.done = true
	m := se.manifest
	return &m, nil
}

// Abort removes the staging directory of this session only. It never
// touches other sessions or published bundles, and is a no-op after a
// successful Commit.
func (se *Session) Abort() error {
	if se.done {
		return nil
	}
	se.done = true
	return os.RemoveAll(se.dir)
}

// LoadManifest reads the manifest of a published bundle.
func (s *Store) LoadManifest(id string) (*Manifest, error) {
	if !IsValidID(id) {
		return nil, ErrInvalidID
	}
	data, err := os.ReadFile(filepath.Join(s.root, id, manifestName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Open returns the published file for (id, key). The key must be listed
// in the published manifest, so downloads can never reach arbitrary
// disk paths even if a caller forges an ID or key.
func (s *Store) Open(id, key string) (*os.File, *FileEntry, error) {
	if !IsValidID(id) || !IsValidID(key) {
		return nil, nil, ErrInvalidID
	}
	m, err := s.LoadManifest(id)
	if err != nil {
		return nil, nil, err
	}
	entry := m.Entry(key)
	if entry == nil {
		return nil, nil, ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.root, id, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return f, entry, nil
}

// CleanupStaging removes staging directories older than the configured
// maximum age, using the store clock. It only ever removes entries
// directly inside the dedicated staging directory: symlinks are removed
// as links (never followed), and published bundles are untouched.
// Returns the number of removed entries.
func (s *Store) CleanupStaging() (int, error) {
	entries, err := os.ReadDir(s.staging)
	if err != nil {
		return 0, err
	}
	now := s.cfg.Now()
	removed := 0
	for _, e := range entries {
		// DirEntry.Info from os.ReadDir reports the link itself, not
		// its target, so a symlink's own age decides its removal.
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < s.cfg.StagingMaxAge {
			continue
		}
		// os.RemoveAll on a symlink removes the link, not the target.
		if err := os.RemoveAll(filepath.Join(s.staging, e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
