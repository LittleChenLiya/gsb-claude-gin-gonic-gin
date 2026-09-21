package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

type testFile struct {
	name    string
	content string
}

func multipartBody(t *testing.T, files []testFile) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range files {
		fw, err := w.CreateFormFile("files", f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fw, f.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func doUpload(t *testing.T, router http.Handler, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/bundles", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func uploadOK(t *testing.T, router http.Handler, files []testFile) Manifest {
	t.Helper()
	body, ct := multipartBody(t, files)
	rec := doUpload(t, router, body, ct)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var m Manifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func download(t *testing.T, router http.Handler, id, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/bundles/"+id+"/files/"+key, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestUploadTwoSameNameFilesEndToEnd(t *testing.T) {
	s := newTestStore(t, Config{})
	router := NewRouter(s)

	m := uploadOK(t, router, []testFile{
		{name: "same.txt", content: "first"},
		{name: "same.txt", content: "second"},
	})
	if len(m.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(m.Files))
	}
	if m.Files[0].Key == m.Files[1].Key {
		t.Fatal("keys must be unique even for duplicate display names")
	}
	for i, want := range []string{"first", "second"} {
		entry := m.Files[i]
		if entry.Name != "same.txt" {
			t.Fatalf("display name = %q", entry.Name)
		}
		sum := sha256.Sum256([]byte(want))
		if entry.SHA256 != hex.EncodeToString(sum[:]) || entry.Size != int64(len(want)) {
			t.Fatalf("manifest entry %d mismatch: %+v", i, entry)
		}
		rec := download(t, router, m.ID, entry.Key)
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Fatalf("download = %d %q, want %q", rec.Code, rec.Body.String(), want)
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "same.txt") {
			t.Fatalf("Content-Disposition = %q", cd)
		}
	}
}

func TestUploadZeroByteFileEndToEnd(t *testing.T) {
	s := newTestStore(t, Config{})
	router := NewRouter(s)

	m := uploadOK(t, router, []testFile{{name: "empty.bin", content: ""}})
	if len(m.Files) != 1 || m.Files[0].Size != 0 {
		t.Fatalf("manifest = %+v", m.Files)
	}
	emptySum := sha256.Sum256(nil)
	if m.Files[0].SHA256 != hex.EncodeToString(emptySum[:]) {
		t.Fatal("zero-byte file must be hashed into the manifest")
	}
	rec := download(t, router, m.ID, m.Files[0].Key)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("download = %d len %d", rec.Code, rec.Body.Len())
	}
}

func TestUploadTotalLimitExceeded(t *testing.T) {
	s := newTestStore(t, Config{MaxFileBytes: 100, MaxTotalBytes: 10})
	router := NewRouter(s)

	body, ct := multipartBody(t, []testFile{
		{name: "a", content: "12345678"},
		{name: "b", content: "12345678"},
	})
	rec := doUpload(t, router, body, ct)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(stagingEntries(t, s)) != 0 {
		t.Fatal("staging must be cleaned after limit failure")
	}
	if len(publishedEntries(t, s)) != 0 {
		t.Fatal("no half bundle may become visible")
	}
}

// errAfterReader yields all of data, then fails like a broken connection.
type errAfterReader struct {
	data []byte
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, errors.New("connection reset")
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func TestUploadSecondFileReadFails(t *testing.T) {
	s := newTestStore(t, Config{})
	router := NewRouter(s)

	body, ct := multipartBody(t, []testFile{
		{name: "ok.txt", content: "fine"},
		{name: "broken.txt", content: strings.Repeat("x", 4096)},
	})
	raw := body.Bytes()
	// Cut the stream inside the second file, then fail the read.
	cut := raw[:len(raw)-2048]
	rec := doUpload(t, router, &errAfterReader{data: cut}, ct)
	if rec.Code == http.StatusCreated {
		t.Fatal("upload with a failing second file must not succeed")
	}
	if len(stagingEntries(t, s)) != 0 {
		t.Fatal("staging must be cleaned after read failure")
	}
	if len(publishedEntries(t, s)) != 0 {
		t.Fatal("first file must not become downloadable when the second fails")
	}
}

func TestUploadPublishRenameFails(t *testing.T) {
	failRename := true
	s := newTestStore(t, Config{
		Rename: func(oldpath, newpath string) error {
			if failRename {
				return errors.New("rename boom")
			}
			return os.Rename(oldpath, newpath)
		},
	})
	router := NewRouter(s)

	body, ct := multipartBody(t, []testFile{{name: "a.txt", content: "data"}})
	rec := doUpload(t, router, body, ct)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(stagingEntries(t, s)) != 0 {
		t.Fatal("staging must be cleaned after publish failure")
	}
	if len(publishedEntries(t, s)) != 0 {
		t.Fatal("nothing may be visible after publish failure")
	}

	// The store keeps working for later requests.
	failRename = false
	m := uploadOK(t, router, []testFile{{name: "b.txt", content: "ok"}})
	if rec := download(t, router, m.ID, m.Files[0].Key); rec.Code != http.StatusOK {
		t.Fatalf("later upload must succeed, got %d", rec.Code)
	}
}

func TestConcurrentFailureDoesNotAffectOthers(t *testing.T) {
	s := newTestStore(t, Config{})
	router := NewRouter(s)

	const good = 4
	manifests := make([]Manifest, good)
	var wg sync.WaitGroup
	for i := 0; i < good; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, ct := multipartBody(t, []testFile{
				{name: fmt.Sprintf("f%d.txt", i), content: strings.Repeat("y", 512)},
			})
			rec := doUpload(t, router, body, ct)
			if rec.Code != http.StatusCreated {
				t.Errorf("concurrent upload %d status = %d", i, rec.Code)
				return
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &manifests[i])
		}(i)
	}
	// A concurrent upload whose second file breaks mid-stream.
	wg.Add(1)
	go func() {
		defer wg.Done()
		body, ct := multipartBody(t, []testFile{
			{name: "ok.txt", content: "fine"},
			{name: "broken.txt", content: strings.Repeat("x", 4096)},
		})
		raw := body.Bytes()
		rec := doUpload(t, router, &errAfterReader{data: raw[:len(raw)-2048]}, ct)
		if rec.Code == http.StatusCreated {
			t.Error("broken upload must not succeed")
		}
	}()
	wg.Wait()

	for i, m := range manifests {
		if m.ID == "" || len(m.Files) != 1 {
			t.Errorf("manifest %d missing", i)
			continue
		}
		if rec := download(t, router, m.ID, m.Files[0].Key); rec.Code != http.StatusOK {
			t.Errorf("good bundle %d not downloadable: %d", i, rec.Code)
		}
	}
	if got := len(publishedEntries(t, s)); got != good {
		t.Fatalf("published bundles = %d, want %d (no half bundle)", got, good)
	}
	if len(stagingEntries(t, s)) != 0 {
		t.Fatal("staging must be empty after all requests finish")
	}
}

func TestPathTraversalAndForgedKeysCannotReachSentinel(t *testing.T) {
	parent := t.TempDir()
	sentinel := filepath.Join(parent, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(filepath.Join(parent, "data"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(s)

	// A traversal file name is only a display name; it must not escape.
	m := uploadOK(t, router, []testFile{{name: "../../sentinel.txt", content: "harmless"}})
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "top secret" {
		t.Fatal("sentinel outside root must be untouched")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "sentinel.txt" && e.Name() != "data" {
			t.Fatalf("unexpected file outside root: %s", e.Name())
		}
	}

	// Forged download keys: unknown well-formed key, and traversal-ish
	// strings. None may reach the sentinel.
	forged := strings.Repeat("cd", 16)
	if rec := download(t, router, m.ID, forged); rec.Code != http.StatusNotFound {
		t.Fatalf("forged key status = %d", rec.Code)
	}
	for _, key := range []string{"..", "sentinel.txt", "..%2fsentinel.txt", "%2e%2e"} {
		req := httptest.NewRequest(http.MethodGet, "/bundles/"+m.ID+"/files/"+key, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "top secret") {
			t.Fatalf("key %q leaked the sentinel", key)
		}
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest && rec.Code != http.StatusMovedPermanently {
			t.Fatalf("key %q unexpected status %d", key, rec.Code)
		}
	}
	// The manifest itself only ever references server-generated keys.
	for _, f := range m.Files {
		if !IsValidID(f.Key) {
			t.Fatalf("manifest key %q is not a server-generated ID", f.Key)
		}
	}
}
