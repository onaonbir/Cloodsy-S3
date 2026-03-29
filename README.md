<div align="center">

# Cloodsy S3

**A lightweight, AWS SDK-compatible S3 server written in Go.**

Ships as a single binary with zero dependencies — no CGO, no external database, no runtime requirements.
All metadata is stored in an embedded SQLite database.

[![Built by OnaOnbir](https://img.shields.io/badge/Built%20by-OnaOnbir-blue?style=flat-square)](https://onaonbir.com)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![Release](https://img.shields.io/github/v/release/onaonbir/Cloodsy-S3?style=flat-square)](https://github.com/onaonbir/Cloodsy-S3/releases/latest)

[Website](https://onaonbir.com) | [Download](https://github.com/onaonbir/Cloodsy-S3/releases/latest) | [Documentation](#quick-start)

</div>

---

## Features

- **AWS SDK Compatible** — Works with AWS CLI, boto3, aws-sdk-go, s3cmd, rclone, Terraform, and any S3-compatible client
- **Single Binary** — One executable, zero dependencies, runs anywhere
- **Per-Bucket Credentials** — Each bucket gets its own access/secret key pairs with read-write or read-only permissions
- **Object Versioning** — Enable, suspend, or disable versioning per bucket with full delete marker support
- **Lifecycle Rules** — Automatic object expiration based on age and prefix filters
- **Webhook Notifications** — Real-time HTTP callbacks for object events with HMAC signing
- **Bucket Quotas** — Per-bucket storage limits to prevent disk exhaustion
- **Custom Storage Directories** — Per-bucket storage paths for multi-disk setups (SSD for hot data, HDD for archives)
- **Presigned URLs** — Time-limited download/upload links without sharing credentials
- **Multipart Upload** — Large file uploads with part copy, listing, and automatic stale upload cleanup
- **Range Requests** — Partial file downloads via standard HTTP Range headers
- **Conditional Requests** — If-Match, If-None-Match, If-Modified-Since, If-Unmodified-Since support
- **Server-Side Copy** — Copy objects between buckets without re-uploading, including partial copy via ranges
- **Admin REST API** — Full management API with session-based authentication for GUI/automation
- **CORS Support** — Browser-based S3 clients work out of the box
- **TLS Support** — Optional HTTPS with certificate configuration
- **Secure Storage** — Files stored with `.cloodsys3ext` extension, path traversal and symlink attack protection
- **Cross-Platform** — Build targets for Linux, macOS, Windows, and Raspberry Pi

## Quick Start

### 1. Build

```bash
make build
```

The binary is created in `build/`. No config file needed — it runs with sensible defaults.

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
./cloodsys3 bucket delete <name>                          # Delete (must be empty)
./cloodsys3 bucket quota <name> 10GB                      # Set storage limit (KB/MB/GB/TB, 0=unlimited)
./cloodsys3 bucket storage <name> --dir=/new/path         # Move storage to new location
./cloodsys3 bucket storage <name> --dir=                  # Reset to default storage
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

The background cleaner runs at a configurable interval (default `1h`) and removes expired objects in batches of 100.

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
- Deleting a bucket removes its storage directory regardless of location
- Server restart required after CLI-based storage changes while server is running

### Webhook Notifications

Receive HTTP callbacks when objects are created or deleted.

```bash
./cloodsys3 bucket webhook add <name> --url=https://example.com/hook
./cloodsys3 bucket webhook add <name> --url=https://example.com/hook --events=s3:ObjectCreated:* --secret=mysecret
./cloodsys3 bucket webhook list <name>
./cloodsys3 bucket webhook delete <name> --id=<webhook-id>
```

**Supported events:** `s3:ObjectCreated:Put`, `s3:ObjectCreated:Copy`, `s3:ObjectRemoved:Delete`, or `*` for all.

When a secret is provided, requests include an `X-Cloodsy-Signature` HMAC-SHA256 header for payload verification. Events are delivered asynchronously with 3 retries and exponential backoff (1s, 2s, 4s). The payload follows the AWS S3 event notification format.

### Admin Management

```bash
./cloodsys3 admin create <username>                     # Create with auto-generated password
./cloodsys3 admin create <username> --password=mypass    # Create with custom password
./cloodsys3 admin list                                  # List admin users
./cloodsys3 admin delete <username>                     # Delete admin user
./cloodsys3 admin password <username>                   # Reset with auto-generated password
./cloodsys3 admin password <username> --password=new    # Reset with custom password
```

Admin users are used to authenticate with the Admin REST API. Passwords are stored as bcrypt hashes.

### Version Info

```bash
./cloodsys3 version
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

| Operation | Method | Endpoint |
|-----------|--------|----------|
| ListBuckets | GET | `/` |
| CreateBucket | PUT | `/<bucket>` |
| DeleteBucket | DELETE | `/<bucket>` |
| HeadBucket | HEAD | `/<bucket>` |
| GetBucketLocation | GET | `/<bucket>?location` |
| ListObjects | GET | `/<bucket>` |
| ListObjectsV2 | GET | `/<bucket>?list-type=2` |
| ListObjectVersions | GET | `/<bucket>?versions` |

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

## Authentication

Cloodsy S3 uses AWS Signature Version 4 (SigV4) for authentication, supporting both header-based signing and presigned URLs.

- Each credential is scoped to a single bucket
- `ListBuckets` only returns the bucket associated with the credential in use
- Multiple credentials can be created per bucket
- Credentials support `read-write` or `read-only` permissions
- Chunked upload signing (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) is supported
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
  listen: ":9001"
  cors_origins: ["*"]
```

### Authentication

```bash
# Create an admin user
./cloodsys3 admin create myadmin

# Login via API
curl -X POST http://localhost:9001/admin/login \
  -H "Content-Type: application/json" \
  -d '{"username":"myadmin","password":"<password>"}'

# Response: {"token":"cks_...","expires_in":86400}

# Use token for all subsequent requests
curl http://localhost:9001/admin/buckets \
  -H "Authorization: Bearer cks_..."
```

Sessions expire after 24 hours. Tokens are stored in-memory and cleared on server restart.

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

Configuration is optional. The server runs with sensible defaults. To customize, pass a YAML file:

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
  cors_origins:               # Allowed origins for CORS
    - "*"
```

## Install

### Quick Install (Linux/macOS)

```bash
curl -fsSL https://raw.githubusercontent.com/onaonbir/Cloodsy-S3/main/install.sh | bash
```

Detects your OS and architecture automatically, downloads the latest release, and installs to `/usr/local/bin/`.

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

## Update

```bash
# Check for updates
./cloodsys3 update --check

# Update to latest version (auto-detects platform)
./cloodsys3 update
```

The `update` command downloads the latest release from GitHub and replaces the current binary. Restart the server after updating.

The server also checks for updates on startup and logs a warning if a newer version is available.

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
└── hot-bucket/
    └── data.bin.cloodsys3ext
```

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

The service automatically restarts on failure with a 5-second delay.

### Secure Storage

Uploaded files are stored on disk with a `.cloodsys3ext` extension to prevent accidental execution. Additional security measures:

- **Path traversal protection** — All paths validated against base directory escape
- **Symlink attack prevention** — Files opened with `O_NOFOLLOW` flag
- **Atomic writes** — Temp file + rename pattern prevents partial reads
- **Security headers** — `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`

## Security

- **S3 API** runs on its own port (default `:9000`) — exposed to S3 clients
- **Admin API** runs on a separate port (default `:9001`) — restrict via firewall to trusted networks
- **Credentials** — Per-bucket scoped, supports read-only permission
- **Admin passwords** — Stored as bcrypt hashes, never in plain text
- **Session tokens** — 24-hour TTL, in-memory only, cleared on restart
- **CORS** — Configurable allowed origins for browser-based access

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
make clean            # Remove build directory
make version          # Print current version
```

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
