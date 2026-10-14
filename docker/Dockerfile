# syntax=docker/dockerfile:1

# Keep GO_VERSION equal to the go directive in go.mod.
ARG GO_VERSION=1.27.2

FROM golang:${GO_VERSION}-trixie AS build
# The release version (the tag without the leading v); `backend version`
# and the OpenAPI document report it.
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/backend ./cmd/backend

FROM debian:trixie-20261005-slim
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown
# source links the GHCR package to the repository, so access to the private
# image follows the repository's.
LABEL org.opencontainers.image.title="radio-atlantida-backend" \
      org.opencontainers.image.source="https://github.com/AstRonin/radio-atlantida-backend" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}"
# ffmpeg: import, analysis, transcoding. tzdata: station timezone validation.
# curl: container health check.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin backend
COPY --from=build /out/backend /usr/local/bin/backend
WORKDIR /app
USER backend
EXPOSE 8080
# Assumes HTTP_ADDR listens on port 8080 (0.0.0.0:8080 in compose).
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD ["curl", "-fsS", "-o", "/dev/null", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["backend"]
CMD ["serve"]
