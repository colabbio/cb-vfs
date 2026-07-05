# ─────────────────────────────────────────────────────────────────────────────
# MedVFS Dockerfile — Multi-stage build
# Build:  docker build -t colabbio/medvfs:latest .
# Run:    docker run --rm colabbio/medvfs:latest version
# Serve:  docker run --privileged -v ./vfs.yaml:/config/vfs.yaml colabbio/medvfs serve
# ─────────────────────────────────────────────────────────────────────────────

# ── Stage 1: Build ──────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache dependency downloads
COPY go.mod go.sum ./
RUN go mod download

# Build static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -ldflags="-s -w -X main.version=$(cat cmd/medvfs/main.go | grep 'medvfs v' | head -1 | sed 's/.*medvfs \(v[0-9.]*\).*/\1/' || echo 'dev')" \
      -o /medvfs \
      ./cmd/medvfs

# ── Stage 2: Runtime ────────────────────────────────────────────────────────
FROM alpine:3.20

# FUSE support for serve --fuse mode
RUN apk add --no-cache \
    fuse \
    ca-certificates \
    tini \
    && echo "user_allow_other" >> /etc/fuse.conf

# Non-root user (but FUSE may require --privileged at runtime)
RUN addgroup -S medvfs && adduser -S medvfs -G medvfs

COPY --from=builder /medvfs /usr/local/bin/medvfs

# Default config mount point
RUN mkdir -p /config /data/vfs /data/output /var/cache/medvfs \
    && chown -R medvfs:medvfs /data /var/cache/medvfs

EXPOSE 8090

# Use tini as PID 1 for proper signal handling
ENTRYPOINT ["tini", "--", "medvfs"]

# Default: serve mode with config from /config/vfs.yaml
CMD ["serve", "--config", "/config/vfs.yaml", "--port", "8090"]

# ── Labels ──────────────────────────────────────────────────────────────────
LABEL org.opencontainers.image.title="MedVFS" \
      org.opencontainers.image.description="Medical Virtual File System — Clinical Data Plane for ML Pipelines" \
      org.opencontainers.image.vendor="ColabBio" \
      org.opencontainers.image.url="https://github.com/colabbio/medvfs" \
      org.opencontainers.image.source="https://github.com/colabbio/medvfs"
