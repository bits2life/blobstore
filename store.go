// Package blobstore is a content-addressed file store for web applications.
//
// Blobs are identified by the lowercase hex SHA-256 of their content and kept
// on local disk. A [Remote] can be attached to back blobs up and to restore
// them when they are missing locally. [Handler] exposes the store as an
// uploads HTTP API with an optional upload library and image resizing.
package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	// ErrNotFound is returned when a blob does not exist locally and cannot be restored.
	ErrNotFound = errors.New("blobstore: blob not found")
	// ErrInvalidHash is returned when a hash is not 64 lowercase hex characters.
	ErrInvalidHash = errors.New("blobstore: invalid hash")
)

const (
	defaultRestoreCooldown = time.Hour
	maxCooldownEntries     = 10000
)

// ValidHash reports whether hash is a lowercase hex SHA-256 digest.
func ValidHash(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Store is a content-addressed blob store rooted at a local directory.
//
// Layout under the root:
//
//	blobs/<hash>                 blob content, written once
//	tmp/                         staging area for writes
//	backup/<hash>                marker for blobs not yet copied to the remote
//	transforms/<hash>_w<n>.<ext> cached image resizes
type Store struct {
	root   string
	remote Remote
	logger *slog.Logger

	mu sync.RWMutex

	cooldownMu     sync.Mutex
	cooldown       map[string]time.Time
	cooldownPeriod time.Duration
}

// Option configures a Store.
type Option func(*Store)

// WithRemote attaches a remote used for backups and for restoring missing blobs.
func WithRemote(r Remote) Option {
	return func(s *Store) { s.remote = r }
}

// WithRestoreCooldown sets how long a failed restore of a hash suppresses
// further restore attempts for that hash. The default is one hour.
func WithRestoreCooldown(d time.Duration) Option {
	return func(s *Store) { s.cooldownPeriod = d }
}

// WithLogger sets the logger used for background work. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// New opens a store at root, creating its directories if needed.
func New(root string, opts ...Option) (*Store, error) {
	s := &Store{
		root:           root,
		cooldownPeriod: defaultRestoreCooldown,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	for _, dir := range []string{"blobs", "tmp", "backup", "transforms"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, fmt.Errorf("blobstore: create %s directory: %w", dir, err)
		}
	}
	return s, nil
}

// Root returns the store's root directory.
func (s *Store) Root() string { return s.root }

func (s *Store) blobPath(hash string) string   { return filepath.Join(s.root, "blobs", hash) }
func (s *Store) markerPath(hash string) string { return filepath.Join(s.root, "backup", hash) }

// Save stores the content of r and returns its hash. Saving content that is
// already stored is a no-op that returns the existing hash.
func (s *Store) Save(r io.Reader) (string, error) {
	tmpPath, hash, err := s.writeTemp(r)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmpPath)

	s.mu.Lock()
	defer s.mu.Unlock()

	blobPath := s.blobPath(hash)
	if _, err := os.Stat(blobPath); err == nil {
		return hash, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("blobstore: stat blob: %w", err)
	}
	if err := os.Rename(tmpPath, blobPath); err != nil {
		return "", fmt.Errorf("blobstore: move blob into place: %w", err)
	}
	if err := s.addMarker(hash); err != nil {
		return "", err
	}
	return hash, nil
}

// writeTemp copies r into a temporary file under tmp/ and returns its path and hash.
func (s *Store) writeTemp(r io.Reader) (string, string, error) {
	f, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "blob-*")
	if err != nil {
		return "", "", fmt.Errorf("blobstore: create temporary file: %w", err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), r)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(f.Name())
		return "", "", fmt.Errorf("blobstore: write blob: %w", errors.Join(copyErr, closeErr))
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// addMarker records that hash still needs to be copied to the remote.
// Markers are relative symlinks to the blob, as in earlier versions of the store.
func (s *Store) addMarker(hash string) error {
	target := filepath.Join("..", "blobs", hash)
	if err := os.Symlink(target, s.markerPath(hash)); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("blobstore: create backup marker: %w", err)
	}
	return nil
}

// Get opens the blob with the given hash. If the blob is missing locally and a
// remote is configured, Get restores it from the remote first. The returned
// reader is an *os.File and therefore also an io.ReadSeeker.
func (s *Store) Get(ctx context.Context, hash string) (io.ReadCloser, error) {
	f, err := s.open(ctx, hash)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Stat returns the size of the blob, restoring it from the remote if needed.
func (s *Store) Stat(ctx context.Context, hash string) (int64, error) {
	f, err := s.open(ctx, hash)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("blobstore: stat blob: %w", err)
	}
	return info.Size(), nil
}

func (s *Store) open(ctx context.Context, hash string) (*os.File, error) {
	if !ValidHash(hash) {
		return nil, ErrInvalidHash
	}
	f, err := s.openLocal(hash)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return f, err
	}
	if s.remote == nil || !s.shouldAttemptRestore(hash) {
		return nil, ErrNotFound
	}
	if err := s.restore(ctx, hash); err != nil {
		s.markRestoreFailed(hash)
		if !errors.Is(err, ErrNotFound) {
			s.logger.Warn("blobstore: restore failed", "hash", hash, "err", err)
		}
		return nil, ErrNotFound
	}
	return s.openLocal(hash)
}

func (s *Store) openLocal(hash string) (*os.File, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, err := os.Open(s.blobPath(hash))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("blobstore: open blob: %w", err)
	}
	return f, nil
}

// restore downloads hash from the remote into a temporary file, verifies its
// content hash, and only then moves it into place.
func (s *Store) restore(ctx context.Context, hash string) error {
	rc, err := s.remote.Get(ctx, hash)
	if err != nil {
		return err
	}
	tmpPath, got, err := s.writeTemp(rc)
	rc.Close()
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	if got != hash {
		return fmt.Errorf("blobstore: restored content hashes to %s", got)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(tmpPath, s.blobPath(hash)); err != nil {
		return fmt.Errorf("blobstore: move restored blob into place: %w", err)
	}
	return nil
}

// Delete removes a blob, its pending backup marker and its cached image
// resizes. If the remote implements [RemoteDeleter] the remote copy is removed
// too; otherwise a later Get may restore the blob from the remote. Deleting a
// blob that does not exist is not an error.
func (s *Store) Delete(ctx context.Context, hash string) error {
	if !ValidHash(hash) {
		return ErrInvalidHash
	}
	s.mu.Lock()
	var errs []error
	for _, p := range []string{s.blobPath(hash), s.markerPath(hash)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	transforms, _ := filepath.Glob(filepath.Join(s.root, "transforms", hash+"_*"))
	for _, p := range transforms {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	s.mu.Unlock()

	if d, ok := s.remote.(RemoteDeleter); ok {
		if err := d.Delete(ctx, hash); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("blobstore: delete %s: %w", hash, errors.Join(errs...))
	}
	return nil
}

func (s *Store) shouldAttemptRestore(hash string) bool {
	s.cooldownMu.Lock()
	defer s.cooldownMu.Unlock()
	failedAt, ok := s.cooldown[hash]
	return !ok || time.Since(failedAt) > s.cooldownPeriod
}

func (s *Store) markRestoreFailed(hash string) {
	s.cooldownMu.Lock()
	defer s.cooldownMu.Unlock()
	if s.cooldown == nil {
		s.cooldown = make(map[string]time.Time)
	}
	if len(s.cooldown) >= maxCooldownEntries {
		for h, t := range s.cooldown {
			if time.Since(t) > s.cooldownPeriod {
				delete(s.cooldown, h)
			}
		}
		if len(s.cooldown) >= maxCooldownEntries {
			clear(s.cooldown)
		}
	}
	s.cooldown[hash] = time.Now()
}
