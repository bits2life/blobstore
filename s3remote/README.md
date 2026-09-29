# s3remote

`blobstore.Remote` for Amazon S3 and S3-compatible services (DigitalOcean Spaces,
Cloudflare R2, MinIO). It is a separate module so that the core blobstore module
does not depend on the AWS SDK.

```go
remote, err := s3remote.NewFromURL("s3://KEY:SECRET@fra1.digitaloceanspaces.com/my-bucket")
store, err := blobstore.New(root, blobstore.WithRemote(remote))
go store.RunBackups(ctx, time.Minute)
```

URL query options: `region` (default `us-east-1`), `prefix` (default `blobs/`),
`path_style=true`. Leave the host empty for Amazon S3. The `digitalocean://`
scheme used by archetype's `CLOUDSTORE_URI` is accepted too.
