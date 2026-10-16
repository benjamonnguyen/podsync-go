#!/bin/bash
# Builds and runs syncr (API on :8081) and the web UI (:8080) against dev/config.toml.
# Media goes to dev/data (git-ignored). Ctrl-C stops both.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p bin
go build -o bin/syncr ./cmd/syncr
go build -o bin/webui ./cmd/webui
bin/syncr -config dev/config.toml &
syncr=$!
bin/webui -listen :8080 -syncr http://127.0.0.1:8081 &
webui=$!
trap 'kill $syncr $webui 2>/dev/null || true' EXIT INT TERM
echo "web UI: http://localhost:8080   API: http://localhost:8081/api/v1/feeds"
wait -n
