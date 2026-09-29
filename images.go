package blobstore

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/image/draw"
)

// ImageConfig controls on-the-fly image resizing.
type ImageConfig struct {
	// AllowedWidths lists the permitted output widths. If empty, any multiple
	// of 320 up to MaxWidth is allowed.
	AllowedWidths []int
	// MaxWidth caps the output width. Default 3840.
	MaxWidth int
	// MaxPixels caps width*height of source images that will be decoded, to
	// guard against decompression bombs. Default 50 megapixels.
	MaxPixels int
	// Quality is the JPEG output quality. Default 85.
	Quality int
}

// DefaultImageConfig returns the default resizing configuration.
func DefaultImageConfig() ImageConfig {
	return ImageConfig{MaxWidth: 3840, MaxPixels: 50_000_000, Quality: 85}
}

func (c ImageConfig) withDefaults() ImageConfig {
	d := DefaultImageConfig()
	if c.MaxWidth <= 0 {
		c.MaxWidth = d.MaxWidth
	}
	if c.MaxPixels <= 0 {
		c.MaxPixels = d.MaxPixels
	}
	if c.Quality <= 0 {
		c.Quality = d.Quality
	}
	return c
}

func (c ImageConfig) allowsWidth(width int) bool {
	if width <= 0 || width > c.MaxWidth {
		return false
	}
	if len(c.AllowedWidths) > 0 {
		return slices.Contains(c.AllowedWidths, width)
	}
	return width%320 == 0
}

var (
	errWidthNotAllowed = errors.New("width not allowed")
	errNotResizable    = errors.New("not a resizable image")
	errImageTooLarge   = errors.New("image too large to resize")
)

type imageResizer struct {
	store  *Store
	config ImageConfig
}

// open returns the image resized to width along with its content type. Images
// narrower than width are returned unchanged. Resized images are cached under
// transforms/ and written atomically.
func (r *imageResizer) open(ctx context.Context, hash string, width int) (*os.File, string, error) {
	if !r.config.allowsWidth(width) {
		return nil, "", errWidthNotAllowed
	}
	src, err := r.store.open(ctx, hash)
	if err != nil {
		return nil, "", err
	}
	cfg, format, err := image.DecodeConfig(src)
	contentType := imageContentType(format)
	if err != nil || contentType == "" {
		src.Close()
		return nil, "", errNotResizable
	}
	if cfg.Width*cfg.Height > r.config.MaxPixels {
		src.Close()
		return nil, "", errImageTooLarge
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		src.Close()
		return nil, "", err
	}
	if width >= cfg.Width {
		return src, contentType, nil
	}

	cachePath := filepath.Join(r.store.root, "transforms", fmt.Sprintf("%s_w%d.%s", hash, width, format))
	if f, err := os.Open(cachePath); err == nil {
		src.Close()
		return f, contentType, nil
	}

	err = r.generate(src, format, cfg, width, cachePath)
	src.Close()
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(cachePath)
	return f, contentType, err
}

func (r *imageResizer) generate(src io.Reader, format string, cfg image.Config, width int, cachePath string) error {
	img, _, err := image.Decode(src)
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}
	height := max(1, cfg.Height*width/cfg.Width)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)

	tmp, err := os.CreateTemp(filepath.Dir(cachePath), ".resize-*")
	if err != nil {
		return err
	}
	switch format {
	case "png":
		err = png.Encode(tmp, dst)
	case "gif":
		err = gif.Encode(tmp, dst, nil)
	default:
		err = jpeg.Encode(tmp, dst, &jpeg.Options{Quality: r.config.Quality})
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), cachePath)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write resized image: %w", err)
	}
	return nil
}
