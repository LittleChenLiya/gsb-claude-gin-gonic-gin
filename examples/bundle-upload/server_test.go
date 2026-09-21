package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func newTestRouter(t *testing.T, mutate func(*Config)) (*Store, *gin.Engine) {
	t.Helper()
	cfg := Config{
		Root:          t.TempDir(),
		MaxFiles:      8,
		MaxFileBytes:  1 << 20,
		MaxTotalBytes: 4 << 20,
		StagingTTL:    time.Hour,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	store := NewStore(cfg, nil)
	return store, NewRouter(store)
}

type testFile struct {
	name string
	data []byte
}

func buildUploadBody(files []testFile) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range files {
		fw, err := w.CreateFormFile("file", f.name)
		if err != nil {
			return nil, "", err
		}
		if _, err := fw.Write(f.data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

func uploadRequest(t *testing.T, files []testFile) *http.Request {
	t.Helper()
	body, contentType, err := buildUploadBody(files)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/bundles", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	return req
}

func serve(router *gin.Engine, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func mustUpload(t *testing.T, router *gin.Engine, files []testFile) Manifest {
	t.Helper()
	rec := serve(router, uploadRequest(t, files))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var m Manifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestUploadSuccessDuplicateNamesAndEmptyFile(t *testing.T) {
	_, router := newTestRouter(t, nil)
	files := []testFile{
		{name: "report.txt", data: []byte("first")},
		{name: "report.txt", data: []byte("second")},
		{name: "empty.bin", data: nil},
	}
	m := mustUpload(t, router, files)
	if len(m.Files) != 3 {
		t.Fatalf("manifest files = %d, want 3", len(m.Files))
	}
	// Duplicate display names are allowed; storage keys must differ.
	keys := map[string]bool{}
	for _, f := range m.Files {
		if keys[f.Key] {
			t.Fatalf("duplicate storage key %q", f.Key)
		}
		keys[f.Key] = true
	}
	if m.Files[0].Name != "report.txt" || m.Files[1].Name != "report.txt" {
		t.Fatalf("display names not preserved: %+v", m.Files)
	}
	wantData := [][]byte{[]byte("first"), []byte("second"), {}}
	for i, f := range m.Files {
		if f.Size != int64(len(wantData[i])) {
			t.Fatalf("file %d size = %d, want %d", i, f.Size, len(wantData[i]))
		}
		if f.SHA256 != sha256Hex(wantData[i]) {
			t.Fatalf("file %d sha256 mismatch", i)
		}
		rec := serve(router, httptest.NewRequest(http.MethodGet, "/bundles/"+m.ID+"/files/"+f.Key, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("download %d status = %d", i, rec.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), wantData[i]) {
			t.Fatalf("download %d body = %q", i, rec.Body.Bytes())
		}
	}
}

func TestUploadTotalLimitExceeded(t *testing.T) {
	store, router := newTestRouter(t, func(c *Config) { c.MaxTotalBytes = 5 })
	rec := serve(router, uploadRequest(t, []testFile{
		{name: "a", data: []byte("1234")},
		{name: "b", data: []byte("1234")},
	}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body = %s", rec.Code, rec.Body.String())
	}
	assertNothingVisible(t, store, router)
}

func TestUploadSecondFileReadFails(t *testing.T) {
	store, router := newTestRouter(t, nil)

	// A body that delivers one complete file, starts a second part, then
	// fails mid-stream (client disconnect / truncated upload).
	var head bytes.Buffer
	w := multipart.NewWriter(&head)
	fw, err := w.CreateFormFile("file", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("complete")); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&head, "\r\n--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"b.txt\"\r\n\r\npartial", w.Boundary())
	req := httptest.NewRequest(http.MethodPost, "/bundles", &errAfterReader{data: head.Bytes()})
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := serve(router, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	assertNothingVisible(t, store, router)

	// Other uploads are unaffected by the failed request.
	m := mustUpload(t, router, []testFile{{name: "ok.txt", data: []byte("fine")}})
	if len(m.Files) != 1 {
		t.Fatalf("follow-up upload files = %d", len(m.Files))
	}
}

type errAfterReader struct {
	data []byte
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, errors.New("connection reset by peer")
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func TestUploadPublishRenameFails(t *testing.T) {
	store, router := newTestRouter(t, nil)
	store.rename = func(_, _ string) error { return errors.New("rename failed") }
	rec := serve(router, uploadRequest(t, []testFile{{name: "a", data: []byte("x")}}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	assertNothingVisible(t, store, router)

	// Once the failure is gone, concurrent uploads still succeed.
	store.rename = os.Rename
	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := []byte(fmt.Sprintf("payload-%d", i))
			body, contentType, err := buildUploadBody([]testFile{{name: "f.txt", data: data}})
			if err != nil {
				t.Errorf("build body: %v", err)
				return
			}
			req := httptest.NewRequest(http.MethodPost, "/bundles", bytes.NewReader(body))
			req.Header.Set("Content-Type", contentType)
			rec := serve(router, req)
			if rec.Code != http.StatusCreated {
				t.Errorf("concurrent upload %d status = %d", i, rec.Code)
				return
			}
			var m Manifest
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
				t.Errorf("decode manifest: %v", err)
				return
			}
			dl := serve(router, httptest.NewRequest(http.MethodGet, "/bundles/"+m.ID+"/files/"+m.Files[0].Key, nil))
			if dl.Code != http.StatusOK || !bytes.Equal(dl.Body.Bytes(), data) {
				t.Errorf("concurrent download %d failed: %d", i, dl.Code)
			}
		}(i)
	}
	wg.Wait()
}

func TestPathTraversalAndForgedKeys(t *testing.T) {
	store, router := newTestRouter(t, nil)

	// Sentinel outside the storage root must be unreachable.
	sentinel := filepath.Join(filepath.Dir(store.cfg.Root), "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A traversal display name is stored as metadata only.
	m := mustUpload(t, router, []testFile{{name: "../../sentinel.txt", data: []byte("x")}})
	if m.Files[0].Key == "" || m.Files[0].Key == "../../sentinel.txt" {
		t.Fatalf("storage key must be server-generated, got %q", m.Files[0].Key)
	}

	forged := []string{
		"/bundles/" + m.ID + "/files/..",
		"/bundles/" + m.ID + "/files/..%2F..%2Fsentinel.txt",
		"/bundles/" + m.ID + "/files/..%5C..%5Csentinel.txt",
		"/bundles/../files/" + m.Files[0].Key,
		"/bundles/" + m.ID + "%2F..%2F..%2Fsentinel/files/x",
		"/bundles/" + m.ID + "/files/manifest",
		"/bundles/" + m.ID + "/files/does-not-exist",
	}
	for _, path := range forged {
		rec := serve(router, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("forged path %q returned 200", path)
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("top secret")) {
			t.Fatalf("forged path %q leaked sentinel content", path)
		}
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "top secret" {
		t.Fatalf("sentinel modified: %q, %v", data, err)
	}
}

// assertNothingVisible verifies that no half-published bundle is reachable
// and that the failed request left no staging residue behind.
func assertNothingVisible(t *testing.T, store *Store, router *gin.Engine) {
	t.Helper()
	entries, err := os.ReadDir(store.stagingRoot())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging root not clean: %d entries", len(entries))
	}
	bundles, err := os.ReadDir(store.bundlesRoot())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, b := range bundles {
		data, err := os.ReadFile(filepath.Join(store.bundlesRoot(), b.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var m Manifest
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		for _, f := range m.Files {
			rec := serve(router, httptest.NewRequest(http.MethodGet, "/bundles/"+m.ID+"/files/"+f.Key, nil))
			if rec.Code == http.StatusOK {
				t.Fatalf("file %s of failed bundle %s is downloadable", f.Key, m.ID)
			}
		}
	}
}
