# podsync-go

Turn YouTube channels and playlists into podcast feeds you can play in any podcast app, and manage them from a web page.

You pick a channel or playlist, podsync-go downloads new episodes with [yt-dlp](https://github.com/yt-dlp/yt-dlp), stores them on your disk, and publishes a normal podcast RSS feed for each one. Add the feed URL to your podcast app and new videos show up as episodes (as video or audio-only, your choice).

It is inspired by [mxpv/podsync](https://github.com/mxpv/podsync); the big difference is the built-in web UI for browsing and managing your library.

- Web UI: library by channel/playlist, search, "check now", delete feeds, settings, load older episodes on demand
- Add feeds with one box: paste a channel or playlist link, type an `@handle`, or search YouTube channels by name
- Per-feed options: audio or video, max resolution (default 720p), minimum video length (skips Shorts), title include/exclude regex, keep-newest-N cleanup, opt out of the OPML export
- OPML export of all subscriptions at `/opml.xml`, for importing your whole library into a podcast app at once
- A status banner that tells you when yt-dlp, YouTube or a download is misbehaving
- One program, one port: **podsync** fetches and stores episodes with yt-dlp, **publishes the podcast feeds and media**, and serves the management web UI, all on the same listener. Disable the web UI with an environment variable if you only want the feeds. A multi-arch (amd64 + arm64) Docker image
- Plain-file storage: no database, just media files with small JSON files next to them, and one `config.toml`

> **The web UI needs a login; the feeds don't.** The feed/media endpoints (what podcast apps fetch) are public to anyone who can reach the port and knows a feed URL. The management web UI (which can change settings and delete feeds) requires the HTTP Basic username and password set by `PODSYNC_USERNAME`/`PODSYNC_PASSWORD`. Basic auth sends credentials unencrypted, so still put this behind HTTPS (a reverse proxy) or a trusted network if you can; set `PODSYNC_WEBUI_DISABLED=true` if you don't want the web UI served at all.

## Quick start (Docker)

1. Get the code, create the folders and copy the example compose file:

   ```sh
   git clone <this repository> podsync-go && cd podsync-go
   mkdir -p config data
   cp compose.example.yaml compose.yaml
   ```

   The example builds one image from the repository. To build it yourself for both architectures:

   ```sh
   docker buildx build --platform linux/amd64,linux/arm64 -t podsync .
   ```

2. **Edit `compose.yaml`**: set `PODSYNC_BASE_URL` to the address podcast apps (and your browser) will use to reach port 8080, for example `http://192.168.1.10:8080` (the placeholder `<this-host>` only works on the same machine, so feed links would be useless on your phone until you change it; your browser also loads episode thumbnails from this address), and set `PODSYNC_USERNAME`/`PODSYNC_PASSWORD` to your own login for the management UI.

3. Start it:

   ```sh
   docker compose up -d
   ```

4. Open the management UI at <http://localhost:8080>, and log in with `PODSYNC_USERNAME`/`PODSYNC_PASSWORD`. On first start a default `config/config.toml` is created, holding only feeds and the settings the web UI's Settings page edits.

5. Click **Add feed**, type or paste a channel name, an `@handle` or a link, and add it. The newest episodes start downloading right away.

6. Open the feed in the UI and click its feed URL to copy it (`http://<your-address>:8080/feeds/<id>.xml`) into your podcast app ("add a show by URL"); feed and media requests need no login.

One port, **8080**, serves feeds and media (publish this to podcast apps) and the management UI (behind its Basic login — keep this reachable only from where you'll actually use it). The container mounts `config` and `data` and runs as the user in `compose.yaml` (`1000:1000` in the example); create the folders as that user so it can write to them. Set `PODSYNC_WEBUI_DISABLED=true` to turn the web UI off entirely (then `PODSYNC_USERNAME`/`PODSYNC_PASSWORD` are not needed) — useful if you only maintain `config.toml` by hand.

Everything about how the service is deployed — the listen address, the public address, the data directory, the yt-dlp cookies file, the web UI toggle and its login — is set with environment variables in `compose.yaml`, not in `config.toml`. `config.toml` holds only feeds and the settings the web UI's Settings page edits (download concurrency, update interval, self-update, and the per-feed-overridable defaults); it has nothing a deployment needs to keep secret or machine-specific, so it's safe to share or back up on its own. yt-dlp itself is not configurable: the image runs the copy baked in at build time.

## Using the web UI

**Library** (home page): one card per feed with episode count, size and when it was last checked; the refresh icon in a card's corner checks just that feed. The search box finds feeds by name and downloaded episodes by title. The refresh icon next to the search box (**Check all feeds now**) starts an update immediately instead of waiting for the next scheduled one.

**Add feed**: one box does the lookup, guided by what you typed:

- a **link** (a channel, a playlist, or a single video, whose channel is used) or an **`@handle`** (or a `UC…` channel ID) goes straight to that feed;
- **anything else** is searched as a channel name, and the matches are listed for you to pick (a single match goes straight to the preview).

Playlists are not searched for by name: paste the playlist's link. You get a preview before anything is added, and can set how many episodes to fetch per update (the default comes from Settings). Press Enter or the magnifying-glass button; nothing is looked up while you type.

**Feed page**: all episodes known for the feed, with a filter box. Newest downloads are at the top. Each row shows the episode's thumbnail, publish date, length, views, resolution and size; **Details** expands the description and a link to the video on YouTube.

- **Load more** (at the bottom) looks further back in the channel and lists older episodes that were not downloaded. They appear faded. Click **Download** on any of them to fetch it. Episodes you download this way are marked *kept* and are never removed by automatic cleanup.
- Episodes removed by cleanup or by you, or that failed to download, also appear faded with **Download again** / **Retry**.
- To free disk space, press the checkmark icon next to the filter box to select episodes: tick the ones you want (or **Select all**) and press the trash icon in the bar. Only the media files are deleted; each episode is remembered as "removed", so it is not downloaded again, and you can fetch it later with **Download again**.
- The icons at the top right: refresh updates just this feed, the gear opens the feed settings (overrides for this feed, and the only place for its title include/exclude patterns), and the trash can deletes it. Click or tap the podcast feed URL to copy it.

**Deleting a feed** removes it from your configuration (its podcast URL stops working). Downloaded files are kept unless you tick the box; kept folders show up under *Not in config* on the library page with a button to delete them.

**Settings**: simultaneous downloads, how often to check, whether to keep yt-dlp up to date, and the defaults for every feed (format, maximum resolution, minimum video length, page size, how many episodes to keep). **Save and apply** validates the values and applies them immediately. The things that belong to whoever runs the service (the public address, the listen address, the data directory, the cookies file and the web UI login) are not shown there; they're environment variables in `compose.yaml` (see Configuration below). The page also links to `/opml.xml`, an OPML export of every feed that hasn't opted out (each feed's settings page has that toggle).

**Status banner**: yt-dlp talks to YouTube, which changes things often and sometimes blocks requests. When something goes wrong (yt-dlp missing, a feed can't be listed, a download fails, search fails, self-update fails) a banner explains it on every page. It clears itself when the problem goes away, and you can dismiss it. While work is running the banner also shows progress.

## How updates work

- A few seconds after start, and then every **Check feeds every** interval (default 30 minutes), each feed is checked.
- Only the **newest `page_size` episodes** of a feed are considered per check (default 10). Adding a feed therefore downloads at most that many, not the whole back catalogue — the very first check for a feed never looks further back than that. Use *Load more* for older ones.
- If a feed publishes **more than `page_size`** episodes between two checks (after the first), podsync notices and automatically looks further back: it keeps paging until it finds an episode it already has a record for (even a since-deleted one — the record survives), so nothing in a publish burst is silently skipped. This catch-up is capped at 10 extra pages per feed per check; if a feed is bursty enough to still outrun that, raise its `page_size` or shorten `update_interval`.
- An episode is downloaded if it is new and passes the feed's filters: videos shorter than `min_duration` (default 61 seconds, so YouTube Shorts of 60 seconds or less are skipped) are skipped, the feed's `title_include` (if set) must match, and its `title_exclude` must not match. Live streams and not-yet-premiered videos are skipped until they are normal videos.
- Failed downloads are retried on the next checks, up to 3 attempts, then wait for you to press **Retry**.
- **Cleanup** runs after each update. `cleanup_keep_last` keeps only the newest N downloaded episodes of a feed (`0` keeps everything; there is no age-based cleanup). Only the media file is removed: the episode is remembered as "removed", so it will not be downloaded again. Episodes you downloaded manually are never cleaned up.
- Video downloads are mp4 (up to `max_resolution`); audio-only downloads are m4a.
- Each episode's thumbnail is saved next to it as a jpg (served by the app and used as the episode image in the podcast feed), together with its title, description, publish date, duration, channel, view count and resolution. Cleanup removes only the media; the thumbnail and details are kept.

## Configuration

podsync-go is deployed with Docker, and how it's deployed — ports, the public address, the data directory, the yt-dlp cookies file — is set with environment variables (`environment:` in `compose.yaml`), not a config file. `config.toml` holds only feeds and the settings the web UI's Settings page edits: nothing in it is machine-specific, so it's safe to share, diff or back up on its own, and it never needs a restart to take effect.

The settings live in one TOML file (default `config.toml` in the working directory, `/config/config.toml` in Docker; override with the `PODSYNC_CONFIG` environment variable). If it doesn't exist it is created with defaults. See [`config.example.toml`](config.example.toml) for a fully commented example.

The web UI rewrites this file when you save changes, so hand-written comments are lost; keep annotated copies elsewhere. Unknown keys are rejected so typos don't go unnoticed. If you edit the file by hand while podsync is running, it's picked up on the web UI's next page load, and applied by the sync loop at its next update at the latest. A file that no longer parses is ignored and reported in the banner; the last good settings stay in use.

| Section / key | Default | Meaning |
|---|---|---|
| `download.concurrency` | `2` | Simultaneous downloads (1-16) |
| `download.update_interval` | `30m` | How often feeds are checked (minimum `1m`) |
| `download.self_update` | `false` | Run `yt-dlp -U` at startup and daily |
| `defaults.format` | `audio` | `audio` or `video` |
| `defaults.max_resolution` | `720` | Max video height in pixels; `0` = no limit |
| `defaults.min_duration` | `"61s"` | Skip videos shorter than this; the default skips YouTube Shorts (60 seconds or less). `"0s"` downloads everything. Videos whose length is unknown are never skipped |
| `defaults.page_size` | `10` | Newest N episodes considered per update |
| `defaults.cleanup_keep_last` | `50` | Keep newest N downloaded episodes; `0` = all |
| `[[feeds]]` | | One block per feed: `id`, `kind` (`channel` or `playlist`), `name`, plus optional overrides of any `defaults` key |
| `feeds.title_include` | empty | Per feed only: regex; if set, only matching titles are downloaded |
| `feeds.title_exclude` | empty | Per feed only: regex; matching titles are skipped, e.g. `(?i)sponsored\|#shorts` |
| `feeds.opml_exclude` | `false` | Per feed only: leave this feed out of the `/opml.xml` export |

Title filters exist on individual feeds only; there are no global patterns. Regular expressions use Go's [RE2 syntax](https://pkg.go.dev/regexp/syntax); prefix with `(?i)` for case-insensitive matching.

`config.toml` is all you need to recreate your list of feeds on a new machine: start with an empty data directory and the feeds are fetched again.

### Deployment settings (environment variables, plus two flags)

Everything below is set once per deployment and needs a restart to change; none of it is in `config.toml` or editable in the web UI. Almost all of it is an environment variable only. podsync fails to start with a message naming whatever's missing or invalid, rather than falling back to a guess.

| Environment | Flag | Default | Meaning |
|---|---|---|---|
| `PODSYNC_CONFIG` | `-config` | `config.toml` | The TOML file holding feeds and defaults |
| `PODSYNC_LISTEN_ADDR` | `-listen` | `:8080` | Where podsync serves feeds, media and (unless disabled) the web UI — one listener for all of it |
| `PODSYNC_BASE_URL` | — | **required** | Public address of `PODSYNC_LISTEN_ADDR` as podcast apps and your browser reach it; used in feed links, enclosure URLs and to load thumbnails. There is no default — podsync refuses to start without it, since a wrong guess would silently break feed links |
| `PODSYNC_DATA_DIR` | — | `./data` | Where media and metadata are stored (`/data` in the Docker image) |
| `PODSYNC_COOKIES_FILE` | — | empty | Optional Netscape-format cookies.txt for YouTube sign-in checks. Checked at startup: podsync refuses to start if the file doesn't exist |
| `PODSYNC_YTDLP_PATH` | — | `yt-dlp` on `PATH` | Run a yt-dlp other than the one on `PATH` — a bare name resolved on `PATH` or an absolute/relative path. If you set this, it's checked at startup with the same logic as running it (podsync refuses to start if it doesn't resolve); leave it unset and a missing default `yt-dlp` is just a status-banner issue, not a startup failure |
| `PODSYNC_WEBUI_DISABLED` | — | `false` | Serve only feeds and media; don't serve the management web UI at all |
| `PODSYNC_USERNAME` | — | empty | Username for the web UI's HTTP Basic login. Required unless `PODSYNC_WEBUI_DISABLED` |
| `PODSYNC_PASSWORD` | — | empty | Password for the web UI's HTTP Basic login. Required unless `PODSYNC_WEBUI_DISABLED` |

`-listen` and `-config` are the only command-line flags (useful for running a few instances by hand without Docker or compose); when given, a flag wins over its environment variable. Everything else is environment-only — there was deliberately no flag added for it, to keep one way to set each thing.

The feed and media endpoints (`/feeds/<id>.xml`, `/media/<id>/<file>`, `/opml.xml`, `/healthz`) are always unauthenticated — that's what podcast apps and health checks fetch. Everything else is the management portal, mounted at `/` behind the HTTP Basic credentials above; set `PODSYNC_WEBUI_DISABLED=true` if you don't want it served at all (then no credentials are required and none are checked).

The container health check runs podsync again with `PODSYNC_HEALTHCHECK=1` set, which just probes the running instance's `/healthz` and exits 0/1.

## Where things are stored

```
data/
  youtube/<channel or playlist id>/
    _meta.json            cached name, image, last check time
    <video id>.mp4        the media (or .m4a for audio feeds)
    <video id>.jpg        the episode thumbnail
    <video id>.json       episode details (title, description, date, duration, channel, views, resolution) and download status
```

It is all plain files, so backing up is just copying the folder. Deleting a media file by hand is safe: the episode is then treated as removed and not re-downloaded. Partial downloads are staged in `data/.tmp` and cleaned up on startup.

## Running without Docker

podsync-go is meant to be deployed with Docker (see the quick start above); this is also fine for development, testing, or running directly on a machine without containers. You need Go 1.27+, [yt-dlp](https://github.com/yt-dlp/yt-dlp), `ffmpeg`, and a JavaScript runtime for yt-dlp (`deno` is its default). By default yt-dlp is run as `yt-dlp` on your `PATH`; set `PODSYNC_YTDLP_PATH` to use a specific binary instead.

```sh
go build -o podsync ./cmd/podsync
PODSYNC_BASE_URL=http://192.168.1.10:8080 PODSYNC_USERNAME=admin PODSYNC_PASSWORD=change-me \
  ./podsync -listen :8080 -config config.toml
```

For a quick look at the UI with a real channel, run `dev/run.sh`: it builds and starts podsync with a ready-made test feed (everything on <http://localhost:8080>; media goes to `dev/data`).

## Troubleshooting

Check the banner at the top of the page first; it quotes yt-dlp's error.

- **"Sign in to confirm you're not a bot"**: YouTube wants a logged-in session. Export cookies from a browser where you're signed in to a `cookies.txt` file (Netscape format), put it somewhere the app can read (in Docker, in the `config` folder), and set `PODSYNC_COOKIES_FILE` (for example `/config/cookies.txt`) in `compose.yaml`, then restart it. Use a throwaway account if you can; the cookies grant access to that account.
- **"Unable to extract..." / downloads suddenly fail everywhere**: yt-dlp is out of date. Tick *Keep yt-dlp up to date* in Settings, or pull a newer image / update the binary. (Self-update only works if the yt-dlp binary is writable by the app, as in the Docker image. It cannot update a yt-dlp installed by a package manager.)
- **HTTP 429 / rate limited**: YouTube is throttling you. It retries on the next update; lower *Simultaneous downloads* and raise *Check feeds every* if it keeps happening.
- **401 Unauthorized opening the web UI**: check `PODSYNC_USERNAME`/`PODSYNC_PASSWORD` in `compose.yaml` match what you're typing. Feed and media URLs never need a login, so podcast apps are unaffected.
- **Feed links don't work on my phone**, or thumbnails are missing in the web UI: set `PODSYNC_BASE_URL` in `compose.yaml` to the machine's LAN address and port (for example `http://192.168.1.10:8080`) and restart.
- **"Permission denied" when saving settings or downloading in Docker**: the `config` and `data` folders must be writable by the user the container runs as (the web UI saves the config and deletes feed folders; podsync writes the media).
- **Settings don't seem to save when `config.toml` is bind-mounted as a single file**: mount the folder instead (see the compose example).
- **An episode shows as failed**: the banner and the episode row show yt-dlp's reason (private, members-only and region-blocked videos can't be downloaded). Press **Retry** to try again.
- **Search returns nothing or errors**: name search scrapes YouTube's search page through yt-dlp and can break when YouTube changes it. Pasting a link or typing an `@handle` goes straight to the feed and keeps working.

## Development

```sh
go vet ./... && go test -race ./...                              # unit and HTTP tests (no network)
PODSYNC_LIVE=1 go test ./youtube -run Live -v                    # talks to real YouTube via your yt-dlp
cd e2e && npm install && ./run.sh                                # browser test: podsync + fake yt-dlp + headless Chromium
dev/run.sh                                                       # run everything locally with a real test feed
```

Layout: the root package `podsync` holds the domain types and interfaces (feeds, episodes, the `Source`, `Library` and `ConfigStore` interfaces, and the small `Syncer` contract). Subpackages are grouped by what they depend on: `youtube` (yt-dlp), `fs` (the on-disk library), `toml` (the config file), `publish` (the public feeds and media), `rss`, `status`, and `syncer` (the sync loop's logic, which implements `podsync.Syncer` and depends only on the root interfaces); `mock` has test doubles. `cmd/podsync` is the single binary: it wires the sync loop, the feed/media publisher and the web UI (templates, handlers, and a model over the config and the library) together behind one listener, calling the sync loop's methods directly in-process (no network hop) and gating the web UI behind HTTP Basic auth. To add another source (Vimeo, ...), implement `podsync.Source` in a new package and register it in `cmd/podsync`. See [`PROJECT.md`](PROJECT.md) for the design.

The UI uses a vendored copy of [htmx](https://htmx.org) (0BSD license).
