// Package main implements a multi-file bundle upload example for Gin.
//
// Storage is split into testable components: staging, manifest, publish and
// cleanup. A bundle becomes visible to the download endpoint only after every
// file has been fully written and validated, and the whole bundle has been
// published with a single directory rename on the same filesystem.
package main

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

// FileEntry describes one stored file inside a published bundle.
type FileEntry struct {
	Key    string `json:"key"`    // server-generated storage key
	Name   string `json:"name"`   // client-supplied display name
	Size   int64  `json:"size"`   // bytes actually read
	SHA256 string `json:"sha256"` // hex digest of the stored bytes
}

// Manifest is the single source of truth for what a bundle contains.
type Manifest struct {
	ID    string      `json:"id"`
	Files []FileEntry `json:"files"`
}

// Config controls limits and locations for the bundle store.
type Config struct {
	// Root is the target root directory. Staging lives under
	// Root/.staging, published bundles under Root/bundles.
	Root string
	// MaxFiles is the maximum number of files per bundle.
	MaxFiles int
	// MaxFileBytes is the maximum size of a single file.
	MaxFileBytes int64
	// MaxTotalBytes is the maximum sum of file bytes per bundle,
	// accounted by bytes actually read, never Content-Length.
	MaxTotalBytes int64
	// StagingTTL is how long an abandoned staging directory may live
	// before startup cleanup removes it.
	StagingTTL time.Duration
}

var (
	ErrTooManyFiles   = errors.New("bundle: too many files")
	ErrFileTooLarge   = errors.New("bundle: file exceeds per-file limit")
	ErrBundleTooLarge = errors.New("bundle: total bytes exceed limit")
	ErrNotFound       = errors.New("bundle: not found")
	ErrInvalidKey     = errors.New("bundle: invalid bundle id or file key")
)

// idRe matches only the identifiers this store generates itself. Anything
// else (path traversal, separators, dots) is rejected before touching disk.
var idRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// Store manages staging, publishing and lookup of bundles.
type Store struct {
	cfg Config
	// now is injectable so cleanup tests control time.
	now func() time.Time
	// rename is injectable so tests can force publish failures.
	rename func(oldpath, newpath string) error
}

// NewStore creates a Store. now may be nil, defaulting to time.Now.
func NewStore(cfg Config, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{cfg: cfg, now: now, rename: os.Rename}
}

func (s *Store) stagingRoot() string { return filepath.Join(s.cfg.Root, ".staging") }
func (s *Store) bundlesRoot() string { return filepath.Join(s.cfg.Root, "bundles") }

func newToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Staging tracks one in-progress upload. It is request-scoped and not
// shared between goroutines.
type Staging struct {
	ID       string
	Dir      string
	manifest Manifest
	total    int64
}

// Begin creates a fresh, private staging directory for a new bundle.
func (s *Store) Begin() (*Staging, error) {
	id, err := newToken(16)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.stagingRoot(), id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Staging{ID: id, Dir: dir, manifest: Manifest{ID: id}}, nil
}

// AddFile streams r into the staging directory under a server-generated
// key, enforcing per-file and cumulative bundle limits by bytes actually
// read. The client name is kept only as display metadata. Empty files are
// valid and recorded in the manifest.
func (s *Store) AddFile(st *Staging, name string, r io.Reader) error {
	if len(st.manifest.Files) >= s.cfg.MaxFiles {
		return ErrTooManyFiles
	}
	suffix, err := newToken(4)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("file-%03d-%s", len(st.manifest.Files), suffix)
	path := filepath.Join(st.Dir, key)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	// Read at most MaxFileBytes+1 so an oversize file is detected by
	// counting real bytes, not headers.
	lr := &io.LimitedReader{R: r, N: s.cfg.MaxFileBytes + 1}
	n, copyErr := io.Copy(io.MultiWriter(f, h), lr)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > s.cfg.MaxFileBytes {
		return ErrFileTooLarge
	}
	if st.total+n > s.cfg.MaxTotalBytes {
		return ErrBundleTooLarge
	}
	st.total += n
	st.manifest.Files = append(st.manifest.Files, FileEntry{
		Key:    key,
		Name:   name,
		Size:   n,
		SHA256: hex.EncodeToString(h.Sum(nil)),
	})
	return nil
}

// Publish writes the manifest and atomically renames the staging directory
// into the published bundles tree. On any failure the staging directory is
// removed; the bundle never becomes partially visible.
func (s *Store) Publish(st *Staging) error {
	data, err := json.MarshalIndent(&st.manifest, "", "  ")
	if err != nil {
		s.Abort(st)
		return err
	}
	if err := os.WriteFile(filepath.Join(st.Dir, "manifest.json"), data, 0o600); err != nil {
		s.Abort(st)
		return err
	}
	if err := os.MkdirAll(s.bundlesRoot(), 0o750); err != nil {
		s.Abort(st)
		return err
	}
	if err := s.rename(st.Dir, filepath.Join(s.bundlesRoot(), st.ID)); err != nil {
		s.Abort(st)
		return err
	}
	return nil
}

// Abort removes the staging directory of this request only. Other
// concurrent uploads keep their own directories.
func (s *Store) Abort(st *Staging) {
	_ = os.RemoveAll(st.Dir)
}

// Lookup resolves a published bundle id and file key to a manifest entry
// and an on-disk path. Only keys listed in the published manifest are
// served; arbitrary disk paths are rejected.
func (s *Store) Lookup(id, key string) (FileEntry, string, error) {
	if !idRe.MatchString(id) || !idRe.MatchString(key) {
		return FileEntry{}, "", ErrInvalidKey
	}
	dir := filepath.Join(s.bundlesRoot(), id)
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileEntry{}, "", ErrNotFound
		}
		return FileEntry{}, "", err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return FileEntry{}, "", err
	}
	for _, e := range m.Files {
		if e.Key == key && idRe.MatchString(e.Key) {
			return e, filepath.Join(dir, e.Key), nil
		}
	}
	return FileEntry{}, "", ErrNotFound
}

// CleanupStaging removes staging directories older than the configured TTL.
// It never follows symlinks and never touches the published bundles tree.
// It is meant to run at startup to reclaim space after a process crash.
func (s *Store) CleanupStaging() (int, error) {
	entries, err := os.ReadDir(s.stagingRoot())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		// DirEntry.Info reports the link itself, not its target, so
		// symlinks are classified without being followed.
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.IsDir() {
			continue
		}
		if s.now().Sub(info.ModTime()) < s.cfg.StagingTTL {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.stagingRoot(), e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
