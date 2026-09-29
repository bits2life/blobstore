package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Remote is secondary storage that blobs are backed up to and restored from.
// Keys are blob hashes; an implementation may add its own prefix.
//
// Get must return an error wrapping [ErrNotFound] when the key does not exist.
// See go.bits2life.com/blobstore/s3remote for an S3-compatible implementation.
type Remote interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// RemoteDeleter is implemented by remotes that support deletion. When the
// store's remote implements it, [Store.Delete] also removes the remote copy.
type RemoteDeleter interface {
	Delete(ctx context.Context, key string) error
}

// Backup copies every blob with a pending backup marker to the remote and
// removes the marker on success. It returns nil if no remote is configured.
// Failures for individual blobs are logged and retried on the next call; the
// returned error reports how many failed.
func (s *Store) Backup(ctx context.Context) error {
	if s.remote == nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "backup"))
	if err != nil {
		return fmt.Errorf("blobstore: read backup markers: %w", err)
	}
	failed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		hash := entry.Name()
		if !ValidHash(hash) {
			continue
		}
		if err := s.backupOne(ctx, hash); err != nil {
			failed++
			s.logger.Warn("blobstore: backup failed", "hash", hash, "err", err)
			continue
		}
		s.logger.Debug("blobstore: backed up blob", "hash", hash)
	}
	if failed > 0 {
		return fmt.Errorf("blobstore: %d blob(s) failed to back up", failed)
	}
	return nil
}

func (s *Store) backupOne(ctx context.Context, hash string) error {
	f, err := s.openLocal(hash)
	if errors.Is(err, ErrNotFound) {
		// The blob was deleted after it was marked; drop the stale marker.
		return removeIfExists(s.markerPath(hash))
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := s.remote.Put(ctx, hash, f, info.Size()); err != nil {
		return err
	}
	return removeIfExists(s.markerPath(hash))
}

// RunBackups calls [Store.Backup] immediately and then every interval until
// ctx is cancelled. Errors are logged. It is meant to run in its own goroutine.
func (s *Store) RunBackups(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.Backup(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("blobstore: backup run failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// DirRemote is a Remote that stores blobs as files in a local directory, for
// example a mounted network volume. It is also convenient in tests.
type DirRemote struct {
	Dir string
}

func (d DirRemote) path(key string) (string, error) {
	if !ValidHash(key) {
		return "", ErrInvalidHash
	}
	return filepath.Join(d.Dir, key), nil
}

// Put writes r to Dir/key atomically.
func (d DirRemote) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	dst, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.Dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(tmp, r)
	closeErr := tmp.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Get opens Dir/key.
func (d DirRemote) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Delete removes Dir/key.
func (d DirRemote) Delete(_ context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	return removeIfExists(p)
}
