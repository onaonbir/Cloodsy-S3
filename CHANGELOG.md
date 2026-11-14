# Changelog

All notable changes to Cloodsy S3 are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

Nothing yet.

## [1.4.0] - 2026-11-14

### Added

Admin API extensions for the Cloodsy S3 GUI 2.0. Every new route requires an
admin session; `GET /admin/status` lists them in `features` so clients can
detect what a server supports.

- **Object transfer**: `GET`/`HEAD /admin/buckets/{name}/object?key=` streams
  an object (optional `versionId`, single `Range` requests with `206`/`416`,
  `inline=1` for previews; `Content-Security-Policy: sandbox` and an RFC 6266
  `Content-Disposition`). `PUT` on the same route uploads the raw request body
  through the same object service as S3 `PutObject` (quota → `413`,
  versioning, image optimization, webhooks, 5 GB limit; an empty body on a
  key ending in `/` creates a folder marker).
- **Multipart upload** for files over 5 GB and resumable uploads: create,
  upload part, list parts, complete and abort, plus listing every incomplete
  upload of a bucket (including those started by S3 clients) and aborting
  them in bulk (`older_than_days`).
- **Object details**: `GET /admin/buckets/{name}/object/info` (content type,
  user metadata, the data file of every version on disk, the optimized image
  variant) and `GET /admin/buckets/{name}/versions` (all versions and delete
  markers of a key).
- **Copy, move and rename**: `POST /admin/buckets/{name}/objects/copy` and
  `.../objects/copy-prefix` (folders) copy server-side through the S3
  `CopyObject` path, across buckets if needed; `move` then deletes the source.
- **Metadata editing**: `PUT /admin/buckets/{name}/object/meta` replaces the
  content type and user metadata (S3 `CopyObject` onto itself with
  `MetadataDirective=REPLACE`).
- **Search and folder size**: `GET /admin/buckets/{name}/search` (recursive,
  case-insensitive, paginated in SQL) and `GET /admin/buckets/{name}/prefix-stats`.
- **Lifecycle rules**: the Admin API now exposes `status`, `noncurrent_days`,
  `abort_multipart_days` and `expire_delete_markers` (validated like
  `PutBucketLifecycle`) and edits or deletes single rules by id.
- **Webhooks**: every delivery attempt is recorded (newest 200 per webhook,
  written off the delivery path); new endpoints list deliveries, send a signed
  `s3:TestEvent`, and update a webhook. Listings report `has_secret`; the
  secret itself is never returned.
- **Maintenance scan**: `POST /admin/maintenance/scan` reports orphan bucket
  directories, objects whose data file is missing and data files no object
  points to (read-only, bounded). `POST /admin/maintenance/orphan-dirs/delete`
  removes only directories a fresh server-side scan reports as orphans.
- **Sessions, logs, config, metrics**: list and revoke admin sessions (with
  IP, user agent and last activity); the last 2000 log records from an
  in-memory ring; the effective configuration with secrets masked; S3 request,
  error and byte counters with a per-minute series for the last hour.
- **Status**: `GET /admin/status` also reports `uptime_seconds`, `tls`,
  `storage` (data directory and free/total disk space), `s3` (listen address,
  TLS, region, virtual-host domains) and `host` (OS, hostname, WSL distro).
- Admin CORS responses expose the download headers (`Content-Disposition`,
  `Content-Length`, `Content-Range`, `Accept-Ranges`, `ETag`, `Last-Modified`,
  `X-Version-Id`).

### Changed

- S3 `CopyObject`, multipart upload/completion/abort and user-metadata
  validation moved into the shared object service, so the S3 API and the
  Admin API run the same code.
- Database migration 3 adds the `webhook_deliveries` table. It only adds a
  table, so downgrading to 1.3.x keeps working.
- `golang.org/x/sys` is now a direct dependency (Windows disk-space query).
- Built with Go 1.27.4 (`go` directive raised from 1.27.2).
- `modernc.org/sqlite` 1.62.0 (SQLite 3.54.1), `golang.org/x/net` 0.62.0,
  `pterm` 0.12.84 and `softprops/action-gh-release` 3.0.4.

### Security

- Object info, the maintenance scan and `GET /admin/config` reveal absolute
  server paths to admins. Keep the Admin API behind TLS or on a trusted
  network (see `admin.tls`).
- Orphan detection is content-based: a directory is reported (and can be
  deleted) only if it holds nothing but directories and the files the server
  writes there, with no symlinks or special files, and it is re-checked right
  before deletion. Storage roots, live bucket trees and the directories of the
  database, ACME cache, config file and TLS certificates/keys are never
  candidates (compared case-insensitively on Windows and macOS). A shared
  `storage_dir` no longer exposes other applications' directories.
- Admin moves hold the source and destination key locks across copy and
  delete, refuse a source that changed since it was looked up (409), and
  delete only the versions that existed when the copy started; a version
  written by an S3 client meanwhile is kept.
- Copies and moves within a bucket between keys or prefixes that differ only
  in letter case are refused (409): on case-insensitive filesystems they are
  the same file.
- The in-memory log ring caps messages at 4 KiB, like attribute values.
- Presigned URL masking in the request log decodes parameter names, so
  percent-encoded or differently cased `X-Amz-Signature`, `X-Amz-Credential`
  and `X-Amz-Security-Token` values are masked too; an unparseable query is
  masked whole.
- Maintenance scans and orphan deletes run one at a time (409 when busy),
  prefix copies take the bucket's busy lock, and both stop when the client
  disconnects.
- A multipart part upload can no longer race an abort or completion of the
  same upload: parts share a per-upload lock (parallel parts still run in
  parallel), abort/complete take it exclusively, and a part for an upload that
  is gone fails with `NoSuchUpload`. Lifecycle and stale-upload cleanup use the
  same lock.
- Go 1.27.4 includes the standard library fixes for GO-2026-6859 (`net/http`
  `MaxBytesReader` race that could crash the server) and GO-2026-6809
  (`crypto/x509`).

## [1.3.0] - 2026-10-11

### Added

- **Automatic TLS certificates via ACME (Let's Encrypt)**: `server.tls.acme`
  obtains and renews certificates with the HTTP-01 (port 80 listener that
  also redirects to HTTPS) or TLS-ALPN-01 challenge, caches them under
  `<database dir>/acme/`, and shares them with the Admin API and WebDAV
  listeners. Configurable with `CLOODSYS3_ACME_*` environment variables; a
  staging `directory_url` is supported for testing. (#15)

### Changed

- Go 1.27.2 toolchain; Docker build image `golang:1.27-alpine`.
- `modernc.org/sqlite` 1.60.1 (SQLite 3.53.4), `golang.org/x/crypto` 0.58.0,
  `x/net` 0.61.0, `x/image` 0.47.0, `x/term` 0.47.0. No known
  vulnerabilities reported by `govulncheck`.

## [1.2.0] - 2026-09-15

### Security

- **UploadPartCopy** could read objects from a bucket the credential had no
  access to; the copy source is now authorised like any other read.
- **Admin delete-prefix** accepted `..` segments and could escape the bucket
  directory; paths are now validated before anything is removed.
- **aws-chunked uploads**: a malformed chunk stream could spin a CPU core
  forever, and a truncated stream was stored silently as a complete object.
  Chunk headers are bounded and every chunk signature is verified.
- **Payload verification**: `x-amz-content-sha256`, per-chunk signatures,
  `Content-MD5` and `x-amz-checksum-*` headers are now verified; mismatches
  are rejected with the proper S3 error. `server.require_payload_signature`
  can additionally refuse `UNSIGNED-PAYLOAD` header-auth requests.
- **Admin login rate limiter** could be bypassed with spoofed
  `X-Forwarded-For` values and inconsistently-normalised IPv6 addresses. The
  header is only honoured from `admin.trusted_proxies`.
- **Admin API and WebDAV** can now run behind their own TLS listeners
  (`admin.tls`, `webdav.tls`); a warning is logged when either is reachable
  without TLS on a non-loopback address.
- **Self-updater**: release archives are verified against the published
  `checksums.txt` (SHA-256) before installation, only the archive entry named
  `cloodsys3` is extracted with a bounded decompression size, and the Windows
  updater no longer writes the raw zip over the running `.exe` (which bricked
  the installation). Dev builds never self-update.
- **Unknown sub-resources** were routed to the bucket handlers, so a request
  such as `DELETE /bucket?cors` deleted the bucket. Unknown sub-resources now
  return `NotImplemented`.
- **Admin passwords** can be entered interactively (no echo) or generated
  (`--generate`); passing `--password=` prints a warning because argv is
  visible in `ps` and shell history. Passwords are length-validated (8–72
  bytes).

### Fixed

- Delimiter listings dropped keys when a page boundary fell inside a common
  prefix; pagination is now exact.
- Prefix matching was case-insensitive on the SQLite side, which let lifecycle
  rules expire objects under a differently-cased prefix (data loss) and
  returned wrong listings.
- Lifecycle `<Filter>` and `<Status>` were ignored; rules are now honoured
  as written, including `NoncurrentVersionExpiration`,
  `AbortIncompleteMultipartUpload` and `ExpiredObjectDeleteMarker`.
- A fixed 60-second server timeout aborted large uploads and downloads. Only
  idle connections are now bounded (`server.idle_timeout`).
- Graceful shutdown: `SIGTERM`/`SIGINT` drain in-flight requests and the
  process exits 0 instead of 1.
- Versioning: `DeleteObjects`, the admin API, WebDAV and deletes on suspended
  buckets destroyed every version instead of adding a delete marker; removing
  a delete marker did not restore the previous version. WebDAV now behaves
  correctly on versioned buckets.
- On-disk key collisions: keys such as `dir/` and `dir` (or names with
  reserved characters) mapped to the same path. Such keys are now stored
  percent-encoded; a one-time automatic migration runs on first `serve`.
- A failed `PUT` over an existing key destroyed the existing object; writes
  are staged and renamed into place only on success.
- `CopyObject` did not URL-decode `x-amz-copy-source`.
- `encoding-type=url` is supported in listings.
- `response-*` query overrides (`response-content-disposition`, …) are
  honoured on `GetObject`; the previously forced `Content-Disposition:
  attachment` and `Cache-Control: no-store` headers were removed.
- System headers sent by clients were persisted as user metadata.
- Multipart uploads: the final ETag is now the S3-style `md5-of-md5s-N`, and
  parts smaller than 5 MiB (except the last) are rejected with
  `EntityTooSmall`.
- Object writes are `fsync`ed before the metadata row is committed.
- Configuration is validated at start-up (listen addresses, TLS files,
  durations, CIDRs) with a clear error instead of failing later.
- `bucket storage` moves the sibling `.<bucket>-multipart` and
  `.<bucket>-cache` trees along with the data directory and refuses
  cross-filesystem moves with manual instructions; `bucket delete` refuses
  buckets that still contain versions or delete markers unless `--force` is
  given and removes all three trees.

### Added

- Environment overrides for the most common settings (`CLOODSYS3_LISTEN`,
  `CLOODSYS3_DB_PATH`, `CLOODSYS3_DATA_DIR`, `CLOODSYS3_ADMIN_ENABLED`, …), so
  containers and systemd units need no config file.
- `server.cors_origins`, `server.virtual_host_domains`,
  `server.max_connections`, `server.idle_timeout`,
  `server.require_payload_signature`, `admin.session_ttl`,
  `admin.trusted_proxies`, `webdav.lock_timeout`, `image.max_source_bytes`,
  `image.max_concurrent`, `image.cache_max_bytes`, `image.dimension_step` and
  `update.check_on_start` configuration keys.
- Dockerfile (multi-stage, static binary, distroless, non-root) and
  `docker-compose.yml`.
- Unit tests across packages and a CI workflow (build, vet, test,
  cross-compile, govulncheck); release builds are reproducible (`-trimpath`)
  and archives now ship `LICENSE` and `THIRD_PARTY_LICENSES.md`.
- `install.sh` verifies checksums, supports `--version` pinning and is safe
  to re-run; `release.sh` refuses dirty trees and existing tags.
- Hardened `cloodsys3.service` (network-online ordering, sandboxing,
  `EnvironmentFile`), with instructions for custom storage directories.
- `SECURITY.md`, `CONTRIBUTING.md`, `THIRD_PARTY_LICENSES.md` and Dependabot
  configuration.

### Changed

- Admin sessions default to 8 hours (`admin.session_ttl`).
- `CreateBucket` through the S3 API is denied; buckets are created with the
  CLI or the admin API.
- Release archives contain the binary as `cloodsys3` / `cloodsys3.exe`
  instead of a platform-suffixed name.

## [1.1.2] - 2026-07-13

- Prevent large upload timeouts by disabling server timeouts.
- Harden SigV4 validation and database migrations.

## [1.1.1] and earlier

See the [GitHub releases](https://github.com/onaonbir/Cloodsy-S3/releases).

[Unreleased]: https://github.com/onaonbir/Cloodsy-S3/compare/v1.4.0...HEAD
[1.4.0]: https://github.com/onaonbir/Cloodsy-S3/compare/v1.3.0...v1.4.0
[1.3.0]: https://github.com/onaonbir/Cloodsy-S3/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/onaonbir/Cloodsy-S3/compare/v1.1.2...v1.2.0
[1.1.2]: https://github.com/onaonbir/Cloodsy-S3/releases/tag/v1.1.2
