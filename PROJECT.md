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
- Apply per-feed filters: exclude Shorts, title regex include/exclude,
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

Two programs that share the config file and the data directory:

```
                      config.toml          data/ (media + JSON sidecars)
                       ▲   ▲                    ▲   ▲
                 writes│   │reads         reads/│   │writes
                       │   │            deletes │   │
 browsers, podcast ──► webui ── REST: apply, pull, status, ──► syncr ──► yt-dlp, ffmpeg, deno
 apps (:8080)          (views, RSS,     resolve, search,       (:8081)
                        media, edits)   listing                 scheduler, queue, cleanup
```

- **webui** (`cmd/webui`) is the htmx server. It reads and edits the config file, reads the library (and deletes
  feed folders), renders pages, and serves the RSS feeds and media files itself. It has no yt-dlp.
- **syncr** (`cmd/syncr`) runs yt-dlp: it lists feeds, downloads episodes, applies cleanup and writes the library.
  It only *reads* the config file. Its REST API is deliberately tiny (below).
- Each program has its own in-memory view of the shared files and refreshes it: webui re-reads the config file and
  rescans changed feed folders on every request (cheap when nothing changed); syncr does so on `apply` and at the
  start of every pull.
- The config file has one writer (webui); the library has one writer (syncr), with two small exceptions in webui:
  deleting feed folders and writing a new feed's `_meta.json`.

### 4.0 syncr's REST API

Only what needs the running service (it holds the queue and yt-dlp):

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/apply` | re-read config + rescan library after webui changed them |
| `POST /api/v1/pull` | pull all feeds / one feed / download one episode now |
| `GET /api/v1/status` | activity, issues, in-flight jobs, tool versions |
| `GET /api/v1/resolve`, `GET /api/v1/search`, `GET /api/v1/feeds/{id}/listing` | source lookups (need yt-dlp) |

### 4.0.1 Package layout

The root package `podsync` is the domain: types and interfaces only, no dependencies beyond the standard
library. Subpackages are grouped by what they depend on, and the `main` packages wire them together.

| Package | Depends on | Role |
|---|---|---|
| `podsync` (root) | stdlib | `Feed`/`Config`, `Episode`, `Source`, `Library`, `ConfigStore`, `StatusTracker`, the small `Syncer` contract, error kinds, `EpisodeFilter`, `Registry` |
| `youtube` | yt-dlp | implements `podsync.Source` |
| `fs` | filesystem | implements `podsync.Library` (+ atomic file writes, `Refresh` for multi-process use) |
| `toml` | BurntSushi/toml | implements `podsync.ConfigStore` (+ `Reload`) |
| `status` | — | implements `podsync.StatusTracker` in memory |
| `syncer` | root interfaces only | implements `podsync.Syncer`: the update loop, queue, cleanup (linked into syncr only) |
| `rest` | net/http | `Server` (syncr's API) and `Client` (implements `podsync.Syncer` over HTTP; used by webui) |
| `webui` | net/http, html/template | the htmx interface, feeds and media, plus its model over config + library |
| `rss` | encoding/xml | podcast feed rendering |
| `mock` | — | test doubles |
| `cmd/syncr`, `cmd/webui` | everything above | wiring only |

`podsync.Syncer` is the contract between the two programs: `syncer.Service` implements it inside syncr, and
`rest.Client` implements it for webui. The webui binary does not link `syncer` or `youtube`. Errors of kind
not-found / invalid / busy survive the HTTP boundary (`podsync.ErrNotFound`, `ErrInvalid`, `ErrBusy`).

### 4.1 Source adapter pattern

Core abstraction all sources implement (`podsync.Source`), so YouTube (package `youtube`) is just the first plugin:

```go
type Source interface {
    Kind() string                                   // stable identifier, e.g. "youtube"
    Check(ctx context.Context) Health               // are the external tools (yt-dlp, ffmpeg) usable?

    // Resolve turns a user-supplied URL, @handle or ID into a canonical feed.
    Resolve(ctx context.Context, ref string) (FeedInfo, error)

    // Search finds channels or playlists by name.
    Search(ctx context.Context, query string, kind SearchKind) ([]SearchResult, error)

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
Filtering (Shorts exclusion, title regex, resolution cap) is applied generically
in the puller using metadata every adapter is required to populate
(`EpisodeInfo.Duration`, `.IsShort`, `.Title`, available formats), so adapters
stay thin.

### 4.2 YouTube adapter

- Wraps `yt-dlp` as a subprocess (JSON output via `--dump-json` /
  `--flat-playlist` for listing, normal invocation for download), rather than
  reimplementing extraction. Avoids an official API key entirely.
- Playlist/channel listing uses `--flat-playlist --dump-single-json` with
  `--playlist-start`/`--playlist-end` derived from the `offset`/`limit` passed
  to `ListEpisodes`, so each call only touches the requested window; full
  metadata fetch happens lazily per-episode when first downloaded (duration,
  upload date, Shorts heuristic).
- Shorts detection: yt-dlp exposes `duration` and the episode URL path
  (`/shorts/...`); treat as Short if either the source URL used the `/shorts/`
  form or duration ≤ 60s, configurable.
- Resolution cap enforced via yt-dlp format selector
  (e.g. `bestvideo[height<=?1080]+bestaudio/best[height<=?1080]`); audio-only
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
  upload date, duration, Shorts flag, applied-filter outcome, download
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

```toml
[server]
listen_addr = ":8080"
base_url = "http://localhost:8080"   # absolute URL podcast apps use to reach this server (RSS enclosures)
data_dir = "./data"          # feeds' media + sidecar JSON live here

[download]
yt_dlp_path = "yt-dlp"
concurrency = 2              # global cap on simultaneous downloads
update_interval = "30m"      # how often every feed is checked
self_update = false          # run `yt-dlp -U` at startup and daily
cookies_file = ""            # optional Netscape cookies.txt passed to yt-dlp

[defaults]                    # template applied to newly-added feeds
format = "video"              # "video" | "audio"
max_resolution = 1080
exclude_shorts = true
title_include = ""            # regex; only matching titles are downloaded (empty = all)
title_exclude = ""            # regex; matching titles are skipped (empty = none)
cleanup_after_days = 0         # 0 = disabled
cleanup_keep_last = 50        # 0 = unlimited, else keep N newest
page_size = 10                # newest N episodes considered per update — see §6.3

[[feeds]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx"
kind = "channel"              # "channel" | "playlist"
name = "Some Channel"
format = "audio"              # overrides [defaults]
max_resolution = 720
exclude_shorts = true
title_exclude = "(?i)#shorts|\\bcompilation\\b"
cleanup_after_days = 60
cleanup_keep_last = 20
page_size = 25
```

Per-feed fields are optional overrides of `[defaults]`; unset fields
inherit the default.

## 6. Feature Requirements

### 6.1 Per-feed / global filters
- Audio vs video output mode.
- Max resolution cap (video mode).
- Exclude Shorts (heuristic in §4.2, overridable).
- Title filters: two Go `regexp` patterns. `title_include` — if set, only
  titles that match are downloaded. `title_exclude` — titles that match are
  skipped. Both apply (include first, then exclude). Patterns are validated
  on save.
- Cleanup policy: delete local media older than N days and/or beyond the N
  most recent per feed (global default `cleanup_keep_last = 50`, overridable
  per feed); RSS/episode metadata rows can persist or be removed based on a
  setting (keep history vs. fully forget). Episodes downloaded manually via
  Load More (`manual = true` in the sidecar) are exempt from both rules —
  otherwise an old episode the user explicitly asked for would be deleted on
  the next cycle for being outside the newest-N window.

### 6.2 RSS feeds
- One RSS document per feed at a stable URL (e.g. `/feeds/{feed_id}.xml`),
  iTunes-podcast-compatible tags, enclosure pointing at locally served media.
  Only downloaded episodes appear; not-yet-downloaded episodes shown in the
  UI (§7, Load More) are never included.

### 6.3 Discovery window (`page_size`)

Modeled on upstream podsync's `page_size`: a single standing knob that caps
how many of a feed's newest episodes are considered on **every** update
cycle, including the first one after the feed is added. There is no separate
backfill mode.

- `page_size` (default `10`, overridable per feed). Each pull lists the
  newest `page_size` episodes (`ListEpisodes(feed, 0, page_size)`), applies
  the feed's filters, and downloads any not already present. Adding a feed
  therefore downloads at most the newest `page_size` matching episodes — the
  back-catalog beyond that is not fetched or even listed.
- Older episodes are reachable on demand through the UI's Load More (§7),
  which lists further windows (`offset = episodes already shown`) and lets
  the user download individual ones.
- Known tradeoff (same as upstream): if the server is down or paused long
  enough for a feed to publish more than `page_size` episodes, the overflow
  is skipped by the scheduler. Load More is the recovery path; raising
  `page_size` for fast-publishing feeds is the preventive one.

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
    pages stay stable. The feed's filters (Shorts, title regex) apply to these
    results as they do to scheduled pulls; whether to show filtered-out ones
    is an open question (§11).
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
- **Settings page**: form view over config.toml's `[server]`, `[download]`,
  and `[defaults]` sections, with an "Apply" action that validates and writes
  config.toml back, then reloads affected state (e.g. rescheduling the
  puller interval) without a full restart where feasible. Per-feed
  overrides are a separate, lighter-weight edit surface inline in the
  library view — editing writes back to that feed's entry in
  config.toml's `[[feeds]]` array (same atomic-write path), not a
  sidecar.
- **Add feed**: paste a channel/playlist URL (and, per §8, optionally
  a bare @handle, or a free-text search) to resolve it, optionally override
  `page_size` (§6.3), and append it to config.toml; the first pull then
  fetches the newest `page_size` episodes.

## 8. Channel discovery: handles and search

### 8.1 Handle / URL resolution (v1, direct add)

- **Why it's needed**: users think in `@handle` terms; internally we want the
  stable `UC...` channel ID (handles can be reassigned/changed; IDs don't).
- **Mechanism**: pass `https://www.youtube.com/@handle` (or a channel/playlist
  URL) straight to `yt-dlp --dump-single-json`; the response includes the
  resolved `channel_id`. No API key, no extra dependency — this is the
  adapter's `Resolve` method handling a direct reference.

### 8.2 Free-text channel search (v1, in scope)

Users can search by name (not just paste a handle/URL) and pick from results.
No YouTube Data API key required. Verified against real yt-dlp output: feeding
YouTube's own search-results URL with the *channel* type filter
(`https://www.youtube.com/results?search_query=<term>&sp=EgIQAg%253D%253D`) to
`yt-dlp --flat-playlist -J` returns real channel entries (id, name, `@handle`,
subscriber count, avatar) — far better than deriving channels from `ytsearch:`
video results. The playlist filter is `sp=EgIQAw%253D%253D`.

- UI flow: search box in "Add feed" → debounced query → results list with
  avatar, name, handle, subscriber count → click to resolve (§8.1) and
  preview before adding.
- Caveats: this scrapes YouTube's search page through yt-dlp's extractor, so
  it takes seconds and can break when YouTube changes markup — best-effort.
  Results are cached briefly per query, failures are reported to the status
  banner, and after a failure further searches are paused for a few seconds
  rather than hammering retries.
- The `sp` filter tokens are undocumented YouTube parameters; if they stop
  working the fallback is `ytsearch:` video results deduplicated by channel.

## 9. Tech Stack (proposed)

- Language: Go.
- Media: `yt-dlp` (external binary) + `ffmpeg` (for yt-dlp's own post-processing).
- Config: TOML (`github.com/BurntSushi/toml` or `github.com/pelletier/go-toml`).
- Storage: no database — filesystem + JSON sidecar files per §4.3, with an
  in-memory index built at startup. Standard library `encoding/json`.
- HTTP: standard library `net/http` (Go 1.22 pattern routing) for both the REST API and the web UI; JSON over HTTP between the two programs.
- Web UI: server-rendered Go `html/template` + **htmx** for partial updates
  (triggering a force-pull and polling its status, debounced live search,
  Load More, the status banner), no Alpine.js needed. No Node build step, no SPA
  framework — templates and any CSS/JS are embedded into the Go binary via
  `go:embed`, keeping the Docker image and cross-compile story simple (one
  static binary per platform, no separate frontend build stage).

## 10. Deployment

- **Dockerfile**: multi-stage with two final targets. `webui` is a distroless static image holding just the
  binary. `syncr` is `debian:bookworm-slim` with `ffmpeg`, a pinned `yt-dlp` binary and `deno` (yt-dlp's default
  JavaScript runtime for YouTube). Both binaries are built once on the build host's native arch and
  cross-compiled (no CGO).
- **Multi-arch (amd64 + arm64)**: `docker buildx build --platform linux/amd64,linux/arm64 --target <syncr|webui> .`
- **Health checks** use the programs' own `-healthcheck` flag (no curl in the web UI image), so they follow
  whatever port is configured.
- **`compose.example.yaml`**: two services mounting the same `./config` directory (not the single file: the app
  rewrites config.toml and a single-file bind mount can't be replaced atomically) and `./data`, both running as
  the user's uid. `syncr` publishes nothing; `webui` publishes `8080` and reaches syncr at `http://syncr:8081`.

## 11. Open Questions

- Load More and feed filters: hide episodes the feed's filters would skip
  (current default), or show them faded with a "filtered" badge so the user
  can still download one deliberately?
