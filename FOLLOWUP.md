# FOLLOWUP — decisions to review

> Sections below were written as the work progressed. Package paths like `internal/puller` or `internal/web` in older sections predate the restructure described under **Restructure into syncr + webui**; the behavior they describe still applies, now living in `syncer/`, `webui/`, `fs/`, `toml/` etc.

Running log of judgment calls made during unsupervised implementation.
**⚠ = I was unsure; worth discussing.** Everything else I'm fairly comfortable with but is listed so you can veto it.

## Start here: the things I'd most like your eyes on
1. ⚠ **HTTP Basic auth only, no TLS** (added in Round 4). Credentials go in the clear unless you put this behind a reverse proxy or a trusted/LAN-only network; `/feeds/` and `/media/` stay deliberately unauthenticated so podcast apps work. → *Security*.
2. **Shorts are now a minimum duration (`min_duration`, default 61s), not a boolean.** It misfired on my first real test (a 19 s channel with the old default looked empty); that is still true by design, since anything under the minimum is skipped, so set `min_duration = "0s"` on tiny or playlist-style feeds. → *Latest round*.
3. ⚠ **Name search depends on an undocumented YouTube `sp=` token** and has no fallback; links and @handles are unaffected. → *Latest round*.
4. ⚠ **Playlists use YouTube's playlist order**, so an oldest-first playlist (a course/series) would only ever pull its first `page_size` episodes. podsync solves this with `playlist_sort`; I didn't. → *yt-dlp / YouTube adapter*.
5. **Age-based cleanup is gone** (`cleanup_after_days` removed); retention is only "keep the newest N" (`cleanup_keep_last`). → *Latest round*.
6. ⚠ **The UI rewrites `config.toml` and drops hand-written comments/ordering.** `config.example.toml` is the documented reference. → *Config*.
7. **Flat listings have no upload date**, so Load More rows show no date and dates only exist after download. → *yt-dlp / YouTube adapter*.
8. ⚠ **Episodes inside the newest window that your filters skip are invisible** (no "filtered" badge, no way to grab one). Open question left in PROJECT.md. → *Web UI*.
9. ⚠ **arm64 runtime image was not built** (only the Go cross-compile and the yt-dlp/deno arm64 layers were verified). → *Docker*.
10. A permanently failed episode keeps its banner entry until retried/dismissed/restart. → *Puller*.
11. Module path is now `podsync`; no license file was chosen; I committed at milestones. → *Setup*.

Real bugs my own tests/browser run caught along the way (all fixed): shared staging dir between concurrent downloads, UI refresh missing short downloads between polls, duplicated rows after Load More downloads, stale header stats. Details in the sections below.

## Scope changes from your answers
- `title_filter` split into `title_include` / `title_exclude` (include applied first, then exclude). Old key `title_filter` is rejected by the strict config loader, not silently ignored.
- **Watched dropped entirely** (no flag, no filter). I also dropped `favorited`, since it had no UI and nothing to drive it. Easy to add back (it's one sidecar field) if you want a "pin" feature.
- yt-dlp problems surface as a banner (see Status banner below).

## Setup
- ⚠ Go module path is now plain `podsync` (was `podsync-go`; changed so the root package's import path equals its name, as in your layout). If this will be `go install`-able or published, rename it to `github.com/you/podsync` early: `go mod edit -module ...` plus a find/replace of the `"podsync` import prefix.
- I committed at each milestone (you said "git init", I read that as "use git"). History is the milestone list; squash if you prefer.

## Config
- Added keys beyond PROJECT.md's first draft: `server.base_url` (RSS enclosure URLs must be absolute), `download.update_interval`, `download.self_update`, `download.cookies_file` (YouTube increasingly demands cookies; one flag, so I included it).
- Strict parsing: unknown keys are a startup error. Good for typos, but it means a config written by a *newer* version fails on an older one.
- ⚠ The UI rewrites config.toml with a TOML encoder, so **hand-written comments and key order are lost on the first UI save**. A header comment warns about it. Preserving comments would need a round-trip TOML library or hand-rolled patching; I judged it not worth it for v1.
- Saving uses write-temp-then-rename, but a Docker single-file bind mount (`./config.toml:/config/config.toml`) cannot be renamed over (EBUSY), so it falls back to an in-place write (not crash-atomic). README tells people to mount the *directory*.
- Default `cleanup_after_days` is **0 (disabled)**; PROJECT.md's example said 30. A 30-day default would silently delete media of slow-publishing channels, which seemed like a bad surprise. `cleanup_keep_last` defaults to 50 as you asked.
- Feed IDs are restricted to `[A-Za-z0-9_-]` because they become directory names (path-traversal guard).

## Storage
- Sidecar status values: `downloaded`, `cleaned` (tombstone, media removed, never re-downloaded by the scheduler), `failed` (retried automatically up to 3 attempts, then waits for a manual Retry).
- If a sidecar says `downloaded` but the media file vanished (someone `rm`'d it), load treats it as `cleaned` rather than serving a dead enclosure or re-downloading behind the user's back.
- Downloads are staged in `data_dir/.tmp` and renamed into the feed dir (same filesystem, so atomic); `.tmp` is wiped on startup.

## yt-dlp / YouTube adapter
All of this was exercised against real YouTube (`PODSYNC_LIVE=1 go test ./internal/source/youtube -run Live -v`): handle resolve, windowed listing, channel + playlist search, video-URL→channel, video and audio download.
- ⚠ **Channel search relies on undocumented `sp=` filter tokens** in YouTube's search-results URL (`EgIQAg%253D%253D` = channels, `EgIQAw%253D%253D` = playlists), fed to yt-dlp's search-URL extractor. It works today and returns far better data (avatar, handle, subscriber count) than the `ytsearch:` approach in the first PROJECT.md draft, but YouTube can change it any time. There is **no automatic fallback** to `ytsearch:`; failure shows in the status banner and the add-by-URL/handle path keeps working. Playlist search also works (bonus, not requested).
- Resolve also accepts a **video URL** (finds the video's channel) and `playlist?list=`/`watch?...&list=` URLs. Anything not on a YouTube host is rejected before reaching yt-dlp. The app has no auth, so without that check it would be a generic "fetch any URL" proxy for anyone on the LAN.
- Every yt-dlp call uses `--ignore-config` (a stray `~/.config/yt-dlp/config` shouldn't change behavior) and `--` before the URL (no flag injection).
- Flat listings contain **no upload date**, so `Published` is empty until an episode is downloaded (the date then comes from the info-json). Consequences: Load More rows show no date; ordering of *known* episodes uses the download-time date. Fetching full metadata per listed video would be ~1-2s each, so I didn't.
- ⚠ **Playlist ordering**: I take the first `page_size` entries in YouTube's playlist order. For "newest first" playlists that's right; for oldest-first playlists (series, courses) you'd get the *first* episodes forever. podsync has a `playlist_sort` option for this; I did not add one. Probably want it.
- (Superseded: Shorts detection is now the `min_duration` filter, see *Latest round*.) Shorts detection was a fixed 60s threshold (or a `/shorts/` URL). PROJECT.md said "configurable"; I left it constant to keep the config surface small. Channel `/videos` tabs normally exclude Shorts anyway, so this mostly matters for playlists. Genuine ≤60s normal videos get excluded when `exclude_shorts` is on.
- Live/upcoming/post-live streams are skipped (listing flag + re-check after metadata fetch).
- Video format: prefers mp4+m4a (H.264/AAC, which podcast apps play), falls back to anything and remuxes to mp4. At 1440p+/4K YouTube only offers VP9/AV1, so those can end up as VP9-in-mp4, which some players won't decode. Audio feeds are always m4a (ffmpeg copies when the source is already AAC).
- Channel avatar: largest *square* thumbnail rather than `avatar_uncropped`, because the latter is `=s0` (original size, can be multi-MB).
- Errors from yt-dlp are reduced to its last `ERROR:` line plus a hint for the common cases (bot check → cookies, 429, "unable to extract" → update yt-dlp).
- Search results are cached 10 min per (query, kind); after a failed search, further searches are refused for 10 s.

## Puller / scheduler
- Pull = list newest `page_size` → filter → queue unseen → wait for downloads → cleanup. Feeds are listed one after another (a few seconds each) but downloads then run in parallel up to `download.concurrency`; the cap is applied from live config, so changing it in Settings takes effect without a restart.
- Only one pull runs at a time (scheduler tick and the Force Pull buttons share a lock). A second Force Pull while one is running gets a "busy" message instead of queuing up.
- Failed downloads are retried on later pulls up to 3 attempts, then wait for a manual Retry (Download button). Manual requests reset the attempt counter.
- ⚠ A permanently failed episode keeps its banner entry until you retry it, dismiss it, or restart (the banner's failure map is in memory, but the `failed` sidecar persists, so after a restart the episode still shows as failed in its feed page, just without a banner).
- ⚠ `cleanup_after_days` is measured from the episode's **publish date**, not the download date. A channel that publishes monthly with `cleanup_after_days = 30` would be mostly emptied. Default is off; say so if you'd rather measure from download time.
- Cleanup never touches manually downloaded episodes (`manual = true`), and keep-last counts only non-manual ones.
- Bug found by my own test while writing this: concurrent downloads in one feed shared a staging directory, so one finishing job could delete another's in-flight file. Staging is now per-episode (`.tmp/<feed>/<episode>`); empty per-feed folders under `.tmp` are left until the next startup wipe.
- Self-update: runs `yt-dlp -U` at startup then every 24 h when `download.self_update = true`; a failure is a warning in the banner (it will always fail for package-manager installs, which is expected and the message says so).

## Web UI
Tested three ways: Go `httptest` for every route, a headless-Chromium run (puppeteer, fake yt-dlp, in `e2e/`) that clicks through search → add → pull → Load More → download → failure → banner → settings → delete, and a manual run against real YouTube. The browser run found three real bugs the Go tests could not (details below).
- htmx 2.0.4 is vendored in `internal/web/static` (no CDN: the app must work offline on a LAN and under a strict CSP). **No Alpine.js**: htmx's debounced triggers covered everything, so PROJECT.md's "touch of Alpine" was dropped.
- Polling: the banner polls every 3 s on every page; an in-flight episode row polls every 2 s. Cheap for one user on a LAN, wasteful if you leave many tabs open.
- **Bug found by the browser run #1**: the episode list refreshed only if a status poll happened to catch work "in progress", so a short download that started and finished between two polls was never shown. Refresh is now driven by a library version counter each page compares against (`/status?v=N`), so it can't be missed.
- **Bug #2**: an episode downloaded via Load More showed up twice (loaded area + main list). Loaded rows now live in their own container and a tiny script in `app.js` drops the duplicate.
- **Bug #3**: header stats ("N episodes · checked X") never updated after a pull; they now refresh with the list.
- Load More offsets count *raw source positions before filtering*, starting at `page_size`. So an episode inside the newest window that your filters skip is never shown anywhere (no "filtered" badge) — this is the open question left in PROJECT.md §11, currently "hide".
- ⚠ Feed-level settings: a blank field means "inherit the default". There is therefore no way to override a non-empty default with an *empty* regex for one feed (e.g. default `title_exclude` set, but this feed wants none). Fixable with an explicit "clear" control; I didn't build one.
- Adding a feed requires a prior preview (cached server-side 15 min) so a forged POST can't add arbitrary IDs; the stored name/avatar come from that preview, not from the form.
- Deleting a feed removes it from config but **keeps the files by default** (checkbox to delete). Kept folders show under "Not in config" with an explicit Delete button, matching the "never silently delete media" rule in PROJECT.md §4.3.
- Settings: `listen_addr` and `data_dir` save but need a restart (the page says so). Changing `data_dir` does not move existing files.

## Security (no auth, as agreed — but a few cheap guards)
- ⚠ **There is no login.** Anyone who can reach the port can browse, delete feeds, change settings and download all media. README says to keep it on a trusted LAN or behind an authenticating reverse proxy. If this should ever be exposed, basic auth is the first thing to add.
- State-changing requests are rejected when `Sec-Fetch-Site` is cross-site or `Origin` doesn't match `Host` (otherwise any web page you visit could silently POST to `http://192.168.x.x:8080/feed/.../delete`). Plain `curl` (no such headers) still works.
- Strict CSP (`default-src 'self'`, images allowed from https so YouTube avatars load), `X-Frame-Options: DENY`, `nosniff`. Hence no inline scripts/styles anywhere.
- Media is served only for files an episode sidecar claims (`.json` sidecars and path tricks 404), with Range support for podcast apps.
- Feed URLs are `/feeds/<channel-id>.xml`; channel IDs are public, so feed URLs are guessable. No "private feed" tokens.

## RSS
- Items are downloaded episodes only; `guid` is `<feedID>:<videoID>` (isPermaLink false); enclosure `length` is the real file size; `itunes:duration`, per-episode and channel images included.
- Not included: `itunes:category`, author, language. Fine for private use with podcast apps; Apple's public directory would want them.
- ⚠ `base_url` defaults to `http://localhost:8080`, which is useless to a phone. The README leads with this; the Settings page labels it clearly, but there is no auto-detection.

## Testing
- `go test -race ./...` — unit tests per package plus `httptest` coverage of every web route (cross-site blocking, add flow, Load More, settings validation, RSS, Range requests, path traversal, delete/orphans).
- Fake `yt-dlp` (helper-process pattern) covers the adapter's listing windows, flags, search cache/backoff, download success/failure and partial-file cleanup.
- `PODSYNC_LIVE=1 go test ./internal/source/youtube -run Live -v` runs the real thing against YouTube (resolve @handle, window listing, channel/playlist search, video→channel, real video + audio download). Passing as of this writing; it's skipped by default because it needs network and yt-dlp.
- `e2e/run.sh` (Node + puppeteer-core + a Chromium binary): 32 browser checks against a fake yt-dlp. Optional dev tooling; nothing in the Go build depends on it.
- Not covered by automation: arm64 *runtime* behavior (see Docker), `self_update` against a real yt-dlp install, cookies-file flow, and anything on a real phone/podcast app. I did not test with an actual podcast client.

## Docker
- **Verified (amd64):** built the image, ran it as uid 1000 with `/config` and `/data` mounted, healthcheck went healthy, `yt-dlp`/`deno`/`ffmpeg` all present, added a real channel through the API and downloaded a real video as m4a (ffprobe: AAC) from inside the container. `yt-dlp -U` runs (reported up to date).
- **Verified (arm64, partially):** the Go binary cross-compiles to a static aarch64 ELF; the `tools` stage built with `--platform linux/arm64` and both the yt-dlp and deno files are aarch64 ELFs (checked the ELF header). ⚠ **I did not build or run the arm64 *runtime* stage** (`apt-get install ffmpeg` under QEMU): the native amd64 apt step alone took ~30 min on this machine, so emulated arm64 would be hours. Debian bookworm ships ffmpeg for arm64 so I expect it to work, but it is unproven. A real arm64 box (or CI with native runners) should run `docker buildx build --platform linux/amd64,linux/arm64 .` once.
- ⚠ **Image size is 688 MB.** Debian's `ffmpeg` pulls ~270 packages (X11, pango, … via libavfilter) and deno is 85 MB. Options if that bothers you: a static ffmpeg build in the `tools` stage (smaller, but third-party hosts and harder to pin), or an Alpine/musl base with yt-dlp's musl binary. I chose boring and big over small and clever.
- yt-dlp and deno versions are pinned with build args (`YTDLP_VERSION`, `DENO_VERSION`). yt-dlp goes stale quickly; bump the arg or enable `download.self_update` (the binary lives in a world-writable `/opt/yt-dlp` so any `--user` can replace it; the update is lost when the container is recreated).
- deno is bundled because yt-dlp now needs a JavaScript runtime for YouTube's player challenges; without one, some videos fail with "format not available".
- Health checks now use each program's own `-healthcheck` flag (reads the configured port), so changing the listen address no longer breaks them (this replaced a hardcoded `curl :8080`).
- The default config written on first start has `data_dir = "./data"`; it lands in `/data` only because the image's `WORKDIR` is `/`. Slightly magical, but it keeps the default config identical to the non-Docker one.
- `compose.example.yaml` uses `build: .` (no published image / CI). It sets `user: "1000:1000"`; `/config` and `/data` are mode 0777 inside the image so other uids work too.

## Deliberately not built (say if you want any)
Watched/favorited (dropped per your note), in-browser player, OPML export, `playlist_sort`, per-feed cookies, hooks/webhooks, private feed tokens, auth/HTTPS, an episode-level delete button, pagination of the library page (fine for dozens of feeds), log-file configuration (logs go to stdout).

## Environment quirks worth knowing
- This sandbox's Docker daemon is slow at disk-heavy steps; builds that take minutes here should be much faster elsewhere.
- Everything was developed with Go 1.27 (`go 1.27.0` in go.mod, `golang:1.27-bookworm` in the Dockerfile).

## Thumbnails, episode metadata and the dev config (second pass)
- Every download now also saves the thumbnail via yt-dlp (`--write-thumbnail --convert-thumbnails jpg`, so ffmpeg is needed, which it already was) as `<id>.jpg` next to the media. A missing/unconvertible thumbnail never fails the episode (unit-tested).
- Sidecar fields added: `thumb_file`, `channel`, `view_count`, `height` (video only), alongside the existing title, description (truncated to 4000 chars), URL, remote thumbnail URL, publish date, duration, size. Listing results (the faded Load More rows) carry thumbnail, channel and view count from the flat listing; they have no date or description until downloaded.
- The local jpg is what the RSS `itunes:image` points at (stable, no hotlink expiry) and what the UI shows; rows fall back to the remote YouTube thumbnail when there is no local one. **Cleanup deletes only the media**: thumbnail and sidecar stay, so cleaned rows still look right. Deleting a feed with "also delete files" ticked removes everything.
- The UI row shows thumbnail, date, duration, views, resolution, size, a **Details** toggle (description + "Open on YouTube"), and for playlist feeds the uploader's channel.
- ⚠ Episodes downloaded *before* this change have no local thumbnail (only the remote URL). There is no backfill job; since nothing was shipped before this it only affects my own test data, but a "re-fetch thumbnails" button would be needed on any real library.
- ⚠ Thumbnails for the **feed/channel image** are still the remote YouTube avatar URL, not saved locally. Fine for podcast apps today; it would break if Google ever expires those URLs.
- When a row is re-rendered before the library knows the episode, a thumbnail URL sent by the browser is accepted **only if it starts with `https://`** (otherwise `javascript:`-style URLs could end up in an `<img>`; tested).
- `dev/config.toml` is a checked-in dev config with `https://youtube.com/@aragusea` (channel `UC9_p50tH3WmMslWRWKnM7dQ`), 480p, `page_size = 5`, data in git-ignored `dev/data`. Run: `go run ./cmd/podsync -config dev/config.toml` from the repo root. The UI rewrites this tracked file when you change settings, so expect a diff after using Settings.
- Observed live: one of the five first-pull downloads failed with `HTTP Error 403: Forbidden` (a transient YouTube/CDN thing; yt-dlp had already retried 3x). It showed up as a failed row plus a red banner exactly as designed, and a manual Retry succeeded. ⚠ It would also have been retried automatically on the next scheduled pull; I did not add an immediate in-job retry for 403s. If you see a lot of these, a quick second attempt with a different yt-dlp player client may be worth adding.

## Architecture as it now stands (after three rounds of your direction)
Round 1: root `podsync` package for domain types, subpackages by dependency, `cmd/syncr` + `cmd/webui`. Round 2: both programs share the config file and data directory; syncr's API shrinks to what only it can do. Round 3 (latest): **the web UI lives entirely in `cmd/webui`, and syncr publishes the feeds; the web UI is internal, for library management.** (Older sections' package paths like `internal/puller` predate all this.)

**Layout** (PROJECT.md §4): root `podsync` = domain types + interfaces (stdlib only) · `youtube` (yt-dlp) · `fs` (library on disk) · `toml` (config file) · `status` · `syncer` (service logic) · `publish` (public feeds + media) · `rest` (control API server + client) · `rss` · `mock`. `cmd/syncr` wires; `cmd/webui` is a self-contained main package (handlers, templates, static files, view model, its tests). Module path is `podsync`.

**On "injected"** (your earlier question): the web UI only gets a `podsync.Syncer` *interface value* (the REST client); the sync service is not in its binary. `go list -deps ./cmd/webui` shows only `podsync`, `fs`, `rest`, `toml`: no `syncer`, `youtube`, `publish` or `rss`.

**Who does what**
- **syncr** runs yt-dlp, writes the library, only *reads* the config, and has **two listeners**: public `server.listen_addr` (default `:8080`) serving only `/feeds/<id>.xml`, `/media/<id>/<file>` and `/healthz` (package `publish`); and the private control API `server.api_addr` (default `127.0.0.1:8081`).
- **webui** edits `config.toml`, reads the library, deletes feed folders, renders pages, and links to syncr's public address for feeds, media and thumbnails. It does not serve feeds or media (tested: they 404).
- **Control API:** `POST /apply`, `POST /pull` (all / one feed / one episode), `GET /status`, `GET /resolve`, `GET /search`, `GET /feeds/{id}/listing`, `/healthz`. You named the first two; the other four exist because only syncr has yt-dlp or the live queue. The alternative to the last three is yt-dlp in the web UI image.

**Decisions from this round**
- ⚠ **Two listeners in syncr (and a new `server.api_addr` setting).** If the public port also carried the control API, publishing feeds to podcast apps would expose an unauthenticated API. So they are separate, config validation refuses `api_addr == listen_addr`, and the API defaults to loopback. The Docker image sets `SYNCR_API_ADDR=:8081` (an *environment override* of the file value, since the web UI is in another container); that override isn't visible on the settings page.
- ⚠ **`server.base_url` now means syncr's public address.** The web UI uses it for the feed links it shows and to load thumbnails/media links **in your browser**, so the browser must be able to reach it, and the web UI's CSP allows exactly that origin for images/media (follows the setting; tested). With the default `http://localhost:8080`, thumbnails break when you browse the UI from another machine until you set it. The dev config uses `http://towhee:8080`.
- Default ports changed again: **8080** feeds/media (public), **8081** control API (loopback), **8090** web UI. The web UI defaults to **loopback** (`127.0.0.1:8090`); the Docker image sets `WEBUI_LISTEN=:8090` and the dev script passes `-listen :8090`.
- ⚠ **Feeds and media are public to anyone who can reach port 8080 and knows a feed URL** (channel IDs are public, so URLs are guessable). That's now the single intentionally public surface. No private-feed tokens.
- ⚠ **The web UI is still unauthenticated** and can change settings and delete feeds. "Internal" is enforced only by where you put it (loopback default, separate port, compose publishes it on its own port). Put it behind an authenticating proxy or keep it on a trusted network.
- The Settings page no longer edits addresses or data dir (they're shown read-only; edit the file and restart both). The label of `base_url` there now says it's the sync service's public address.
- `cmd/webui` being a main package means its logic can't be imported elsewhere and its tests are internal (`package main`). Fine for a leaf program.
- New tests: `publish` (RSS content, media/thumbnail/Range, nothing else served, orphaned folders not published); config validation for the two addresses; a test that `config.example.toml` and `dev/config.toml` still parse; web UI tests assert it 404s on feeds/media/API and that links and CSP follow `base_url`.

**Keeping two processes consistent** (unchanged from round 2; the part to check)
- `fs.Library.Refresh()` rescans a feed folder only when its directory mtime moved (every sidecar write is a rename) plus a 3-second settle window. The web UI calls it on every request; syncr on `apply` and at the start of each pull. `Load()` doesn't wipe `.tmp` (only syncr's `CleanTemp()` does).
- `toml.Store.Reload()` compares file bytes: a hand edit is picked up by the web UI on the next request and by syncr on `apply` **or at its next scheduled pull at the latest**; a broken file is ignored (last good settings stay) and shown as a non-dismissible banner.
- ⚠ Accepted races: webui deleting a feed's folder while syncr downloads into it (syncr discards the staged file); both writing a feed's `_meta.json` (cache only).
- ⚠ Network filesystems (NFS/SMB attribute caching) may show stale data in the web UI for a while; local disks and Docker bind mounts are fine.
- ⚠ `data_dir` must resolve to the same place in both programs (Docker: both `WORKDIR /`; elsewhere run both from one directory or use an absolute path).
- If syncr is unreachable the web UI keeps working for everything file-based and shows a non-dismissible banner; status calls time out after 3 s. Changes saved meanwhile apply at syncr's next start or pull. Podcast apps are unaffected as long as syncr's public port is up.
- ⚠ Root types carry both `toml` and `json` tags (no separate DTOs); `syncer` is outside the root package; `toml` imports `fs` for `WriteFileAtomic`.

## Round 4: consolidated into one binary, one port, Basic auth

You asked to merge `cmd/syncr` and `cmd/webui`, run both behind one listener, and add real login for the
management portal instead of relying on "keep it internal." Round 3's two-process, two-listener design (above)
is now history, not the current state — see PROJECT.md §4 for what replaced it. Summary of what changed:

- **One binary, `cmd/podsync`.** `cmd/syncr` and `cmd/webui` are gone; the web UI's handlers/templates/model
  moved into `cmd/podsync` unchanged (they only ever depended on the `podsync.Syncer` interface, not the REST
  client specifically). `rest.Server`/`rest.Client` and the whole control API are deleted — the web UI calls
  `*syncer.Service` directly now, since it's the same process. `rest.CrossSiteGuard` was inlined into
  `cmd/podsync/server.go` as `crossSiteGuard`; package `rest` no longer exists.
- **One listener.** `PODSYNC_LISTEN_ADDR` (default `:8080`) serves everything: `/feeds/`, `/media/`
  and `/healthz` are always public (mounted from package `publish`, unauthenticated); `/` is the web UI, wrapped
  in HTTP Basic auth, unless disabled.
- **The "sync service unreachable" failure mode is gone.** Since the web UI calls the sync loop in-process,
  there's no second process to be down. Removed the webui tests that simulated an unreachable `rest.Client`
  (`TestSettingsSavedEvenWhenSyncServiceIsDown`, `TestSyncServiceOutageIsVisibleButBrowsingStillWorks`) — that
  scenario can no longer happen.
- **New env vars**: `PODSYNC_WEBUI_DISABLED` (default `false`) replaces `PODSYNC_API_ENABLED`; `PODSYNC_USERNAME` and
  `PODSYNC_PASSWORD` are required unless the web UI is disabled (checked in `config.validate`,
  constant-time-compared in `basicAuth`). `PODSYNC_API_ADDR`, `SYNCR_URL`, `WEBUI_LISTEN` are gone.
- Dockerfile is single-stage-output now (one image, no `--target syncr|webui`); `compose.example.yaml` is one
  service on port 8080; `dev/run.sh` and `e2e/run.sh` build and run the one binary (`e2e/run.mjs` now calls
  `page.authenticate()` with the test credentials before driving the UI).
- ⚠ Not done: TLS. Basic auth sends credentials in the clear, so the README still tells people to put this
  behind a reverse proxy or a trusted network if the deployment isn't already LAN-only.

### Round 4.1: dropped command-line flags entirely

You followed up: no command flags at all, environment variables only, fail with a clear message if an
essential one is missing; rename the web UI's login env vars to the plainer `PODSYNC_USERNAME`/`PODSYNC_PASSWORD` (no
`PODSYNC_` prefix, no separate OS user to create — it's just the HTTP Basic pair). Changes:

- `cmd/podsync/main.go` lost `parseFlags`/the `flag` import entirely; `configFromEnv()` reads the environment
  directly. `PODSYNC_WEBUI_USER`/`PODSYNC_WEBUI_PASSWORD` renamed to `PODSYNC_USERNAME`/`PODSYNC_PASSWORD`.
- `PODSYNC_BASE_URL` is now **required** (previously defaulted to `http://localhost:8080`, which only worked
  by accident on the same machine); `config.validate()` fails with `"PODSYNC_BASE_URL is required (your
  address, e.g. http://192.168.1.10:8080)"` if unset, before touching the network or the config file.
- `-version` and `-healthcheck` (and `showVersion`/`healthcheck` in `main()`) are gone along with the rest of
  the flags. Health checks now work by re-invoking the same binary with `PODSYNC_HEALTHCHECK=1` set, which
  just probes `/healthz` and exits 0/1 — see the Dockerfile's `HEALTHCHECK` line (`env PODSYNC_HEALTHCHECK=1
  /usr/local/bin/podsync`). `-version`'s only use was a human typing it at a shell; not worth keeping a
  special-case env var for it, so it's gone with no replacement (the version still appears in the startup log
  line, from the ldflags-injected `main.version`).
- Updated `Dockerfile`, `compose.example.yaml`, `dev/run.sh`, `e2e/run.sh` and the README/PROJECT.md tables to
  match; verified by hand (no flags passed, env vars only): missing `PODSYNC_BASE_URL` and missing
  `PODSYNC_USERNAME`/`PODSYNC_PASSWORD` each fail fast with the expected message and exit 1; a full set of env vars starts the
  server, `/healthz` and `/feeds/...` answer without credentials, `/` 401s without or with wrong credentials
  and 200s with the right ones, and `PODSYNC_HEALTHCHECK=1` exits 0 against the running instance.
- Bug fixed along the way (since the first pass): `html/template` doesn't URL-escape `hx-*` attributes, so a title with `&`, `#` or `+` broke that row's polling. Now `urlquery`-escaped, with a test.
- Tests: the web UI suite runs a two-program setup in one process (two `ConfigStore`s and `Library`s over one temp config file and data dir, the real syncer behind the real REST server and client). The browser run (`e2e/run.sh`) starts both real binaries on one file and one directory.

### Docker, superseded by the Round 4 merge
(This subsection described the two-container shape from Round 3 — separate `podsync-syncr`/`podsync-webui` images, 12 MB + 686 MB. Round 4 deleted `cmd/syncr`/`cmd/webui` in favor of one `cmd/podsync` binary and one image, so that verification no longer describes reality.) ⚠ **Not re-verified since the merge**: an actual `docker build`/`docker compose up` of the current single-image `Dockerfile`/`compose.example.yaml`, current image size, and arm64 runtime. Covered by the pre-release checklist's "walk the quick start from a clean clone" item — do that before relying on this being tested.

### The dev instance
`dev/run.sh` builds and runs the one `cmd/podsync` binary on `dev/config.toml`: everything (feeds, media, web UI) on **:8080**, `PODSYNC_BASE_URL=http://100.120.50.37:8080`, Basic auth `dev`/`dev`. The host's `ufw` is default-deny with only port 22 open, so other machines can't reach 8080 until you allow it; I can't change that without sudo.


## Latest round: lookup, defaults, and what the web UI may edit
**What you asked for, as built**
1. **Not editable in the web UI:** the public address (`server.base_url`), `download.yt_dlp_path` and `download.cookies_file`. The Settings page now *shows* them (plus the addresses and data dir) read-only, and the server side ignores them even in a forged POST (tested: a post carrying `base_url`, `yt_dlp_path`, `cookies_file`, addresses and `data_dir` leaves them untouched). The web UI form can no longer repoint the downloader or the published address. Still editable there: concurrency, update interval, yt-dlp self-update, and all feed defaults.
2. **Default max resolution is 720** (was 1080). `dev/config.toml` stays at 480 on purpose (small test downloads).
3. **`exclude_shorts` (bool) → `min_duration` (duration, default `"60s"`)**, per-feed overridable, `"0s"` = no minimum. Written in the file as a Go duration (`"60s"` is saved back as `"1m"`; the forms show seconds, and also accept a bare number as seconds). Semantics: an episode *shorter than* the minimum is skipped; one of unknown length (0) is never skipped. The old rule was "≤ 60 s or a /shorts/ URL"; now exactly-60 s videos pass and the URL check is gone (flat channel listings use watch URLs anyway), and the `IsShort` field/`is_short` sidecar key were removed (old sidecars still load; the key is ignored). ⚠ **Old config files with `exclude_shorts` now fail to load** (strict parsing names the key); nothing was released so I added no migration.
4. **No playlist search; playlists by URL only.** Bare playlist IDs are not a hint either (a bare ID goes to channel search). `Resolve` still accepts a playlist ID internally (syncr uses it to fill in a feed's image).
5. **One lookup with hints** (`Source.Find`, one `GET /api/v1/find?q=`, one box on the Add page): a **link** → resolve; an **`@handle`** or `UC…` ID → resolve; **anything else** → channel name search. A bare word like `LinusTechTips` is a *name*, not a handle (it used to be treated as one). `Source.Search`, `SearchKind`, `SearchResult` and `/resolve` + `/search` are gone; results are `FeedInfo` (which gained `Handle` and `Subscribers`).

**Decisions you may want to flip**
- ⚠ **The Add page is submit-driven (Enter / Find), not live-as-you-type.** Previously name search ran 700 ms after typing. With one box, live lookup would resolve half-typed handles (`@lin`) and hit YouTube on every pause, so I made it explicit. One match goes straight to the preview (even for a name search that returns a single channel); several are listed, and Select looks the pick up again by ID (a second 1–2 s yt-dlp call, but it gives the full description instead of the truncated search snippet).
- A mistyped/unresolvable *reference* is reported to the user as invalid input and does **not** raise a status banner; a failing *name search* does ("Looking up feeds failed", clears on the next success) and backs off for 10 s. ⚠ Side effect: a yt-dlp outage during a link lookup shows up only inline, not in the banner (the health check still flags a missing yt-dlp).
- Per-feed `min_duration`: blank = inherit, `0` = explicitly no minimum (an explicit zero survives the TOML round trip; tested).
- README/PROJECT.md cleaned: I found the README carried two stale duplicate API sections from an earlier edit; removed.

**Verified**: unit/HTTP tests (race), the live YouTube test (name search → channel with handle and subscriber count, `@LinusTechTips` and a playlist link resolve correctly), and the browser run (35 checks, now covering the single box, list → select → preview, an unresolvable handle shown inline, and a pasted link going straight to the preview).


## Latest round (2): leaner retention and filters
- **`cleanup_after_days` deleted** from the config (defaults and per feed), the syncer, both settings forms, tests and docs. Cleanup is now only `cleanup_keep_last` (default 50, `0` = keep everything; manual downloads stay exempt). This also retires the "slow-publishing channels get emptied" concern (it was my review item 5).
- **No global title patterns.** `title_include` / `title_exclude` are removed from `[defaults]` and the global Settings page; they are plain per-feed fields (`TitleInclude`, `TitleExclude`: strings, empty = off) on the `[[feeds]]` entry, edited on the feed's own settings page. They are validated per feed (a bad regex names the feed and the field), and `Feed.Filter(defaults)` builds the episode filter from the feed's effective minimum duration plus its own patterns. Side effect that is a *fix*: the old review item "a blank per-feed field can't override a non-empty default regex" no longer exists, since there is no default regex.
- **Default `min_duration` is `"61s"`** (was 60s), so videos of exactly 60 s, which YouTube counts as Shorts, are skipped too. It is written as `"1m1s"` by the TOML encoder when the UI saves the file (a Go duration; the forms show `61s`).
- ⚠ **Old config files fail to load** if they contain `cleanup_after_days`, `[defaults] title_include/title_exclude`, or `exclude_shorts`; strict parsing names the offending key. Nothing was released, so no migration. Tests assert each of these is rejected, and a test that `config.example.toml` and `dev/config.toml` still parse caught my own stale example during this change.
- Sidecars and feeds written by earlier builds still load: unknown JSON fields in sidecars are ignored.
- Verified: unit/HTTP tests (race), browser run (35 checks), dev instance restarted on the new build.


## Pre-release checklist (before the first public release)
Nothing has been released yet, which is why config keys were renamed and removed without migrations. Go through this before tagging and publishing.

**Docker env vars and config setup (review these first)**
- [ ] **List every environment variable and decide the final set.** Today, one binary/one image: `PODSYNC_CONFIG`, `PODSYNC_DATA_DIR`, `PODSYNC_LISTEN_ADDR`, `PODSYNC_BASE_URL` (required), `PODSYNC_USERNAME`/`PODSYNC_PASSWORD` (required unless `PODSYNC_WEBUI_DISABLED=true`), `PODSYNC_WEBUI_DISABLED`, `PODSYNC_COOKIES_FILE`, `PODSYNC_YTDLP_PATH`, `PODSYNC_HEALTHCHECK` (internal, set by the Dockerfile's `HEALTHCHECK` line, not user-facing), plus `TZ` (compose) and `HOME`/`XDG_CACHE_HOME` (image internals). Check each is in the README table and `compose.example.yaml`, and that env-over-file precedence is documented. (Superseded: the old two-process design's `SYNCR_API_ADDR`, `SYNCR_URL`, `WEBUI_LISTEN` are gone along with that split — see Round 4.)
- [ ] **First-run experience in Docker.** `PODSYNC_BASE_URL` has no default and podsync now refuses to start without it, which avoids the old "thumbnails break from another machine" trap. Still worth walking the quick start fresh to make sure the failure message is clear enough for someone who hasn't read this file.
- [ ] **Walk the quick start from a clean clone** on a machine with nothing cached: build or pull the image, `docker compose up -d` with `PODSYNC_BASE_URL`/`PODSYNC_USERNAME`/`PODSYNC_PASSWORD` set, add a feed, play an episode in a podcast app on another device. Also try an arbitrary `--user`, the `config` and `data` folders created by root, and the single-file bind-mount mistake.
- [ ] **Web-UI-disabled variant:** a compose file with `PODSYNC_WEBUI_DISABLED=true` and no credentials set; check the container healthcheck and what the logs say.
- [ ] **Image permissions:** `/config`, `/data` and `/opt/yt-dlp` are `chmod 0777` so any `--user` works and yt-dlp can self-update. Confirm that is acceptable for a public image.
- [ ] **Published image name and tags.** The compose example builds locally. Pick a registry name, push a multi-arch (amd64 + arm64) image, set a real version instead of `dev` (the ldflags-injected `main.version`), and point the quick start at the published image.

**Config format**
- [ ] **Compatibility policy.** Strict parsing rejects unknown keys, so every removed or renamed key breaks existing files. After the first release that needs either a migration or a documented policy. Decide before tagging, and note the removals so far (`exclude_shorts`, `cleanup_after_days`, global title patterns).
- [ ] **Defaults sanity check:** media format now defaults to **audio**, max resolution 720, `min_duration` 61s, `page_size` 10, `cleanup_keep_last` 50. Confirm these are what a new user should get, and that the README, `config.example.toml` and PROJECT.md all say the same.
- [ ] `dev/config.toml` points at a private Tailscale address. `.dockerignore` already excludes `e2e` and the markdown files but **not `dev/`**: confirm it cannot end up in an image (only the built binaries are copied into the final stages today) and decide whether `dev/` belongs in the public repo.

**Docs and housekeeping**
- [ ] Decide whether FOLLOWUP.md, PROJECT.md and the dev notes ship in the repo at all.
- [ ] LICENSE and a short security note. There is **no login** anywhere; the README warns about it, but re-read that warning once more from a stranger's point of view before the project is public.
- [ ] Re-run everything: `go vet ./...`, `go test -race ./...`, the browser suite (`e2e/run.sh`), and the live YouTube test, then run the image on both architectures if possible.
- [ ] Known scaling limit worth a line in the README or an issue: the sidecar index rescans a whole feed when any file in it changes, and the feed page renders every episode at once (fine to a few thousand episodes per feed).
