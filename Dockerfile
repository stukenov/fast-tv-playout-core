# syntax=docker/dockerfile:1
# One Dockerfile for all services: docker build --build-arg SERVICE=packager .
FROM golang:1.26-alpine AS build
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY pkg ./pkg
COPY services ./services
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    test -n "$SERVICE" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./services/${SERVICE} \
    && mkdir -p /out/hls

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
# a named volume mounted here inherits the owner, so the packager can write playlists as non-root
COPY --from=build --chown=65532:65532 /out/hls /var/www/hls
USER nonroot:nonroot
ENTRYPOINT ["/app"]
