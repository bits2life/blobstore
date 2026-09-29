package blobstore

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testFile struct {
	name    string
	content []byte
}

func uploadRequest(t *testing.T, target string, files []testFile, tags ...string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range files {
		w, err := mw.CreateFormFile("files[]", f.name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(f.content)
	}
	for _, tag := range tags {
		mw.WriteField("tags", tag)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newTestHandler(t *testing.T, opts HandlerOptions) (*Handler, *Store) {
	t.Helper()
	s := newStore(t)
	if opts.Metadata == nil {
		opts.Metadata = NewMetadataStore(filepath.Join(s.Root(), "metadata.jsonl"))
	}
	return NewHandler(s, "/uploads", opts), s
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestUploadDescribeAndList(t *testing.T) {
	cfg := DefaultImageConfig()
	h, _ := newTestHandler(t, HandlerOptions{Images: &cfg})

	req := uploadRequest(t, "/uploads", []testFile{
		{"pixel.png", pngBytes(t, 2, 3)},
		{"notes.txt", []byte("hello")},
	}, "header, home", "home")
	req = req.WithContext(ContextWithUploader(req.Context(), "erik"))
	rec := serve(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status %d: %s", rec.Code, rec.Body.String())
	}
	results := decode[[]UploadResult](t, rec)
	if len(results) != 2 || results[0].Filename != "pixel.png" || results[1].Filename != "notes.txt" {
		t.Fatalf("results = %+v", results)
	}
	img := results[0]
	if img.ContentType != "image/png" || img.Media == nil || img.Media.Width != 2 || img.Media.Height != 3 || !img.Media.Transformable {
		t.Fatalf("image result = %+v media=%+v", img, img.Media)
	}
	if img.Path != "/uploads/"+img.Hash+"/pixel.png" {
		t.Fatalf("path = %s", img.Path)
	}
	if results[1].Media != nil || results[1].Size != 5 {
		t.Fatalf("text result = %+v", results[1])
	}

	rec = serve(h, httptest.NewRequest(http.MethodGet, "/uploads/_meta/"+img.Hash, nil))
	if meta := decode[BlobMeta](t, rec); meta.Media == nil || meta.Media.Width != 2 {
		t.Fatalf("describe = %s", rec.Body.String())
	}

	rec = serve(h, httptest.NewRequest(http.MethodGet, "/uploads/_meta?include=blob", nil))
	items := decode[[]UploadListItem](t, rec)
	if len(items) != 2 || items[0].Uploader != "erik" || items[0].Blob == nil || strings.Join(items[0].Tags, ",") != "header,home,home" {
		t.Fatalf("list = %s", rec.Body.String())
	}

	patch := httptest.NewRequest(http.MethodPatch, "/uploads/_meta/"+img.Hash, strings.NewReader(`{"tags":["hero"]}`))
	if rec := serve(h, patch); rec.Code != http.StatusOK {
		t.Fatalf("patch status %d", rec.Code)
	}
	rec = serve(h, httptest.NewRequest(http.MethodGet, "/uploads", nil))
	if recs := decode[[]UploadRecord](t, rec); recs[0].Tags[0] != "hero" {
		t.Fatalf("tags not updated: %s", rec.Body.String())
	}
}

func TestTransformableFalseWithoutImages(t *testing.T) {
	h, _ := newTestHandler(t, HandlerOptions{})
	rec := serve(h, uploadRequest(t, "/uploads", []testFile{{"p.png", pngBytes(t, 4, 4)}}))
	res := decode[[]UploadResult](t, rec)
	if res[0].Media == nil || res[0].Media.Transformable {
		t.Fatalf("media = %+v", res[0].Media)
	}
	hash := res[0].Hash
	if rec := serve(h, httptest.NewRequest(http.MethodGet, "/uploads/"+hash+"/w320/p.png", nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("resize without images: status %d", rec.Code)
	}
}

func TestServeBlobHeaders(t *testing.T) {
	h, s := newTestHandler(t, HandlerOptions{})
	hash, _ := s.Save(strings.NewReader("<script>alert(1)</script>"))

	cases := []struct {
		path, contentType, disposition string
	}{
		{"/uploads/" + hash + "/page.html", "application/octet-stream", `attachment; filename=page.html`},
		{"/uploads/" + hash + "/icon.svg", "application/octet-stream", `attachment; filename=icon.svg`},
		{"/uploads/" + hash + "/notes.txt", "text/plain; charset=utf-8", `inline; filename=notes.txt`},
		{"/uploads/" + hash + "/my photo.png", "image/png", `inline; filename="my photo.png"`},
		{"/uploads/" + hash, "application/octet-stream", `attachment`},
	}
	for _, c := range cases {
		rec := serve(h, httptest.NewRequest(http.MethodGet, strings.ReplaceAll(c.path, " ", "%20"), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != c.contentType {
			t.Errorf("%s: Content-Type = %q, want %q", c.path, got, c.contentType)
		}
		if got := rec.Header().Get("Content-Disposition"); got != c.disposition {
			t.Errorf("%s: Content-Disposition = %q, want %q", c.path, got, c.disposition)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", c.path)
		}
		if rec.Header().Get("ETag") != `"sha256-`+hash+`"` {
			t.Errorf("%s: ETag = %q", c.path, rec.Header().Get("ETag"))
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/uploads/"+hash+"/notes.txt", nil)
	req.Header.Set("Range", "bytes=0-7")
	if rec := serve(h, req); rec.Code != http.StatusPartialContent || rec.Body.String() != "<script>" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/uploads/"+hash+"/notes.txt", nil)
	req.Header.Set("If-None-Match", `"sha256-`+hash+`"`)
	if rec := serve(h, req); rec.Code != http.StatusNotModified {
		t.Fatalf("conditional GET: %d", rec.Code)
	}
}

func TestNotFoundAndBadPaths(t *testing.T) {
	h, _ := newTestHandler(t, HandlerOptions{})
	for _, p := range []string{
		"/uploads/" + hashOf("nope"),
		"/uploads/metadata.jsonl",
		"/uploads/../uploads/metadata.jsonl",
		"/uploads/_meta/" + hashOf("nope"),
		"/uploads/_meta/..%2Fmetadata.jsonl",
	} {
		if rec := serve(h, httptest.NewRequest(http.MethodGet, p, nil)); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, rec.Code)
		}
	}
	if rec := serve(h, httptest.NewRequest(http.MethodDelete, "/uploads/"+hashOf("x"), nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: status %d", rec.Code)
	}
}

func TestUploadLimits(t *testing.T) {
	h, s := newTestHandler(t, HandlerOptions{MaxUploadSize: 1024})
	rec := serve(h, uploadRequest(t, "/uploads", []testFile{{"big.bin", bytes.Repeat([]byte("x"), 4096)}}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: status %d", rec.Code)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root(), "blobs")); len(entries) != 0 {
		t.Fatalf("oversized upload left %d blobs", len(entries))
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root(), "tmp")); len(entries) != 0 {
		t.Fatalf("oversized upload left %d temp files", len(entries))
	}
	if rec := serve(h, uploadRequest(t, "/uploads", nil, "tag")); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty upload: status %d", rec.Code)
	}
}

func TestResize(t *testing.T) {
	cfg := DefaultImageConfig()
	cfg.MaxPixels = 1000 * 1000
	h, s := newTestHandler(t, HandlerOptions{Images: &cfg})
	hash, _ := s.Save(bytes.NewReader(pngBytes(t, 800, 400)))

	rec := serve(h, httptest.NewRequest(http.MethodGet, "/uploads/"+hash+"/w320/any-name.jpg", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resize status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("resized Content-Type = %s, want the source format", ct)
	}
	out, err := png.DecodeConfig(rec.Body)
	if err != nil || out.Width != 320 || out.Height != 160 {
		t.Fatalf("resized = %+v, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "transforms", hash+"_w320.png")); err != nil {
		t.Fatalf("resize not cached: %v", err)
	}

	// Wider than the source: the original is served.
	rec = serve(h, httptest.NewRequest(http.MethodGet, "/uploads/"+hash+"/w1280/a.png", nil))
	if out, _ := png.DecodeConfig(rec.Body); out.Width != 800 {
		t.Fatalf("upscale request returned width %d", out.Width)
	}

	if rec := serve(h, httptest.NewRequest(http.MethodGet, "/uploads/"+hash+"/w300/a.png", nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("disallowed width: status %d", rec.Code)
	}
	textHash, _ := s.Save(strings.NewReader("not an image"))
	if rec := serve(h, httptest.NewRequest(http.MethodGet, "/uploads/"+textHash+"/w320/a.png", nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-image resize: status %d", rec.Code)
	}
	bigHash, _ := s.Save(bytes.NewReader(pngBytes(t, 1200, 1000)))
	if rec := serve(h, httptest.NewRequest(http.MethodGet, "/uploads/"+bigHash+"/w320/a.png", nil)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized image: status %d", rec.Code)
	}
}

func TestServesClient(t *testing.T) {
	h, _ := newTestHandler(t, HandlerOptions{})
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/uploads/client/index.js", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || !bytes.Contains(body, []byte("createBlobstoreClient")) {
		t.Fatalf("client: status %d", rec.Code)
	}
}
