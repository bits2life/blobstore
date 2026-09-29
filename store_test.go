package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := New(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readBlob(t *testing.T, s *Store, hash string) string {
	t.Helper()
	rc, err := s.Get(context.Background(), hash)
	if err != nil {
		t.Fatalf("Get(%s): %v", hash, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSaveGetDelete(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	hash, err := s.Save(strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if hash != hashOf("hello") {
		t.Fatalf("hash = %s, want %s", hash, hashOf("hello"))
	}
	again, err := s.Save(strings.NewReader("hello"))
	if err != nil || again != hash {
		t.Fatalf("second Save = %s, %v", again, err)
	}
	if got := readBlob(t, s, hash); got != "hello" {
		t.Fatalf("content = %q", got)
	}
	if size, err := s.Stat(ctx, hash); err != nil || size != 5 {
		t.Fatalf("Stat = %d, %v", size, err)
	}
	if _, err := os.Lstat(filepath.Join(s.Root(), "backup", hash)); err != nil {
		t.Fatalf("expected backup marker: %v", err)
	}

	transform := filepath.Join(s.Root(), "transforms", hash+"_w320.png")
	if err := os.WriteFile(transform, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, hash); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(s.Root(), "blobs", hash), filepath.Join(s.Root(), "backup", hash), transform} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Delete", p)
		}
	}
	if _, err := s.Get(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, hash); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestInvalidHashRejected(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, h := range []string{"", "../metadata.jsonl", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		if _, err := s.Get(ctx, h); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Get(%q) = %v, want ErrInvalidHash", h, err)
		}
		if err := s.Delete(ctx, h); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidHash", h, err)
		}
	}
}

func TestBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	remote := DirRemote{Dir: t.TempDir()}
	s := newStore(t, WithRemote(remote))

	hash, err := s.Save(strings.NewReader("backed up"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(s.Root(), "backup", hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("backup marker not removed after Backup")
	}
	if _, err := os.Stat(filepath.Join(remote.Dir, hash)); err != nil {
		t.Fatalf("remote copy missing: %v", err)
	}

	// Lose the local copy; Get restores it from the remote.
	if err := os.Remove(filepath.Join(s.Root(), "blobs", hash)); err != nil {
		t.Fatal(err)
	}
	if got := readBlob(t, s, hash); got != "backed up" {
		t.Fatalf("restored content = %q", got)
	}

	// Delete removes the remote copy too, so the blob stays deleted.
	if err := s.Delete(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestRestoreRejectsCorruptRemoteContent(t *testing.T) {
	remote := DirRemote{Dir: t.TempDir()}
	s := newStore(t, WithRemote(remote))
	hash := hashOf("original")
	if err := os.WriteFile(filepath.Join(remote.Dir, hash), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "blobs", hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt restore left a blob in place")
	}
}

type countingRemote struct {
	Remote
	gets int
}

func (c *countingRemote) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets++
	return c.Remote.Get(ctx, key)
}

func TestRestoreCooldown(t *testing.T) {
	ctx := context.Background()
	remote := &countingRemote{Remote: DirRemote{Dir: t.TempDir()}}
	s := newStore(t, WithRemote(remote), WithRestoreCooldown(time.Hour))
	hash := hashOf("missing")

	for range 3 {
		if _, err := s.Get(ctx, hash); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get = %v, want ErrNotFound", err)
		}
	}
	if remote.gets != 1 {
		t.Fatalf("remote consulted %d times, want 1", remote.gets)
	}

	s.cooldownPeriod = 0
	time.Sleep(time.Millisecond)
	s.Get(ctx, hash)
	if remote.gets != 2 {
		t.Fatalf("remote consulted %d times after cooldown, want 2", remote.gets)
	}
}

func TestBackupDropsMarkersForDeletedBlobs(t *testing.T) {
	remote := DirRemote{Dir: t.TempDir()}
	s := newStore(t, WithRemote(remote))
	hash, _ := s.Save(strings.NewReader("gone"))
	os.Remove(filepath.Join(s.Root(), "blobs", hash))
	if err := s.Backup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(s.Root(), "backup", hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale marker not removed")
	}
}

func TestMetadataStore(t *testing.T) {
	m := NewMetadataStore(filepath.Join(t.TempDir(), "metadata.jsonl"))
	if recs, err := m.List(); err != nil || len(recs) != 0 {
		t.Fatalf("empty List = %v, %v", recs, err)
	}
	a, b := hashOf("a"), hashOf("b")
	for _, r := range []UploadRecord{{Hash: a, Filename: "a.txt"}, {Hash: b, Filename: "b.txt"}, {Hash: a, Filename: "a2.txt"}} {
		if err := m.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Update(a, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(hashOf("nope"), nil); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("Update unknown = %v", err)
	}
	recs, _ := m.List()
	if len(recs) != 3 || len(recs[0].Tags) != 1 || len(recs[2].Tags) != 1 || len(recs[1].Tags) != 0 {
		t.Fatalf("after Update: %+v", recs)
	}
	if err := m.Delete(a); err != nil {
		t.Fatal(err)
	}
	recs, _ = m.List()
	if len(recs) != 1 || recs[0].Hash != b {
		t.Fatalf("after Delete: %+v", recs)
	}
}
