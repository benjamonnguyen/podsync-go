# syntax=docker/dockerfile:1

# One multi-arch (linux/amd64, linux/arm64) image with podsync, yt-dlp, deno and ffmpeg:
#   docker buildx build --platform linux/amd64,linux/arm64 -t podsync .

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
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/podsync ./cmd/podsync

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

# ---- Runtime image: podsync (sync + publish + web UI) + yt-dlp + deno + ffmpeg ----
# One port (8080) serves feeds, media, and (unless PODSYNC_WEBUI_DISABLED=true) the management
# web UI, which requires the HTTP Basic credentials set by PODSYNC_USERNAME/PODSYNC_PASSWORD.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg \
    && rm -rf /var/lib/apt/lists/*
# yt-dlp lives in its own world-writable directory so `download.self_update` can replace it
# even when the container runs as an arbitrary --user.
COPY --from=tools /out/yt-dlp /opt/yt-dlp
COPY --from=tools /out/deno /usr/local/bin/deno
RUN chmod 0777 /opt/yt-dlp && mkdir -p /config /data && chmod 0777 /config /data
COPY --from=build /out/podsync /usr/local/bin/podsync

ENV PATH="/opt/yt-dlp:${PATH}" \
    PODSYNC_CONFIG=/config/config.toml \
    PODSYNC_DATA_DIR=/data \
    XDG_CACHE_HOME=/tmp/.cache \
    HOME=/tmp
# PODSYNC_BASE_URL has no default (it's your address, e.g. http://192.168.1.10:8080) and must be
# set in compose.yaml, or podsync refuses to start. PODSYNC_USERNAME and PODSYNC_PASSWORD are
# required too, unless you set PODSYNC_WEBUI_DISABLED=true. Everything is an environment
# variable (podsync accepts -listen/-config flags too, but the image doesn't need them), and
# podsync fails fast naming whatever's missing.
WORKDIR /
EXPOSE 8080
VOLUME ["/config", "/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["env", "PODSYNC_HEALTHCHECK=1", "/usr/local/bin/podsync"]
ENTRYPOINT ["/usr/local/bin/podsync"]
