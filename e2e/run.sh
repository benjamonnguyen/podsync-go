#!/bin/bash
# Browser end-to-end test. Builds syncr and webui, starts them against a fake yt-dlp
# (syncr: feeds on 127.0.0.1:8098, API on 8097; web UI on 127.0.0.1:8099; sharing one config file and data dir) and drives the UI with headless
# Chromium (set CHROMIUM=/path/to/chromium if not /usr/bin/chromium).
# Requires: node, npm install (once, in this directory), a Chromium/Chrome binary.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(dirname "$here")"
work="$(mktemp -d)"
trap 'kill "$syncr_pid" "$webui_pid" 2>/dev/null || true; rm -rf "$work"' EXIT
cat > "$work/config.toml" <<CFG
[server]
listen_addr = "127.0.0.1:8098"
api_addr = "127.0.0.1:8097"
base_url = "http://127.0.0.1:8098"
data_dir = "$work/data"

[download]
yt_dlp_path = "$here/fake-yt-dlp"
concurrency = 2
update_interval = "1h"

[defaults]
format = "video"
max_resolution = 720
min_duration = "1m"
page_size = 4
cleanup_keep_last = 0
CFG
(cd "$root" && go build -o "$work/syncr" ./cmd/syncr && go build -o "$work/webui" ./cmd/webui)
PODSYNC_CONFIG="$work/config.toml" "$work/syncr" >"$work/syncr.log" 2>&1 &
syncr_pid=$!
"$work/webui" -config "$work/config.toml" -listen 127.0.0.1:8099 -syncr http://127.0.0.1:8097 >"$work/webui.log" 2>&1 &
webui_pid=$!
sleep 1
cd "$here"
SHOTS="${SHOTS:-$work}" node run.mjs
