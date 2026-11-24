# Better Red broker: a static Go binary on a distroless base (no shell, runs as non-root).
#
#   podman build -t better-red-broker .                      (docker: add -f Containerfile)
#   podman run -d -p 8080:8080 -v better-red-data:/data better-red-broker
#
# Everything the server keeps is in the /data volume: broker.db (+ -wal, -shm) and server.key.
# Back up server.key; the database can be lost (the phones refill it). See docs/operations.md.

# ---- build: runs on the build machine's platform and cross-compiles (no emulation needed)
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.24-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/better-red-broker . \
 && mkdir -p /out/data

# ---- run
FROM gcr.io/distroless/static-debian12:nonroot
# Only for the image label; the binary takes its version from the VERSION file.
ARG VERSION=dev

LABEL org.opencontainers.image.title="better-red-broker" \
      org.opencontainers.image.description="Relay for Better Red partner sharing: stores only end-to-end encrypted blobs" \
      org.opencontainers.image.source="https://github.com/bRuttaZz/better-red-broker" \
      org.opencontainers.image.url="https://better-red.brutt.site" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}" \
      site.brutt.better-red.volume="/data" \
      site.brutt.better-red.volume.description="broker.db (disposable cache) and server.key (signing key: back it up, keep it secret)"

COPY --from=build /out/better-red-broker /usr/local/bin/better-red-broker
# Owned by the distroless "nonroot" user (65532), so a new named volume starts writable.
COPY --from=build --chown=65532:65532 /out/data /data

ENV BR_ADDR=:8080 \
    BR_DB=/data/broker.db \
    BR_KEY_FILE=/data/server.key \
    BR_TRUSTED_PROXIES=private
# private: believe X-Forwarded-For from a proxy on the host / another container. So publish the
# port only on 127.0.0.1 (or not at all), never publicly. See docs/operations.md.

# The only state: mount a named volume (or a host folder owned by uid 65532) here.
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/better-red-broker"]
