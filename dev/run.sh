#!/bin/bash
# Builds and runs podsync (feeds, media and the web UI, all on :8080), on dev/config.toml and
# dev/data. The public address, the data directory and the web UI's Basic auth credentials are
# environment variables here, not in the config file (it holds only feeds and defaults; see
# config.go). The listen address and config path could be -listen/-config flags instead, but env
# vars keep this consistent with everything else. Media goes to dev/data (git-ignored). Ctrl-C
# stops it. The YouTube API key comes from dev/.env (git-ignored), e.g.
# PODSYNC_YOUTUBE_API_KEY=AIza...
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -f dev/.env ]; then set -a; . dev/.env; set +a; fi
mkdir -p bin
go build -o bin/podsync ./cmd/podsync
PODSYNC_CONFIG=dev/config.toml \
  PODSYNC_LISTEN_ADDR=:8080 \
  PODSYNC_BASE_URL=http://100.120.50.37:8080 \
  PODSYNC_DATA_DIR=./dev/data \
  PODSYNC_USERNAME=dev PODSYNC_PASSWORD=dev \
  bin/podsync &
podsync=$!
trap 'kill $podsync 2>/dev/null || true' EXIT INT TERM
echo "web UI: http://localhost:8080 (user: dev, password: dev)"
wait -n
