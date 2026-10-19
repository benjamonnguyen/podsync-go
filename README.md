# podsync-go

Turn YouTube channels and playlists into podcast feeds you can play in any podcast app, and manage them from a web page.

You pick a channel or playlist, podsync-go downloads new episodes with [yt-dlp](https://github.com/yt-dlp/yt-dlp), stores them on your disk, and publishes a normal podcast RSS feed for each one. Add the feed URL to your podcast app and new videos show up as episodes (as video or audio-only, your choice).

Inspired by [mxpv/podsync](https://github.com/mxpv/podsync); Turn YouTube channels and playlists into podcast feeds the big difference is the built-in web UI for browsing and managing your library.

- Web UI: library by channel/playlist, search, "check now", delete feeds, settings, load older episodes on demand
- Add feeds with one box: paste a channel or playlist link, type an `@handle`, or search YouTube channels by name
- Per-feed options: audio or video, max resolution (default 720p), minimum video length (skips Shorts), title include/exclude regex, keep-newest-N cleanup, opt out of the OPML export
- OPML export of all subscriptions at `/opml.xml`, for importing your whole library into a podcast app at once
- A status banner that tells you when yt-dlp, YouTube or a download is misbehaving
- One program, one port: **podsync** fetches and stores episodes with yt-dlp, **publishes the podcast feeds and media**, and serves the management web UI, all on the same listener. Disable the web UI with an environment variable if you only want the feeds. A multi-arch (amd64 + arm64) Docker image
- Plain-file storage: no database, just media files with small JSON files next to them, and one `config.toml`

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

## Using the web UI

TODO insert screen grabs showing Library and search views

## Configuration

The feed settings live in one TOML file (default `config.toml` in the working directory, `/config/config.toml` in Docker; override with the `PODSYNC_CONFIG` environment variable). If it doesn't exist it is created with defaults. See [`config.example.toml`](config.example.toml) for a fully commented example.

| Section / key                | Default | Meaning                                                                                                                                                             |
| ---------------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `download.concurrency`       | `2`     | Simultaneous downloads (1-16)                                                                                                                                       |
| `download.update_interval`   | `30m`   | How often feeds are checked (minimum `1m`)                                                                                                                          |
| `download.self_update`       | `false` | Run `yt-dlp -U` at startup and daily                                                                                                                                |
| `defaults.format`            | `audio` | `audio` or `video`                                                                                                                                                  |
| `defaults.max_resolution`    | `720`   | Max video height in pixels; `0` = no limit                                                                                                                          |
| `defaults.min_duration`      | `"61s"` | Skip videos shorter than this; the default skips YouTube Shorts (60 seconds or less). `"0s"` downloads everything. Videos whose length is unknown are never skipped |
| `defaults.page_size`         | `10`    | Newest N episodes considered per update                                                                                                                             |
| `defaults.cleanup_keep_last` | `50`    | Keep newest N downloaded episodes; `0` = all                                                                                                                        |
| `[[feeds]]`                  |         | One block per feed: `id`, `kind` (`channel` or `playlist`), `name`, plus optional overrides of any `defaults` key                                                   |
| `feeds.title_include`        | empty   | Per feed only: regex; if set, only matching titles are downloaded                                                                                                   |
| `feeds.title_exclude`        | empty   | Per feed only: regex; matching titles are skipped, e.g. `(?i)sponsored\|#shorts`                                                                                    |
| `feeds.opml_exclude`         | `false` | Per feed only: leave this feed out of the `/opml.xml` export                                                                                                        |

`config.toml` is all you need to recreate your list of feeds on a new machine: start with an empty data directory and the feeds are fetched again.

### Deployment settings

Everything below is set once per deployment and needs a restart to change; none of it is in `config.toml` or editable in the web UI. Almost all of it is an environment variable only. podsync fails to start with a message naming whatever's missing or invalid, rather than falling back to a guess.

| Environment              | Default            | Meaning                                                                                                                                                                                                                                                                                                                                  |
| ------------------------ | ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PODSYNC_CONFIG`         | `config.toml`      | The TOML file holding feeds and defaults                                                                                                                                                                                                                                                                                                 |
| `PODSYNC_LISTEN_ADDR`    | `:8080`            | Where podsync serves feeds, media and (unless disabled) the web UI — one listener for all of it                                                                                                                                                                                                                                          |
| `PODSYNC_BASE_URL`       | **required**       | Public address of `PODSYNC_LISTEN_ADDR` as podcast apps and your browser reach it; used in feed links, enclosure URLs and to load thumbnails. There is no default — podsync refuses to start without it, since a wrong guess would silently break feed links                                                                             |
| `PODSYNC_DATA_DIR`       | `./data`           | Where media and metadata are stored (`/data` in the Docker image)                                                                                                                                                                                                                                                                        |
| `PODSYNC_COOKIES_FILE`   | empty              | Optional Netscape-format cookies.txt for YouTube sign-in checks. Checked at startup: podsync refuses to start if the file doesn't exist                                                                                                                                                                                                  |
| `PODSYNC_YTDLP_PATH`     | `yt-dlp` on `PATH` | Run a yt-dlp other than the one on `PATH` — a bare name resolved on `PATH` or an absolute/relative path. If you set this, it's checked at startup with the same logic as running it (podsync refuses to start if it doesn't resolve); leave it unset and a missing default `yt-dlp` is just a status-banner issue, not a startup failure |
| `PODSYNC_WEBUI_DISABLED` | `false`            | Serve only feeds and media; don't serve the management web UI at all                                                                                                                                                                                                                                                                     |
| `PODSYNC_USERNAME`       | empty              | Username for the web UI's HTTP Basic login. Required unless `PODSYNC_WEBUI_DISABLED`                                                                                                                                                                                                                                                     |
| `PODSYNC_PASSWORD`       | empty              | Password for the web UI's HTTP Basic login. Required unless `PODSYNC_WEBUI_DISABLED`                                                                                                                                                                                                                                                     |
| `PODSYNC_TLS_CERT_FILE`  | empty              | Certificate file; when set (together with `PODSYNC_TLS_KEY_FILE`), the one listener speaks HTTPS instead of HTTP. Checked at startup: podsync refuses to start if either file is missing or only one of the pair is set                                                                                                                  |
| `PODSYNC_TLS_KEY_FILE`   | empty              | Private key file matching `PODSYNC_TLS_CERT_FILE`                                                                                                                                                                                                                                                                                        |

## Troubleshooting

Check the banner at the top of the page first; it quotes yt-dlp's error.

- **"Sign in to confirm you're not a bot"**: YouTube wants a logged-in session. Export cookies from a browser where you're signed in to a `cookies.txt` file (Netscape format), put it somewhere the app can read (in Docker, in the `config` folder), and set `PODSYNC_COOKIES_FILE` (for example `/config/cookies.txt`) in `compose.yaml`, then restart it. Use a throwaway account if you can; the cookies grant access to that account.
- **"Unable to extract..." / downloads suddenly fail everywhere**: yt-dlp is out of date. Tick _Keep yt-dlp up to date_ in Settings, or pull a newer image / update the binary. (Self-update only works if the yt-dlp binary is writable by the app, as in the Docker image. It cannot update a yt-dlp installed by a package manager.)
- **HTTP 429 / rate limited**: YouTube is throttling you. It retries on the next update; lower _Simultaneous downloads_ and raise _Check feeds every_ if it keeps happening.
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
