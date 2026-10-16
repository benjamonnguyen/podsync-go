# podsync-go

Turn YouTube channels and playlists into podcast feeds you can play in any podcast app, and manage them from a web page.

You pick a channel or playlist, podsync-go downloads new episodes with [yt-dlp](https://github.com/yt-dlp/yt-dlp), stores them on your disk, and publishes a normal podcast RSS feed for each one. Add the feed URL to your podcast app and new videos show up as episodes (as video or audio-only, your choice).

It is inspired by [mxpv/podsync](https://github.com/mxpv/podsync); the big difference is the built-in web UI for browsing and managing your library.

- Web UI: library by channel/playlist, search, "check now", delete feeds, settings, load older episodes on demand
- Add feeds by pasting a link, `@handle` or ID, or by searching YouTube by name
- Per-feed options: audio or video, max resolution, skip Shorts, title include/exclude regex, automatic cleanup
- A status banner that tells you when yt-dlp, YouTube or a download is misbehaving
- Two small programs: **syncr** (the sync service: fetches and stores episodes with yt-dlp) and **webui** (the web interface, which also serves the podcast feeds). They share the config file and the data directory, and syncr has a tiny REST API for the few things that need the running service. Multi-arch (amd64 + arm64) Docker images for both
- Plain-file storage: no database, just media files with small JSON files next to them, and one `config.toml`

> **No login.** podsync-go has no authentication. Run it on a trusted home network, or put it behind a reverse proxy that does authentication. Anyone who can open the web UI can change settings, delete feeds and download your media. syncr's API port can trigger updates and lookups, so keep it off untrusted networks; the compose example publishes only the web UI.

## Quick start (Docker)

1. Get the code, create the folders and copy the example compose file:

   ```sh
   git clone <this repository> podsync-go && cd podsync-go
   mkdir -p config data
   cp compose.example.yaml compose.yaml
   ```

   The example builds two images from the repository: `syncr` (the sync service) and `webui`. To build them yourself for both architectures:

   ```sh
   docker buildx build --platform linux/amd64,linux/arm64 --target syncr -t podsync-syncr .
   docker buildx build --platform linux/amd64,linux/arm64 --target webui  -t podsync-webui .
   ```

2. Start it:

   ```sh
   docker compose up -d
   ```

3. Open <http://localhost:8080>. On first start a default `config/config.toml` is created.

4. **Open Settings and set "Public address of this server"** to the address your podcast app will use to reach the web UI, for example `http://192.168.1.10:8080`. The default `http://localhost:8080` only works on the same machine, so feed links would be useless on your phone until you change it.

5. Click **Add feed**, paste a channel link (or an `@handle`, or search by name), and add it. The newest episodes start downloading right away.

6. Open the feed in the app and copy its **Podcast feed URL** (`http://<your-address>/feeds/<id>.xml`) into your podcast app ("add a show by URL").

Both containers mount the same `config` and `data` folders and run as the user in `compose.yaml` (`1000:1000` in the example). Create the folders as that user so both programs can write to them. If the web UI shows "Cannot reach the sync service", syncr isn't running or `SYNCR_URL` is wrong; you can still browse and listen meanwhile, but nothing new is downloaded.

## Using the web UI

**Library** (home page): one card per feed with episode count, size and when it was last checked. The search box finds feeds by name and downloaded episodes by title. **Check all feeds now** starts an update immediately instead of waiting for the next scheduled one.

**Add feed**: paste a channel URL, `@handle`, channel ID (`UC...`), playlist URL or even a single video URL (its channel is used). Or search by name and pick a result. You get a preview before anything is added. You can set how many episodes to fetch per update; the default comes from Settings.

**Feed page**: all episodes known for the feed, with a filter box. Newest downloads are at the top. Each row shows the episode's thumbnail, publish date, length, views, resolution and size; **Details** expands the description and a link to the video on YouTube.

- **Load more** (at the bottom) looks further back in the channel and lists older episodes that were not downloaded. They appear faded. Click **Download** on any of them to fetch it. Episodes you download this way are marked *kept* and are never removed by automatic cleanup.
- Episodes removed by cleanup, or that failed to download, also appear faded with **Download again** / **Retry**.
- **Check now** updates just this feed. **Feed settings** overrides the defaults for this feed. **Delete** removes it.

**Deleting a feed** removes it from your configuration (its podcast URL stops working). Downloaded files are kept unless you tick the box; kept folders show up under *Not in config* on the library page with a button to delete them.

**Settings**: server address, yt-dlp path, simultaneous downloads, how often to check, and the defaults for every feed. **Save and apply** validates the values and applies them immediately, except the listen address and data directory, which need a restart.

**Status banner**: yt-dlp talks to YouTube, which changes things often and sometimes blocks requests. When something goes wrong (yt-dlp missing, a feed can't be listed, a download fails, search fails, self-update fails) a banner explains it on every page. It clears itself when the problem goes away, and you can dismiss it. While work is running the banner also shows progress.

## How updates work

- A few seconds after start, and then every **Check feeds every** interval (default 30 minutes), each feed is checked.
- Only the **newest `page_size` episodes** of a feed are considered per check (default 10). Adding a feed therefore downloads at most that many, not the whole back catalogue. Use *Load more* for older ones. If a feed publishes more than `page_size` videos between checks, raise its `page_size`.
- An episode is downloaded if it is new and passes the feed's filters: Shorts are skipped (videos of 60 seconds or less, when enabled), `title_include` (if set) must match, and `title_exclude` must not match. Live streams and not-yet-premiered videos are skipped until they are normal videos.
- Failed downloads are retried on the next checks, up to 3 attempts, then wait for you to press **Retry**.
- **Cleanup** runs after each update. `cleanup_keep_last` keeps only the newest N downloaded episodes of a feed; `cleanup_after_days` also removes episodes published more than N days ago. Only the media file is removed: the episode is remembered as "removed by cleanup", so it will not be downloaded again. Episodes you downloaded manually are never cleaned up.
- Video downloads are mp4 (up to `max_resolution`); audio-only downloads are m4a.
- Each episode's thumbnail is saved next to it as a jpg (served by the app and used as the episode image in the podcast feed), together with its title, description, publish date, duration, channel, view count and resolution. Cleanup removes only the media; the thumbnail and details are kept.

## Configuration

Everything can be changed in the web UI. The settings live in one TOML file (syncr's config: default `config.toml` in the working directory, `/config/config.toml` in Docker; override with `-config` or the `PODSYNC_CONFIG` environment variable). If it doesn't exist it is created with defaults. See [`config.example.toml`](config.example.toml) for a fully commented example.

The web UI rewrites this file when you save changes, so hand-written comments are lost; keep annotated copies elsewhere. Unknown keys are rejected so typos don't go unnoticed. If you edit the file by hand while the programs run, the web UI notices on its next page load and syncr applies the change at its next update at the latest (or immediately with `curl -X POST localhost:8081/api/v1/apply`). A file that no longer parses is ignored and reported in the banner; the last good settings stay in use.

| Section / key | Default | Meaning |
|---|---|---|
| `server.listen_addr` | `:8081` | Address and port syncr's API listens on (edit the file, restart) |
| `server.base_url` | `http://localhost:8080` | Address podcast apps use to reach the web UI; used in feed links |
| `server.data_dir` | `./data` | Where media and metadata are stored; both programs use it (edit the file, restart) |
| `download.yt_dlp_path` | `yt-dlp` | yt-dlp executable |
| `download.concurrency` | `2` | Simultaneous downloads (1-16) |
| `download.update_interval` | `30m` | How often feeds are checked (minimum `1m`) |
| `download.self_update` | `false` | Run `yt-dlp -U` at startup and daily |
| `download.cookies_file` | empty | Netscape-format cookies.txt for YouTube sign-in checks |
| `defaults.format` | `video` | `video` or `audio` |
| `defaults.max_resolution` | `1080` | Max video height in pixels; `0` = no limit |
| `defaults.exclude_shorts` | `true` | Skip videos of 60 seconds or less |
| `defaults.title_include` | empty | Regex; if set, only matching titles are downloaded |
| `defaults.title_exclude` | empty | Regex; matching titles are skipped |
| `defaults.page_size` | `10` | Newest N episodes considered per update |
| `defaults.cleanup_keep_last` | `50` | Keep newest N downloaded episodes; `0` = all |
| `defaults.cleanup_after_days` | `0` | Remove episodes published more than N days ago; `0` = never |
| `[[feeds]]` | | One block per feed: `id`, `kind` (`channel` or `playlist`), `name`, plus optional overrides of any `defaults` key |

### The two programs

`syncr` takes `-config` (or `PODSYNC_CONFIG`). `webui` takes the **same** config file plus two more options (flags, with environment variable equivalents):

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-config` | `PODSYNC_CONFIG` | `config.toml` | The config file shared with syncr (also gives webui the data directory) |
| `-listen` | `WEBUI_LISTEN` | `:8080` | Address to serve the web UI on |
| `-syncr` | `SYNCR_URL` | `http://127.0.0.1:8081` | Where syncr's API is (`server.listen_addr` in the config) |

The web UI serves the podcast feeds (`/feeds/<id>.xml`) and media files (`/media/...`) itself, straight from the data directory, so `server.base_url` is the web UI's address. Both programs accept `-version` and `-healthcheck` (exit status 0 if the running instance answers), used by the container health checks.

Both programs must see the same config file and the same data directory. If `data_dir` is a relative path (like the default `./data`) they resolve it from their own working directory, so start them from the same directory or use an absolute path. The settings page edits everything except `server.listen_addr` and `server.data_dir`, which both programs depend on; change those in the file and restart both.

Regular expressions use Go's [RE2 syntax](https://pkg.go.dev/regexp/syntax); prefix with `(?i)` for case-insensitive matching.

`config.toml` is all you need to recreate your list of feeds on a new machine: start with an empty data directory and the feeds are fetched again.

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

You need Go 1.27+, [yt-dlp](https://github.com/yt-dlp/yt-dlp), `ffmpeg`, and a JavaScript runtime for yt-dlp (`deno` is its default) on your `PATH`. The web UI needs none of those.

```sh
go build -o syncr ./cmd/syncr
go build -o webui ./cmd/webui
./syncr -config config.toml &                                              # sync service, API on :8081
./webui -config config.toml -listen :8080 -syncr http://127.0.0.1:8081     # web UI on :8080
```

Start both from the same directory (or use an absolute `data_dir`). For a quick look at the UI with a real channel, run `dev/run.sh`: it builds and starts both programs with a ready-made test feed on <http://localhost:8080> (media goes to `dev/data`).

## syncr's REST API

Everything the web UI can do with the config and the library it does directly on the shared files. syncr's API covers only what needs the running service (it runs yt-dlp and holds the download queue):

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/apply` | Re-read `config.toml` and rescan the library after the web UI changed them. Replies `{"changed":…,"restart_required":…}` |
| `POST /api/v1/pull` | Start an update of every feed, or `{"feed":"UC…"}` for one, or `{"feed":"UC…","episode":{"id":"…"}}` to download one episode now. `409` if an update is already running |
| `GET /api/v1/status` | Activity, current problems, in-flight downloads, tool versions |
| `GET /api/v1/resolve?ref=` | Turn a URL, `@handle` or ID into a feed description |
| `GET /api/v1/search?q=&kind=channel\|playlist` | Search YouTube by name |
| `GET /api/v1/feeds/{id}/listing?offset=&limit=` | A window of a feed's episodes straight from YouTube (nothing is stored) |
| `GET /healthz` | Liveness |

Errors come back as `{"error":{"kind":"not_found|invalid|busy","message":"…"}}` with status 404, 400 or 409; writes need `Content-Type: application/json`. There is no authentication; do not expose the port to untrusted networks.

## The REST API

syncr's API is plain JSON under `/api/v1`; the web UI is just one client of it, so you can script against it too. Errors come back as `{"error":{"kind":"not_found|invalid|busy","message":"..."}}` with status 404, 400 or 409.

```sh
curl localhost:8081/api/v1/feeds                         # configured feeds (and orphaned folders)
curl localhost:8081/api/v1/feeds/UCXuqSBlHAE6Xw-yeJA0Tunw/episodes
curl -X POST localhost:8081/api/v1/pull                  # check every feed now
curl localhost:8081/api/v1/status                        # activity and current problems
```

Writes need `Content-Type: application/json`. The feeds, media and health endpoints (`/feeds/<id>.xml`, `/media/<id>/<file>`, `/healthz`) live at the top level. The API has no authentication; do not expose it to untrusted networks.

## Troubleshooting

Check the banner at the top of the page first; it quotes yt-dlp's error.

- **"Sign in to confirm you're not a bot"**: YouTube wants a logged-in session. Export cookies from a browser where you're signed in to a `cookies.txt` file (Netscape format), put it somewhere the app can read (in Docker, in the `config` folder), and set `download.cookies_file` (for example `/config/cookies.txt`) in Settings. Use a throwaway account if you can; the cookies grant access to that account.
- **"Unable to extract..." / downloads suddenly fail everywhere**: yt-dlp is out of date. Tick *Keep yt-dlp up to date* in Settings, or pull a newer image / update the binary. (Self-update only works if the yt-dlp binary is writable by the app, as in the Docker image. It cannot update a yt-dlp installed by a package manager.)
- **HTTP 429 / rate limited**: YouTube is throttling you. It retries on the next update; lower *Simultaneous downloads* and raise *Check feeds every* if it keeps happening.
- **"Cannot reach the sync service" in the banner**: the web UI can't talk to syncr, so nothing new is downloaded, and Add feed (name search), Load more, Check now and Download won't work. Browsing, listening and settings still do. Check that syncr is running and that `-syncr` / `SYNCR_URL` points at it. Changes you saved meanwhile are picked up when syncr next starts or runs an update.
- **Feed links don't work on my phone**: set *Public address of this server* in Settings to the machine's LAN address.
- **"Permission denied" when saving settings or downloading in Docker**: the `config` and `data` folders must be writable by the user both containers run as (the web UI saves the config and deletes feed folders; syncr writes the media).
- **Settings don't seem to save when `config.toml` is bind-mounted as a single file**: mount the folder instead (see the compose example).
- **An episode shows as failed**: the banner and the episode row show yt-dlp's reason (private, members-only and region-blocked videos can't be downloaded). Press **Retry** to try again.
- **Search returns nothing or errors**: name search scrapes YouTube's search page through yt-dlp and can break when YouTube changes it. Pasting a link or `@handle` keeps working.

## Development

```sh
go vet ./... && go test -race ./...                              # unit and HTTP tests (no network)
PODSYNC_LIVE=1 go test ./youtube -run Live -v                    # talks to real YouTube via your yt-dlp
cd e2e && npm install && ./run.sh                                # browser test: both programs + fake yt-dlp + headless Chromium
dev/run.sh                                                       # run everything locally with a real test feed
```

Layout: the root package `podsync` holds the domain types and interfaces (feeds, episodes, the `Source`, `Library` and `ConfigStore` interfaces, and the small `Syncer` contract). Subpackages are grouped by what they depend on: `youtube` (yt-dlp), `fs` (the on-disk library), `toml` (the config file), `rest` (syncr's HTTP API server and the client the web UI uses), `webui` (templates and handlers), `rss`, `status`, and `syncer` (the sync service's logic, which depends only on the root interfaces); `mock` has test doubles. The programs in `cmd/` only wire these together; the web UI binary does not contain the sync service or yt-dlp support. To add another source (Vimeo, ...), implement `podsync.Source` in a new package and register it in `cmd/syncr`. See [`PROJECT.md`](PROJECT.md) for the design.

The UI uses a vendored copy of [htmx](https://htmx.org) (0BSD license).
