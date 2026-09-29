package s3remote

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"go.bits2life.com/blobstore"
)

func TestParseURL(t *testing.T) {
	cfg, err := ParseURL("digitalocean://KEY:se%2Fcret@fra1.digitaloceanspaces.com/my-bucket")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Endpoint: "fra1.digitaloceanspaces.com", Bucket: "my-bucket", AccessKey: "KEY", SecretKey: "se/cret"}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
	cfg, err = ParseURL("s3://K:S@localhost:9000/b?region=eu-west-1&prefix=backups/&path_style=true")
	if err != nil || cfg.Region != "eu-west-1" || cfg.Prefix != "backups/" || !cfg.PathStyle || cfg.Endpoint != "localhost:9000" {
		t.Fatalf("got %+v, %v", cfg, err)
	}
	if _, err := ParseURL("ftp://x/y"); err == nil {
		t.Fatal("accepted unsupported scheme")
	}
}

// TestStoreRoundTrip backs a store up to a fake S3 server and restores from it.
func TestStoreRoundTrip(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket("blobs"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(gofakes3.New(backend).Server())
	defer server.Close()

	remote, err := NewFromConfig(Config{Endpoint: server.URL, Bucket: "blobs", AccessKey: "k", SecretKey: "s", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := blobstore.New(t.TempDir(), blobstore.WithRemote(remote))
	if err != nil {
		t.Fatal(err)
	}

	hash, err := store.Save(strings.NewReader("to the cloud"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Backup(ctx); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := os.Remove(filepath.Join(store.Root(), "blobs", hash)); err != nil {
		t.Fatal(err)
	}
	rc, err := store.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get after local loss: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "to the cloud" {
		t.Fatalf("restored %q", b)
	}

	if _, err := remote.Get(ctx, strings.Repeat("0", 64)); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("missing key: %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Get(ctx, hash); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("after Delete: %v, want ErrNotFound", err)
	}
}
