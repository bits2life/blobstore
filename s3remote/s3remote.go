// Package s3remote implements blobstore.Remote on Amazon S3 and S3-compatible
// services such as DigitalOcean Spaces, Cloudflare R2 and MinIO.
//
//	remote, err := s3remote.NewFromURL(os.Getenv("BLOBSTORE_REMOTE"))
//	store, err := blobstore.New(root, blobstore.WithRemote(remote))
//	go store.RunBackups(ctx, time.Minute)
package s3remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.bits2life.com/blobstore"
)

// DefaultPrefix is the key prefix used when Config.Prefix is empty. It matches
// the layout of backups written by earlier versions of the store.
const DefaultPrefix = "blobs/"

// Config describes a bucket.
type Config struct {
	// Endpoint is the service URL, e.g. "https://fra1.digitaloceanspaces.com".
	// Leave empty for Amazon S3. A value without a scheme gets https://.
	Endpoint string
	// Region defaults to "us-east-1", which S3-compatible services generally accept.
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// Prefix is prepended to every key. Default DefaultPrefix.
	Prefix string
	// PathStyle selects path-style addressing, needed by e.g. MinIO.
	PathStyle bool
}

// Remote stores blobs as objects named Prefix+hash.
type Remote struct {
	client *s3.Client
	bucket string
	prefix string
}

var (
	_ blobstore.Remote        = (*Remote)(nil)
	_ blobstore.RemoteDeleter = (*Remote)(nil)
)

// New returns a Remote using an existing S3 client.
func New(client *s3.Client, bucket, prefix string) *Remote {
	return &Remote{client: client, bucket: bucket, prefix: prefix}
}

// NewFromConfig builds an S3 client from cfg and returns a Remote.
func NewFromConfig(cfg Config) (*Remote, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3remote: bucket is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3remote: access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	endpoint := cfg.Endpoint
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}

	client := s3.New(s3.Options{
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle: cfg.PathStyle,
		// Many S3-compatible services reject the checksum trailers the SDK
		// sends by default; only send checksums where the API requires them.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	return New(client, cfg.Bucket, cfg.Prefix), nil
}

// ParseURL parses a remote URL of the form
//
//	s3://ACCESS_KEY:SECRET_KEY@ENDPOINT_HOST/BUCKET?region=R&prefix=P&path_style=true
//
// Use "s3://KEY:SECRET@/BUCKET" (empty host) for Amazon S3. The secret must be
// URL-encoded. For compatibility with archetype's cloudstore, the scheme
// "digitalocean" is accepted as a synonym of "s3".
func ParseURL(raw string) (Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Config{}, fmt.Errorf("s3remote: parse URL: %w", err)
	}
	if u.Scheme != "s3" && u.Scheme != "digitalocean" {
		return Config{}, fmt.Errorf("s3remote: unsupported scheme %q", u.Scheme)
	}
	q := u.Query()
	cfg := Config{
		Endpoint: u.Host,
		Region:   q.Get("region"),
		Bucket:   strings.Trim(u.Path, "/"),
		Prefix:   q.Get("prefix"),
	}
	if u.User != nil {
		cfg.AccessKey = u.User.Username()
		cfg.SecretKey, _ = u.User.Password()
	}
	if v := q.Get("path_style"); v != "" {
		if cfg.PathStyle, err = strconv.ParseBool(v); err != nil {
			return Config{}, fmt.Errorf("s3remote: path_style: %w", err)
		}
	}
	return cfg, nil
}

// NewFromURL is ParseURL followed by NewFromConfig.
func NewFromURL(raw string) (*Remote, error) {
	cfg, err := ParseURL(raw)
	if err != nil {
		return nil, err
	}
	return NewFromConfig(cfg)
}

func (r *Remote) key(k string) *string { return aws.String(r.prefix + k) }

// Put uploads an object. The store passes an *os.File, which lets the SDK sign
// the payload; other callers should pass an io.ReadSeeker too.
func (r *Remote) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(r.bucket),
		Key:           r.key(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/octet-stream"),
	})
	if err != nil {
		return fmt.Errorf("s3remote: put %s: %w", key, err)
	}
	return nil
}

// Get downloads an object. A missing object yields an error wrapping blobstore.ErrNotFound.
func (r *Remote) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    r.key(key),
	})
	if err != nil {
		var noKey *types.NoSuchKey
		if errors.As(err, &noKey) {
			return nil, fmt.Errorf("s3remote: get %s: %w", key, blobstore.ErrNotFound)
		}
		return nil, fmt.Errorf("s3remote: get %s: %w", key, err)
	}
	return out.Body, nil
}

// Delete removes an object. Deleting a missing object is not an error.
func (r *Remote) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    r.key(key),
	})
	if err != nil {
		return fmt.Errorf("s3remote: delete %s: %w", key, err)
	}
	return nil
}
