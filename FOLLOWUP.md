# FOLLOWUP — decisions to review

> Sections below were written as the work progressed. Package paths like `internal/puller` or `internal/web` in older sections predate the restructure described under **Restructure into syncr + webui**; the behavior they describe still applies, now living in `syncer/`, `webui/`, `fs/`, `toml/` etc.

Running log of judgment calls made during unsupervised implementation.
**⚠ = I was unsure; worth discussing.** Everything else I'm fairly comfortable with but is listed so you can veto it.

## Start here: the things I'd most like your eyes on

Reviewed 2026-10-19: TLS support was added (item below), `playlist_sort` was implemented, and the
rest of the original round-4 list was reviewed and accepted as intentional/documented behavior
with no further action — removed from this list rather than left to re-litigate. The permanently-
failed-episode banner concern is already covered by the existing per-issue Dismiss control in the
status banner (`/status/dismiss`), so it needed no new work either.

1. **TLS is now supported but optional.** Set `PODSYNC_TLS_CERT_FILE`/`PODSYNC_TLS_KEY_FILE` to have
   the one listener speak HTTPS instead of HTTP; both are required together and checked at startup.
   Basic auth still goes in the clear if you don't set them (or use a reverse proxy, or keep this on
   a trusted/LAN-only network) — podsync does not fetch or renew certificates itself. → _Security_.
2. **`playlist_sort = "oldest"` is now a per-feed, playlist-only setting.** It passes yt-dlp
   `--playlist-reverse` so a course/series playlist ordered oldest-first still has its newest
   addition at offset 0, instead of updates only ever seeing the first `page_size` episodes ever
   added to it. → _yt-dlp / YouTube adapter_.
3. ⚠ **Episodes inside the newest window that your filters skip are invisible** (no "filtered" badge, no way to grab one). Open question left in PROJECT.md. → _Web UI_.
4. ⚠ **arm64 runtime image was not built** (only the Go cross-compile and the yt-dlp/deno arm64 layers were verified). → _Docker_.

## Pre-release checklist (before the first public release)

Nothing has been released yet, which is why config keys were renamed and removed without migrations. Go through this before tagging and publishing.

**Docker env vars and config setup (review these first)**

- [ ] **Web-UI-disabled variant:** a compose file with `PODSYNC_WEBUI_DISABLED=true` and no credentials set; check the container healthcheck and what the logs say.
  - add var to compose file with comment that it can be set to true
- [ ] **Image permissions:** `/config`, `/data` and `/opt/yt-dlp` are `chmod 0777` so any `--user` works and yt-dlp can self-update. Confirm that is acceptable for a public image.
  - confirming
- [ ] **Published image name and tags.** The compose example builds locally. Pick a registry name, push a multi-arch (amd64 + arm64) image, set a real version instead of `dev` (the ldflags-injected `main.version`), and point the quick start at the published image.
  - confirming

**Config format**

- [ ] **Compatibility policy.** Strict parsing rejects unknown keys, so every removed or renamed key breaks existing files. After the first release that needs either a migration or a documented policy. Decide before tagging, and note the removals so far (`exclude_shorts`, `cleanup_after_days`, global title patterns).
- [ ] **Defaults sanity check:** media format now defaults to **audio**, max resolution 720, `min_duration` 61s, `page_size` 10, `cleanup_keep_last` 50. Confirm these are what a new user should get, and that the README, `config.example.toml` and PROJECT.md all say the same.
  - confirm for me
- [ ] `dev/config.toml` points at a private Tailscale address. `.dockerignore` already excludes `e2e` and the markdown files but **not `dev/`**: confirm it cannot end up in an image (only the built binaries are copied into the final stages today) and decide whether `dev/` belongs in the public repo.
  - [ ] exclude dev/

**Docs and housekeeping**

- [ ] Decide whether FOLLOWUP.md, PROJECT.md and the dev notes ship in the repo at all.
  - A: FOLLOWUP.md should not be included in git history
- [ ] Re-run everything: `go vet ./...`, `go test -race ./...`, the browser suite (`e2e/run.sh`), and the live YouTube test, then run the image on both architectures if possible.
- [ ] Known scaling limit worth a line in the README or an issue: the sidecar index rescans a whole feed when any file in it changes, and the feed page renders every episode at once (fine to a few thousand episodes per feed).
