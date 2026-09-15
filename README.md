<div align="center">

# Cloodsy S3

**A lightweight, AWS SDK-compatible S3 server written in Go.**

Ships as a single static binary with zero dependencies — no CGO (pure-Go SQLite), no external database, no runtime requirements.
All metadata is stored in an embedded SQLite database.

[![Built by OnaOnbir](https://img.shields.io/badge/Built%20by-OnaOnbir-blue?style=flat-square)](https://onaonbir.com)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![CI](https://img.shields.io/github/actions/workflow/status/onaonbir/Cloodsy-S3/ci.yml?branch=main&style=flat-square&label=CI)](https://github.com/onaonbir/Cloodsy-S3/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/onaonbir/Cloodsy-S3?style=flat-square)](https://github.com/onaonbir/Cloodsy-S3/releases/latest)

[Website](https://onaonbir.com) | [Download](https://github.com/onaonbir/Cloodsy-S3/releases/latest) | [Documentation](#quick-start)

</div>

---

## Features

- **AWS SDK Compatible** — Works with AWS CLI, boto3, aws-sdk-go, s3cmd, rclone, Terraform, and any S3-compatible client
- **Single Binary** — One executable, zero dependencies, runs anywhere
- **Per-Bucket Credentials** — Each bucket gets its own access/secret key pairs with read-write or read-only permissions
- **Object Versioning** — Enable or suspend versioning per bucket (S3 semantics: once enabled it can only be suspended) with full delete marker support
- **Lifecycle Rules** — Expiration by age and prefix, noncurrent-version expiration, incomplete-multipart cleanup and delete-marker cleanup, with `Filter`/`Status` honored
- **Webhook Notifications** — Real-time HTTP callbacks for object events with HMAC signing
- **Bucket Quotas** — Per-bucket storage limits to prevent disk exhaustion
- **Custom Storage Directories** — Per-bucket storage paths for multi-disk setups (SSD for hot data, HDD for archives)
- **Presigned URLs** — Time-limited download/upload links without sharing credentials
- **Multipart Upload** — Large file uploads with part copy, listing, and automatic stale upload cleanup
- **Range Requests** — Partial file downloads via standard HTTP Range headers
- **Conditional Requests** — If-Match, If-None-Match, If-Modified-Since, If-Unmodified-Since support
- **Server-Side Copy** — Copy objects between buckets without re-uploading, including partial copy via ranges
- **On-the-Fly Image Resizing** — Resize and re-encode images straight from the object URL with `?w=&h=&m=&q=`; originals are never modified and derivatives are cached on disk
- **Public-Read Buckets** — Opt-in anonymous object reads (GET/HEAD only) so resized image links work in plain `<img>` tags without signing
- **Image Optimization on Upload** — Automatically generate a smaller optimized variant alongside each uploaded image (small images inline, large ones in the background); the original is preserved
- **WebDAV Mounting** — Mount a bucket as a network drive over WebDAV (per-bucket opt-in, Basic Auth maps to bucket credentials)
- **Admin REST API** — Full management API with session-based authentication for GUI/automation
- **CORS Support** — Browser-based S3 clients work once you list their origins in `server.cors_origins`
- **TLS Support** — Optional HTTPS for the S3 API, the Admin API and WebDAV (each listener can carry its own certificate)
- **Virtual-Host Addressing** — Optional `<bucket>.<domain>` style requests via `server.virtual_host_domains`
- **Verified Uploads** — `Content-MD5`, `x-amz-content-sha256`, `x-amz-checksum-*` and `aws-chunked` chunk signatures are checked, so corrupt or tampered uploads are rejected
- **Secure Storage** — Files stored with `.cloodsys3ext` extension, path traversal and symlink attack protection
- **Cross-Platform** — Build targets for Linux, macOS, Windows, and Raspberry Pi; Docker image included
- **Safe Self-Update** — `cloodsys3 update` verifies release checksums before replacing the binary; the startup check can be switched off

## Quick Start

### 1. Build

```bash
make build
```

The binary is created in `build/`. No config file needed — it runs with sensible defaults (data under `./.cloodsys3/` in the current directory). Prefer a release binary? See [Install](#install).

### 2. Create a Bucket and Credentials

```bash
./cloodsys3 bucket create my-bucket
./cloodsys3 credential create my-bucket
```

Output:
```
Bucket:     my-bucket
Access Key: AK7F2B9X4MPLEPHOTO1
Secret Key: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLE

Warning: Save the secret key now. It will not be shown again.
```

### 3. Start the Server

```bash
./cloodsys3 serve
```

The server starts on port `9000` by default.

### 4. Use with AWS CLI

```bash
# Configure a profile
aws configure --profile cloodsy
# Enter Access Key, Secret Key, Region: us-east-1

# Upload
aws --endpoint-url http://localhost:9000 --profile cloodsy \
    s3 cp file.txt s3://my-bucket/file.txt

# List
aws --endpoint-url http://localhost:9000 --profile cloodsy \
    s3 ls s3://my-bucket/

# Sync a directory
aws --endpoint-url http://localhost:9000 --profile cloodsy \
    s3 sync ./local-dir s3://my-bucket/remote-dir/

# Download
aws --endpoint-url http://localhost:9000 --profile cloodsy \
    s3 cp s3://my-bucket/file.txt downloaded.txt

# Delete
aws --endpoint-url http://localhost:9000 --profile cloodsy \
    s3 rm s3://my-bucket/file.txt
```

## CLI Reference

All CLI commands work while the server is running. SQLite WAL mode allows concurrent access from the server and CLI simultaneously.

### Server

```bash
./cloodsys3 serve                        # Start with defaults
./cloodsys3 serve -config config.yaml    # Start with custom config
```

### Bucket Management

```bash
./cloodsys3 bucket create <name>                          # Create a bucket
./cloodsys3 bucket create <name> --storage-dir=/mnt/ssd   # Create with custom storage
./cloodsys3 bucket list                                   # List all buckets
./cloodsys3 bucket info <name>                            # Show details
./cloodsys3 bucket delete <name>                          # Delete (refuses if any object/version/marker exists)
./cloodsys3 bucket delete <name> --force                  # Delete together with all objects, versions and cached variants
./cloodsys3 bucket quota <name> 10GB                      # Set storage limit (KB/MB/GB/TB, 0=unlimited)
./cloodsys3 bucket storage <name> --dir=/new/path         # Move storage to new location (same filesystem)
./cloodsys3 bucket storage <name> --dir=                  # Reset to default storage
./cloodsys3 bucket public-read enable <name>              # Allow anonymous object reads (GET/HEAD)
./cloodsys3 bucket public-read disable <name>             # Disable anonymous reads (default)
./cloodsys3 bucket public-read status <name>              # Show public-read state
./cloodsys3 bucket webdav enable <name>                   # Make bucket mountable via WebDAV
./cloodsys3 bucket webdav disable <name>                  # Disable WebDAV for the bucket (default)
./cloodsys3 bucket webdav status <name>                   # Show WebDAV state
./cloodsys3 bucket reprocess <name> [--prefix=p/]         # Regenerate optimized image variants
```

### Credential Management

Each bucket can have multiple access/secret key pairs. A key grants access only to the bucket it belongs to.

```bash
./cloodsys3 credential create <bucket>              # Create read-write key pair
./cloodsys3 credential create <bucket> --read-only  # Create read-only key pair
./cloodsys3 credential list <bucket>                # List keys for a bucket
./cloodsys3 credential delete <access-key>          # Revoke a specific key
```

**Permission model:**
- `read-write` (default) — GET, PUT, DELETE, HEAD, POST — full access
- `read-only` — GET, HEAD, ListObjects only — writes return `AccessDenied`

### Versioning

```bash
./cloodsys3 bucket versioning enable <name>    # Enable versioning
./cloodsys3 bucket versioning suspend <name>   # Suspend versioning
./cloodsys3 bucket versioning status <name>    # Check current state
```

When enabled, every PUT creates a new version with a unique ID. Deleting an object creates a delete marker instead of removing data. Previous versions remain accessible by version ID.

### Lifecycle Rules

Automatically expire objects after a specified number of days.

```bash
./cloodsys3 bucket lifecycle set <name> --days=30                  # Expire all objects after 30 days
./cloodsys3 bucket lifecycle set <name> --days=7 --prefix=logs/    # Expire only objects under logs/
./cloodsys3 bucket lifecycle get <name>                            # List rules
./cloodsys3 bucket lifecycle delete <name>                         # Delete all rules
./cloodsys3 bucket lifecycle delete <name> --prefix=logs/          # Delete specific rule
```

The background cleaner runs at a configurable interval (default `1h`) and removes expired objects in batches of 100. `lifecycle get` shows every rule with its status and actions.

Rules written through the S3 API (`PutBucketLifecycle`) support the full set: `<Filter>` (prefix), `<Status>` (`Enabled`/`Disabled`), `<Expiration>`, `<NoncurrentVersionExpiration>`, `<AbortIncompleteMultipartUpload>` and `<ExpiredObjectDeleteMarker>`. Prefix matching is case-sensitive.

### Custom Storage Directories

By default all buckets store data under the global `root_dir`. You can assign a custom storage directory per bucket so different buckets can live on different disks:

```bash
# Hot data on SSD
./cloodsys3 bucket create hot-data --storage-dir=/mnt/ssd

# Archives on HDD
./cloodsys3 bucket create archives --storage-dir=/mnt/hdd

# Move an existing bucket to a new location (migrates data)
./cloodsys3 bucket storage my-bucket --dir=/mnt/nvme

# Verify
./cloodsys3 bucket info hot-data
# Storage: /mnt/ssd/hot-data/ (custom)
```

Notes:
- `--storage-dir` / `--dir` must be an absolute path
- `bucket storage` moves the data directory **and** the sibling `.<bucket>-multipart/` and `.<bucket>-cache/` trees with a rename, so source and target must be on the same filesystem. Cross-filesystem moves are refused; the command prints the `cp -a` steps to do it by hand.
- Deleting a bucket removes all three trees regardless of location
- **A running server caches storage locations.** After `bucket create --storage-dir` or `bucket storage` from the CLI, restart the server (or make the change through the admin API instead) — the CLI prints a reminder.
- **systemd:** the shipped unit uses `ProtectSystem=strict`, so every custom storage directory must be added to `ReadWritePaths=` in a drop-in (`sudo systemctl edit cloodsys3`), otherwise writes to that bucket fail with permission errors. See [Running as a Service](#running-as-a-service-systemd).

### Image Resizing & Optimization

Cloodsy S3 can transform images on the fly and optimize them on upload. Both features keep the **original object byte-for-byte intact** — every derivative is content-addressed and cached in a sibling `.<bucket>-cache/` tree (which honors custom storage directories) and is invalidated automatically when the object changes or is deleted.

**On-the-fly resizing** — append query parameters to any image object URL:

```
GET /<bucket>/<key>?w=800&h=600&m=f&q=75
```

| Param | Meaning | Values |
|-------|---------|--------|
| `w` | Target width (px) | 1–5000 |
| `h` | Target height (px) | 1–5000 |
| `m` | Fit mode | `f` = fit/proportional (default), `c` = cover/center-crop, `e` = exact/stretch |
| `q` | JPEG quality | 1–100 (default 75) |

Decoders: JPEG, PNG, GIF, and WebP (decode-only — WebP/GIF inputs are transcoded to JPEG/PNG since there is no pure-Go WebP encoder). Sources larger than ~50 MP are rejected before decode to bound memory, and any transform error transparently falls back to serving the original. Transformed responses are returned `inline` with a distinct ETag and a long `Cache-Control`, so they render directly in `<img>` tags and cache well at CDNs.

**Optimization on upload** — enable in config to auto-generate a smaller, re-encoded variant for each uploaded image:

```yaml
image:
  enabled: true
  sync_max_bytes: 2000000   # <= optimize inline; larger uploads go to the background queue
  quality: 75
  workers: 2
  queue_size: 256
```

To (re)generate variants for objects that already exist, run:

```bash
./cloodsys3 bucket reprocess <name>              # Whole bucket
./cloodsys3 bucket reprocess <name> --prefix=img/ # Only a prefix
```

### Public-Read Buckets

By default every request requires SigV4 or a presigned URL. Flagging a bucket **public-read** additionally allows **anonymous GET/HEAD of objects** — useful for public assets and for resized image links embedded in web pages. Listings and all writes still require authentication, so a public bucket exposes object reads but never enumeration. Signed access is unchanged.

```bash
./cloodsys3 bucket public-read enable <name>
./cloodsys3 bucket public-read disable <name>
./cloodsys3 bucket public-read status <name>
```

```html
<!-- Works without signing when the bucket is public-read -->
<img src="http://localhost:9000/images/photo.jpg?w=400&q=70">
```

### WebDAV Mounting

> **⚠️ Experimental.** WebDAV is a young feature and behavior varies by client. Enable it only on buckets you control and accept the trade-offs below. It is **opt-in per bucket** and **off by default**; write operations require a **read-write** credential.

Cloodsy S3 can expose buckets over WebDAV so they can be mounted as a network drive. WebDAV runs on its own port and is gated twice: a global master switch in config, and a **per-bucket opt-in** (every bucket is OFF by default).

```yaml
webdav:
  enabled: true       # master switch — runs the WebDAV server
  listen: ":9002"
  prefix: "/"
  tls:
    enabled: true     # strongly recommended: Basic auth carries the secret key on every request
    cert_file: ""     # leave empty to reuse server.tls
    key_file: ""
  lock_timeout: "10m"
```

WebDAV works on versioned buckets too: writes create new versions and deletes add delete markers, exactly like the S3 API. Windows refuses Basic auth over plain HTTP by default, so enable `webdav.tls` (or terminate TLS in a reverse proxy) for anything beyond localhost.

```bash
./cloodsys3 bucket webdav enable <name>    # opt the bucket in
./cloodsys3 bucket webdav disable <name>
./cloodsys3 bucket webdav status <name>
```

**Mounting** — authenticate with HTTP Basic Auth where the **username is an access key** and the **password is its secret key**. One credential maps to exactly one bucket, which becomes the mount root.

- **Windows:** File Explorer → "Map network drive" → `http://<host>:9002/`
- **macOS:** Finder → Cmd+K → `http://<host>:9002/`
- **Linux/CLI:** `rclone`, `cadaver`, or `davfs2`

Read-only credentials may browse and download but receive `403 Forbidden` on any write (PUT/DELETE/MKCOL/MOVE). Files written over WebDAV are real S3 objects and are visible through the S3 API as well. Folders are virtual key prefixes; locks are in-memory (non-persistent across restarts).

**Caching / stale listings.** The server holds **no cache of its own** — directory listings are read live from the metadata DB and content live from disk on every request, and responses are sent with `Cache-Control: no-cache`. Any staleness you see comes from the **OS WebDAV client** (the Windows WebClient redirector, Finder, and davfs2 all cache directory listings and file attributes). If listings look out of date, refresh the client or tune its cache (e.g. Windows `FileAttributesLimitInBytes`, davfs2 `cache_size`/`dir_refresh`). Deleting the last file in a folder also removes the now-empty folder marker automatically.

### Webhook Notifications

Receive HTTP callbacks when objects are created or deleted.

```bash
./cloodsys3 bucket webhook add <name> --url=https://example.com/hook
./cloodsys3 bucket webhook add <name> --url=https://example.com/hook --events=s3:ObjectCreated:* --secret=mysecret
./cloodsys3 bucket webhook list <name>
./cloodsys3 bucket webhook delete <name> --id=<webhook-id>
```

**Supported events:** `s3:ObjectCreated:Put`, `s3:ObjectCreated:Copy`, `s3:ObjectRemoved:Delete`, or `*` for all.

When a secret is provided, requests include an `X-Cloodsy-Signature` HMAC-SHA256 header for payload verification. Passing `--secret` on the command line is visible in `ps` and shell history (the CLI warns); the secret is never printed back. Webhook URLs must be `http(s)` and are validated on creation. Events are delivered asynchronously with 3 retries and exponential backoff (1s, 2s, 4s). The payload follows the AWS S3 event notification format.

### Admin Management

```bash
./cloodsys3 admin create <username>                     # Prompts for the password (no echo, asked twice)
./cloodsys3 admin create <username> --generate          # Random 20-character password, printed once
./cloodsys3 admin create <username> --password=mypass   # For scripts only (argv is visible in ps / history)
./cloodsys3 admin list                                  # List admin users
./cloodsys3 admin delete <username>                     # Delete admin user (the last admin cannot be deleted)
./cloodsys3 admin password <username>                   # Reset interactively
./cloodsys3 admin password <username> --generate        # Reset with a generated password
./cloodsys3 admin password <username> --password=new    # Reset from a script
```

Admin users are used to authenticate with the Admin REST API. Passwords must be 8–72 bytes and are stored as bcrypt hashes. A custom password is never echoed back; `--password` prints a one-line warning because command-line arguments are visible to other local users and land in shell history.

### Version Info

```bash
./cloodsys3 version     # first line: "Cloodsy S3 vX.Y.Z", then commit and build date
```

## S3 API Operations

### Object Operations

| Operation | Method | Endpoint |
|-----------|--------|----------|
| PutObject | PUT | `/<bucket>/<key>` |
| GetObject | GET | `/<bucket>/<key>` |
| HeadObject | HEAD | `/<bucket>/<key>` |
| DeleteObject | DELETE | `/<bucket>/<key>` |
| DeleteObjects | POST | `/<bucket>?delete` |
| CopyObject | PUT | `/<bucket>/<key>` + `X-Amz-Copy-Source` |

### Bucket Operations

| Operation | Method | Endpoint | Notes |
|-----------|--------|----------|-------|
| ListBuckets | GET | `/` | |
| CreateBucket | PUT | `/<bucket>` | Buckets are created with the CLI or the admin API. A PUT for the credential's own bucket returns `BucketAlreadyOwnedByYou` (so `aws s3 mb` and Terraform stay idempotent); any other name is denied |
| DeleteBucket | DELETE | `/<bucket>` | |
| HeadBucket | HEAD | `/<bucket>` | |
| GetBucketLocation | GET | `/<bucket>?location` | |
| ListObjects | GET | `/<bucket>` | |
| ListObjectsV2 | GET | `/<bucket>?list-type=2` | |
| ListObjectVersions | GET | `/<bucket>?versions` | |

### Versioning & Lifecycle

| Operation | Method | Endpoint |
|-----------|--------|----------|
| GetBucketVersioning | GET | `/<bucket>?versioning` |
| PutBucketVersioning | PUT | `/<bucket>?versioning` |
| GetBucketLifecycle | GET | `/<bucket>?lifecycle` |
| PutBucketLifecycle | PUT | `/<bucket>?lifecycle` |
| DeleteBucketLifecycle | DELETE | `/<bucket>?lifecycle` |

### Multipart Upload

| Operation | Method | Endpoint |
|-----------|--------|----------|
| CreateMultipartUpload | POST | `/<bucket>/<key>?uploads` |
| UploadPart | PUT | `/<bucket>/<key>?partNumber=N&uploadId=X` |
| UploadPartCopy | PUT | `/<bucket>/<key>?partNumber=N&uploadId=X` + `X-Amz-Copy-Source` |
| ListParts | GET | `/<bucket>/<key>?uploadId=X` |
| ListMultipartUploads | GET | `/<bucket>?uploads` |
| CompleteMultipartUpload | POST | `/<bucket>/<key>?uploadId=X` |
| AbortMultipartUpload | DELETE | `/<bucket>/<key>?uploadId=X` |

### Notifications

| Operation | Method | Endpoint |
|-----------|--------|----------|
| GetBucketNotification | GET | `/<bucket>?notification` |
| PutBucketNotification | PUT | `/<bucket>?notification` |
| DeleteBucketNotification | DELETE | `/<bucket>?notification` |

### Compatibility Stubs

These operations are accepted for compatibility with tools like Terraform, s3cmd, and rclone but do not persist data:

| Operation | Method | Endpoint | Behavior |
|-----------|--------|----------|----------|
| GetBucketAcl | GET | `/<bucket>?acl` | Returns FULL_CONTROL |
| PutBucketAcl | PUT | `/<bucket>?acl` | Accepted, ignored |
| GetObjectAcl | GET | `/<bucket>/<key>?acl` | Returns FULL_CONTROL |
| PutObjectAcl | PUT | `/<bucket>/<key>?acl` | Accepted, ignored |
| GetBucketEncryption | GET | `/<bucket>?encryption` | Returns SSE-S3 (AES256) |
| PutBucketEncryption | PUT | `/<bucket>?encryption` | Accepted, ignored |
| GetBucketTagging | GET | `/<bucket>?tagging` | Returns NoSuchTagSet |
| PutBucketTagging | PUT | `/<bucket>?tagging` | Accepted, ignored |
| DeleteBucketTagging | DELETE | `/<bucket>?tagging` | No-op |
| GetObjectTagging | GET | `/<bucket>/<key>?tagging` | Returns empty TagSet |
| PutObjectTagging | PUT | `/<bucket>/<key>?tagging` | Accepted, ignored |
| DeleteObjectTagging | DELETE | `/<bucket>/<key>?tagging` | No-op |
| GetBucketPolicy | GET | `/<bucket>?policy` | Returns NoSuchBucketPolicy |
| PutBucketPolicy | PUT | `/<bucket>?policy` | Accepted, ignored |
| DeleteBucketPolicy | DELETE | `/<bucket>?policy` | No-op |

Unknown sub-resources (for example `?cors`, `?website`, `?replication`) return `NotImplemented` and never fall through to the plain bucket/object handlers.

### S3 Compatibility Notes

- **Multipart ETag** is S3-style: `"<md5-of-concatenated-part-md5s>-<partCount>"`. Clients that compare ETags (rclone, s3cmd, the AWS SDK checksum validators) behave as they do against AWS.
- **Minimum part size** is 5 MiB for every part except the last (`EntityTooSmall` otherwise), matching AWS.
- **Integrity headers are verified**: `Content-MD5`, `x-amz-content-sha256` (real digests, `UNSIGNED-PAYLOAD`, and `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` with per-chunk signatures), and `x-amz-checksum-crc32/crc32c/sha1/sha256`. A mismatch is rejected with `BadDigest` / `XAmzContentSHA256Mismatch` / `SignatureDoesNotMatch`. Set `server.require_payload_signature: true` to refuse `UNSIGNED-PAYLOAD` on header-authenticated requests.
- **`encoding-type=url`** is supported on all list operations.
- **`response-*` query overrides** (`response-content-type`, `response-content-disposition`, `response-cache-control`, …) are honored on `GetObject`, including presigned URLs. Objects are otherwise served with the stored `Content-Type` and no forced `Content-Disposition`.
- **Lifecycle** supports `Filter`, `Status`, `Expiration`, `NoncurrentVersionExpiration`, `AbortIncompleteMultipartUpload` and `ExpiredObjectDeleteMarker`.
- **Versioning** can be `Enabled` or `Suspended`; there is no way back to the unversioned state, exactly like AWS.
- **CreateBucket via the S3 API is denied**: credentials are bound to one existing bucket, so a PUT for that bucket answers `BucketAlreadyOwnedByYou` and any other name is refused. Create buckets with `cloodsys3 bucket create` or the admin API.
- **Virtual-hosted-style** requests work when the domain is listed in `server.virtual_host_domains`; path-style is always available.

## Authentication

Cloodsy S3 uses AWS Signature Version 4 (SigV4) for authentication, supporting both header-based signing and presigned URLs.

- Each credential is scoped to a single bucket
- `ListBuckets` only returns the bucket associated with the credential in use
- Multiple credentials can be created per bucket
- Credentials support `read-write` or `read-only` permissions
- Chunked upload signing (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) is supported and every chunk signature is verified; malformed or truncated chunk streams are rejected instead of being stored
- Payload hashes (`x-amz-content-sha256`) are verified when the client sends a real digest
- Time skew tolerance: 5 minutes

### Presigned URLs

Generate time-limited URLs for sharing without exposing credentials:

```bash
# AWS CLI (valid for 1 hour)
aws --endpoint-url http://localhost:9000 --profile cloodsy \
    s3 presign s3://my-bucket/photo.jpg --expires-in 3600
```

```python
# boto3 (valid for 1 hour, max 7 days)
url = s3.generate_presigned_url(
    'get_object',
    Params={'Bucket': 'my-bucket', 'Key': 'photo.jpg'},
    ExpiresIn=3600
)
```

## Admin REST API

The Admin API provides a JSON-based management interface on a separate port. Enable it in your config:

```yaml
admin:
  enabled: true
  listen: "127.0.0.1:9001"   # or enable admin.tls before exposing it
  cors_origins: ["*"]        # only needed for browser clients such as the Flutter web GUI
  tls:
    enabled: false           # true = HTTPS; leave cert/key empty to reuse server.tls
    cert_file: ""
    key_file: ""
  trusted_proxies: []        # e.g. ["127.0.0.1", "10.0.0.0/8"] when behind a reverse proxy
  session_ttl: "8h"
```

The admin API carries passwords and secret keys: bind it to localhost, enable `admin.tls`, or put a TLS reverse proxy in front. The server logs a warning at start-up when the admin listener is reachable without TLS on a non-loopback address. Login attempts are rate limited per client IP; `X-Forwarded-For` is only trusted from the addresses in `admin.trusted_proxies`.

### Authentication

```bash
# Create an admin user
./cloodsys3 admin create myadmin

# Login via API
curl -X POST http://localhost:9001/admin/login \
  -H "Content-Type: application/json" \
  -d '{"username":"myadmin","password":"<password>"}'

# Response: {"token":"cks_...","expires_in":28800}

# Use token for all subsequent requests
curl http://localhost:9001/admin/buckets \
  -H "Authorization: Bearer cks_..."
```

Sessions expire after 8 hours by default (`admin.session_ttl`). Tokens are stored in-memory and cleared on server restart.

### Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/admin/login` | Login, returns session token |
| POST | `/admin/logout` | Logout |
| GET | `/admin/status` | Server status |
| GET | `/admin/admins` | List admin users |
| POST | `/admin/admins` | Create admin |
| DELETE | `/admin/admins/{username}` | Delete admin |
| PUT | `/admin/admins/{username}/password` | Change password |
| GET | `/admin/buckets` | List all buckets with stats |
| POST | `/admin/buckets` | Create bucket |
| GET | `/admin/buckets/{name}` | Bucket details |
| DELETE | `/admin/buckets/{name}` | Delete bucket |
| PUT | `/admin/buckets/{name}/quota` | Set quota |
| PUT | `/admin/buckets/{name}/storage` | Change storage directory |
| GET/PUT | `/admin/buckets/{name}/versioning` | Get/set versioning |
| PUT | `/admin/buckets/{name}/public-read` | Toggle anonymous object reads |
| PUT | `/admin/buckets/{name}/webdav` | Toggle WebDAV mountability |
| POST | `/admin/buckets/{name}/reprocess` | Regenerate optimized image variants (background) |
| GET | `/admin/buckets/{name}/credentials` | List credentials (includes secret keys) |
| POST | `/admin/buckets/{name}/credentials` | Create credential |
| DELETE | `/admin/credentials/{accessKey}` | Delete credential |
| GET | `/admin/buckets/{name}/lifecycle` | List lifecycle rules |
| POST | `/admin/buckets/{name}/lifecycle` | Create lifecycle rule |
| DELETE | `/admin/buckets/{name}/lifecycle` | Delete lifecycle rules |
| GET | `/admin/buckets/{name}/webhooks` | List webhooks |
| POST | `/admin/buckets/{name}/webhooks` | Create webhook |
| DELETE | `/admin/webhooks/{id}` | Delete webhook |
| GET | `/admin/buckets/{name}/objects` | List objects (prefix/delimiter) |
| DELETE | `/admin/buckets/{name}/objects/{key}` | Delete object |
| POST | `/admin/buckets/{name}/objects/delete-prefix` | Delete folder |

## Configuration

Configuration is optional. The server runs with sensible defaults. To customize, pass a YAML file (or set `CLOODSYS3_CONFIG`). Unknown keys are rejected and every value is validated at start-up, so typos fail fast. [`config.yaml.example`](config.yaml.example) documents every key.

```bash
./cloodsys3 serve -config config.yaml
```

```yaml
server:
  listen: ":9000"
  region: "us-east-1"
  tls:
    enabled: false
    cert_file: ""
    key_file: ""
  cors_origins: []                  # browser origins allowed on the S3 API (empty = no CORS headers)
  virtual_host_domains: []          # ["s3.example.com"] enables <bucket>.s3.example.com addressing
  max_connections: 0                # 0 = unlimited
  idle_timeout: "60s"               # per-request stall timeout; large transfers are not capped
  require_payload_signature: false  # reject UNSIGNED-PAYLOAD header-auth requests

database:
  path: "./.cloodsys3/cloodsys3.db"
  busy_timeout: 5000          # Write lock wait time (ms)
  cache_size: 64000           # Page cache size (KB)
  mmap_size: 134217728        # Memory-mapped I/O (bytes, 128MB)
  max_readers: 4              # Parallel read connections

storage:
  root_dir: "./.cloodsys3/data"
  multipart_max_age: "24h"    # Auto-cleanup for incomplete uploads
  lifecycle_interval: "1h"    # How often to check lifecycle rules

logging:
  level: "info"               # debug, info, warn, error
  format: "text"              # text or json

admin:
  enabled: false              # Enable Admin REST API
  listen: ":9001"             # Admin API port (separate from S3)
  cors_origins: []            # Allowed origins for browser clients
  tls:                        # Per-listener TLS (empty cert/key = reuse server.tls)
    enabled: false
    cert_file: ""
    key_file: ""
  trusted_proxies: []         # IPs/CIDRs whose X-Forwarded-For is honored
  session_ttl: "8h"           # Admin session lifetime

image:
  enabled: false              # Auto image optimization on upload (original preserved)
  sync_max_bytes: 2000000     # <= optimize inline; larger uploads go async
  quality: 75                 # JPEG quality (1-100)
  workers: 2                  # Async optimization workers
  queue_size: 256             # Async queue buffer size
  max_source_bytes: 33554432  # Never decode images larger than 32 MB
  max_concurrent: 4           # Simultaneous decodes (transforms + optimizer)
  cache_max_bytes: 1073741824 # Per-bucket variant cache bound (1 GB, 0 = unlimited)
  dimension_step: 16          # Round requested sizes up to a multiple of 16

webdav:
  enabled: false              # Run the WebDAV server (master switch)
  listen: ":9002"             # WebDAV port (separate from S3 and Admin)
  prefix: "/"                 # URL path prefix; buckets are opt-in (default OFF)
  tls:                        # Per-listener TLS (empty cert/key = reuse server.tls)
    enabled: false
    cert_file: ""
    key_file: ""
  lock_timeout: "10m"         # Maximum WebDAV lock lifetime

update:
  check_on_start: true        # Set false to never contact GitHub at start-up
```

### Environment Variables

The most common settings can be overridden without a config file — useful under systemd (`EnvironmentFile=/etc/cloodsys3/env`) and in containers. Environment values take precedence over the YAML file.

| Variable | Config key |
|----------|------------|
| `CLOODSYS3_CONFIG` | path of the config file (same as `-config`) |
| `CLOODSYS3_LISTEN` | `server.listen` |
| `CLOODSYS3_REGION` | `server.region` |
| `CLOODSYS3_TLS_ENABLED` / `CLOODSYS3_TLS_CERT` / `CLOODSYS3_TLS_KEY` | `server.tls.*` |
| `CLOODSYS3_DB_PATH` | `database.path` |
| `CLOODSYS3_DATA_DIR` | `storage.root_dir` |
| `CLOODSYS3_LOG_LEVEL` / `CLOODSYS3_LOG_FORMAT` | `logging.*` |
| `CLOODSYS3_ADMIN_ENABLED` / `CLOODSYS3_ADMIN_LISTEN` | `admin.enabled` / `admin.listen` |
| `CLOODSYS3_WEBDAV_ENABLED` / `CLOODSYS3_WEBDAV_LISTEN` | `webdav.enabled` / `webdav.listen` |
| `CLOODSYS3_UPDATE_CHECK` | `update.check_on_start` |

## Install

### Quick Install (Linux/macOS)

```bash
curl -fsSL https://raw.githubusercontent.com/onaonbir/Cloodsy-S3/main/install.sh | bash
```

Detects your OS and architecture automatically, downloads the latest release, verifies its SHA-256 against the release's `checksums.txt`, and installs to `/usr/local/bin/`. The script is safe to re-run (it skips when the same version is already installed) and accepts a pinned version:

```bash
curl -fsSL https://raw.githubusercontent.com/onaonbir/Cloodsy-S3/main/install.sh | bash -s -- --version v1.2.0
```

### Manual Download

Download from [GitHub Releases](https://github.com/onaonbir/Cloodsy-S3/releases/latest):

| Platform | File |
|----------|------|
| Linux x64 | `cloodsys3-linux-amd64.tar.gz` |
| Linux ARM64 (Raspberry Pi) | `cloodsys3-linux-arm64.tar.gz` |
| Linux ARMv7 | `cloodsys3-linux-armv7.tar.gz` |
| Windows x64 | `cloodsys3-windows-amd64.zip` |
| macOS Apple Silicon | `cloodsys3-darwin-arm64.tar.gz` |
| macOS Intel | `cloodsys3-darwin-amd64.tar.gz` |

Every archive contains the `cloodsys3` binary (`cloodsys3.exe` on Windows), `LICENSE` and `THIRD_PARTY_LICENSES.md`. Verify a manual download with `sha256sum -c checksums.txt`.

### Docker

A multi-stage `Dockerfile` builds the static binary and ships it on a distroless, non-root image. All state lives under the `/data` volume.

```bash
docker build -t cloodsys3 .
docker run -d --name cloodsys3 -p 9000:9000 -v cloodsys3-data:/data cloodsys3

# Manage buckets/credentials with the same binary inside the container
docker exec cloodsys3 /cloodsys3 bucket create my-bucket
docker exec cloodsys3 /cloodsys3 credential create my-bucket
docker exec -it cloodsys3 /cloodsys3 admin create admin --generate
```

Or with Compose (`docker-compose.yml` publishes the S3 port, keeps the admin API on localhost and enables JSON logs):

```bash
docker compose up -d
docker compose exec cloodsys3 /cloodsys3 bucket create my-bucket
```

The image sets `CLOODSYS3_DB_PATH=/data/cloodsys3.db`, `CLOODSYS3_DATA_DIR=/data/data` and `CLOODSYS3_UPDATE_CHECK=false`; every other setting comes from the environment variables above or a mounted config file (`CLOODSYS3_CONFIG=/config/config.yaml`). Custom `--storage-dir` paths must be mounted into the container as well.
## Update

```bash
# Check for updates
./cloodsys3 update --check

# Update to latest version (auto-detects platform)
./cloodsys3 update
```

The `update` command downloads the latest release from GitHub, verifies the archive's SHA-256 against the release's `checksums.txt`, extracts the `cloodsys3` binary and swaps it in atomically (the previous binary is restored if anything fails). It refuses to install anything without a matching checksum. On Windows the running `.exe` is renamed to `.old` and can be deleted after the restart. Restart the server after updating.

Dev builds (`make build` without a release version) never self-update. Use `install.sh` or a release archive instead.

The server also checks for updates once on startup and logs a warning if a newer version is available. Opt out for air-gapped or privacy-sensitive deployments:

```yaml
update:
  check_on_start: false     # or CLOODSYS3_UPDATE_CHECK=false
```

## Deployment

Only the binary needs to be deployed. All runtime data is created automatically:

```bash
make build
scp build/cloodsys3 server:/opt/cloodsys3/
```

On the server:
```bash
cd /opt/cloodsys3
./cloodsys3 bucket create my-bucket
./cloodsys3 credential create my-bucket
./cloodsys3 admin create myadmin          # Optional: for Admin API
./cloodsys3 serve
```

Runtime directory structure:
```
/opt/cloodsys3/
├── cloodsys3                  # Binary
├── config.yaml                # Optional config
└── .cloodsys3/                # Runtime data (auto-created)
    ├── cloodsys3.db           # SQLite database
    ├── cloodsys3.db-wal       # WAL file
    ├── cloodsys3.db-shm       # Shared memory
    └── data/                  # Object storage (default)
        └── my-bucket/
            ├── photo.jpg.cloodsys3ext
            └── docs/
                └── report.pdf.cloodsys3ext

# Buckets with --storage-dir use custom locations:
/mnt/ssd/
├── hot-bucket/
│   └── data.bin.cloodsys3ext
├── .hot-bucket-multipart/     # in-progress multipart parts
└── .hot-bucket-cache/         # resized / optimized image variants
```

**On-disk key encoding.** Keys map to paths one-to-one; keys that would collide or are unsafe as file names (a trailing `/` such as `dir/`, `//`, `.`/`..` segments, characters reserved on Windows, control characters) are stored percent-encoded on disk. Nothing changes for normal keys. Existing deployments are migrated automatically once on the first `serve` after upgrading; the migration is idempotent and safe to interrupt. Always use the S3 API or the CLI to move data — never rename files under the data directory by hand.

### Windows

```powershell
# Cross-compile from Linux
make build-windows

# On Windows
cd C:\CloodsyS3
.\cloodsys3.exe bucket create my-bucket -config config.yaml
.\cloodsys3.exe credential create my-bucket -config config.yaml
.\cloodsys3.exe serve -config config.yaml
```

### Running as a Service (systemd)

```bash
# Create a system user
sudo useradd -r -s /bin/false cloodsys3
sudo mkdir -p /opt/cloodsys3
sudo cp cloodsys3 config.yaml /opt/cloodsys3/
sudo chown -R cloodsys3:cloodsys3 /opt/cloodsys3

# Install service
sudo cp cloodsys3.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable cloodsys3
sudo systemctl start cloodsys3

# Check status
sudo systemctl status cloodsys3

# View logs
sudo journalctl -u cloodsys3 -f
```

The service automatically restarts on failure with a 5-second delay, waits for `network-online.target`, and runs sandboxed (`ProtectSystem=strict`, `PrivateTmp`, `PrivateDevices`, kernel/cgroup protection, restricted address families and syscall architectures, `UMask=0077`). Optional environment overrides go into `/etc/cloodsys3/env` (`EnvironmentFile=`), for example `CLOODSYS3_LOG_FORMAT=json`.

**Custom storage directories and `ReadWritePaths`.** `ProtectSystem=strict` makes the whole filesystem read-only for the service except `ReadWritePaths=/opt/cloodsys3`. Every bucket created with `--storage-dir` (or moved with `bucket storage --dir`) lives outside that path and must be added in a drop-in, otherwise all writes to that bucket fail with permission errors:

```bash
sudo systemctl edit cloodsys3
```

```ini
[Service]
ReadWritePaths=/mnt/ssd
ReadWritePaths=/mnt/hdd
```

```bash
sudo systemctl daemon-reload && sudo systemctl restart cloodsys3
```

Run CLI commands as the service user (`sudo -u cloodsys3 /opt/cloodsys3/cloodsys3 bucket create ...`) so the database and data directories keep the right ownership. Stopping or restarting the service sends `SIGTERM`; the server drains in-flight requests (up to `TimeoutStopSec=30`) before exiting.

### Secure Storage

Uploaded files are stored on disk with a `.cloodsys3ext` extension to prevent accidental execution. Additional security measures:

- **Path traversal protection** — All paths validated against base directory escape
- **Symlink attack prevention** — Files opened with `O_NOFOLLOW` flag
- **Atomic writes** — Temp file + `fsync` + rename; a failed upload never touches the existing object
- **Collision-free layout** — Keys that cannot be represented safely as file names are percent-encoded on disk (see [Deployment](#deployment))
- **Security headers** — `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`

## Security

- **S3 API** runs on its own port (default `:9000`) — exposed to S3 clients; enable `server.tls` or terminate TLS in a reverse proxy
- **Admin API** runs on a separate port (default `:9001`) — bind to localhost, enable `admin.tls`, or restrict via firewall; the server warns when it is reachable in cleartext
- **WebDAV** (default `:9002`, opt-in) — Basic auth carries the secret key on every request; enable `webdav.tls`
- **Credentials** — Per-bucket scoped, supports read-only permission
- **Admin passwords** — Stored as bcrypt hashes, never in plain text; create them interactively or with `--generate` so they never land in shell history
- **Session tokens** — 8-hour TTL by default (`admin.session_ttl`), in-memory only, cleared on restart
- **Login rate limiting** — Per client IP; `X-Forwarded-For` is only honored from `admin.trusted_proxies`
- **Upload integrity** — `Content-MD5`, `x-amz-content-sha256`, `x-amz-checksum-*` and aws-chunked chunk signatures are verified; `server.require_payload_signature` rejects unsigned payloads
- **CORS** — Off unless origins are listed in `server.cors_origins` / `admin.cors_origins`
- **Self-update** — Release archives are verified against `checksums.txt` before installation; disable the start-up check with `update.check_on_start: false`
- **Reporting** — See [SECURITY.md](SECURITY.md) for the disclosure policy and supported versions

## SDK Examples

### Python (boto3)

```python
import boto3

s3 = boto3.client(
    "s3",
    endpoint_url="http://localhost:9000",
    aws_access_key_id="AKXXXXXXXXXXXXXXXXXX",
    aws_secret_access_key="YYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYY",
    region_name="us-east-1",
)

# Upload
s3.put_object(Bucket="my-bucket", Key="test.txt", Body=b"hello world")

# Download
resp = s3.get_object(Bucket="my-bucket", Key="test.txt")
print(resp["Body"].read())

# List
for obj in s3.list_objects_v2(Bucket="my-bucket")["Contents"]:
    print(obj["Key"], obj["Size"])
```

### Go (aws-sdk-go-v2)

```go
cfg, _ := awsconfig.LoadDefaultConfig(context.TODO(),
    awsconfig.WithRegion("us-east-1"),
    awsconfig.WithCredentialsProvider(
        credentials.NewStaticCredentialsProvider("AKXX...", "YYYY...", ""),
    ),
)

client := s3sdk.NewFromConfig(cfg, func(o *s3sdk.Options) {
    o.BaseEndpoint = aws.String("http://localhost:9000")
    o.UsePathStyle = true
})

client.PutObject(context.TODO(), &s3sdk.PutObjectInput{
    Bucket: aws.String("my-bucket"),
    Key:    aws.String("test.txt"),
    Body:   strings.NewReader("hello world"),
})
```

### JavaScript (AWS SDK v3)

```javascript
import { S3Client, PutObjectCommand } from "@aws-sdk/client-s3";

const client = new S3Client({
  endpoint: "http://localhost:9000",
  region: "us-east-1",
  credentials: {
    accessKeyId: "AKXX...",
    secretAccessKey: "YYYY...",
  },
  forcePathStyle: true,
});

await client.send(new PutObjectCommand({
  Bucket: "my-bucket",
  Key: "test.txt",
  Body: "hello world",
}));
```

## Build Targets

```bash
make build            # Current platform
make build-linux      # Linux x86_64
make build-windows    # Windows x86_64
make build-mac        # macOS Apple Silicon (ARM64)
make build-mac-intel  # macOS Intel (x86_64)
make build-pi         # Raspberry Pi 3/4/5 (ARM64)
make build-armv7      # Raspberry Pi 2 / older ARM (ARMv7)
make build-all        # Linux (amd64 + arm64 + armv7)
make run              # Build and start `serve` with data under ./.cloodsys3 (gitignored)
make test             # go test ./...
make vet              # go vet ./...
make lint             # staticcheck ./... (when installed)
make vuln             # govulncheck ./...
make check            # vet + lint + test + vuln (same as CI)
make clean            # Remove the build directory only (runtime data is never touched)
make version          # Print current version
```

All builds are static (`CGO_ENABLED=0`), reproducible (`-trimpath`) and stripped (`-ldflags "-s -w"`) with the version, commit and build date injected. Releases are cut with `./release.sh <version>` and built by GitHub Actions; see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Cloodsy S3 is source-available under the [Cloodsy S3 Community License 1.0](LICENSE).

You can use it freely for personal, internal, educational, and self-hosted purposes.
Commercial resale, SaaS offerings, and competing hosted services require a separate commercial license.

Contact: [trademark@onaonbir.com](mailto:trademark@onaonbir.com)

---

<div align="center">

**Cloodsy S3** is built and maintained by **[OnaOnbir](https://onaonbir.com)**

[onaonbir.com](https://onaonbir.com)

</div>
