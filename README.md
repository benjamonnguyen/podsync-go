# podsync-go

Turn YouTube channels and playlists into podcast feeds you can play in any podcast app, and manage them from a web page.

You pick a channel or playlist, podsync-go downloads new episodes with [yt-dlp](https://github.com/yt-dlp/yt-dlp), stores them on your disk, and publishes a normal podcast RSS feed for each one. Add the feed URL to your podcast app and new videos show up as episodes (as video or audio-only, your choice).

It is inspired by [mxpv/podsync](https://github.com/mxpv/podsync); the big difference is the built-in web UI for browsing and managing your library.

- Web UI: library by channel/playlist, search, "check now", delete feeds, settings, load older episodes on demand
- Add feeds by pasting a link, `@handle` or ID, or by searching YouTube by name
- Per-feed options: audio or video, max resolution, skip Shorts, title include/exclude regex, automatic cleanup
- A status banner that tells you when yt-dlp, YouTube or a download is misbehaving
- Single static binary or a multi-arch (amd64 + arm64) Docker image
- Plain-file storage: no database, just media files with small JSON files next to them, and one `config.toml`

> **No login.** podsync-go has no authentication. Run it on a trusted home network, or put it behind a reverse proxy that does authentication. Anyone who can open the page can change settings, delete feeds and download your media.

## Quick start (Docker)

1. Get the code, create the folders and copy the example compose file:

   ```sh
   git clone <this repository> podsync-go && cd podsync-go
   mkdir -p config data
   cp compose.example.yaml compose.yaml
   ```

   The example builds the image from the repository. To build the multi-architecture image yourself:

   ```sh
   docker buildx build --platform linux/amd64,linux/arm64 -t podsync-go .
   ```

2. Start it:

   ```sh
   docker compose up -d
   ```

3. Open <http://localhost:8080>. On first start a default `config/config.toml` is created.

4. **Open Settings and set "Public address of this server"** to the address your podcast app will use to reach it, for example `http://192.168.1.10:8080`. The default `http://localhost:8080` only works on the same machine, so feed links would be useless on your phone until you change it.

5. Click **Add feed**, paste a channel link (or an `@handle`, or search by name), and add it. The newest episodes start downloading right away.

6. Open the feed in the app and copy its **Podcast feed URL** (`http://<your-address>/feeds/<id>.xml`) into your podcast app ("add a show by URL").

The container runs as the user in `compose.yaml` (`1000:1000` in the example). Create `config` and `data` as that user so it can write to them.

## Using the web UI

**Library** (home page): one card per feed with episode count, size and when it was last checked. The search box finds feeds by name and downloaded episodes by title. **Check all feeds now** starts an update immediately instead of waiting for the next scheduled one.

**Add feed**: paste a channel URL, `@handle`, channel ID (`UC...`), playlist URL or even a single video URL (its channel is used). Or search by name and pick a result. You get a preview before anything is added. You can set how many episodes to fetch per update; the default comes from Settings.

**Feed page**: all episodes known for the feed, with a filter box. Newest downloads are at the top.

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

## Configuration

Everything can be changed in the web UI. The settings live in one TOML file (default `config.toml` in the working directory, `/config/config.toml` in Docker; override with `-config` or the `PODSYNC_CONFIG` environment variable). If it doesn't exist it is created with defaults. See [`config.example.toml`](config.example.toml) for a fully commented example.

The web UI rewrites this file when you save changes, so hand-written comments are lost; keep annotated copies elsewhere. Unknown keys are rejected at startup so typos don't go unnoticed.

| Section / key | Default | Meaning |
|---|---|---|
| `server.listen_addr` | `:8080` | Address and port to listen on (restart needed) |
| `server.base_url` | `http://localhost:8080` | Address podcast apps use to reach the server; used in feed links |
| `server.data_dir` | `./data` | Where media and metadata are stored (restart needed) |
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

Regular expressions use Go's [RE2 syntax](https://pkg.go.dev/regexp/syntax); prefix with `(?i)` for case-insensitive matching.

`config.toml` is all you need to recreate your list of feeds on a new machine: start with an empty data directory and the feeds are fetched again.

## Where things are stored

```
data/
  youtube/<channel or playlist id>/
    _meta.json            cached name, image, last check time
    <video id>.mp4        the media (or .m4a for audio feeds)
    <video id>.json       episode details and download status
```

It is all plain files, so backing up is just copying the folder. Deleting a media file by hand is safe: the episode is then treated as removed and not re-downloaded. Partial downloads are staged in `data/.tmp` and cleaned up on startup.

## Running without Docker

You need Go 1.27+, [yt-dlp](https://github.com/yt-dlp/yt-dlp), `ffmpeg`, and a JavaScript runtime for yt-dlp (`deno` is its default) on your `PATH`.

```sh
go build -o podsync ./cmd/podsync
./podsync -config config.toml
```

## Troubleshooting

Check the banner at the top of the page first; it quotes yt-dlp's error.

- **"Sign in to confirm you're not a bot"**: YouTube wants a logged-in session. Export cookies from a browser where you're signed in to a `cookies.txt` file (Netscape format), put it somewhere the app can read (in Docker, in the `config` folder), and set `download.cookies_file` (for example `/config/cookies.txt`) in Settings. Use a throwaway account if you can; the cookies grant access to that account.
- **"Unable to extract..." / downloads suddenly fail everywhere**: yt-dlp is out of date. Tick *Keep yt-dlp up to date* in Settings, or pull a newer image / update the binary. (Self-update only works if the yt-dlp binary is writable by the app, as in the Docker image. It cannot update a yt-dlp installed by a package manager.)
- **HTTP 429 / rate limited**: YouTube is throttling you. It retries on the next update; lower *Simultaneous downloads* and raise *Check feeds every* if it keeps happening.
- **Feed links don't work on my phone**: set *Public address of this server* in Settings to the machine's LAN address.
- **"Permission denied" when saving settings or downloading in Docker**: the `config` and `data` folders must be writable by the user the container runs as.
- **Settings don't seem to save when `config.toml` is bind-mounted as a single file**: mount the folder instead (see the compose example).
- **An episode shows as failed**: the banner and the episode row show yt-dlp's reason (private, members-only and region-blocked videos can't be downloaded). Press **Retry** to try again.
- **Search returns nothing or errors**: name search scrapes YouTube's search page through yt-dlp and can break when YouTube changes it. Pasting a link or `@handle` keeps working.

## Development

```sh
go vet ./... && go test -race ./...                       # unit and HTTP tests (no network)
PODSYNC_LIVE=1 go test ./internal/source/youtube -run Live -v   # talks to real YouTube via your yt-dlp
cd e2e && npm install && ./run.sh                          # browser test: fake yt-dlp + headless Chromium
```

Layout: `cmd/podsync` (entrypoint), `internal/config`, `internal/library` (files + sidecars), `internal/source` (adapter interface) and `internal/source/youtube` (yt-dlp adapter), `internal/puller` (scheduler, queue, cleanup), `internal/rss`, `internal/web` (templates and static files are embedded in the binary). Other sources can be added by implementing `source.Source`; see [`PROJECT.md`](PROJECT.md) for the design.

The UI uses a vendored copy of [htmx](https://htmx.org) (0BSD license).
