// Package youtube implements podsync.Source by shelling out to yt-dlp.
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"podsync"
)

const Kind = "youtube"

var (
	_ podsync.Source        = (*YouTube)(nil)
	_ podsync.SourceUpdater = (*YouTube)(nil)
)

// Settings are read on every call so UI changes apply without a restart.
type Settings struct {
	CookiesFile string
	// BinPath is the yt-dlp executable to run: a bare name resolved on PATH
	// (the default, "yt-dlp") or an absolute/relative path. Empty means the default.
	BinPath string
}

// DefaultBinPath is the executable name used when Settings.BinPath is empty:
// resolved on PATH (the Docker image puts its bundled copy there).
const DefaultBinPath = "yt-dlp"

type YouTube struct {
	settings func() Settings

	mu         sync.Mutex
	cache      map[string]cachedSearch
	searchHold time.Time
	now        func() time.Time
}

type cachedSearch struct {
	at      time.Time
	results []podsync.FeedInfo
}

func New(settings func() Settings) *YouTube {
	return &YouTube{settings: settings, cache: map[string]cachedSearch{}, now: time.Now}
}

func (y *YouTube) Kind() string { return Kind }

const (
	listTimeout     = 3 * time.Minute
	resolveTimeout  = 2 * time.Minute
	downloadTimeout = 4 * time.Hour
	searchCacheTTL  = 10 * time.Minute
	searchPause     = 10 * time.Second
)

// ---- running yt-dlp ----

type ytdlpError struct {
	Message string
	Err     error
}

func (e *ytdlpError) Error() string { return e.Message }
func (e *ytdlpError) Unwrap() error { return e.Err }

func (y *YouTube) baseArgs() []string {
	args := []string{"--ignore-config", "--no-warnings", "--no-colors", "--socket-timeout", "30"}
	if c := y.settings().CookiesFile; c != "" {
		args = append(args, "--cookies", c)
	}
	return args
}

func (y *YouTube) binPath() string {
	if p := y.settings().BinPath; p != "" {
		return p
	}
	return DefaultBinPath
}

func (y *YouTube) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bin := y.binPath()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, &ytdlpError{Message: fmt.Sprintf("yt-dlp not found at %q", bin), Err: err}
		}
		if ctx.Err() != nil {
			return nil, &ytdlpError{Message: "yt-dlp timed out or was cancelled", Err: ctx.Err()}
		}
		return nil, &ytdlpError{Message: friendlyError(stderr.String(), err), Err: err}
	}
	return stdout.Bytes(), nil
}

var errLine = regexp.MustCompile(`(?m)^ERROR:\s*(.+)$`)

// friendlyError reduces yt-dlp's stderr to one useful line plus a hint.
func friendlyError(stderr string, err error) string {
	msg := ""
	if m := errLine.FindAllStringSubmatch(stderr, -1); len(m) > 0 {
		msg = strings.TrimSpace(m[len(m)-1][1])
	} else {
		lines := strings.Split(strings.TrimSpace(stderr), "\n")
		msg = strings.TrimSpace(lines[len(lines)-1])
	}
	if msg == "" {
		msg = err.Error()
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "sign in to confirm") || strings.Contains(low, "not a bot"):
		msg += " — YouTube is asking for a login; set PODSYNC_COOKIES_FILE to a cookies.txt exported from a signed-in browser"
	case strings.Contains(low, "http error 429") || strings.Contains(low, "too many requests"):
		msg += " — rate limited by YouTube; it will be retried on the next update"
	case strings.Contains(low, "unable to extract") || strings.Contains(low, "please report this issue"):
		msg += " — yt-dlp probably needs updating (enable download.self_update or upgrade the binary)"
	case strings.Contains(low, "ffmpeg") && strings.Contains(low, "not found"):
		msg += " — install ffmpeg"
	}
	return msg
}

// ---- health & self-update ----

func (y *YouTube) Check(ctx context.Context) podsync.Health {
	var h podsync.Health
	out, err := y.run(ctx, 30*time.Second, "--version")
	if err != nil {
		h.Errors = append(h.Errors, err.Error())
	} else {
		h.Version = strings.TrimSpace(string(out))
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		h.Warnings = append(h.Warnings, "ffmpeg not found: audio extraction and merging video+audio will fail")
	}
	return h
}

func (y *YouTube) SelfUpdate(ctx context.Context) (string, error) {
	out, err := y.run(ctx, 5*time.Minute, "--ignore-config", "-U")
	return strings.TrimSpace(string(out)), err
}

// ---- reference parsing ----

var (
	channelIDRe  = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
	playlistIDRe = regexp.MustCompile(`^(PL|UU|FL|OL|RD|LL)[A-Za-z0-9_-]{10,}$`)
	handleRe     = regexp.MustCompile(`^@[\p{L}\p{N}_.\-]{1,100}$`)
	videoIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{6,20}$`)
	youtubeHosts = map[string]bool{
		"youtube.com": true, "www.youtube.com": true, "m.youtube.com": true,
		"music.youtube.com": true, "youtu.be": true,
	}
	channelPathRe = regexp.MustCompile(`^/(channel/[^/]+|@[^/]+|c/[^/]+|user/[^/]+)`)
)

type refKind int

const (
	refChannel refKind = iota
	refPlaylist
	refVideo
)

// parseRef turns user input into a YouTube URL yt-dlp can resolve, refusing
// anything that is not on a YouTube host (the app has no auth, so it must not
// become a generic URL fetcher).
func parseRef(ref string) (string, refKind, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", 0, podsync.Invalid("enter a channel URL, @handle, or ID")
	}
	if strings.ContainsAny(ref, " \t\r\n") {
		return "", 0, podsync.Invalid("a URL, @handle or ID cannot contain spaces")
	}
	if looksLikeURL(ref) {
		if !strings.Contains(ref, "://") {
			ref = "https://" + ref
		}
		u, err := url.Parse(ref)
		if err != nil || !youtubeHosts[strings.ToLower(u.Hostname())] {
			return "", 0, podsync.Invalid("only youtube.com URLs are supported")
		}
		u.Scheme = "https"
		host := strings.ToLower(u.Hostname())
		q := u.Query()
		switch {
		case host == "youtu.be":
			if list := q.Get("list"); list != "" && playlistIDRe.MatchString(list) {
				return playlistURL(list), refPlaylist, nil
			}
			id := strings.Trim(u.Path, "/")
			if !videoIDRe.MatchString(id) {
				return "", 0, podsync.Invalid("unrecognized youtu.be URL")
			}
			return "https://www.youtube.com/watch?v=" + id, refVideo, nil
		case u.Path == "/playlist" || (u.Path == "/watch" && q.Get("list") != ""):
			list := q.Get("list")
			if !playlistIDRe.MatchString(list) {
				return "", 0, podsync.Invalid("unrecognized playlist ID")
			}
			return playlistURL(list), refPlaylist, nil
		case u.Path == "/watch":
			id := q.Get("v")
			if !videoIDRe.MatchString(id) {
				return "", 0, podsync.Invalid("unrecognized video URL")
			}
			return "https://www.youtube.com/watch?v=" + id, refVideo, nil
		case strings.HasPrefix(u.Path, "/shorts/") || strings.HasPrefix(u.Path, "/live/"):
			id := strings.Trim(strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(u.Path, "/shorts/"), "/live/"), "/", 2)[0], "/")
			if !videoIDRe.MatchString(id) {
				return "", 0, podsync.Invalid("unrecognized video URL")
			}
			return "https://www.youtube.com/watch?v=" + id, refVideo, nil
		}
		if m := channelPathRe.FindString(u.Path); m != "" {
			return "https://www.youtube.com" + m + "/videos", refChannel, nil
		}
		return "", 0, podsync.Invalid("could not find a channel or playlist in that URL")
	}
	switch {
	case channelIDRe.MatchString(ref):
		return channelURL(ref), refChannel, nil
	case playlistIDRe.MatchString(ref):
		return playlistURL(ref), refPlaylist, nil
	case handleRe.MatchString(ref):
		return "https://www.youtube.com/" + ref + "/videos", refChannel, nil
	}
	return "", 0, podsync.Invalid("not a recognized channel URL, @handle, channel ID or playlist ID")
}

// looksLikeURL is the "this is a link" hint: a scheme, or a bare YouTube host.
func looksLikeURL(s string) bool {
	l := strings.ToLower(s)
	if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
		return true
	}
	for h := range youtubeHosts {
		if strings.HasPrefix(l, h) {
			return true
		}
	}
	return false
}

// isDirectReference is the hint that a query names one specific feed rather
// than describing it: a URL, an @handle, or a channel ID. Anything else is a
// channel name to search for. (A bare playlist ID is deliberately not a hint:
// playlists are added by URL.)
func isDirectReference(q string) bool {
	return looksLikeURL(q) || strings.HasPrefix(q, "@") || channelIDRe.MatchString(q)
}

func channelURL(id string) string  { return "https://www.youtube.com/channel/" + id + "/videos" }
func playlistURL(id string) string { return "https://www.youtube.com/playlist?list=" + id }

func feedURL(f podsync.FeedInfo) string {
	if f.Kind == "playlist" {
		return playlistURL(f.ID)
	}
	return channelURL(f.ID)
}

// ---- JSON shapes ----

type thumb struct {
	URL    string `json:"url"`
	ID     string `json:"id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// thumbs accepts yt-dlp's list form and the single-object form used by
// search results.
type thumbs []thumb

func (t *thumbs) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '{' {
		var one thumb
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*t = thumbs{one}
		return nil
	}
	var many []thumb
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*t = many
	return nil
}

func absURL(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

// avatar picks the best channel/playlist image: the largest square thumbnail
// (the uncropped avatar is `=s0`, i.e. original size and often several MB),
// then the uncropped avatar, then the last thumbnail.
func (t thumbs) avatar() string {
	best, bestArea := "", -1
	uncropped := ""
	for _, th := range t {
		if th.ID == "avatar_uncropped" {
			uncropped = th.URL
		}
		if th.Width > 0 && th.Width == th.Height && th.Width*th.Height > bestArea {
			best, bestArea = th.URL, th.Width*th.Height
		}
	}
	switch {
	case best != "":
		return absURL(best)
	case uncropped != "":
		return absURL(uncropped)
	case len(t) > 0:
		return absURL(t[len(t)-1].URL)
	}
	return ""
}

type ytEntry struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	Duration    float64   `json:"duration"`
	ViewCount   float64   `json:"view_count"`
	Height      float64   `json:"height"`
	LiveStatus  string    `json:"live_status"`
	Timestamp   float64   `json:"timestamp"`
	UploadDate  string    `json:"upload_date"`
	Channel     string    `json:"channel"`
	ChannelID   string    `json:"channel_id"`
	Uploader    string    `json:"uploader"`
	UploaderID  string    `json:"uploader_id"`
	Followers   float64   `json:"channel_follower_count"`
	Thumbnail   string    `json:"thumbnail"`
	Thumbnails  thumbs    `json:"thumbnails"`
	WebpageURL  string    `json:"webpage_url"`
	Type        string    `json:"_type"`
	Entries     []ytEntry `json:"entries"`
}

func parseJSON(out []byte, v any) error {
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("could not parse yt-dlp output: %w", err)
	}
	return nil
}

func publishedOf(e ytEntry) time.Time {
	if e.Timestamp > 0 {
		return time.Unix(int64(e.Timestamp), 0).UTC()
	}
	if len(e.UploadDate) == 8 {
		if t, err := time.Parse("20060102", e.UploadDate); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ---- Resolve ----

func (y *YouTube) Resolve(ctx context.Context, ref string) (podsync.FeedInfo, error) {
	u, kind, err := parseRef(ref)
	if err != nil {
		return podsync.FeedInfo{}, err
	}
	if kind == refVideo {
		out, err := y.run(ctx, resolveTimeout, append(y.baseArgs(), "-J", "--no-playlist", "--skip-download", "--", u)...)
		if err != nil {
			return podsync.FeedInfo{}, err
		}
		var v ytEntry
		if err := parseJSON(out, &v); err != nil {
			return podsync.FeedInfo{}, err
		}
		if !channelIDRe.MatchString(v.ChannelID) {
			return podsync.FeedInfo{}, errors.New("could not determine the channel of that video")
		}
		u = channelURL(v.ChannelID)
	}
	out, err := y.run(ctx, resolveTimeout, append(y.baseArgs(), "-J", "--flat-playlist", "--playlist-end", "1", "--", u)...)
	if err != nil {
		return podsync.FeedInfo{}, err
	}
	var p ytEntry
	if err := parseJSON(out, &p); err != nil {
		return podsync.FeedInfo{}, err
	}
	if p.ID == "" {
		return podsync.FeedInfo{}, errors.New("yt-dlp returned no feed id")
	}
	info := podsync.FeedInfo{Source: Kind, ID: p.ID, Description: strings.TrimSpace(p.Description), ImageURL: p.Thumbnails.avatar()}
	if channelIDRe.MatchString(p.ID) {
		info.Kind = podsync.FeedChannel
		info.Subscribers = int64(p.Followers)
		if strings.HasPrefix(p.UploaderID, "@") {
			info.Handle = p.UploaderID
		}
		info.Name = firstNonEmpty(p.Channel, p.Uploader, strings.TrimSuffix(p.Title, " - Videos"), p.ID)
	} else {
		info.Kind = podsync.FeedPlaylist
		info.Name = firstNonEmpty(p.Title, p.ID)
	}
	return info, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ---- ListEpisodes ----

func (y *YouTube) ListEpisodes(ctx context.Context, feed podsync.FeedInfo, offset, limit int) ([]podsync.EpisodeInfo, error) {
	if limit < 1 {
		return nil, nil
	}
	if offset < 0 {
		offset = 0
	}
	args := append(y.baseArgs(), "-J", "--flat-playlist")
	if feed.Reverse {
		args = append(args, "--playlist-reverse")
	}
	args = append(args, "--playlist-start", fmt.Sprint(offset+1), "--playlist-end", fmt.Sprint(offset+limit), "--", feedURL(feed))
	out, err := y.run(ctx, listTimeout, args...)
	if err != nil {
		return nil, err
	}
	var p ytEntry
	if err := parseJSON(out, &p); err != nil {
		return nil, err
	}
	eps := make([]podsync.EpisodeInfo, 0, len(p.Entries))
	for _, e := range p.Entries {
		if e.ID == "" || !videoIDRe.MatchString(e.ID) {
			continue
		}
		ep := podsync.EpisodeInfo{
			ID:          e.ID,
			Title:       strings.TrimSpace(e.Title),
			URL:         "https://www.youtube.com/watch?v=" + e.ID,
			Published:   publishedOf(e),
			DurationSec: int(e.Duration),
			Description: e.Description,
			Channel:     firstNonEmpty(e.Channel, e.Uploader),
			ViewCount:   int64(e.ViewCount),
			IsLive:      e.LiveStatus == "is_live" || e.LiveStatus == "is_upcoming" || e.LiveStatus == "post_live",
			Unavailable: e.Title == "[Private video]" || e.Title == "[Deleted video]",
		}
		if n := len(e.Thumbnails); n > 0 {
			ep.Thumbnail = absURL(e.Thumbnails[n-1].URL)
		}
		eps = append(eps, ep)
	}
	return eps, nil
}

// ---- Find and search ----

// Find answers what the user typed when adding a feed. A URL, @handle or
// channel ID resolves to exactly that feed (a failure is the user's input not
// resolving, so it is reported as ErrInvalid); everything else is a channel
// name search. Playlists are found by URL only.
func (y *YouTube) Find(ctx context.Context, query string) ([]podsync.FeedInfo, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, podsync.Invalid("type a channel name, an @handle or a YouTube link")
	}
	if isDirectReference(query) {
		info, err := y.Resolve(ctx, query)
		if err != nil {
			return nil, podsync.InvalidErr(err)
		}
		return []podsync.FeedInfo{info}, nil
	}
	return y.searchChannels(ctx, query)
}

// sp=EgIQAg%253D%253D is YouTube's own "type: channel" search filter. It is an
// undocumented URL parameter; if it stops working, search fails and the banner says so.
const spChannel = "EgIQAg%253D%253D"

func (y *YouTube) searchChannels(ctx context.Context, query string) ([]podsync.FeedInfo, error) {
	if len(query) > 200 {
		query = query[:200]
	}
	key := strings.ToLower(query)

	y.mu.Lock()
	if c, ok := y.cache[key]; ok && y.now().Sub(c.at) < searchCacheTTL {
		y.mu.Unlock()
		return c.results, nil
	}
	if wait := y.searchHold.Sub(y.now()); wait > 0 {
		y.mu.Unlock()
		return nil, fmt.Errorf("search paused after a recent failure; try again in %ds", int(wait.Seconds())+1)
	}
	y.mu.Unlock()

	u := "https://www.youtube.com/results?search_query=" + url.QueryEscape(query) + "&sp=" + spChannel
	out, err := y.run(ctx, resolveTimeout, append(y.baseArgs(), "-J", "--flat-playlist", "--playlist-end", "12", "--", u)...)
	if err == nil {
		var p ytEntry
		if err = parseJSON(out, &p); err == nil {
			results := parseSearch(p.Entries)
			y.mu.Lock()
			y.cache[key] = cachedSearch{at: y.now(), results: results}
			if len(y.cache) > 64 {
				y.pruneLocked()
			}
			y.mu.Unlock()
			return results, nil
		}
	}
	y.mu.Lock()
	y.searchHold = y.now().Add(searchPause)
	y.mu.Unlock()
	return nil, err
}

func (y *YouTube) pruneLocked() {
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(y.cache))
	for k, c := range y.cache {
		all = append(all, kv{k, c.at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, e := range all[:len(all)/2] {
		delete(y.cache, e.k)
	}
}

func parseSearch(entries []ytEntry) []podsync.FeedInfo {
	out := []podsync.FeedInfo{}
	seen := map[string]bool{}
	for _, e := range entries {
		if !channelIDRe.MatchString(e.ID) || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		r := podsync.FeedInfo{
			Source:      Kind,
			Kind:        podsync.FeedChannel,
			ID:          e.ID,
			Name:        firstNonEmpty(e.Title, e.Channel, e.Uploader, e.ID),
			Description: strings.TrimSpace(e.Description),
			ImageURL:    e.Thumbnails.avatar(),
			Subscribers: int64(e.Followers),
		}
		if strings.HasPrefix(e.UploaderID, "@") {
			r.Handle = e.UploaderID
		}
		out = append(out, r)
	}
	return out
}

// ---- Download ----

var mediaIgnore = []string{".json", ".part", ".ytdl", ".temp", ".jpg", ".jpeg", ".png", ".webp", ".vtt", ".srt"}

func formatArgs(opts podsync.DownloadOptions) []string {
	if opts.Audio {
		return []string{"-f", "bestaudio[ext=m4a]/bestaudio/best", "-x", "--audio-format", "m4a"}
	}
	h := ""
	if opts.MaxHeight > 0 {
		h = fmt.Sprintf("[height<=?%d]", opts.MaxHeight)
	}
	f := fmt.Sprintf("bv*%[1]s[ext=mp4]+ba[ext=m4a]/b%[1]s[ext=mp4]/bv*%[1]s+ba/b%[1]s", h)
	return []string{"-f", f, "--merge-output-format", "mp4"}
}

func (y *YouTube) Download(ctx context.Context, ep podsync.EpisodeInfo, opts podsync.DownloadOptions, destDir string) (podsync.DownloadResult, error) {
	if !videoIDRe.MatchString(ep.ID) {
		return podsync.DownloadResult{}, fmt.Errorf("invalid video id %q", ep.ID)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return podsync.DownloadResult{}, err
	}
	args := append(y.baseArgs(), "--no-playlist", "--write-info-json", "--no-write-comments",
		"--write-thumbnail", "--convert-thumbnails", "jpg",
		"--retries", "3", "--fragment-retries", "3",
		"-P", destDir, "-o", ep.ID+".%(ext)s")
	args = append(args, formatArgs(opts)...)
	args = append(args, "--", "https://www.youtube.com/watch?v="+ep.ID)
	if _, err := y.run(ctx, downloadTimeout, args...); err != nil {
		cleanupPartials(destDir, ep.ID)
		return podsync.DownloadResult{}, err
	}

	meta := ep
	infoPath := filepath.Join(destDir, ep.ID+".info.json")
	if b, err := os.ReadFile(infoPath); err == nil {
		var info ytEntry
		if json.Unmarshal(b, &info) == nil {
			if info.LiveStatus == "is_live" || info.LiveStatus == "is_upcoming" {
				cleanupPartials(destDir, ep.ID)
				return podsync.DownloadResult{}, errors.New("episode is a live or upcoming stream")
			}
			meta.Title = firstNonEmpty(info.Title, meta.Title)
			meta.Description = truncate(info.Description, 4000)
			meta.Published = publishedOf(info)
			if info.Duration > 0 {
				meta.DurationSec = int(info.Duration)
			}
			meta.Thumbnail = firstNonEmpty(info.Thumbnail, meta.Thumbnail)
			meta.Channel = firstNonEmpty(info.Channel, info.Uploader, meta.Channel)
			if info.ViewCount > 0 {
				meta.ViewCount = int64(info.ViewCount)
			}
			if !opts.Audio && info.Height > 0 {
				meta.Height = int(info.Height)
			}
			meta.URL = firstNonEmpty(info.WebpageURL, "https://www.youtube.com/watch?v="+ep.ID)
		}
		_ = os.Remove(infoPath)
	}

	file, size, err := findMedia(destDir, ep.ID)
	if err != nil {
		return podsync.DownloadResult{}, err
	}
	res := podsync.DownloadResult{File: file, Size: size, Episode: meta}
	// The thumbnail is a nice-to-have: a missing or unconvertible one must not fail the episode.
	if st, err := os.Stat(filepath.Join(destDir, ep.ID+".jpg")); err == nil && !st.IsDir() && st.Size() > 0 {
		res.Thumb = ep.ID + ".jpg"
	}
	return res, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func findMedia(dir, id string) (string, int64, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, escapeGlob(id)+".*"))
	var best string
	var bestSize int64 = -1
	for _, m := range matches {
		skip := false
		for _, ext := range mediaIgnore {
			if strings.HasSuffix(strings.ToLower(m), ext) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		st, err := os.Stat(m)
		if err != nil || st.IsDir() {
			continue
		}
		if st.Size() > bestSize {
			best, bestSize = m, st.Size()
		}
	}
	if best == "" || bestSize <= 0 {
		return "", 0, errors.New("yt-dlp finished but produced no media file")
	}
	return filepath.Base(best), bestSize, nil
}

func escapeGlob(s string) string {
	return strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]").Replace(s)
}

func cleanupPartials(dir, id string) {
	matches, _ := filepath.Glob(filepath.Join(dir, escapeGlob(id)+".*"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
