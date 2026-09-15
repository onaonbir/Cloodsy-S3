# Changelog

All notable changes to Cloodsy S3 are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

Nothing yet.

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

[Unreleased]: https://github.com/onaonbir/Cloodsy-S3/compare/v1.2.0...HEAD
[1.2.0]: https://github.com/onaonbir/Cloodsy-S3/compare/v1.1.2...v1.2.0
[1.1.2]: https://github.com/onaonbir/Cloodsy-S3/releases/tag/v1.1.2
