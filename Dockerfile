# syntax=docker/dockerfile:1

# Two images from one Dockerfile (both multi-arch: linux/amd64, linux/arm64):
#   docker buildx build --platform linux/amd64,linux/arm64 --target syncr -t podsync-syncr .
#   docker buildx build --platform linux/amd64,linux/arm64 --target webui -t podsync-webui .
# Building without --target gives the syncr image.

# ---- Go build (always runs on the build host's native arch and cross-compiles) ----
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/syncr ./cmd/syncr
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/webui ./cmd/webui

# ---- Fetch pinned yt-dlp and deno for the target arch ----
# yt-dlp needs a JavaScript runtime to solve YouTube's player challenges; deno is its default.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS tools
ARG TARGETARCH
ARG YTDLP_VERSION=2026.08.19
ARG DENO_VERSION=2.9.5
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip \
    && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) ytdlp=yt-dlp_linux;         deno=deno-x86_64-unknown-linux-gnu ;; \
      arm64) ytdlp=yt-dlp_linux_aarch64; deno=deno-aarch64-unknown-linux-gnu ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    mkdir -p /out/yt-dlp; \
    curl -fsSL -o /out/yt-dlp/yt-dlp "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/${ytdlp}"; \
    curl -fsSL -o /tmp/deno.zip "https://github.com/denoland/deno/releases/download/v${DENO_VERSION}/${deno}.zip"; \
    unzip -q /tmp/deno.zip -d /out; \
    chmod 0755 /out/yt-dlp/yt-dlp /out/deno

# ---- Web UI image: a static binary, nothing else ----
# For managing the library, not for publishing. It shares /config and /data with the syncr
# container and calls syncr's control API at SYNCR_URL.
FROM gcr.io/distroless/static-debian12:nonroot AS webui
COPY --from=build /out/webui /usr/local/bin/webui
ENV PODSYNC_CONFIG=/config/config.toml \
    WEBUI_LISTEN=:8090 \
    SYNCR_URL=http://syncr:8081
# Relative paths in the shared config (data_dir = "./data") must resolve like they do in the syncr image.
WORKDIR /
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/usr/local/bin/webui", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/webui"]

# ---- Sync service image (default target): syncr + yt-dlp + deno + ffmpeg ----
# Port 8080 publishes the feeds and media. The control API listens on 8081 (SYNCR_API_ADDR) for the
# web UI container only; do not publish it.
FROM debian:bookworm-slim AS syncr
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg \
    && rm -rf /var/lib/apt/lists/*
# yt-dlp lives in its own world-writable directory so `download.self_update` can replace it
# even when the container runs as an arbitrary --user.
COPY --from=tools /out/yt-dlp /opt/yt-dlp
COPY --from=tools /out/deno /usr/local/bin/deno
RUN chmod 0777 /opt/yt-dlp && mkdir -p /config /data && chmod 0777 /config /data
COPY --from=build /out/syncr /usr/local/bin/syncr

ENV PATH="/opt/yt-dlp:${PATH}" \
    PODSYNC_CONFIG=/config/config.toml \
    SYNCR_API_ADDR=:8081 \
    XDG_CACHE_HOME=/tmp/.cache \
    HOME=/tmp
# Relative paths in the default config (data_dir = "./data") resolve under "/".
WORKDIR /
EXPOSE 8080
VOLUME ["/config", "/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/syncr", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/syncr"]
