# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
# Static build: no cgo, stripped symbol table (-s -w) to keep the image small.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/dataplane ./cmd/dataplane

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget

# Run as an unprivileged user rather than root. The proxy needs no
# elevated capabilities: it binds only high ports (>=1024 by default) and
# reads no privileged paths.
RUN addgroup -S app && adduser -S -G app app

COPY --from=build /out/dataplane /usr/local/bin/dataplane

USER app

# Traffic (:8080), metrics (:9100), and ops/probe (:9101) listeners.
EXPOSE 8080 9100 9101

# Liveness via the dedicated ops listener's /healthz (see LB_OPS_ADDR,
# default :9101), which works in both http and tcp proxy modes. Uses the
# default wget with a short timeout; the ops server is a lightweight HTTP
# server independent of the traffic path.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --quiet --tries=1 --spider http://127.0.0.1:9101/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/dataplane"]
