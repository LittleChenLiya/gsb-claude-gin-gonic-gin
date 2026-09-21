package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}
	return NewStore(cfg, nil)
}

func TestAddFileLimitsAndEmptyFile(t *testing.T) {
	s := testStore(t, Config{MaxFiles: 2, MaxFileBytes: 4, MaxTotalBytes: 9})
	st, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort(st)

	// Zero-byte file is a valid manifest member.
	emptyHash := sha256.Sum256(nil)
	if err := s.AddFile(st, "empty.bin", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	got := st.manifest.Files[0]
	if got.Size != 0 || got.SHA256 != hex.EncodeToString(emptyHash[:]) {
		t.Fatalf("empty file entry = %+v", got)
	}

	// Per-file limit is enforced by bytes actually read.
	if err := s.AddFile(st, "big.bin", strings.NewReader("12345")); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("want ErrFileTooLarge, got %v", err)
	}
	if err := s.AddFile(st, "ok.bin", strings.NewReader("1234")); err != nil {
		t.Fatal(err)
	}

	// File count limit.
	if err := s.AddFile(st, "third.bin", strings.NewReader("")); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("want ErrTooManyFiles, got %v", err)
	}
}

func TestAddFileTotalLimitCountsRealBytes(t *testing.T) {
	s := testStore(t, Config{MaxFiles: 4, MaxFileBytes: 100, MaxTotalBytes: 6})
	st, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort(st)
	if err := s.AddFile(st, "a", strings.NewReader("1234")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddFile(st, "b", strings.NewReader("1234")); !errors.Is(err, ErrBundleTooLarge) {
		t.Fatalf("want ErrBundleTooLarge, got %v", err)
	}
}

func TestAddFileReadError(t *testing.T) {
	s := testStore(t, Config{MaxFiles: 1, MaxFileBytes: 100, MaxTotalBytes: 100})
	st, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort(st)
	boom := errors.New("read failed")
	r := io.MultiReader(strings.NewReader("partial"), &failReader{err: boom})
	if err := s.AddFile(st, "x", r); !errors.Is(err, boom) {
		t.Fatalf("want read error, got %v", err)
	}
}

type failReader struct{ err error }

func (f *failReader) Read([]byte) (int, error) { return 0, f.err }

func TestPublishRenameFailureCleansStaging(t *testing.T) {
	s := testStore(t, Config{MaxFiles: 1, MaxFileBytes: 10, MaxTotalBytes: 10})
	s.rename = func(_, _ string) error { return errors.New("rename failed") }
	st, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddFile(st, "a", strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(st); err == nil {
		t.Fatal("expected publish error")
	}
	if _, err := os.Stat(st.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging dir must be removed after failed publish, stat err=%v", err)
	}
	if _, _, err := s.Lookup(st.ID, "file-000-0000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bundle must not be visible, got %v", err)
	}
}

func TestLookupRejectsTraversal(t *testing.T) {
	s := testStore(t, Config{})
	for _, id := range []string{"..", "../..", "a/b", `a\b`, ".", ""} {
		if _, _, err := s.Lookup(id, "file-000-abcd"); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("id %q: want ErrInvalidKey, got %v", id, err)
		}
	}
	for _, key := range []string{"..", "../sentinel", `..\sentinel`, "a/b", ""} {
		if _, _, err := s.Lookup("abc123", key); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("key %q: want ErrInvalidKey, got %v", key, err)
		}
	}
}

func TestCleanupStagingRemovesOnlyStale(t *testing.T) {
	root := t.TempDir()
	base := time.Now()
	now := base
	s := NewStore(Config{Root: root, StagingTTL: time.Hour}, func() time.Time { return now })

	staging := s.stagingRoot()
	oldDir := filepath.Join(staging, "old-bundle")
	newDir := filepath.Join(staging, "new-bundle")
	for _, d := range []string{oldDir, newDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	stale := base.Add(-2 * time.Hour)
	if err := os.Chtimes(oldDir, stale, stale); err != nil {
		t.Fatal(err)
	}

	// A published bundle with an old mtime must survive cleanup.
	published := filepath.Join(s.bundlesRoot(), "published1")
	if err := os.MkdirAll(published, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(published, stale, stale); err != nil {
		t.Fatal(err)
	}

	// A symlink inside staging must not be followed to the published tree.
	link := filepath.Join(staging, "link")
	symlinkOK := os.Symlink(s.bundlesRoot(), link) == nil

	removed, err := s.CleanupStaging()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(oldDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale staging dir must be removed")
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Fatal("fresh staging dir must be kept")
	}
	if _, err := os.Stat(published); err != nil {
		t.Fatal("published bundle must never be removed by cleanup")
	}
	if symlinkOK {
		if _, err := os.Stat(filepath.Join(s.bundlesRoot(), "published1")); err != nil {
			t.Fatal("cleanup followed a symlink into the published tree")
		}
	}
}
