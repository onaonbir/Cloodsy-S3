# Contributing to Cloodsy S3

Thanks for helping out. Bug reports, S3-compatibility findings and pull
requests are all welcome.

## Before you start

- Check the [open issues](https://github.com/onaonbir/Cloodsy-S3/issues) to
  avoid duplicate work; open one for larger changes so the design can be
  discussed first.
- Security problems go through [SECURITY.md](SECURITY.md), never a public
  issue.
- By contributing you agree that your contribution is licensed under the
  [Cloodsy S3 Community License 1.0](LICENSE).

## Development setup

Requirements: Go (the version in `go.mod`; the toolchain is downloaded
automatically) and `make`. No cgo, no C toolchain.

```bash
git clone https://github.com/onaonbir/Cloodsy-S3.git
cd Cloodsy-S3
make build      # -> build/cloodsys3
make run        # builds and starts `serve` with data under ./.cloodsys3 (gitignored)
```

Handy targets:

| Target | What it does |
|--------|--------------|
| `make test` | `go test ./...` |
| `make vet` | `go vet ./...` |
| `make lint` | `staticcheck ./...` when staticcheck is installed |
| `make vuln` | `govulncheck ./...` |
| `make check` | vet + lint + test + vuln (what CI runs) |
| `make build-all` | cross-compile the Linux targets |

Run the AWS CLI against a dev server to exercise the S3 API end to end:

```bash
./build/cloodsys3 bucket create dev
./build/cloodsys3 credential create dev
aws --endpoint-url http://localhost:9000 s3 ls s3://dev/
```

## Making changes

- Keep packages focused: `handler/` (S3 HTTP layer), `service/` (object
  operations shared by S3, admin and WebDAV), `storage/` (on-disk layout),
  `db/` (SQLite metadata), `admin/`, `webdav/`, `cli/`, `config/`.
- Run `gofmt` (CI rejects unformatted code) and keep `go.mod`/`go.sum` tidy.
- Add or update tests next to the code you touch. S3 behaviour changes should
  cite the AWS API reference in the PR description and, where practical, come
  with a test that reproduces the client behaviour.
- Never widen what an unauthenticated request can do without discussing it in
  an issue first.
- Update `README.md`, `config.yaml.example` and `CHANGELOG.md` ("Unreleased")
  when behaviour or configuration changes.

## Pull requests

1. Branch from `main`.
2. Make sure `make check` passes locally.
3. Write a clear PR description: what changed, why, and how it was tested.
   Reference the issue it closes.
4. Keep PRs reasonably small; unrelated refactors belong in their own PR.

CI (`.github/workflows/ci.yml`) runs gofmt, `go mod tidy` drift, build, vet,
tests, cross-compilation of every release target and govulncheck on every PR.

## Releasing (maintainers)

```bash
./release.sh 1.2.3 -m "short summary"
```

The script refuses a dirty tree or an existing tag, runs vet + tests, commits
only the `VERSION` file, tags `v1.2.3` and pushes. GitHub Actions builds the
static binaries, packages them with `LICENSE` and `THIRD_PARTY_LICENSES.md`,
publishes `checksums.txt`, and creates the release. Move the "Unreleased"
section of `CHANGELOG.md` under the new version before running it.
