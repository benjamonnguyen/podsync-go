# podsync-go

Turn YouTube channels and playlists into podcast feeds you can play in any podcast app, and manage them from a web page.

You pick a channel or playlist, podsync-go downloads new episodes with [yt-dlp](https://github.com/yt-dlp/yt-dlp), stores them on your disk, and publishes a normal podcast RSS feed for each one. Add the feed URL to your podcast app and new videos show up as episodes (as video or audio-only, your choice).

It is inspired by [mxpv/podsync](https://github.com/mxpv/podsync); the big difference is the built-in web UI for browsing and managing your library.

- Web UI: library by channel/playlist, search, "check now", delete feeds, settings, load older episodes on demand
- Add feeds with one box: paste a channel or playlist link, type an `@handle`, or search YouTube channels by name
- Per-feed options: audio or video, max resolution (default 720p), minimum video length (skips Shorts), title include/exclude regex, automatic cleanup
- A status banner that tells you when yt-dlp, YouTube or a download is misbehaving
- Two small programs: **syncr** (the sync service: fetches and stores episodes with yt-dlp, and **publishes the podcast feeds and media**) and **webui** (the library management interface, for your own use). They share the config file and the data directory; syncr also has a tiny private control API for the few things that need the running service. Multi-arch (amd64 + arm64) Docker images for both
- Plain-file storage: no database, just media files with small JSON files next to them, and one `config.toml`

> **No login.** podsync-go has no authentication. The only thing meant to be reachable by podcast apps is syncr's feed port (feeds and media are public to anyone who can reach it and knows a feed URL). The web UI can change settings and delete feeds, so keep it on a trusted network or behind an authenticating reverse proxy; syncr's control API listens on a separate private address and is not published by the compose example.

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

3. Open the management UI at <http://localhost:8090>. On first start a default `config/config.toml` is created.

4. **Edit `config/config.toml` and set `server.base_url`** to the address podcast apps (and your browser) will use to reach syncr's port 8080, for example `base_url = "http://192.168.1.10:8080"`, then restart both containers. The default `http://localhost:8080` only works on the same machine, so feed links would be useless on your phone until you change it. Your browser also loads episode thumbnails from this address. (The Settings page shows this value but does not edit it.)

5. Click **Add feed**, type or paste a channel name, an `@handle` or a link, and add it. The newest episodes start downloading right away.

6. Open the feed in the UI and copy its **Podcast feed URL** (`http://<your-address>:8080/feeds/<id>.xml`) into your podcast app ("add a show by URL").

Ports: **8080** syncr's public feeds and media (publish this to podcast apps), **8090** the management UI (keep it internal). syncr's control API on 8081 is only reachable by the web UI container. Both containers mount the same `config` and `data` folders and run as the user in `compose.yaml` (`1000:1000` in the example); create the folders as that user so both programs can write to them. If the web UI shows "Cannot reach the sync service", syncr isn't running or `SYNCR_URL` is wrong; you can still browse and edit settings, and podcast apps keep working, but nothing new is downloaded.

## Using the web UI

**Library** (home page): one card per feed with episode count, size and when it was last checked. The search box finds feeds by name and downloaded episodes by title. **Check all feeds now** starts an update immediately instead of waiting for the next scheduled one.

**Add feed**: one box does the lookup, guided by what you typed:

- a **link** (a channel, a playlist, or a single video, whose channel is used) or an **`@handle`** (or a `UC…` channel ID) goes straight to that feed;
- **anything else** is searched as a channel name, and the matches are listed for you to pick (a single match goes straight to the preview).

Playlists are not searched for by name: paste the playlist's link. You get a preview before anything is added, and can set how many episodes to fetch per update (the default comes from Settings). Press Enter or **Find**; nothing is looked up while you type.

**Feed page**: all episodes known for the feed, with a filter box. Newest downloads are at the top. Each row shows the episode's thumbnail, publish date, length, views, resolution and size; **Details** expands the description and a link to the video on YouTube.

- **Load more** (at the bottom) looks further back in the channel and lists older episodes that were not downloaded. They appear faded. Click **Download** on any of them to fetch it. Episodes you download this way are marked *kept* and are never removed by automatic cleanup.
- Episodes removed by cleanup, or that failed to download, also appear faded with **Download again** / **Retry**.
- **Check now** updates just this feed. **Feed settings** overrides the defaults for this feed. **Delete** removes it.

**Deleting a feed** removes it from your configuration (its podcast URL stops working). Downloaded files are kept unless you tick the box; kept folders show up under *Not in config* on the library page with a button to delete them.

**Settings**: simultaneous downloads, how often to check, whether to keep yt-dlp up to date, and the defaults for every feed (format, maximum resolution, minimum video length, title filters, page size, cleanup). **Save and apply** validates the values and applies them immediately. The things that belong to whoever runs the service (the public address, the listen and API addresses, the data directory, the yt-dlp path and the cookies file) are shown there but are only changed in `config.toml`.

**Status banner**: yt-dlp talks to YouTube, which changes things often and sometimes blocks requests. When something goes wrong (yt-dlp missing, a feed can't be listed, a download fails, search fails, self-update fails) a banner explains it on every page. It clears itself when the problem goes away, and you can dismiss it. While work is running the banner also shows progress.

## How updates work

- A few seconds after start, and then every **Check feeds every** interval (default 30 minutes), each feed is checked.
- Only the **newest `page_size` episodes** of a feed are considered per check (default 10). Adding a feed therefore downloads at most that many, not the whole back catalogue. Use *Load more* for older ones. If a feed publishes more than `page_size` videos between checks, raise its `page_size`.
- An episode is downloaded if it is new and passes the feed's filters: videos shorter than `min_duration` (default 60 seconds, which is how YouTube Shorts are skipped) are skipped, `title_include` (if set) must match, and `title_exclude` must not match. Live streams and not-yet-premiered videos are skipped until they are normal videos.
- Failed downloads are retried on the next checks, up to 3 attempts, then wait for you to press **Retry**.
- **Cleanup** runs after each update. `cleanup_keep_last` keeps only the newest N downloaded episodes of a feed; `cleanup_after_days` also removes episodes published more than N days ago. Only the media file is removed: the episode is remembered as "removed by cleanup", so it will not be downloaded again. Episodes you downloaded manually are never cleaned up.
- Video downloads are mp4 (up to `max_resolution`); audio-only downloads are m4a.
- Each episode's thumbnail is saved next to it as a jpg (served by the app and used as the episode image in the podcast feed), together with its title, description, publish date, duration, channel, view count and resolution. Cleanup removes only the media; the thumbnail and details are kept.

## Configuration

Everything can be changed in the web UI. The settings live in one TOML file (syncr's config: default `config.toml` in the working directory, `/config/config.toml` in Docker; override with `-config` or the `PODSYNC_CONFIG` environment variable). If it doesn't exist it is created with defaults. See [`config.example.toml`](config.example.toml) for a fully commented example.

The web UI rewrites this file when you save changes, so hand-written comments are lost; keep annotated copies elsewhere. Unknown keys are rejected so typos don't go unnoticed. If you edit the file by hand while the programs run, the web UI notices on its next page load and syncr applies the change at its next update at the latest (or immediately with `curl -X POST localhost:8081/api/v1/apply`). A file that no longer parses is ignored and reported in the banner; the last good settings stay in use.

| Section / key | Default | Meaning |
|---|---|---|
| `server.listen_addr` | `:8080` | Where syncr publishes feeds and media (edit the file, restart) |
| `server.api_addr` | `127.0.0.1:8081` | Where syncr's private control API listens; must differ from `listen_addr` (edit the file, restart) |
| `server.base_url` | `http://localhost:8080` | Public address of `listen_addr` as podcast apps and your browser reach it; used in feed links and enclosure URLs |
| `server.data_dir` | `./data` | Where media and metadata are stored; both programs use it (edit the file, restart) |
| `download.yt_dlp_path` | `yt-dlp` | yt-dlp executable |
| `download.concurrency` | `2` | Simultaneous downloads (1-16) |
| `download.update_interval` | `30m` | How often feeds are checked (minimum `1m`) |
| `download.self_update` | `false` | Run `yt-dlp -U` at startup and daily |
| `download.cookies_file` | empty | Netscape-format cookies.txt for YouTube sign-in checks |
| `defaults.format` | `video` | `video` or `audio` |
| `defaults.max_resolution` | `720` | Max video height in pixels; `0` = no limit |
| `defaults.min_duration` | `"60s"` | Skip videos shorter than this; this is how Shorts are skipped. `"0s"` downloads everything. Videos whose length is unknown are never skipped |
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
| `-listen` | `WEBUI_LISTEN` | `127.0.0.1:8090` | Address to serve the web UI on (loopback by default; the Docker image sets `:8090`) |
| `-syncr` | `SYNCR_URL` | `http://127.0.0.1:8081` | Where syncr's control API is (`server.api_addr` in the config) |

syncr listens on two addresses: `server.listen_addr` (default `:8080`) for the public feeds and media, and `server.api_addr` (default `127.0.0.1:8081`, loopback only) for the control API. They are separate on purpose so publishing the feeds never exposes the API; the config check refuses to start if they are the same. The Docker image sets `SYNCR_API_ADDR=:8081` (an environment override of `server.api_addr`) because the web UI runs in another container.

`server.base_url` is syncr's *public* address as podcast apps reach it. The web UI uses it for the feed links it shows and to load thumbnails in your browser, so your browser must be able to reach it too. Both programs accept `-version` and `-healthcheck` (exit status 0 if the running instance answers), used by the container health checks.

Both programs must see the same config file and the same data directory. If `data_dir` is a relative path (like the default `./data`) they resolve it from their own working directory, so start them from the same directory or use an absolute path. The settings page does not edit the addresses, `server.base_url`, `server.data_dir`, `download.yt_dlp_path` or `download.cookies_file`: change those in the file and restart both programs. (`download.yt_dlp_path` and `download.cookies_file` are only used by syncr.)

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
./syncr -config config.toml &                                              # feeds on :8080, control API on 127.0.0.1:8081
./webui -config config.toml -syncr http://127.0.0.1:8081                      # management UI on 127.0.0.1:8090 (add -listen :8090 for your LAN)
```

Start both from the same directory (or use an absolute `data_dir`). For a quick look at the UI with a real channel, run `dev/run.sh`: it builds and starts both programs with a ready-made test feed (UI on <http://localhost:8090>, feeds on `:8080`; media goes to `dev/data`).

## syncr's control API

Everything the web UI can do with the config and the library it does directly on the shared files. syncr's private API (`server.api_addr`) covers only what needs the running service (it runs yt-dlp and holds the download queue):

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/apply` | Re-read `config.toml` and rescan the library after the web UI changed them. Replies `{"changed":…,"restart_required":…}` |
| `POST /api/v1/pull` | Start an update of every feed, or `{"feed":"UC…"}` for one, or `{"feed":"UC…","episode":{"id":"…"}}` to download one episode now. `409` if an update is already running |
| `GET /api/v1/status` | Activity, current problems, in-flight downloads, tool versions |
| `GET /api/v1/find?q=` | Look up what a user typed to add a feed: a link or `@handle` resolves to that feed, anything else searches channels by name (see below) |
| `GET /api/v1/feeds/{id}/listing?offset=&limit=` | A window of a feed's episodes straight from YouTube (nothing is stored) |
| `GET /healthz` | Liveness |

Errors come back as `{"error":{"kind":"not_found|invalid|busy","message":"…"}}` with status 404, 400 or 409; writes need `Content-Type: application/json`. There is no authentication; keep this address private. The public port serves only `/feeds/<id>.xml`, `/media/<id>/<file>` and `/healthz`.

## Troubleshooting

Check the banner at the top of the page first; it quotes yt-dlp's error.

- **"Sign in to confirm you're not a bot"**: YouTube wants a logged-in session. Export cookies from a browser where you're signed in to a `cookies.txt` file (Netscape format), put it somewhere the app can read (in Docker, in the `config` folder), and set `download.cookies_file` (for example `/config/cookies.txt`) in Settings. Use a throwaway account if you can; the cookies grant access to that account.
- **"Unable to extract..." / downloads suddenly fail everywhere**: yt-dlp is out of date. Tick *Keep yt-dlp up to date* in Settings, or pull a newer image / update the binary. (Self-update only works if the yt-dlp binary is writable by the app, as in the Docker image. It cannot update a yt-dlp installed by a package manager.)
- **HTTP 429 / rate limited**: YouTube is throttling you. It retries on the next update; lower *Simultaneous downloads* and raise *Check feeds every* if it keeps happening.
- **"Cannot reach the sync service" in the banner**: the web UI can't talk to syncr, so nothing new is downloaded, and Add feed, Load more, Check now and Download won't work. Browsing and settings still do, and podcast apps keep fetching from syncr's feed port if it is up. Check that syncr is running and that `-syncr` / `SYNCR_URL` points at it. Changes you saved meanwhile are picked up when syncr next starts or runs an update.
- **Feed links don't work on my phone**, or thumbnails are missing in the web UI: set `server.base_url` in `config.toml` to the machine's LAN address and syncr's public port (for example `http://192.168.1.10:8080`) and restart.
- **"Permission denied" when saving settings or downloading in Docker**: the `config` and `data` folders must be writable by the user both containers run as (the web UI saves the config and deletes feed folders; syncr writes the media).
- **Settings don't seem to save when `config.toml` is bind-mounted as a single file**: mount the folder instead (see the compose example).
- **An episode shows as failed**: the banner and the episode row show yt-dlp's reason (private, members-only and region-blocked videos can't be downloaded). Press **Retry** to try again.
- **Search returns nothing or errors**: name search scrapes YouTube's search page through yt-dlp and can break when YouTube changes it. Pasting a link or typing an `@handle` goes straight to the feed and keeps working.

## Development

```sh
go vet ./... && go test -race ./...                              # unit and HTTP tests (no network)
PODSYNC_LIVE=1 go test ./youtube -run Live -v                    # talks to real YouTube via your yt-dlp
cd e2e && npm install && ./run.sh                                # browser test: both programs + fake yt-dlp + headless Chromium
dev/run.sh                                                       # run everything locally with a real test feed
```

Layout: the root package `podsync` holds the domain types and interfaces (feeds, episodes, the `Source`, `Library` and `ConfigStore` interfaces, and the small `Syncer` contract). Subpackages are grouped by what they depend on: `youtube` (yt-dlp), `fs` (the on-disk library), `toml` (the config file), `publish` (the public feeds and media), `rest` (syncr's private control API and the client the web UI uses), `rss`, `status`, and `syncer` (the sync service's logic, which depends only on the root interfaces); `mock` has test doubles. `cmd/syncr` only wires these together. `cmd/webui` is the whole web UI in its main package (templates, handlers, and a model over the config and the library); its binary does not contain the sync service or yt-dlp support. To add another source (Vimeo, ...), implement `podsync.Source` in a new package and register it in `cmd/syncr`. See [`PROJECT.md`](PROJECT.md) for the design.

The UI uses a vendored copy of [htmx](https://htmx.org) (0BSD license).
