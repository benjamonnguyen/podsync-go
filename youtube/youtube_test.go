package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"podsync"
)

func TestParseRef(t *testing.T) {
	ok := map[string]struct {
		url  string
		kind refKind
	}{
		"@LinusTechTips": {"https://www.youtube.com/@LinusTechTips/videos", refChannel},
		"https://www.youtube.com/@LinusTechTips/shorts":                  {"https://www.youtube.com/@LinusTechTips/videos", refChannel},
		"youtube.com/channel/UCXuqSBlHAE6Xw-yeJA0Tunw":                   {"https://www.youtube.com/channel/UCXuqSBlHAE6Xw-yeJA0Tunw/videos", refChannel},
		"UCXuqSBlHAE6Xw-yeJA0Tunw":                                       {"https://www.youtube.com/channel/UCXuqSBlHAE6Xw-yeJA0Tunw/videos", refChannel},
		"PLrAXtmErZgOeiKm4sgNOknGvNjby9efdf":                             {"https://www.youtube.com/playlist?list=PLrAXtmErZgOeiKm4sgNOknGvNjby9efdf", refPlaylist},
		"https://www.youtube.com/playlist?list=PLabcdefghijk":            {"https://www.youtube.com/playlist?list=PLabcdefghijk", refPlaylist},
		"https://www.youtube.com/watch?v=jNQXAC9IVRw&list=PLabcdefghijk": {"https://www.youtube.com/playlist?list=PLabcdefghijk", refPlaylist},
		"https://www.youtube.com/watch?v=jNQXAC9IVRw":                    {"https://www.youtube.com/watch?v=jNQXAC9IVRw", refVideo},
		"https://youtu.be/jNQXAC9IVRw?t=3":                               {"https://www.youtube.com/watch?v=jNQXAC9IVRw", refVideo},
		"https://www.youtube.com/shorts/abcdEFGhij1":                     {"https://www.youtube.com/watch?v=abcdEFGhij1", refVideo},
	}
	for in, want := range ok {
		got, kind, err := parseRef(in)
		if err != nil || got != want.url || kind != want.kind {
			t.Errorf("parseRef(%q) = %q,%v,%v; want %q,%v", in, got, kind, err, want.url, want.kind)
		}
	}
	bad := []string{"", "linus tech tips", "LinusTechTips", "https://evil.example.com/@x", "http://169.254.169.254/latest", "-o /etc/passwd", "--exec=rm", "file:///etc/passwd", "https://youtube.com.evil.com/@x", "@@@"}
	for _, in := range bad {
		if u, _, err := parseRef(in); err == nil {
			t.Errorf("parseRef(%q) should fail, got %q", in, u)
		}
	}
}

func TestThumbsAndAvatar(t *testing.T) {
	var e ytEntry
	_ = json.Unmarshal([]byte(`{"thumbnails":{"url":"//yt3.ggpht.com/x=s176","width":176,"height":176}}`), &e)
	if got := e.Thumbnails.avatar(); got != "https://yt3.ggpht.com/x=s176" {
		t.Fatalf("dict form: %q", got)
	}
	_ = json.Unmarshal([]byte(`{"thumbnails":[{"url":"banner","width":1000,"height":100},{"url":"sq","width":900,"height":900},{"url":"u","id":"avatar_uncropped"}]}`), &e)
	if got := e.Thumbnails.avatar(); got != "sq" {
		t.Fatalf("largest square should win: %q", got)
	}
	_ = json.Unmarshal([]byte(`{"thumbnails":[{"url":"banner","width":1000,"height":100},{"url":"u","id":"avatar_uncropped"}]}`), &e)
	if got := e.Thumbnails.avatar(); got != "u" {
		t.Fatalf("uncropped is the fallback: %q", got)
	}
}

func TestFriendlyError(t *testing.T) {
	msg := friendlyError("WARNING: x\nERROR: [youtube] abc: Sign in to confirm you’re not a bot\n", fmt.Errorf("exit 1"))
	if !strings.Contains(msg, "cookies.txt") || !strings.Contains(msg, "Settings") {
		t.Fatalf("expected cookies hint: %s", msg)
	}
	if got := friendlyError("", fmt.Errorf("exit status 2")); got != "exit status 2" {
		t.Fatalf("fallback: %q", got)
	}
}

func TestFormatArgs(t *testing.T) {
	v := strings.Join(formatArgs(podsync.DownloadOptions{MaxVideoHeight: 720}, false), " ")
	if !strings.Contains(v, "height<=?720") || !strings.Contains(v, "--merge-output-format mp4") {
		t.Fatal(v)
	}
	if strings.Contains(strings.Join(formatArgs(podsync.DownloadOptions{}, false), " "), "height") {
		t.Fatal("no cap expected")
	}
	if a := strings.Join(formatArgs(podsync.DownloadOptions{Audio: true}, false), " "); !strings.Contains(a, "-x") || !strings.Contains(a, "bestaudio") || !strings.Contains(a, "--audio-format m4a") {
		t.Fatal(a)
	}
	if a := strings.Join(formatArgs(podsync.DownloadOptions{Audio: true, AudioQuality: podsync.AudioQualityLow}, false), " "); !strings.Contains(a, "worstaudio") {
		t.Fatal(a)
	}
	if a := strings.Join(formatArgs(podsync.DownloadOptions{Audio: true}, true), " "); !strings.Contains(a, "--audio-format mp3") {
		t.Fatal(a)
	}
	if a := strings.Join(formatArgs(podsync.DownloadOptions{Audio: false}, true), " "); strings.Contains(a, "mp3") {
		t.Fatal("mp3 encoding must not affect video downloads")
	}
}

// ---- fake yt-dlp via the helper-process pattern ----

func TestMain(m *testing.M) {
	if os.Getenv("FAKE_YTDLP") == "1" {
		os.Exit(fakeYtDlp(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeYtDlp(args []string) int {
	has := func(s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}
	val := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	last := args[len(args)-1]
	switch {
	case has("--version"):
		fmt.Println("2099.01.01")
	case strings.Contains(last, "results?search_query="):
		if os.Getenv("FAKE_FAIL") == "1" {
			fmt.Fprintln(os.Stderr, "ERROR: boom")
			return 1
		}
		_ = os.WriteFile(os.Getenv("FAKE_COUNT_FILE"), []byte("x"), 0o644)
		json.NewEncoder(os.Stdout).Encode(map[string]any{"entries": []map[string]any{
			{"id": "UCXuqSBlHAE6Xw-yeJA0Tunw", "title": "Linus Tech Tips", "uploader_id": "@LinusTechTips", "channel_follower_count": 16900000, "thumbnails": map[string]any{"url": "//yt3.ggpht.com/a", "width": 176, "height": 176}},
			{"id": "UCXuqSBlHAE6Xw-yeJA0Tunw", "title": "dupe"},
			{"id": "dQw4w9WgXcQ", "title": "a video, not a channel"},
		}})
	case has("--flat-playlist"): // a /c/ custom URL being turned into a channel ID
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"id": "UCXuqSBlHAE6Xw-yeJA0Tunw", "channel_id": "UCXuqSBlHAE6Xw-yeJA0Tunw", "channel": "Linus Tech Tips", "title": "Linus Tech Tips - Videos",
			"thumbnails": []map[string]any{{"url": "av", "id": "avatar_uncropped"}},
		})
	case has("--write-info-json"):
		dir := val("-P")
		id := strings.TrimPrefix(strings.SplitN(val("-o"), ".", 2)[0], "")
		if os.Getenv("FAKE_FAIL") == "members" {
			_ = os.WriteFile(os.Getenv("FAKE_COUNT_FILE"), []byte("x"), 0o644)
			fmt.Fprintln(os.Stderr, "ERROR: [youtube] "+id+": This video is available to this channel's members on level: Bonus Videos! (or any higher level). Join this channel to get access to members-only content and other exclusive perks.")
			return 1
		}
		if os.Getenv("FAKE_FAIL") == "1" {
			_ = os.WriteFile(filepath.Join(dir, id+".mp4.part"), []byte("partial"), 0o644)
			fmt.Fprintln(os.Stderr, "ERROR: [youtube] Video unavailable")
			return 1
		}
		ext := "mp4"
		if has("-x") {
			ext = "m4a"
		}
		_ = os.WriteFile(filepath.Join(dir, id+"."+ext), []byte("media-bytes"), 0o644)
		if os.Getenv("FAKE_NO_THUMB") != "1" {
			_ = os.WriteFile(filepath.Join(dir, id+".jpg"), []byte("jpgbytes"), 0o644)
		}
		info, _ := json.Marshal(map[string]any{"id": id, "title": "Real Title", "description": "desc", "timestamp": 1700000000, "duration": 321, "webpage_url": "https://www.youtube.com/watch?v=" + id, "channel": "Chan Name", "view_count": 4321, "height": 720, "thumbnail": "https://i.ytimg.com/vi/" + id + "/maxresdefault.jpg"})
		_ = os.WriteFile(filepath.Join(dir, id+".info.json"), info, 0o644)
	default:
		fmt.Fprintln(os.Stderr, "fake yt-dlp: unhandled", args)
		return 2
	}
	return 0
}

// newFake puts a fake "yt-dlp" script on PATH (yt-dlp is always run by that
// name, never a configurable path) and returns the dir it lives in, so tests
// can find the sentinel files it writes.
func newFake(t *testing.T, env ...string) (*YouTube, string) {
	t.Helper()
	y, dir, _ := newFakeWithAPI(t, env...)
	return y, dir
}

func newFakeWithAPI(t *testing.T, env ...string) (*YouTube, string, *fakeAPI) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "yt-dlp")
	body := "#!/bin/sh\nexport FAKE_YTDLP=1 FAKE_COUNT_FILE=" + filepath.Join(dir, "count") + "\n"
	for _, e := range env {
		body += "export " + e + "\n"
	}
	body += "exec " + os.Args[0] + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	api := newFakeAPI(t)
	return New(func() Settings { return Settings{APIKey: fakeKey, APIBase: api.srv.URL} }), dir, api
}

var feed = podsync.FeedInfo{Source: Kind, Kind: "channel", ID: "UCXuqSBlHAE6Xw-yeJA0Tunw"}

func TestCheckVersionAndMissingBinary(t *testing.T) {
	y, _ := newFake(t)
	if h := y.Check(context.Background()); h.Version != "2099.01.01" || len(h.Errors) != 0 {
		t.Fatalf("%+v", h)
	}
	t.Setenv("PATH", t.TempDir()) // no yt-dlp on this PATH
	missing := New(func() Settings { return Settings{} })
	h := missing.Check(context.Background())
	if len(h.Errors) != 1 || !strings.Contains(h.Errors[0], "not found") {
		t.Fatalf("%+v", h)
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	y, _ := newFake(t)
	for _, ref := range []string{"@LinusTechTips", "UCXuqSBlHAE6Xw-yeJA0Tunw", "https://www.youtube.com/user/linustechtips", "https://www.youtube.com/c/LinusTechTips", "https://youtu.be/jNQXAC9IVRw"} {
		info, err := y.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if info.ID != "UCXuqSBlHAE6Xw-yeJA0Tunw" || info.Kind != "channel" || info.Name != "Linus Tech Tips" || info.ImageURL != "https://yt3/high.jpg" ||
			info.Handle != "@LinusTechTips" || info.Subscribers != 16900000 || info.URL != "https://www.youtube.com/channel/UCXuqSBlHAE6Xw-yeJA0Tunw/videos" {
			t.Fatalf("%s: %+v", ref, info)
		}
	}
	pl, err := y.Resolve(ctx, "https://www.youtube.com/playlist?list=PLabcdefghijk")
	if err != nil || pl.Kind != "playlist" || pl.ID != "PLabcdefghijk" || pl.Name != "A Course" || pl.URL != "https://www.youtube.com/playlist?list=PLabcdefghijk" {
		t.Fatalf("playlist: %v %+v", err, pl)
	}
	if _, err := y.Resolve(ctx, "https://example.com/x"); err == nil {
		t.Fatal("non-youtube host must be rejected")
	}
}

func TestListEpisodesWindowAndFlags(t *testing.T) {
	ctx := context.Background()
	y, _, api := newFakeWithAPI(t)
	eps, err := y.ListEpisodes(ctx, feed, 0, 5)
	if err != nil || len(eps) != 5 {
		t.Fatalf("%v %d", err, len(eps))
	}
	if ids(eps) != "vid00000001,vid00000002,vid00000003,vid00000004,vid00000005" {
		t.Fatalf("uploads are listed newest first: %s", ids(eps))
	}
	if eps[1].DurationSec != 30 || eps[0].DurationSec != 600 {
		t.Fatalf("durations come from the listing (the minimum-duration filter uses them): %+v %+v", eps[0], eps[1])
	}
	if eps[0].Channel != "Some Channel" || eps[0].ViewCount != 1001 || eps[0].Title != "Episode 1" || eps[0].Thumbnail != "https://i.ytimg.com/vi/vid00000001/maxres.jpg" ||
		!eps[0].Published.Equal(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("listing metadata: %+v", eps[0])
	}
	if !eps[2].IsLive || !eps[3].Unavailable || eps[0].IsLive || eps[0].Unavailable {
		t.Fatalf("live/unavailable flags: %+v %+v", eps[2], eps[3])
	}
	if got := api.count("channels"); got != 0 {
		t.Fatalf("a channel ID needs no lookup (uploads = UU + ID), made %d channels calls", got)
	}
	// Pages are 5 long in the fake: offset 10 needs three pages and one videos call.
	api.reset()
	next, err := y.ListEpisodes(ctx, feed, 10, 5)
	if err != nil || ids(next) != "vid00000011,vid00000012" {
		t.Fatalf("offset window: %v %s", err, ids(next))
	}
	if api.count("playlistItems") != 3 || api.count("videos") != 1 {
		t.Fatalf("calls: %v", api.calls)
	}
	if past, err := y.ListEpisodes(ctx, feed, 50, 5); err != nil || len(past) != 0 {
		t.Fatalf("past end: %v %d", err, len(past))
	}

	// A handle is looked up once, then its uploads playlist is remembered.
	api.reset()
	byHandle := podsync.FeedInfo{Source: Kind, Kind: "channel", ID: "ltt", URL: "https://www.youtube.com/@LinusTechTips"}
	for i := 0; i < 2; i++ {
		if eps, err := y.ListEpisodes(ctx, byHandle, 0, 3); err != nil || len(eps) != 3 {
			t.Fatalf("handle listing: %v %d", err, len(eps))
		}
	}
	if api.count("channels") != 1 {
		t.Fatalf("handle lookups: %v", api.calls)
	}
}

// TestListEpisodesReverseForOldestFirstPlaylists covers playlist_sort =
// "desc": a playlist ordered oldest first is read to its end so offset 0 is
// still its newest addition, and later pages keep going back in time.
func TestListEpisodesReverseForOldestFirstPlaylists(t *testing.T) {
	ctx := context.Background()
	y, _, api := newFakeWithAPI(t)
	oldestFirst := podsync.FeedInfo{Source: Kind, Kind: podsync.FeedPlaylist, ID: "PLsomethingCourse", Reverse: true}
	eps, err := y.ListEpisodes(ctx, oldestFirst, 0, 5)
	if err != nil || ids(eps) != "vid00000012,vid00000011,vid00000010,vid00000009,vid00000008" {
		t.Fatalf("Reverse should list the newest 5, newest first: %v %s", err, ids(eps))
	}
	if api.count("playlistItems") != 3 {
		t.Fatalf("a reversed playlist is read to the end: %v", api.calls)
	}
	next, err := y.ListEpisodes(ctx, oldestFirst, 5, 5)
	if err != nil || ids(next) != "vid00000007,vid00000006,vid00000005,vid00000004,vid00000003" {
		t.Fatalf("second page should continue backwards: %v %s", err, ids(next))
	}
	tail, err := y.ListEpisodes(ctx, oldestFirst, 10, 5)
	if err != nil || ids(tail) != "vid00000002,vid00000001" {
		t.Fatalf("clamped tail page: %v %s", err, ids(tail))
	}
	plain, _ := y.ListEpisodes(ctx, podsync.FeedInfo{Source: Kind, Kind: podsync.FeedPlaylist, ID: "PLsomethingCourse"}, 0, 2)
	if ids(plain) != "vid00000001,vid00000002" {
		t.Fatalf("without Reverse the playlist order stands: %s", ids(plain))
	}
}

func TestAPIErrors(t *testing.T) {
	ctx := context.Background()
	y, _ := newFake(t)
	_, err := y.ListEpisodes(ctx, podsync.FeedInfo{Source: Kind, Kind: podsync.FeedPlaylist, ID: "PLquotaquotaquota"}, 0, 5)
	if err == nil || !strings.Contains(err.Error(), "daily quota") {
		t.Fatalf("quota: %v", err)
	}
	_, err = y.ListEpisodes(ctx, podsync.FeedInfo{Source: Kind, Kind: podsync.FeedPlaylist, ID: "PLmissingmissing"}, 0, 5)
	if !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("missing playlist: %v", err)
	}
	api := newFakeAPI(t)
	badKey := New(func() Settings { return Settings{APIKey: "wrong-secret", APIBase: api.srv.URL} })
	_, err = badKey.ListEpisodes(ctx, feed, 0, 5)
	if err == nil || !strings.Contains(err.Error(), "not valid") || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("bad key: %v", err)
	}
	down := New(func() Settings { return Settings{APIKey: "secret-key-123", APIBase: "http://127.0.0.1:1"} })
	_, err = down.ListEpisodes(ctx, feed, 0, 5)
	if err == nil || strings.Contains(err.Error(), "secret-key-123") {
		t.Fatalf("transport errors must not leak the key: %v", err)
	}
	none := New(func() Settings { return Settings{} })
	if _, err := none.ListEpisodes(ctx, feed, 0, 5); err == nil || !strings.Contains(err.Error(), "PODSYNC_YOUTUBE_API_KEY") {
		t.Fatalf("no key: %v", err)
	}
}

func TestParseISODuration(t *testing.T) {
	for in, want := range map[string]int{"PT30S": 30, "PT10M": 600, "PT1H2M3S": 3723, "P1DT2H": 93600, "P0D": 0, "": 0, "garbage": 0} {
		if got := parseISODuration(in); got != want {
			t.Errorf("parseISODuration(%q) = %d, want %d", in, got, want)
		}
	}
}

func ids(eps []podsync.EpisodeInfo) string {
	ss := make([]string, len(eps))
	for i, e := range eps {
		ss[i] = e.ID
	}
	return strings.Join(ss, ",")
}

func searched(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "count"))
	return err == nil
}

func TestFindUsesHintsToChooseResolveOrSearch(t *testing.T) {
	ctx := context.Background()
	for _, direct := range []string{
		"@LinusTechTips", "https://www.youtube.com/@LinusTechTips", "youtube.com/@LinusTechTips/videos",
		"https://www.youtube.com/channel/UCXuqSBlHAE6Xw-yeJA0Tunw", "UCXuqSBlHAE6Xw-yeJA0Tunw",
		"https://youtu.be/jNQXAC9IVRw",
	} {
		y, dir := newFake(t)
		res, err := y.Find(ctx, "  "+direct+"  ")
		if err != nil || len(res) != 1 || res[0].ID != "UCXuqSBlHAE6Xw-yeJA0Tunw" || res[0].Kind != "channel" {
			t.Errorf("%q should resolve to one feed: %v %+v", direct, err, res)
		}
		if searched(dir) {
			t.Errorf("%q is a reference and must not trigger a name search", direct)
		}
	}
	for _, name := range []string{"linus", "Linus Tech Tips", "LinusTechTips", "PLabcdefghijk1", "kotlin @home"} {
		y, dir := newFake(t)
		res, err := y.Find(ctx, name)
		if err != nil || len(res) != 1 || res[0].Handle != "@LinusTechTips" || res[0].Kind != "channel" || res[0].Subscribers != 16900000 || res[0].ImageURL != "https://yt3.ggpht.com/a" {
			t.Errorf("%q should fall back to a channel search: %v %+v", name, err, res)
		}
		if !searched(dir) {
			t.Errorf("%q is a name and must be searched", name)
		}
	}
}

func TestFindErrorsAndSearchBehavior(t *testing.T) {
	ctx := context.Background()
	y, dir := newFake(t)
	if _, err := y.Find(ctx, "   "); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("empty query: %v", err)
	}
	if _, err := y.Find(ctx, "https://example.com/@x"); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("non-YouTube link: %v", err)
	}
	// Playlists are not searched: results are channels only, deduplicated (the fake returns a duplicate and a video).
	res, _ := y.Find(ctx, "linus")
	if len(res) != 1 {
		t.Fatalf("channels only, deduplicated: %+v", res)
	}
	// Identical searches are cached.
	os.Remove(filepath.Join(dir, "count"))
	if _, err := y.Find(ctx, "LINUS"); err != nil {
		t.Fatal(err)
	}
	if searched(dir) {
		t.Fatal("second identical search should be served from cache")
	}

	// A failing reference is the user's input (invalid); a failing search is not, and backs off.
	bad, _ := newFake(t, "FAKE_FAIL=1")
	if _, err := bad.Find(ctx, "x y"); err == nil || errors.Is(err, podsync.ErrInvalid) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a failed search is a service problem, got %v", err)
	}
	if _, err := bad.Find(ctx, "y z"); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("expected backoff error, got %v", err)
	}
	bad.now = func() time.Time { return time.Now().Add(time.Minute) }
	if _, err := bad.Find(ctx, "z w"); err == nil || strings.Contains(err.Error(), "paused") {
		t.Fatalf("backoff should have expired, got %v", err)
	}
	if res, err := y.Find(ctx, "https://www.youtube.com/playlist?list=PLabcdefghijk"); err != nil || len(res) != 1 || res[0].Kind != "playlist" {
		t.Fatalf("a playlist link resolves to that playlist: %v %+v", err, res)
	}
	if _, err := y.Find(ctx, "https://www.youtube.com/playlist?list=PLmissingmissing"); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("a link that does not resolve is invalid input: %v", err)
	}
}

func TestDownloadSuccessAndFailure(t *testing.T) {
	y, _ := newFake(t)
	dest := t.TempDir()
	res, err := y.Download(context.Background(), podsync.EpisodeInfo{ID: "abcdEFGhij1", Title: "placeholder"}, podsync.DownloadOptions{}, dest)
	if err != nil {
		t.Fatal(err)
	}
	if res.File != "abcdEFGhij1.mp4" || res.Size != int64(len("media-bytes")) || res.Episode.Title != "Real Title" || res.Episode.DurationSec != 321 || res.Episode.Published.Unix() != 1700000000 {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(dest, "abcdEFGhij1.info.json")); err == nil {
		t.Fatal("info json should be removed")
	}
	if res.Thumb != "abcdEFGhij1.jpg" || res.Episode.Channel != "Chan Name" || res.Episode.ViewCount != 4321 || res.Episode.Height != 720 || !strings.Contains(res.Episode.Thumbnail, "maxresdefault") {
		t.Fatalf("metadata not extracted: %+v", res)
	}
	audio, err := y.Download(context.Background(), podsync.EpisodeInfo{ID: "abcdEFGhij2"}, podsync.DownloadOptions{Audio: true}, dest)
	if err != nil || audio.File != "abcdEFGhij2.m4a" || audio.Episode.Height != 0 {
		t.Fatalf("audio downloads have no video height: %v %+v", err, audio)
	}
	noThumb, _ := newFake(t, "FAKE_NO_THUMB=1")
	nt, err := noThumb.Download(context.Background(), podsync.EpisodeInfo{ID: "abcdEFGhij9"}, podsync.DownloadOptions{}, t.TempDir())
	if err != nil || nt.Thumb != "" || nt.File == "" {
		t.Fatalf("a missing thumbnail must not fail the download: %v %+v", err, nt)
	}
	if _, err := y.Download(context.Background(), podsync.EpisodeInfo{ID: "../../etc"}, podsync.DownloadOptions{}, dest); err == nil {
		t.Fatal("bad id must be rejected")
	}

	bad, _ := newFake(t, "FAKE_FAIL=1")
	d2 := t.TempDir()
	_, err = bad.Download(context.Background(), podsync.EpisodeInfo{ID: "abcdEFGhij3"}, podsync.DownloadOptions{}, d2)
	if err == nil || !strings.Contains(err.Error(), "Video unavailable") {
		t.Fatalf("%v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(d2, "*")); len(left) != 0 {
		t.Fatalf("partials should be cleaned: %v", left)
	}
}

func TestDownloadMembersOnly(t *testing.T) {
	ctx := context.Background()
	members, dir := newFake(t, "FAKE_FAIL=members")
	// Flagged by the listing and no cookies: refused without running yt-dlp.
	_, err := members.Download(ctx, podsync.EpisodeInfo{ID: "abcdEFGhij3", MembersOnly: true}, podsync.DownloadOptions{}, t.TempDir())
	if !errors.Is(err, podsync.ErrMembersOnly) {
		t.Fatalf("want ErrMembersOnly, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "count")); statErr == nil {
		t.Fatal("yt-dlp must not run for a members-only video without cookies")
	}
	// Not flagged (or cookies present): yt-dlp's members-only error is recognised.
	_, err = members.Download(ctx, podsync.EpisodeInfo{ID: "abcdEFGhij3"}, podsync.DownloadOptions{}, t.TempDir())
	if !errors.Is(err, podsync.ErrMembersOnly) || errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("want ErrMembersOnly from the yt-dlp message, got %v", err)
	}
	withCookies := New(func() Settings { return Settings{CookiesFile: filepath.Join(dir, "cookies.txt")} })
	_, err = withCookies.Download(ctx, podsync.EpisodeInfo{ID: "abcdEFGhij3", MembersOnly: true}, podsync.DownloadOptions{}, t.TempDir())
	if !errors.Is(err, podsync.ErrMembersOnly) {
		t.Fatalf("with cookies yt-dlp runs and its refusal is still recognised: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "count")); statErr != nil {
		t.Fatal("with cookies, yt-dlp must be tried")
	}
}

// Live tests hit real YouTube through the installed yt-dlp.
// Run with: PODSYNC_LIVE=1 PODSYNC_YOUTUBE_API_KEY=... go test ./youtube -run Live -v
func TestLive(t *testing.T) {
	if os.Getenv("PODSYNC_LIVE") != "1" {
		t.Skip("set PODSYNC_LIVE=1 to run against real YouTube")
	}
	key := os.Getenv("PODSYNC_YOUTUBE_API_KEY")
	if key == "" {
		t.Skip("set PODSYNC_YOUTUBE_API_KEY")
	}
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		t.Skip("yt-dlp not installed")
	}
	y := New(func() Settings { return Settings{APIKey: key} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	info, err := y.Resolve(ctx, "@LinusTechTips")
	if err != nil || info.ID != "UCXuqSBlHAE6Xw-yeJA0Tunw" || info.Kind != "channel" || info.ImageURL == "" {
		t.Fatalf("resolve: %v %+v", err, info)
	}
	t.Logf("resolved: %+v", info)
	eps, err := y.ListEpisodes(ctx, info, 0, 5)
	if err != nil || len(eps) != 5 {
		t.Fatalf("list: %v %d", err, len(eps))
	}
	more, err := y.ListEpisodes(ctx, info, 5, 3)
	if err != nil || len(more) != 3 || more[0].ID == eps[0].ID {
		t.Fatalf("offset list: %v %+v", err, more)
	}
	res, err := y.Find(ctx, "linus tech tips")
	if err != nil || len(res) == 0 || res[0].Kind != "channel" || res[0].Handle == "" {
		t.Fatalf("search: %v %+v", err, res)
	}
	t.Logf("first search hit: %+v", res[0])
	byHandle, err := y.Find(ctx, "@LinusTechTips")
	if err != nil || len(byHandle) != 1 || byHandle[0].ID != info.ID {
		t.Fatalf("handle: %v %+v", err, byHandle)
	}
	byURL, err := y.Find(ctx, "https://www.youtube.com/playlist?list=PL8mG-RkN2uTwt8wqaIaIH-mCANbhDLmjv")
	if err != nil || len(byURL) != 1 || byURL[0].Kind != "playlist" {
		t.Fatalf("playlist url: %v %+v", err, byURL)
	}
	t.Logf("playlist by url: %+v", byURL[0])
	v, err := y.Resolve(ctx, "https://www.youtube.com/watch?v=jNQXAC9IVRw")
	if err != nil || v.ID != "UC4QobU6STFB0P71PMvOGN5A" {
		t.Fatalf("video->channel: %v %+v", err, v)
	}

	dest := t.TempDir()
	dl, err := y.Download(ctx, podsync.EpisodeInfo{ID: "jNQXAC9IVRw"}, podsync.DownloadOptions{MaxVideoHeight: 360}, dest)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	t.Logf("download: %+v", dl)
	if dl.Episode.Title == "" || dl.Episode.Published.IsZero() || dl.Size < 10_000 || dl.Thumb == "" || dl.Episode.Channel == "" || dl.Episode.ViewCount == 0 || dl.Episode.Height == 0 {
		t.Fatalf("incomplete: %+v", dl)
	}
	if st, err := os.Stat(filepath.Join(dest, dl.Thumb)); err != nil || st.Size() < 1000 {
		t.Fatalf("thumbnail not saved: %v", err)
	}
	aud, err := y.Download(ctx, podsync.EpisodeInfo{ID: "jNQXAC9IVRw"}, podsync.DownloadOptions{Audio: true}, t.TempDir())
	if err != nil || !strings.HasSuffix(aud.File, ".m4a") {
		t.Fatalf("audio download: %v %+v", err, aud)
	}
}
