# blobstore

A content-addressed file store and uploads API for Go web applications.

- Files are stored on local disk under their SHA-256 hash, so identical uploads are stored once.
- `Handler` is a plain `net/http` handler for uploading, serving, listing and tagging files.
- Optional on-the-fly image resizing (JPEG, PNG, GIF) with a disk cache.
- Optional backup to, and automatic restore from, a remote such as S3 ([`s3remote`](s3remote)).
- Optional ZIP downloads of many blobs ([`archive`](archive)).
- A small browser client, served by the handler.

The core module depends only on the standard library and `golang.org/x/image`.

```sh
go get go.bits2life.com/blobstore
```

## Quick start

```go
store, err := blobstore.New("./var/blobstore")
if err != nil {
	log.Fatal(err)
}

images := blobstore.DefaultImageConfig()
uploads := blobstore.NewHandler(store, "/uploads", blobstore.HandlerOptions{
	Metadata: blobstore.NewMetadataStore("./var/blobstore/metadata.jsonl"),
	Images:   &images,
})

mux := http.NewServeMux()
mux.Handle("/uploads", requireLogin(uploads))
mux.Handle("/uploads/", requireLogin(uploads))
```

The handler does no authentication. Wrap it in your own middleware, and call
`blobstore.ContextWithUploader(ctx, name)` there to record who uploaded each file.

## HTTP API

Paths are relative to the handler's prefix (`/uploads` above).

| Method | Path | |
|---|---|---|
| `POST` | `/` | Upload one or more files (`multipart/form-data`, any field name). Optional `tags` fields, comma-separated or repeated. |
| `GET` | `/<hash>/<filename>` | The file. `<filename>` is optional and only affects the response headers. |
| `GET` | `/<hash>/w<width>/<filename>` | The image resized to `width` (requires `HandlerOptions.Images`). |
| `GET` | `/` or `/_meta` | Upload records (requires `HandlerOptions.Metadata`). `?include=blob` adds each blob's metadata. |
| `GET` | `/_meta/<hash>` | Metadata derived from the blob: size, content type, image dimensions. |
| `PATCH` | `/_meta/<hash>` | Replace an upload's tags: `{"tags": ["a", "b"]}`. |
| `GET` | `/client/index.js` | The browser client. |

An upload responds with one entry per file:

```json
[
  {
    "hash": "8b1a99…",
    "size": 141284,
    "filename": "photo.jpg",
    "path": "/uploads/8b1a99…/photo.jpg",
    "content_type": "image/jpeg",
    "media": { "kind": "image", "width": 800, "height": 463, "transformable": true }
  }
]
```

### Serving and safety

Blob responses are immutable (`Cache-Control: public, max-age=31536000, immutable`,
`ETag: "sha256-<hash>"`) and support range and conditional requests.

The content type comes from the filename in the URL, but only images (except SVG),
audio, video, PDF and plain text are served inline. Everything else, including
HTML and SVG, is sent as `application/octet-stream` with `Content-Disposition:
attachment`, and every response carries `X-Content-Type-Options: nosniff`. This
keeps an uploaded HTML file from running as a page on your application's origin.

Upload requests are limited to 64 MiB by default (`HandlerOptions.MaxUploadSize`).
Image resizing refuses source images over 50 megapixels (`ImageConfig.MaxPixels`)
and only produces the widths in `ImageConfig.AllowedWidths`, or multiples of 320
up to `MaxWidth` when that list is empty.

## Go API

```go
store, _ := blobstore.New(root, blobstore.WithRemote(remote))

hash, _ := store.Save(r)             // store content, returns its hash
rc, _ := store.Get(ctx, hash)        // open (restores from the remote if missing)
meta, _ := store.Describe(ctx, hash) // content type, image dimensions
_ = store.Delete(ctx, hash)          // remove the blob, its resizes and its remote copy
```

`MetadataStore` is the upload library, a JSON Lines file with one record per
upload (`Append`, `List`, `Update`, `Delete`). It suits libraries of up to tens of
thousands of files. Deleting a blob does not delete its upload records; call
`MetadataStore.Delete` as well.

## Backups

Attach a `Remote` and run backups in the background:

```go
remote, err := s3remote.NewFromURL(os.Getenv("BLOBSTORE_REMOTE"))
// e.g. s3://KEY:SECRET@fra1.digitaloceanspaces.com/my-bucket
store, err := blobstore.New(root, blobstore.WithRemote(remote))
go store.RunBackups(ctx, time.Minute)
```

Every new blob gets a marker in `backup/`. `Backup` uploads marked blobs and
clears their markers. When a blob is missing locally, `Get` downloads it from the
remote, checks its hash and moves it into place. A hash that could not be restored
is not tried again for an hour (`WithRestoreCooldown`).

`Remote` is two methods, `Put` and `Get`. A remote that also implements
`RemoteDeleter` has its copy removed by `Store.Delete`. `DirRemote` stores
backups in a local directory, for example a mounted volume. `s3remote` is a
separate module, so the AWS SDK is only downloaded if you use it:

```sh
go get go.bits2life.com/blobstore/s3remote
```

## ZIP downloads

```go
mux.Handle("/archive/", http.StripPrefix("/archive", archive.NewHandler(store)))
```

`POST /archive/zip` with

```json
{
  "filename": "branding.zip",
  "mode": "strict",
  "contents": [{ "hash": "8b1a99…", "path": "images/logo.png" }]
}
```

streams a ZIP. In `strict` mode (the default) a missing blob fails the request
with 404. In `best-effort` mode missing blobs are skipped and counted in
`X-Archive-Skipped`. Paths must be relative, without `..`, and unique; at most 1000
entries.

## Browser client

```js
import uploads from "/uploads/client/index.js"

const [file] = await uploads.upload(input.files, { tags: ["header"] })
img.src = uploads.resizedURL(file.hash, 640, file.filename)
const library = await uploads.listWithBlobs()
await uploads.updateTags(file.hash, ["hero"])
```

The default export talks to the prefix the script was loaded from. Use
`createBlobstoreClient({ baseURL })` for another location.

## On-disk layout

```
<root>/blobs/<hash>                  file content
<root>/backup/<hash>                 pending-backup marker (symlink to the blob)
<root>/transforms/<hash>_w<n>.<ext>  cached resizes
<root>/tmp/                          staging for writes
```

The layout matches the blobstore in bits2life/archetype, so an existing data
directory can be used as is.
