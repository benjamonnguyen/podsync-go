# Podsync-Go — Project Overview & Requirements

> Status: implemented. This document is the original design; where the implementation
> differs or a call was uncertain, see [FOLLOWUP.md](FOLLOWUP.md). User docs are in [README.md](README.md).

## 1. Overview

A self-hosted Go application that turns YouTube channels/playlists into podcast
RSS feeds, similar to [mxpv/podsync](https://github.com/mxpv/podsync). Episodes
are downloaded with `yt-dlp`, stored on disk, and exposed both as podcast RSS
feeds (for podcast apps) and through a built-in web UI for library management.

Terminology: a **feed** is one configured channel or playlist (an entry in
config.toml's `[[feeds]]`) together with the RSS feed generated for it.

Primary differentiator from upstream podsync: a web UI for browsing and
managing the library (channels/playlists, episodes, search,
manual refresh, settings, issue banner) instead of config-file-only operation.

## 2. Goals

- Support YouTube channels and playlists as feed sources.
- Download audio or video via `yt-dlp`, with configurable quality/resolution caps.
- Generate a standards-compliant podcast RSS feed for each configured feed.
- Apply per-feed filters: minimum video duration (skips Shorts), title regex include/exclude,
  max resolution, audio-only vs video.
- Configurable retention/cleanup of downloaded episodes (age- and/or count-based).
- Persist configuration in a plaintext file that the app loads at startup and
  saves back after changes made via the UI.
- Provide a web UI to manage feeds and browse episodes without editing
  config files by hand.
- Be architected so new sources (e.g. Vimeo, RSS passthrough) can be added
  later without touching core logic.

## 3. Non-Goals (v1)

- No multi-user auth/accounts — single-operator, LAN/home-server use case.
- No transcoding pipeline beyond what `yt-dlp`/`ffmpeg` already provide.
- No mobile app — podcast apps consume the generated RSS; web UI is
  responsive but not a packaged app.
- No official YouTube Data API key requirement (see §8).
- No multi-instance/horizontal scaling — exactly one app process is assumed
  to run against a given `data_dir`/config.toml at a time (confirmed; this
  is what makes the filesystem-as-truth model in §4.3 safe — a single
  in-process `sync.RWMutex`-guarded index with no cross-instance
  coordination needed).

## 4. Architecture

One process, one listener, sharing the config file and the data directory with itself. **podsync publishes
the feeds and serves the management web UI from the same port.**

```
                      config.toml          data/ (media + JSON sidecars)
                       ▲   ▲                    ▲   ▲
                 writes│   │reads         reads/│   │writes
                       │   │            deletes │   │
 you (browser) ──► web UI (in-process call) ──► sync loop ──► yt-dlp, ffmpeg, deno
                   (views, edits)                  │  scheduler, queue, cleanup
                   login required                   │
                                                    └─ public, same :8080: /feeds/<format>/<id>.xml, /media/...  ◄── podcast apps
```

- **cmd/podsync** is the one binary. It runs the sync loop (lists feeds, downloads episodes, applies cleanup,
  writes the library), publishes the RSS feeds/media/thumbnails (package `publish`), and — unless
  `PODSYNC_WEBUI_DISABLED` is set — serves the management web UI, all behind one `http.Server` on
  `PODSYNC_LISTEN_ADDR`. The web UI calls the sync loop's methods directly (it implements `podsync.Syncer`),
  in-process: no network hop, no second port, no "service unreachable" failure mode. Every deployment
  setting is a `PODSYNC_`-prefixed environment variable; `-listen`/`-config` also accept a command-line flag
  (for running instances by hand without Docker), and a flag wins over its environment variable when both
  are given. podsync fails fast naming whatever's missing or invalid, before listening or touching yt-dlp.
- Routing on the single mux: `/feeds/`, `/media/` and `/healthz` are always public and unauthenticated (that's
  what podcast apps and container health checks fetch); everything else is mounted at `/` behind HTTP Basic
  auth (`PODSYNC_USERNAME`/`PODSYNC_PASSWORD`, required unless the web UI is disabled).
- The web UI reads and edits the config file, reads the library (and deletes feed folders), and renders pages;
  it does not itself publish feeds — that's always package `publish`, mounted separately on the same mux. The
  feed links and thumbnail/media URLs it shows point at the public address (`PODSYNC_BASE_URL`).
- The web UI re-reads the config file and rescans changed feed folders on every request (cheap when nothing
  changed), so hand edits to `config.toml` or changes from the sync loop are visible without a restart.
- The config file has one writer (the web UI); the library has one writer (the sync loop), with two small
  exceptions in the web UI: deleting feed folders and writing a new feed's `_meta.json`.

### 4.0.1 Package layout

The root package `podsync` is the domain: types and interfaces only, no dependencies beyond the standard
library. Subpackages are grouped by what they depend on. `cmd/podsync` wires everything together (sync loop,
publisher, web UI) behind one listener.

| Package | Depends on | Role |
|---|---|---|
| `podsync` (root) | stdlib | `Feed`/`Config`, `Episode`, `Source`, `Library`, `ConfigStore`, `StatusTracker`, the small `Syncer` contract, error kinds, `EpisodeFilter`, `Registry` |
| `youtube` | yt-dlp | implements `podsync.Source` |
| `fs` | filesystem | implements `podsync.Library` (+ atomic file writes, `Refresh` for shared-state use) |
| `toml` | BurntSushi/toml | implements `podsync.ConfigStore` (+ `Reload`) |
| `status` | — | implements `podsync.StatusTracker` in memory |
| `syncer` | root interfaces only | implements `podsync.Syncer`: the update loop, queue, cleanup |
| `publish` | net/http, rss | the public side: RSS feeds, media, thumbnails |
| `rss` | encoding/xml | podcast feed rendering |
| `mock` | — | test doubles |
| `cmd/podsync` | everything above | wiring, the single HTTP listener, HTTP Basic auth for the web UI, the web UI's templates/handlers/view model |

`podsync.Syncer` is implemented by `syncer.Service` and called directly by the web UI in the same process —
there is no RPC boundary left to preserve error kinds across; `podsync.ErrNotFound`/`ErrInvalid`/`ErrBusy` are
just ordinary Go errors now.

### 4.1 Source adapter pattern

Core abstraction all sources implement (`podsync.Source`), so YouTube (package `youtube`) is just the first plugin:

```go
type Source interface {
    Kind() string                                   // stable identifier, e.g. "youtube"
    Check(ctx context.Context) Health               // are the external tools (yt-dlp, ffmpeg) usable?

    // Find answers what a user typed when adding a feed, guided by hints: a URL, an
    // @handle or a channel ID resolves to exactly that feed; anything else is searched
    // by name among channels. Playlists are only found by URL.
    Find(ctx context.Context, query string) ([]FeedInfo, error)

    // Resolve turns an exact reference (URL, @handle or ID) into its canonical feed.
    Resolve(ctx context.Context, ref string) (FeedInfo, error)

    // ListEpisodes returns the window [offset, offset+limit) of a feed's episodes,
    // newest first. The scheduler calls it with offset 0 and limit = page_size;
    // the UI's Load More calls it with later offsets. Unavailable entries are
    // returned (flagged) so callers can tell a short page means "end of feed".
    ListEpisodes(ctx context.Context, feed FeedInfo, offset, limit int) ([]EpisodeInfo, error)

    // Download fetches one episode into destDir and returns the file plus the
    // full metadata discovered while downloading (title, date, duration).
    Download(ctx context.Context, ep EpisodeInfo, opts DownloadOptions, destDir string) (DownloadResult, error)
}

// Optional: sources that can update their own tooling (yt-dlp -U).
type Updater interface{ SelfUpdate(ctx context.Context) (string, error) }
```

A `SourceRegistry` maps `Kind() -> Source`. The puller, resolver, and API
handlers depend only on the `Source` interface, never on YouTube specifics.
Filtering (minimum duration, title regex) is applied generically
in the puller using metadata every adapter is required to populate
(`EpisodeInfo.DurationSec`, `.Title`, ...), so adapters
stay thin.

### 4.2 YouTube adapter

- Wraps `yt-dlp` as a subprocess (JSON output via `--dump-json` /
  `--flat-playlist` for listing, normal invocation for download), rather than
  reimplementing extraction. Avoids an official API key entirely.
- Playlist/channel listing uses `--flat-playlist --dump-single-json` with
  `--playlist-start`/`--playlist-end` derived from the `offset`/`limit` passed
  to `ListEpisodes`, so each call only touches the requested window; full
  metadata fetch happens lazily per-episode when first downloaded (upload
  date, description, resolution). The flat listing already carries `duration`.
- Shorts have no special handling: the `min_duration` filter (default 61s)
  skips anything shorter, using the listing's duration.
- Resolution cap enforced via yt-dlp format selector
  (e.g. `bestvideo[height<=?720]+bestaudio/best[height<=?720]`); audio-only
  mode uses `-x --audio-format <fmt>`.

### 4.3 Library storage: filesystem + sidecar files (no DB)

No SQLite/BoltDB. Ownership is split between two things, deliberately not
merged into one store:

- **config.toml is the authoritative definition of *what* to subscribe to**
  (the `[[feeds]]` list — §5) — it's what a fresh instance bootstraps
  from. Point the app at a config file and an *empty* `data_dir`, and it
  reconstructs the whole feed set by re-resolving and re-pulling
  each entry; this is also how you'd migrate to a new host or recover from
  a lost `data_dir`.
- **`data_dir` sidecar files hold per-episode library state** — download
  status/path, manual-download flag — which is intentionally *not* part of
  config.toml and *not* reconstructable from it. This is state about a
  specific library on a specific disk, not a declaration of intent; losing
  it on a fresh bootstrap (re-pulling from empty `data_dir`) is expected and
  fine, same as it would be for upstream podsync.

Directory layout:

```
data_dir/
  youtube/UCxxxxxxxxxxxxxxxxxxxxxx/        # one dir per feed (id from config.toml)
    _meta.json                             # cache only: display name, thumbnail,
                                            # last-pull timestamp
    <episode-id>.json                      # episode metadata + download status
    <episode-id>.mp4 / .m4a                # media file (present once downloaded)
    <episode-id>.jpg                       # thumbnail (kept after cleanup)
```

- **`_meta.json`** is a *cache*, not configuration — display name/thumbnail
  and the last-pull timestamp, all cheaply regenerable by re-resolving via
  yt-dlp if the file is missing or stale. It is never the only place a
  feed's existence is recorded; config.toml is.
- **Episode sidecar** (`<episode-id>.json`): app-owned metadata — title,
  upload date, duration, download
  timestamp, `status` (`downloaded` | `cleaned` | `failed`), `manual` bool (downloaded via
  the UI's Load More rather than the scheduler — exempt from cleanup, §6.1),
  and room for similar flags later. Sidecars exist only for episodes that
  have been downloaded; episodes merely listed by Load More are ephemeral
  and not persisted. Deliberately a small app-defined schema, not yt-dlp's raw
  `--write-info-json` dump — keeps the schema stable and gives us a place
  for app-only fields yt-dlp knows nothing about. (yt-dlp's own info-json
  can still be requested as a debug artifact if useful, but isn't parsed as
  the source of truth.)
- **Reconciliation on startup / settings apply**: the app diffs config.toml's
  feed list against `data_dir`'s subdirectories — missing ones get
  created (and queued for an initial pull of the newest `page_size`
  episodes, §6.3);
  ones present on disk but removed from config.toml are *not* silently
  deleted (that would destroy downloaded media on a config typo) — flagged
  in the UI as orphaned, with an explicit delete action left to the user.
- **In-memory index**: on startup, walk `data_dir` once and build an
  in-memory index (feed list + episode list with metadata) for fast
  UI queries (search, sort) without re-parsing JSON on every
  request. Updated incrementally as the puller downloads episodes or the UI
  mutates state (delete, manual download); the filesystem write is the
  durable step, the index is a cache derived from it and can always be
  rebuilt by rescanning.
- **Write safety**: sidecar writes use the same atomic write-temp-then-rename
  pattern as the config file (§5), so a crash mid-write can't corrupt a
  sidecar — worst case the rename never happens and the old file (or none)
  survives.
- **Tradeoffs accepted**: no SQL queries/joins or transactional multi-row
  updates; "search" is a linear scan over the in-memory index rather than an
  indexed query, which is fine at home-library scale (hundreds to low
  thousands of episodes) but wouldn't scale to a multi-tenant deployment —
  acceptable given §3's single-operator scope. In exchange, the library
  state is plain files: trivially backed up with `rsync`/`cp`, human-
  readable/debuggable, and inherently consistent with what's actually on
  disk (no DB-vs-filesystem drift to reconcile).

### 4.4 Scheduler / puller

- Background loop on a configurable interval (default e.g. 30 min) calls
  `Source.ListEpisodes(feed, 0, page_size)` per feed — i.e. only the newest
  `page_size` episodes are considered each cycle (§6.3) — applies filters,
  enqueues unseen episodes for download, then runs cleanup.
- "Force pull" from the UI triggers the same path on demand for one
  feed or all of them.

## 5. Configuration

- Stored as a single plaintext file (TOML) at a configurable path
  (default `~/.config/podsync-go/config.toml` or `./config.toml`).
- Loaded at startup; the app is the sole writer — UI-driven settings changes
  are applied in-memory then serialized back to the same file (atomic
  write-temp-then-rename to avoid corruption).
- Holds global settings **and** the feed list — config.toml is the
  one portable, authoritative declaration of what the library should
  contain (§4.3); a fresh instance loads entirely from this file. Per-
  episode library state (download status, manual flag) is
  deliberately *not* here — see §4.3. Shape (illustrative):

Deployment settings (the listen address, `base_url`, `data_dir`, the yt-dlp cookies file, the web
UI toggle and its login) are environment variables, not in this file (there are no command-line
flags). config.toml holds only feeds and what the web UI edits:

```toml
[download]
concurrency = 2              # global cap on simultaneous downloads
update_interval = "30m"      # how often every feed is checked
self_update = false          # run `yt-dlp -U` at startup and daily

[defaults]                    # template applied to newly-added feeds
format = "audio"              # "audio" | "video"
max_video_height = 720        # only applies when format = "video"
audio_quality = "high"        # "high" | "low"; only applies when format = "audio"
min_duration = "61s"          # skip videos shorter than this: 61s skips Shorts (60s or less); "0s" = none
cleanup_keep_last = 50        # 0 = unlimited, else keep N newest
page_size = 10                # newest N episodes considered per update — see §6.3

[[feeds]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
kind = "channel"              # "channel" | "playlist"
name = "Some Channel"
format = "audio"              # overrides [defaults]
max_video_height = 480
min_duration = "0s"
title_exclude = "(?i)#shorts|\\bcompilation\\b"   # title filters are per feed only (no global patterns)
cleanup_keep_last = 20
page_size = 25
```

Per-feed fields are optional overrides of `[defaults]`; unset fields
inherit the default.

## 6. Feature Requirements

### 6.1 Per-feed / global filters
- Audio vs video output mode.
- Max resolution cap (video mode).
- Minimum duration (`min_duration`, default 61s): videos shorter than this are skipped, which is how YouTube Shorts are excluded; `0s` disables it, and videos of unknown length are never skipped.
- Title filters, per feed only (there are no global patterns): two Go `regexp`
  patterns on the `[[feeds]]` entry. `title_include` — if set, only titles that match
  are downloaded. `title_exclude` — titles that match are skipped. Both apply (include
  first, then exclude). Patterns are validated on save.
- Cleanup policy: keep only the newest N downloaded episodes per feed (global default
  `cleanup_keep_last = 50`, overridable per feed, `0` = keep everything). Only the media
  file is removed; the episode record stays as "cleaned" so it is not downloaded again.
  There is no age-based rule. Episodes downloaded manually via Load More (`manual = true`
  in the sidecar) are exempt: otherwise an old episode the user explicitly asked for
  would be deleted on the next cycle for being outside the newest-N window.

### 6.2 RSS feeds
- One RSS document per feed at a stable URL, `/feeds/{format}/{feed_id}.xml` (`{format}` is the
  feed's own effective format, `audio` or `video` — a URL whose format segment doesn't match
  404s), iTunes-podcast-compatible tags, enclosure pointing at locally served media.
  Only downloaded episodes appear; not-yet-downloaded episodes shown in the
  UI (§7, Load More) are never included.

### 6.3 Discovery window (`page_size`)

Modeled on upstream podsync's `page_size`: a single standing knob that caps
how many of a feed's newest episodes a check considers by default. There is
no separate backfill mode.

- `page_size` (default `10`, overridable per feed). A feed's first-ever check
  lists exactly the newest `page_size` episodes (`ListEpisodes(feed, 0,
  page_size)`), applies the feed's filters, and downloads any that pass.
  Adding a feed therefore downloads at most the newest `page_size` matching
  episodes on that first check — the back-catalog beyond that is not fetched
  or even listed. (First-ever check = the library has no record — downloaded,
  cleaned or failed — for this feed at all yet.)
- Older episodes are also reachable on demand through the UI's Load More
  (§7), which lists further windows (`offset = episodes already shown`) and
  lets the user download individual ones.
- **Catch-up on later checks**: unlike upstream, a `page_size`-sized window
  that turns out to be *entirely* episodes with no library record at all is
  treated as a signal that a publish burst outran `page_size`, not as "that's
  just what's new." `listAndEnqueue` (package `syncer`) keeps fetching further
  pages (`offset += page_size`) until a page contains at least one episode it
  already has a record for (the boundary with what the previous check saw —
  a record survives even after the media is deleted, so this works even if
  the episode was since cleaned up), the feed runs out, or it has fetched
  `maxCatchUpPages` (10) extra pages, whichever comes first. This never
  triggers on a feed's first-ever check (nothing can be "known" yet — see
  above), and it only runs extra `ListEpisodes` calls when the fast path
  (single page, overlaps with what's known) wasn't enough.
- Remaining tradeoff: a feed bursty enough to publish more than
  `page_size * (maxCatchUpPages + 1)` episodes between two checks (or one
  down/paused long enough to accumulate that many) will still lose the
  oldest part of the burst to the scheduler — Load More is the manual
  recovery path; raising `page_size` or shortening `update_interval` is the
  preventive one.

## 7. Web UI Requirements

- **Library view**: feeds grouped by channel/playlist; clicking a feed opens
  its detail view with the episode list (downloaded episodes, newest first).
- **Load More (feed detail view)**: after scrolling to the end of a feed's
  episode list, a **Load More** button queries the source for the next
  window of older episodes that aren't downloaded.
  - Mechanics: htmx `GET /feed/{feed_id}/older?offset=N`, where `N` is the
    number of source episodes already seen; the handler calls
    `ListEpisodes(feed, N, page_size)`, drops any already downloaded, and
    returns HTML rows appended before the button (the button re-renders with
    the new offset). Offsets count raw source positions *before* filtering so
    pages stay stable. The feed's filters (minimum duration, title regex) apply to these
    results as they do to scheduled pulls: a filtered episode is dropped before
    it reaches the template, with no row, badge, or count marking its position.
    This matches scheduled pulls (which never download it either) and is
    deliberate, not an oversight — filtering is an explicit per-feed decision,
    not something to work around from the episode list.
  - Appearance: not-downloaded episodes render **faded** (reduced opacity),
    with title, date, and duration, plus a **Download** button.
  - Download: `POST /feed/{feed_id}/episodes/{episode_id}/download` enqueues
    the episode on the normal download queue (same concurrency cap, same
    format/resolution settings), writes its sidecar with `manual = true` on
    success, and the row un-fades in place (htmx polling on the row until the
    download completes or fails, with an error/retry state on failure).
  - These faded rows are ephemeral browse results: nothing is written to disk
    until an episode is downloaded, they're excluded from the RSS feed, and a page reload discards them.
- **Delete feed**: removes it (and, per a confirm step, optionally its
  downloaded media) from the library.
- **Status banner**: yt-dlp is the most fragile part of the system
  (YouTube changes break it, bot checks, rate limits, missing binary), so
  problems are surfaced as a banner on every page rather than only in logs:
  yt-dlp/ffmpeg missing, listing or download failures per feed, search
  failures, self-update failures, config save errors. Issues clear
  themselves when the failing operation next succeeds, and can be dismissed.
  The banner also shows live activity (pulling, N downloading, N queued).
- **Search bar**: filter episodes (and/or feeds) by title text.
- **Force pull button**: trigger an immediate check-for-new-episodes pass,
  globally or per-feed, with visible progress/status.
- **Settings page**: edits the operational knobs (simultaneous downloads, update
  interval, yt-dlp self-update) and the `[defaults]` for feeds, with an "Apply" action that
  validates and writes config.toml, then applies it to the running sync loop directly (an
  in-process call, not a network request). Everything that belongs to whoever runs the
  service — the public address (`base_url`), the listen address, the data directory, the
  cookies file, the yt-dlp binary to use and the web UI's login — is an environment variable
  (`-listen`/`-config` also accept a flag), not in config.toml at all, so the page doesn't show it
  and a forged form post has nothing to repoint. Per-feed overrides are edited on the feed's own
  settings page and written to that feed's entry in `[[feeds]]`.
- **Add feed**: one box (§8): a link or @handle resolves to that feed, anything else is a
  channel name search; one match goes straight to a preview, several are listed. Playlists
  are added by link only. Optionally override `page_size` (§6.3); the feed is appended to
  config.toml and the first pull fetches the newest `page_size` episodes.

## 8. Adding feeds: one lookup, guided by hints

The Add page has a single input. `Source.Find` decides what to do from hints in
what was typed, in this order:

1. **A link** (`http(s)://…` or a bare YouTube host): resolve it. Channel, playlist and
   single-video links all work (a video resolves to its channel).
2. **An `@handle`** (or a `UC…` channel ID): resolve it.
3. **Anything else**: search channels by that name.

Playlists are never searched for; a playlist is added by pasting its link, because a
name search cannot tell playlists apart reliably. A bare word is a name, not a handle:
`@` is the hint that makes it one.

- **Resolving** passes the reference to `yt-dlp --flat-playlist -J` and reads the
  canonical `channel_id` (so a handle is stored as a stable `UC…` ID, since handles can
  change) plus name, avatar, handle and subscriber count. No API key. A reference that
  does not resolve is reported to the user as invalid input and does not raise a status
  banner.
- **Searching** feeds YouTube's own search-results URL with the *channel* type filter
  (`https://www.youtube.com/results?search_query=<term>&sp=EgIQAg%253D%253D`) to
  yt-dlp, which returns channel entries (id, name, handle, subscriber count, avatar). The
  `sp` token is an undocumented YouTube parameter and the page is scraped through
  yt-dlp's extractor, so it takes seconds and can break when YouTube changes; failures
  go to the status banner, results are cached for a few minutes, and after a failure
  searches pause for a few seconds. Link and handle lookups keep working.
- **UI flow**: submit (Enter) → one match: preview straight away; several: a list, and
  choosing one looks it up again by ID for its full details → preview → Add. Nothing is
  looked up while typing, so half-typed handles don't hit YouTube.

## 9. Tech Stack (proposed)

- Language: Go.
- Media: `yt-dlp` (external binary) + `ffmpeg` (for yt-dlp's own post-processing).
- Config: TOML (`github.com/BurntSushi/toml` or `github.com/pelletier/go-toml`).
- Storage: no database — filesystem + JSON sidecar files per §4.3, with an
  in-memory index built at startup. Standard library `encoding/json`.
- HTTP: standard library `net/http` (Go 1.22 pattern routing) for the one listener (feeds, media and the web UI); the web UI calls the sync loop in-process, no JSON-over-HTTP boundary between them.
- Web UI: server-rendered Go `html/template` + **htmx** for partial updates
  (triggering a force-pull and polling its status, debounced live search,
  Load More, the status banner), no Alpine.js needed. No Node build step, no SPA
  framework — templates and any CSS/JS are embedded into the Go binary via
  `go:embed`, keeping the Docker image and cross-compile story simple (one
  static binary per platform, no separate frontend build stage).

## 10. Deployment

- **Dockerfile**: single multi-stage build, one final image: `debian:bookworm-slim` with `ffmpeg`, a pinned
  `yt-dlp` binary and `deno` (yt-dlp's default JavaScript runtime for YouTube), plus the `podsync` binary. Built
  once on the build host's native arch and cross-compiled (no CGO).
- **Multi-arch (amd64 + arm64)**: `docker buildx build --platform linux/amd64,linux/arm64 .`
- **Health checks** re-invoke the binary with `PODSYNC_HEALTHCHECK=1` (no flag — the program has none), which
  just probes `/healthz` on `PODSYNC_LISTEN_ADDR`, unauthenticated regardless of whether the web UI is enabled.
- **`compose.example.yaml`**: one service mounting `./config` (not the single file: the app rewrites
  config.toml and a single-file bind mount can't be replaced atomically) and `./data`, running as the user's
  uid, publishing one port, `8080` (feeds, media, and — behind its Basic login — the web UI).
