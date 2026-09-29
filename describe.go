package blobstore

import (
	"context"
	"fmt"
	"image"
	_ "image/gif"  // register GIF for DecodeConfig
	_ "image/jpeg" // register JPEG for DecodeConfig
	_ "image/png"  // register PNG for DecodeConfig
	"io"
	"net/http"
	"os"
)

// BlobMeta is metadata derived from a blob's content.
type BlobMeta struct {
	Hash        string     `json:"hash"`
	Size        int64      `json:"size"`
	ContentType string     `json:"content_type,omitempty"`
	Media       *BlobMedia `json:"media,omitempty"`
}

// BlobMedia describes media-specific properties of a blob.
type BlobMedia struct {
	Kind   string `json:"kind"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	// Transformable reports whether the blob can be resized by the handler.
	Transformable bool `json:"transformable"`
}

// Describe sniffs the content type of a blob and, for JPEG, PNG and GIF
// images, reads its dimensions. Media.Transformable reports whether the format
// is one the image resizer supports; [Handler] clears it when resizing is off.
func (s *Store) Describe(ctx context.Context, hash string) (*BlobMeta, error) {
	f, err := s.open(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return describeFile(hash, f)
}

func describeFile(hash string, f *os.File) (*BlobMeta, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("blobstore: stat blob: %w", err)
	}

	header := make([]byte, 512)
	n, err := io.ReadFull(f, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, fmt.Errorf("blobstore: read blob header: %w", err)
	}
	meta := &BlobMeta{
		Hash:        hash,
		Size:        info.Size(),
		ContentType: http.DetectContentType(header[:n]),
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("blobstore: rewind blob: %w", err)
	}
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return meta, nil
	}
	if ct := imageContentType(format); ct != "" {
		meta.ContentType = ct
	}
	meta.Media = &BlobMedia{
		Kind:          "image",
		Width:         cfg.Width,
		Height:        cfg.Height,
		Transformable: imageContentType(format) != "",
	}
	return meta, nil
}

// imageContentType returns the content type for image formats the resizer
// supports, or "" for anything else.
func imageContentType(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	}
	return ""
}
