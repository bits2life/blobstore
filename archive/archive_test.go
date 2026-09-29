package archive

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.bits2life.com/blobstore"
)

func TestValidateArchivePath(t *testing.T) {
	valid := map[string]string{"a.txt": "a.txt", "dir/./b.txt": "dir/b.txt", `win\c.txt`: "win/c.txt"}
	for in, want := range valid {
		got, err := validateArchivePath(in)
		if err != nil || got != want {
			t.Errorf("validateArchivePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "/abs.txt", "../up.txt", "a/../../up.txt", "."} {
		if _, err := validateArchivePath(in); err == nil {
			t.Errorf("validateArchivePath(%q) accepted", in)
		}
	}
}

func TestValidateZipRequestRejectsDuplicatePaths(t *testing.T) {
	h := strings.Repeat("a", 64)
	_, _, err := validateZipRequest(&ZipRequest{Contents: []ContentItem{{Hash: h, Path: "x"}, {Hash: h, Path: "./x"}}})
	if err == nil {
		t.Fatal("expected duplicate path error")
	}
}

func TestZip(t *testing.T) {
	store, err := blobstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hashA, _ := store.Save(strings.NewReader("alpha"))
	hashB, _ := store.Save(strings.NewReader("beta"))
	missing := strings.Repeat("a", 64)
	h := http.StripPrefix("/archive", NewHandler(store))

	post := func(req ZipRequest) *httptest.ResponseRecorder {
		body, _ := json.Marshal(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/archive/zip", bytes.NewReader(body)))
		return rec
	}

	rec := post(ZipRequest{Contents: []ContentItem{{Hash: hashA, Path: "a.txt"}, {Hash: missing, Path: "m.txt"}}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("strict: status %d", rec.Code)
	}

	rec = post(ZipRequest{
		Filename: "my files",
		Mode:     ModeBestEffort,
		Contents: []ContentItem{{Hash: hashA, Path: "a.txt"}, {Hash: missing, Path: "m.txt"}, {Hash: hashB, Path: "nested/b.txt"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("best-effort: status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Archive-Skipped") != "1" || rec.Header().Get("X-Archive-Written") != "2" {
		t.Fatalf("headers: %v", rec.Header())
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="my files.zip"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		got[f.Name] = string(b)
	}
	if len(got) != 2 || got["a.txt"] != "alpha" || got["nested/b.txt"] != "beta" {
		t.Fatalf("zip contents = %v", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/archive/zip", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status %d", rec.Code)
	}
}
