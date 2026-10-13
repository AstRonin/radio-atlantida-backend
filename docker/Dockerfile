# syntax=docker/dockerfile:1

# Keep GO_VERSION equal to the go directive in go.mod.
ARG GO_VERSION=1.27.2

FROM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backend ./cmd/backend

FROM debian:trixie-20261005-slim
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
