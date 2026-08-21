# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/controlplane ./cmd/controlplane

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/controlplane /usr/local/bin/controlplane

# Default location for the admin web UI's local store file (password hash
# + session secret) — see LB_ADMIN_STORE_PATH. Mount a volume here to
# persist the admin password across container restarts.
RUN mkdir -p /var/lib/go-loadbalancer

EXPOSE 9090 9091
ENTRYPOINT ["/usr/local/bin/controlplane"]
