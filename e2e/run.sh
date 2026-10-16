#!/bin/bash
# Browser end-to-end test. Builds the app, starts it against a fake yt-dlp on 127.0.0.1:8099,
# and drives the UI with headless Chromium (set CHROMIUM=/path/to/chromium if not /usr/bin/chromium).
# Requires: node, npm install (once, in this directory), a Chromium/Chrome binary.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(dirname "$here")"
work="$(mktemp -d)"
trap 'kill "$pid" 2>/dev/null || true; rm -rf "$work"' EXIT
cat > "$work/config.toml" <<CFG
[server]
listen_addr = "127.0.0.1:8099"
base_url = "http://127.0.0.1:8099"
data_dir = "$work/data"

[download]
yt_dlp_path = "$here/fake-yt-dlp"
concurrency = 2
update_interval = "1h"

[defaults]
format = "video"
max_resolution = 720
exclude_shorts = true
page_size = 4
cleanup_keep_last = 0
CFG
(cd "$root" && go build -o "$work/podsync" ./cmd/podsync)
PODSYNC_CONFIG="$work/config.toml" "$work/podsync" >"$work/server.log" 2>&1 &
pid=$!
sleep 1
cd "$here"
SHOTS="${SHOTS:-$work}" node run.mjs
