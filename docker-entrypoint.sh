#!/bin/sh
# podsync itself never needs root; this just repairs ownership before dropping to it. A bind
# mount at /data or /config that didn't already exist on the host gets auto-created by the
# Docker daemon as root:root, which a non-root podsync then can't write into. Fix that here
# (cheap: only the two top-level directories, not a recursive chown of everything already
# downloaded) and hand off to PUID:PGID (default 1000:1000, override to match your host user).
#
# If the container was started with an explicit --user (the old way), there's nothing to
# chown (we wouldn't have permission anyway) and nothing to drop, so just exec directly.
set -eu

if [ "$(id -u)" = "0" ]; then
	PUID="${PUID:-1000}"
	PGID="${PGID:-1000}"
	mkdir -p /data /config
	chown "$PUID:$PGID" /data /config
	exec chroot --userspec="$PUID:$PGID" / "$@"
fi

exec "$@"
