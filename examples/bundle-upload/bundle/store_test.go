package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "data"), cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func stagingEntries(t *testing.T, s *Store) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(s.Staging())
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	return entries
}

func publishedEntries(t *testing.T, s *Store) []os.DirEntry {
	t.Helper()
	all, err := os.ReadDir(s.Root())
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	var out []os.DirEntry
	for _, e := range all {
		if e.Name() != stagingDirName {
			out = append(out, e)
		}
	}
	return out
}

func TestAddFileDuplicateNamesAndEmptyFile(t *testing.T) {
	s := newTestStore(t, Config{})
	se, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	e1, err := se.AddFile("same.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	e2, err := se.AddFile("same.txt", strings.NewReader("world!"))
	if err != nil {
		t.Fatal(err)
	}
	e3, err := se.AddFile("empty.txt", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if e1.Key == e2.Key {
		t.Fatal("duplicate display names must still get distinct keys")
	}
	if e3.Size != 0 {
		t.Fatalf("empty file size = %d, want 0", e3.Size)
	}
	emptySum := sha256.Sum256(nil)
	if e3.SHA256 != hex.EncodeToString(emptySum[:]) {
		t.Fatal("empty file must still be hashed and listed")
	}
	m, err := se.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 3 {
		t.Fatalf("manifest files = %d, want 3", len(m.Files))
	}
}

func TestLimitsCountedFromActualReads(t *testing.T) {
	// Content-Length is not involved at all: limits apply to bytes read.
	s := newTestStore(t, Config{MaxFileBytes: 4, MaxTotalBytes: 100})
	se, _ := s.Begin()
	if _, err := se.AddFile("a", strings.NewReader("12345")); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
	_ = se.Abort()

	s2 := newTestStore(t, Config{MaxFileBytes: 100, MaxTotalBytes: 9})
	se2, _ := s2.Begin()
	if _, err := se2.AddFile("a", strings.NewReader("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := se2.AddFile("b", strings.NewReader("12345")); !errors.Is(err, ErrTotalTooLarge) {
		t.Fatalf("err = %v, want ErrTotalTooLarge", err)
	}
	if _, err := se2.Commit(); err != nil {
		t.Fatal(err)
	}
	m, err := s2.LoadManifest(se2.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 {
		t.Fatalf("committed files = %d, want 1 (rejected file must not appear)", len(m.Files))
	}
}

func TestTooManyFiles(t *testing.T) {
	s := newTestStore(t, Config{MaxFiles: 1})
	se, _ := s.Begin()
	if _, err := se.AddFile("a", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := se.AddFile("b", strings.NewReader("y")); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("err = %v, want ErrTooManyFiles", err)
	}
	_ = se.Abort()
}

func TestAbortRemovesOnlyOwnStaging(t *testing.T) {
	s := newTestStore(t, Config{})
	se1, _ := s.Begin()
	se2, _ := s.Begin()
	if _, err := se1.AddFile("a", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := se2.AddFile("b", strings.NewReader("y")); err != nil {
		t.Fatal(err)
	}
	if err := se1.Abort(); err != nil {
		t.Fatal(err)
	}
	entries := stagingEntries(t, s)
	if len(entries) != 1 || entries[0].Name() != se2.ID() {
		t.Fatalf("staging = %v, want only session 2", entries)
	}
	if _, err := se2.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(stagingEntries(t, s)) != 0 {
		t.Fatal("staging must be empty after commit")
	}
}

func TestCommitRenameFailureCleansUp(t *testing.T) {
	s := newTestStore(t, Config{
		Rename: func(oldpath, newpath string) error { return errors.New("boom") },
	})
	se, _ := s.Begin()
	if _, err := se.AddFile("a", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := se.Commit(); err == nil {
		t.Fatal("commit must fail when rename fails")
	}
	if len(stagingEntries(t, s)) != 0 {
		t.Fatal("staging dir must be removed after publish failure")
	}
	if len(publishedEntries(t, s)) != 0 {
		t.Fatal("nothing may be published after publish failure")
	}
}

func TestOpenRejectsForgedIDsAndKeys(t *testing.T) {
	s := newTestStore(t, Config{})
	se, _ := s.Begin()
	entry, _ := se.AddFile("a.txt", strings.NewReader("x"))
	if _, err := se.Commit(); err != nil {
		t.Fatal(err)
	}

	bad := []string{"..", "../..", "..%2f..", "sentinel.txt", strings.Repeat("g", 32), ""}
	for _, b := range bad {
		if _, _, err := s.Open(b, entry.Key); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("Open(%q, key) err = %v, want ErrInvalidID", b, err)
		}
		if _, _, err := s.Open(se.ID(), b); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("Open(id, %q) err = %v, want ErrInvalidID", b, err)
		}
	}
	// Well-formed but unknown key must not resolve either.
	other := strings.Repeat("ab", 16)
	if _, _, err := s.Open(se.ID(), other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(id, unknown key) err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Open(other, entry.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(unknown id, key) err = %v, want ErrNotFound", err)
	}
}

func TestCleanupStagingRemovesOnlyExpired(t *testing.T) {
	// Fake clock sits 48h in the future so any entry with a real
	// filesystem mtime counts as expired; entries we want to keep are
	// explicitly re-dated to the fake "now".
	fakeNow := time.Now().Add(48 * time.Hour)
	s := newTestStore(t, Config{
		StagingMaxAge: time.Hour,
		Now:           func() time.Time { return fakeNow },
	})

	old := filepath.Join(s.Staging(), strings.Repeat("01", 16))
	fresh := filepath.Join(s.Staging(), strings.Repeat("02", 16))
	for _, d := range []string{old, fresh} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(fresh, fakeNow, fakeNow); err != nil {
		t.Fatal(err)
	}

	// A published bundle, deliberately aged: cleanup must never touch it.
	se, _ := s.Begin()
	if _, err := se.AddFile("a", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := se.Commit(); err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(s.Root(), se.ID())

	// A symlink inside staging pointing at the published bundle: cleanup
	// may remove the link but must never follow it into the target.
	link := filepath.Join(s.Staging(), strings.Repeat("03", 16))
	linkCreated := false
	if err := os.Symlink(published, link); err != nil {
		t.Logf("symlink not permitted here, skipping link case: %v", err)
	} else {
		linkCreated = true
	}

	removed, err := s.CleanupStaging()
	if err != nil {
		t.Fatal(err)
	}
	wantRemoved := 1
	if linkCreated {
		wantRemoved = 2
	}
	if removed != wantRemoved {
		t.Fatalf("removed = %d, want %d", removed, wantRemoved)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired staging dir must be removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh staging dir must be kept")
	}
	if _, err := os.Stat(filepath.Join(published, manifestName)); err != nil {
		t.Fatal("published bundle must survive cleanup (symlink must not be followed)")
	}
}
