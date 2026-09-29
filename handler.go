package blobstore

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed client/index.js
var clientFiles embed.FS

// ClientFS holds the browser client, index.js, an ES module that talks to the
// Handler. The Handler serves it at <prefix>/client/index.js.
var ClientFS fs.FS = mustSub(clientFiles, "client")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

const (
	defaultMaxUploadSize = 64 << 20
	maxFormValueSize     = 64 << 10
	maxJSONBodySize      = 1 << 20
)

// HandlerOptions configures a Handler. The zero value is valid.
type HandlerOptions struct {
	// Metadata enables the upload library (listing and tagging). Nil disables it.
	Metadata *MetadataStore
	// Images enables on-the-fly resizing at <prefix>/<hash>/w<width>/<filename>.
	// Nil disables it.
	Images *ImageConfig
	// MaxUploadSize caps the size of an upload request body. Default 64 MiB.
	MaxUploadSize int64
	// Logger receives errors that are not returned to the client. Default slog.Default().
	Logger *slog.Logger
}

// UploadResult is returned for each file stored by an upload request.
type UploadResult struct {
	Hash        string     `json:"hash"`
	Size        int64      `json:"size"`
	Filename    string     `json:"filename"`
	Path        string     `json:"path"`
	ContentType string     `json:"content_type,omitempty"`
	Media       *BlobMedia `json:"media,omitempty"`
}

// UploadListItem is an upload record with the blob's derived metadata.
type UploadListItem struct {
	UploadRecord
	Blob *BlobMeta `json:"blob,omitempty"`
}

// Handler serves the uploads HTTP API. Mount it at its prefix and at the
// prefix with a trailing slash:
//
//	h := blobstore.NewHandler(store, "/uploads", blobstore.HandlerOptions{})
//	mux.Handle("/uploads", h)
//	mux.Handle("/uploads/", h)
//
// The Handler performs no authentication or authorization; wrap it in your
// own middleware. Use [ContextWithUploader] in that middleware to record who
// uploaded a file.
type Handler struct {
	store   *Store
	prefix  string
	meta    *MetadataStore
	resizer *imageResizer
	maxBody int64
	logger  *slog.Logger
	client  http.Handler
}

// NewHandler returns a Handler for store mounted at prefix (for example "/uploads").
func NewHandler(store *Store, prefix string, opts HandlerOptions) *Handler {
	h := &Handler{
		store:   store,
		prefix:  "/" + strings.Trim(prefix, "/"),
		meta:    opts.Metadata,
		maxBody: opts.MaxUploadSize,
		logger:  opts.Logger,
		client:  http.FileServerFS(ClientFS),
	}
	if h.prefix == "/" {
		h.prefix = ""
	}
	if h.maxBody <= 0 {
		h.maxBody = defaultMaxUploadSize
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	if opts.Images != nil {
		h.resizer = &imageResizer{store: store, config: opts.Images.withDefaults()}
	}
	return h
}

type uploaderContextKey struct{}

// ContextWithUploader returns a context that records the uploader's name in
// upload records created by requests carrying it.
func ContextWithUploader(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, uploaderContextKey{}, name)
}

// UploaderFromContext returns the name set by [ContextWithUploader].
func UploaderFromContext(ctx context.Context) string {
	name, _ := ctx.Value(uploaderContextKey{}).(string)
	return name
}

// ServeHTTP routes:
//
//	POST  /                          upload files (multipart/form-data, optional "tags")
//	GET   /  and  /_meta             list upload records (?include=blob adds BlobMeta)
//	GET   /_meta/<hash>              derived blob metadata
//	PATCH /_meta/<hash>              replace tags: {"tags": [...]}
//	GET   /client/index.js           browser client
//	GET   /<hash>[/<filename>]       blob content
//	GET   /<hash>/w<width>/<filename> resized image
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, h.prefix)
	sub = strings.Trim(path.Clean("/"+sub), "/")

	switch {
	case sub == "":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.handleList(w, r)
		case http.MethodPost:
			h.handleUpload(w, r)
		default:
			methodNotAllowed(w, "GET, HEAD, POST")
		}
	case sub == "_meta":
		if !isRead(r) {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		h.handleList(w, r)
	case strings.HasPrefix(sub, "_meta/"):
		hash := strings.TrimPrefix(sub, "_meta/")
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.handleDescribe(w, r, hash)
		case http.MethodPatch:
			h.handleTags(w, r, hash)
		default:
			methodNotAllowed(w, "GET, HEAD, PATCH")
		}
	case strings.HasPrefix(sub, "client/"):
		if !isRead(r) {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + strings.TrimPrefix(sub, "client/")
		h.client.ServeHTTP(w, r2)
	default:
		if !isRead(r) {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		h.handleBlob(w, r, strings.Split(sub, "/"))
	}
}

func (h *Handler) handleBlob(w http.ResponseWriter, r *http.Request, parts []string) {
	hash := parts[0]
	if !ValidHash(hash) || len(parts) > 3 {
		http.NotFound(w, r)
		return
	}
	var filename string
	switch len(parts) {
	case 2:
		filename = parts[1]
	case 3:
		width, err := strconv.Atoi(strings.TrimPrefix(parts[1], "w"))
		if !strings.HasPrefix(parts[1], "w") || err != nil || h.resizer == nil {
			http.NotFound(w, r)
			return
		}
		h.handleResize(w, r, hash, width, parts[2])
		return
	}

	rc, err := h.store.Get(r.Context(), hash)
	if err != nil {
		h.blobError(w, r, err)
		return
	}
	defer rc.Close()

	contentType := "application/octet-stream"
	if filename != "" {
		if t := mime.TypeByExtension(filepath.Ext(filename)); t != "" {
			contentType = t
		}
	}
	serveImmutable(w, r, rc.(*os.File), `"sha256-`+hash+`"`, contentType, filename)
}

func (h *Handler) handleResize(w http.ResponseWriter, r *http.Request, hash string, width int, filename string) {
	f, contentType, err := h.resizer.open(r.Context(), hash, width)
	switch {
	case errors.Is(err, errWidthNotAllowed), errors.Is(err, errNotResizable):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, errImageTooLarge):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	case err != nil:
		h.blobError(w, r, err)
		return
	}
	defer f.Close()
	serveImmutable(w, r, f, `"sha256-`+hash+`-w`+strconv.Itoa(width)+`"`, contentType, filename)
}

func (h *Handler) blobError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidHash) {
		http.NotFound(w, r)
		return
	}
	h.logger.Error("blobstore: serve blob", "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// serveImmutable serves content-addressed bytes with long-lived caching and
// range support. Only media types that browsers cannot execute are served
// inline; everything else is sent as an octet-stream attachment so that
// uploaded HTML or SVG never runs on the application's origin.
func serveImmutable(w http.ResponseWriter, r *http.Request, f *os.File, etag, contentType, filename string) {
	disposition := "inline"
	if !safeInline(contentType) {
		contentType = "application/octet-stream"
		disposition = "attachment"
	}
	if filename != "" {
		if v := mime.FormatMediaType(disposition, map[string]string{"filename": filename}); v != "" {
			disposition = v
		}
	}
	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	hdr.Set("Content-Disposition", disposition)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
	hdr.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, f)
}

func safeInline(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case mediaType == "image/svg+xml":
		return false
	case strings.HasPrefix(mediaType, "image/"),
		strings.HasPrefix(mediaType, "video/"),
		strings.HasPrefix(mediaType, "audio/"):
		return true
	case mediaType == "application/pdf", mediaType == "text/plain":
		return true
	}
	return false
}

func (h *Handler) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart/form-data", http.StatusBadRequest)
		return
	}

	var (
		results = []UploadResult{}
		tags    []string
	)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			uploadReadError(w, err)
			return
		}
		filename := part.FileName()
		if filename == "" {
			if part.FormName() == "tags" {
				value, err := io.ReadAll(io.LimitReader(part, maxFormValueSize))
				if err != nil {
					uploadReadError(w, err)
					return
				}
				tags = append(tags, splitTags(string(value))...)
			}
			part.Close()
			continue
		}

		counter := &countingReader{r: part}
		hash, err := h.store.Save(counter)
		part.Close()
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				uploadReadError(w, tooLarge)
				return
			}
			h.logger.Error("blobstore: save upload", "err", err)
			http.Error(w, "failed to save file", http.StatusInternalServerError)
			return
		}
		result := UploadResult{
			Hash:     hash,
			Size:     counter.n,
			Filename: filename,
			Path:     h.prefix + "/" + hash + "/" + url.PathEscape(filename),
		}
		if meta, err := h.describeLocal(hash); err == nil {
			result.ContentType = meta.ContentType
			result.Media = meta.Media
		}
		results = append(results, result)
	}
	if len(results) == 0 {
		http.Error(w, "no files in request", http.StatusBadRequest)
		return
	}

	if h.meta != nil {
		uploader := UploaderFromContext(r.Context())
		now := time.Now().UTC()
		for _, res := range results {
			err := h.meta.Append(UploadRecord{
				Hash:       res.Hash,
				Filename:   res.Filename,
				Size:       res.Size,
				UploadedAt: now,
				Uploader:   uploader,
				Tags:       tags,
			})
			if err != nil {
				// The blob is stored; a missing library record should not fail the upload.
				h.logger.Error("blobstore: record upload", "hash", res.Hash, "err", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, results)
}

func uploadReadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "malformed multipart body", http.StatusBadRequest)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func splitTags(value string) []string {
	var tags []string
	for _, t := range strings.Split(value, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// describeLocal describes a blob without restoring it from the remote, and
// applies the handler's resizing setting to Media.Transformable.
func (h *Handler) describeLocal(hash string) (*BlobMeta, error) {
	f, err := h.store.openLocal(hash)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return h.adjust(describeFile(hash, f))
}

func (h *Handler) adjust(meta *BlobMeta, err error) (*BlobMeta, error) {
	if err == nil && meta.Media != nil && h.resizer == nil {
		meta.Media.Transformable = false
	}
	return meta, err
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	if h.meta == nil {
		http.NotFound(w, r)
		return
	}
	records, err := h.meta.List()
	if err != nil {
		h.logger.Error("blobstore: list uploads", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if r.URL.Query().Get("include") != "blob" {
		writeJSON(w, http.StatusOK, records)
		return
	}

	described := make(map[string]*BlobMeta, len(records))
	items := make([]UploadListItem, 0, len(records))
	for _, rec := range records {
		meta, seen := described[rec.Hash]
		if !seen && ValidHash(rec.Hash) {
			meta, _ = h.describeLocal(rec.Hash)
			described[rec.Hash] = meta
		}
		items = append(items, UploadListItem{UploadRecord: rec, Blob: meta})
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) handleDescribe(w http.ResponseWriter, r *http.Request, hash string) {
	meta, err := h.adjust(h.store.Describe(r.Context(), hash))
	if err != nil {
		h.blobError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (h *Handler) handleTags(w http.ResponseWriter, r *http.Request, hash string) {
	if h.meta == nil {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONBodySize)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.meta.Update(hash, body.Tags); err != nil {
		if errors.Is(err, ErrRecordNotFound) {
			http.NotFound(w, r)
			return
		}
		h.logger.Error("blobstore: update tags", "hash", hash, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func isRead(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
