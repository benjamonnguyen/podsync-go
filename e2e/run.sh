#!/bin/bash
# Browser end-to-end test. Builds podsync, starts it against a fake yt-dlp (one listener on
# 127.0.0.1:8099 serving feeds, media and the Basic-auth-protected web UI) and drives the UI with
# headless Chromium (set CHROMIUM=/path/to/chromium if not /usr/bin/chromium).
# Requires: node, npm install (once, in this directory), a Chromium/Chrome binary.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(dirname "$here")"
work="$(mktemp -d)"
trap 'kill "$podsync_pid" 2>/dev/null || true; rm -rf "$work"' EXIT
cat > "$work/config.toml" <<CFG
[download]
concurrency = 2
update_interval = "1h"

[defaults]
format = "video"
max_resolution = 720
min_duration = "61s"
page_size = 4
cleanup_keep_last = 0
CFG
(cd "$root" && go build -o "$work/podsync" ./cmd/podsync)
# yt-dlp is always run by that name (on PATH), never a configurable path: put the fake
# script on PATH under that name for podsync's process only.
ln -s "$here/fake-yt-dlp" "$work/yt-dlp"
export E2E_USER="e2e"
export E2E_PASSWORD="e2e-password"
PATH="$work:$PATH" \
	PODSYNC_CONFIG="$work/config.toml" \
	PODSYNC_LISTEN_ADDR=127.0.0.1:8099 \
	PODSYNC_BASE_URL=http://127.0.0.1:8099 \
	PODSYNC_DATA_DIR="$work/data" \
	PODSYNC_USERNAME="$E2E_USER" PODSYNC_PASSWORD="$E2E_PASSWORD" \
	"$work/podsync" >"$work/podsync.log" 2>&1 &
podsync_pid=$!
sleep 1
cd "$here"
SHOTS="${SHOTS:-$work}" node run.mjs
