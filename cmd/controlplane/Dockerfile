# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
# Static build: no cgo, stripped symbol table (-s -w) to keep the image small.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/controlplane ./cmd/controlplane

FROM alpine:3.20
RUN apk add --no-cache ca-certificates

# Run as an unprivileged user rather than root. The control plane's admin
# store lives under /var/lib/go-loadbalancer, so that directory is created
# and owned by the app user below.
RUN addgroup -S app && adduser -S -G app app

COPY --from=build /out/controlplane /usr/local/bin/controlplane

# Default location for the admin web UI's local store file (password hash
# + session secret) — see LB_ADMIN_STORE_PATH. Mount a volume here to
# persist the admin password across container restarts. Owned by the app
# user so the unprivileged process can write to it.
RUN mkdir -p /var/lib/go-loadbalancer && chown -R app:app /var/lib/go-loadbalancer

USER app

# gRPC control API (:9090) and admin web UI (:9091).
EXPOSE 9090 9091

# The control plane exposes no unauthenticated HTTP probe surface (the
# admin UI is login-gated), so liveness is a TCP connect check against the
# gRPC listener — the process is alive if it is accepting connections.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD nc -z 127.0.0.1 9090 || exit 1

ENTRYPOINT ["/usr/local/bin/controlplane"]
