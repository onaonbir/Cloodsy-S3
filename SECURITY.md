# Security Policy

## Supported versions

Only the latest release line receives security fixes. Older releases are not
patched; upgrade with `cloodsys3 update` (or the installer) to stay supported.

| Version | Supported |
|---------|-----------|
| Latest `1.x` release | ✅ |
| Older `1.x` releases | ❌ (upgrade) |
| `main` branch | best effort — not a release |

## Reporting a vulnerability

**Please do not open a public GitHub issue for security problems.**

Report privately through one of:

- GitHub private vulnerability reporting: <https://github.com/onaonbir/Cloodsy-S3/security/advisories/new>
- E-mail: <trademark@onaonbir.com> (the contact address listed in [LICENSE](LICENSE)); put "Cloodsy S3 security" in the subject

Include the affected version (`cloodsys3 version`), the deployment mode
(binary / systemd / Docker), reproduction steps and, if possible, a
proof-of-concept request. Encrypting the report is welcome but not required.

What to expect:

- acknowledgement within **3 business days**;
- a fix or mitigation plan within **30 days** for confirmed issues, faster for
  anything remotely exploitable without credentials;
- credit in the release notes and `CHANGELOG.md` unless you prefer to stay
  anonymous.

Please give us a reasonable window to ship a fix before publishing details.

## Scope

In scope: the `cloodsys3` binary (S3 API, admin API, WebDAV, self-updater,
CLI), the published release artifacts, `install.sh`, the Dockerfile and the
systemd unit shipped in this repository.

Out of scope: vulnerabilities in third-party clients, misconfigured reverse
proxies, and deployments that expose the admin API or WebDAV without TLS
against the documented recommendations.

## Hardening checklist for operators

- Bind the admin API (`admin.listen`) and WebDAV (`webdav.listen`) to
  `127.0.0.1` or enable `admin.tls` / `webdav.tls`; both carry credentials.
- Run the service as a dedicated user with the shipped `cloodsys3.service`
  (systemd sandboxing) or the distroless Docker image (non-root).
- Keep `update.check_on_start` enabled (or subscribe to release notifications)
  and update promptly; the updater verifies SHA-256 checksums before
  installing.
- Create admin passwords interactively or with `--generate` instead of
  `--password=` so they never land in shell history.
- Set `server.require_payload_signature: true` when all your clients sign
  payloads, and list your reverse proxies in `admin.trusted_proxies` so login
  rate limiting cannot be bypassed via `X-Forwarded-For`.
