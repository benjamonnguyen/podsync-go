#!/bin/bash
# Builds and runs syncr (feeds and media on :8080, control API on 127.0.0.1:8081) and the web UI (:8090),
# both on dev/config.toml and dev/data.
# Media goes to dev/data (git-ignored). Ctrl-C stops both.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p bin
go build -o bin/syncr ./cmd/syncr
go build -o bin/webui ./cmd/webui
bin/syncr -config dev/config.toml &
syncr=$!
bin/webui -config dev/config.toml -listen :8090 -syncr http://127.0.0.1:8081 &
webui=$!
trap 'kill $syncr $webui 2>/dev/null || true' EXIT INT TERM
echo "web UI: http://localhost:8090   feeds: http://localhost:8080/feeds/<id>.xml   control API: http://127.0.0.1:8081"
wait -n
