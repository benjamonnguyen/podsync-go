# FOLLOWUP — decisions to review

Running log of judgment calls made during unsupervised implementation.
**⚠ = I was unsure; worth discussing.** Everything else I'm fairly comfortable with but is listed so you can veto it.

## Scope changes from your answers
- `title_filter` split into `title_include` / `title_exclude` (include applied first, then exclude). Old key `title_filter` is rejected by the strict config loader, not silently ignored.
- **Watched dropped entirely** (no flag, no filter). I also dropped `favorited`, since it had no UI and nothing to drive it. Easy to add back (it's one sidecar field) if you want a "pin" feature.
- yt-dlp problems surface as a banner (see Status banner below).

## Setup
- Go module path is plain `podsync-go` (no host), since you didn't give a GitHub namespace. If this will be `go install`-able or published, rename it before the codebase grows: `go mod edit -module github.com/you/podsync-go` plus a find/replace of the import prefix.
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
- ⚠ Shorts detection is a fixed 60s duration threshold (or a `/shorts/` URL). PROJECT.md said "configurable"; I left it constant to keep the config surface small. Channel `/videos` tabs normally exclude Shorts anyway, so this mostly matters for playlists. Genuine ≤60s normal videos get excluded when `exclude_shorts` is on.
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
