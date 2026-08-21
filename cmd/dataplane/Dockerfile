# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/dataplane ./cmd/dataplane

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/dataplane /usr/local/bin/dataplane

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dataplane"]
