# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
FROM golang:1.25-alpine AS build

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Pure-Go stack: a static binary with no libc dependency.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION} -X main.CommitHash=${COMMIT} -X main.BuildDate=${BUILD_DATE}" \
      -o /out/cloodsys3 . \
 && mkdir -p /out/data /out/licenses \
 && cp LICENSE THIRD_PARTY_LICENSES.md /out/licenses/

# ---- runtime stage ---------------------------------------------------------
# distroless/static: no shell, no package manager, runs as uid 65532 (nonroot).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build --chown=65532:65532 /out/cloodsys3 /cloodsys3
COPY --from=build --chown=65532:65532 /out/licenses /licenses
# Pre-created so a fresh named volume inherits the nonroot ownership.
COPY --from=build --chown=65532:65532 /out/data /data

# All runtime state lives under /data (mount a volume there).
ENV CLOODSYS3_DB_PATH=/data/cloodsys3.db \
    CLOODSYS3_DATA_DIR=/data/data \
    CLOODSYS3_LISTEN=:9000 \
    CLOODSYS3_ADMIN_LISTEN=:9001 \
    CLOODSYS3_WEBDAV_LISTEN=:9002 \
    CLOODSYS3_UPDATE_CHECK=false

VOLUME ["/data"]
# 9000 S3 API, 9001 admin API (opt-in), 9002 WebDAV (opt-in)
EXPOSE 9000 9001 9002

USER nonroot:nonroot
WORKDIR /data

ENTRYPOINT ["/cloodsys3"]
CMD ["serve"]
