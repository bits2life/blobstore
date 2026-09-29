// Package archive serves ZIP downloads assembled from blobstore blobs.
//
//	h := archive.NewHandler(store)
//	mux.Handle("/archive/", http.StripPrefix("/archive", h))
//
// POST /zip with {"filename": "...", "mode": "strict"|"best-effort",
// "contents": [{"hash": "...", "path": "dir/name.ext"}]} streams the archive.
package archive

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"go.bits2life.com/blobstore"
)

const (
	// ModeStrict fails the request when any blob is missing. It is the default.
	ModeStrict = "strict"
	// ModeBestEffort skips missing blobs and reports how many were skipped.
	ModeBestEffort = "best-effort"
)

const (
	defaultArchiveFilename = "archive.zip"
	maxArchiveEntries      = 1000
	maxRequestBody         = 1 << 20
)

// Source provides blob content. *blobstore.Store implements it.
type Source interface {
	Get(ctx context.Context, hash string) (io.ReadCloser, error)
}

// ContentItem places one blob at a path inside the archive.
type ContentItem struct {
	Hash string `json:"hash"`
	Path string `json:"path"`
}

// ZipRequest is the body of POST /zip.
type ZipRequest struct {
	Filename string        `json:"filename"`
	Mode     string        `json:"mode"`
	Contents []ContentItem `json:"contents"`
}

// ValidationError describes an invalid ZipRequest field.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Handler serves POST /zip. Mount it with http.StripPrefix.
type Handler struct {
	src    Source
	logger *slog.Logger
}

// NewHandler returns a Handler reading blobs from src.
func NewHandler(src Source) *Handler {
	return &Handler{src: src, logger: slog.Default()}
}

// ServeHTTP handles POST /zip.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSuffix(r.URL.Path, "/") != "/zip" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ZipRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	items, mode, err := validateZipRequest(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Check every blob before streaming, since the status cannot change once
	// the archive has started.
	ctx := r.Context()
	available := make([]ContentItem, 0, len(items))
	for _, item := range items {
		rc, err := h.src.Get(ctx, item.Hash)
		if err != nil {
			if !errors.Is(err, blobstore.ErrNotFound) {
				h.logger.Error("archive: open blob", "hash", item.Hash, "err", err)
			}
			if mode == ModeStrict {
				writeError(w, http.StatusNotFound, "blob not found: "+item.Hash)
				return
			}
			continue
		}
		rc.Close()
		available = append(available, item)
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "application/zip")
	hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": sanitizeArchiveFilename(req.Filename)}))
	hdr.Set("X-Archive-Mode", mode)
	hdr.Set("X-Archive-Written", strconv.Itoa(len(available)))
	if mode == ModeBestEffort {
		hdr.Set("X-Archive-Skipped", strconv.Itoa(len(items)-len(available)))
	}

	// Errors past this point truncate the download; the client sees an invalid ZIP.
	zw := zip.NewWriter(w)
	for _, item := range available {
		if err := h.writeEntry(ctx, zw, item); err != nil {
			h.logger.Error("archive: write entry", "hash", item.Hash, "err", err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		h.logger.Error("archive: finish zip", "err", err)
	}
}

func (h *Handler) writeEntry(ctx context.Context, zw *zip.Writer, item ContentItem) error {
	rc, err := h.src.Get(ctx, item.Hash)
	if err != nil {
		return err
	}
	defer rc.Close()
	entry, err := zw.Create(item.Path)
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, rc)
	return err
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func normalizeMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "", ModeStrict:
		return ModeStrict
	case ModeBestEffort:
		return ModeBestEffort
	default:
		return ""
	}
}

func sanitizeArchiveFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return defaultArchiveFilename
	}
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	if name == "." || name == ".." || name == "" {
		return defaultArchiveFilename
	}
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		name += ".zip"
	}
	return name
}

func validateHash(hash string) error {
	if !blobstore.ValidHash(strings.TrimSpace(hash)) {
		return ValidationError{Field: "hash", Message: "must be a lowercase sha-256 hex string"}
	}
	return nil
}

func validateArchivePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", ValidationError{Field: "path", Message: "must not be empty"}
	}

	normalized := strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(normalized, "/") {
		return "", ValidationError{Field: "path", Message: "must be relative"}
	}

	cleaned := path.Clean(normalized)
	if cleaned == "." {
		return "", ValidationError{Field: "path", Message: "must not resolve to current directory"}
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", ValidationError{Field: "path", Message: "must not contain path traversal"}
	}
	return cleaned, nil
}

func validateZipRequest(req *ZipRequest) ([]ContentItem, string, error) {
	if req == nil {
		return nil, "", ValidationError{Field: "body", Message: "is required"}
	}
	if len(req.Contents) == 0 {
		return nil, "", ValidationError{Field: "contents", Message: "must contain at least one entry"}
	}
	if len(req.Contents) > maxArchiveEntries {
		return nil, "", ValidationError{Field: "contents", Message: fmt.Sprintf("too many entries (max %d)", maxArchiveEntries)}
	}

	mode := normalizeMode(req.Mode)
	if mode == "" {
		return nil, "", ValidationError{Field: "mode", Message: `must be "strict" or "best-effort"`}
	}

	seenPaths := make(map[string]struct{}, len(req.Contents))
	validated := make([]ContentItem, 0, len(req.Contents))
	for i, item := range req.Contents {
		if err := validateHash(item.Hash); err != nil {
			return nil, "", ValidationError{Field: fmt.Sprintf("contents[%d].hash", i), Message: err.Error()}
		}
		cleanPath, err := validateArchivePath(item.Path)
		if err != nil {
			return nil, "", ValidationError{Field: fmt.Sprintf("contents[%d].path", i), Message: err.Error()}
		}
		if _, exists := seenPaths[cleanPath]; exists {
			return nil, "", ValidationError{Field: fmt.Sprintf("contents[%d].path", i), Message: "duplicate archive destination path"}
		}
		seenPaths[cleanPath] = struct{}{}
		validated = append(validated, ContentItem{
			Hash: strings.TrimSpace(item.Hash),
			Path: cleanPath,
		})
	}

	return validated, mode, nil
}
