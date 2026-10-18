# Test tooling: media generator and the end-to-end checker. Build context is the repository root.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY pkg ./pkg
COPY tools/e2e ./tools/e2e
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/e2e ./tools/e2e

FROM alpine:3.21
RUN apk add --no-cache ffmpeg font-dejavu bash curl
COPY --from=build /out/e2e /usr/local/bin/e2e
COPY tools/e2e/make-assets.sh /usr/local/bin/make-assets.sh
COPY tools/normalize.sh /usr/local/bin/normalize.sh
