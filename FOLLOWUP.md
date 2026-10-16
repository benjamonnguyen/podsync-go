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
