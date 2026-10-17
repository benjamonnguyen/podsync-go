package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"podsync"
	"podsync/fs"
	"podsync/mock"
	"podsync/rest"
	"podsync/status"
	"podsync/syncer"
	"podsync/toml"
)

// harness models the real deployment: the sync service and the web UI are
// separate programs, each with its own ConfigStore and Library over one
// shared config file and data directory, talking over the REST API.
var bg = context.Background()

type harness struct {
	t        *testing.T
	h        http.Handler // the web UI
	cfg      *toml.Store  // the web UI's view of the config file
	lib      *fs.Library  // the web UI's view of the data directory
	cfgPath  string
	syncrCfg *toml.Store // the sync service's view
	syncrLib *fs.Library // the sync service's view
	svc      *syncer.Service
	src      *mock.Source
	st       *status.Tracker
	sync     *rest.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	c := podsync.Default()
	c.Server.DataDir = filepath.Join(dir, "data")
	c.Defaults.PageSize = 3
	c.Defaults.CleanupKeepLast = 0
	c.Server.BaseURL = "http://pod.example:8080"
	c.Feeds = []podsync.Feed{{ID: "UCone", Name: "Channel One", Kind: "channel", Source: "youtube"}}
	c.Normalize()
	cfg, err := toml.Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(x *podsync.Config) error { *x = c; return nil }); err != nil {
		t.Fatal(err)
	}
	syncrCfg, err := toml.Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	lib, syncrLib := fs.New(c.Server.DataDir), fs.New(c.Server.DataDir)
	for _, l := range []*fs.Library{lib, syncrLib} {
		if err := l.Load(); err != nil {
			t.Fatal(err)
		}
	}
	src := mock.NewSource()
	for i := 0; i < 8; i++ {
		src.Episodes = append(src.Episodes, podsync.EpisodeInfo{
			ID: fmt.Sprintf("vid%07d", i), Title: fmt.Sprintf("Video %d", i),
			Thumbnail: fmt.Sprintf("https://i.ytimg.com/vi/vid%07d/hq.jpg", i), ViewCount: 2500, Channel: "Remote Chan",
		})
	}
	newInfo := podsync.FeedInfo{Source: "youtube", Kind: "channel", ID: "UCnew", Name: "New <Channel>", Description: "about", ImageURL: "https://img/x.jpg", Handle: "@new", Subscribers: 1500000}
	src.FindFn = func(q string) ([]podsync.FeedInfo, error) { // stands in for the adapter's hints
		switch q {
		case "":
			return nil, podsync.Invalid("type a channel name, an @handle or a YouTube link")
		case "nope":
			return nil, podsync.Invalid("not a recognized channel URL")
		case "boom":
			return nil, errors.New("boom")
		case "@new", "UCnew":
			return []podsync.FeedInfo{newInfo}, nil
		case "found":
			return []podsync.FeedInfo{
				{Source: "youtube", Kind: "channel", ID: "UCnew", Name: "Found One", Handle: "@found", Subscribers: 1500000},
				{Source: "youtube", Kind: "channel", ID: "UCtwo", Name: "Found Two", Handle: "@two", Subscribers: 2000},
			}, nil
		case "lonely":
			return []podsync.FeedInfo{newInfo}, nil
		}
		return []podsync.FeedInfo{}, nil
	}
	st := status.New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc := syncer.New(ctx, syncrCfg, syncrLib, podsync.NewRegistry(src), st)
	syncrCfg.OnChange(svc.ConfigChanged)
	svc.Reconcile()
	api := httptest.NewServer(rest.NewServer(svc).Handler())
	t.Cleanup(api.Close)
	client := rest.NewClient(api.URL, nil)
	ui, err := newServer(options{Config: cfg, Library: lib, Syncer: client})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t, ui.Handler(), cfg, lib, cfgPath, syncrCfg, syncrLib, svc, src, st, client}
}

func (h *harness) do(method, path string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

func (h *harness) get(path string) *httptest.ResponseRecorder { return h.do("GET", path, nil) }

func (h *harness) mustContain(rec *httptest.ResponseRecorder, code int, subs ...string) {
	h.t.Helper()
	body := rec.Body.String()
	if rec.Code != code {
		h.t.Fatalf("status %d (want %d): %s", rec.Code, code, body)
	}
	for _, s := range subs {
		if !strings.Contains(body, s) {
			h.t.Errorf("missing %q in:\n%s", s, body)
		}
	}
}

// holdDownloads makes downloads block, so a row can be inspected while in flight.
func (h *harness) holdDownloads() chan struct{} {
	gate := make(chan struct{})
	h.src.With(func(m *mock.Source) { m.Gate = gate })
	return gate
}

func (h *harness) releaseDownloads(gate chan struct{}) {
	close(gate)
	h.src.With(func(m *mock.Source) { m.Gate = nil })
	h.waitIdle()
}

func (h *harness) waitIdle() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a := h.svc.Activity()
		if !a.Pulling && a.Downloading == 0 && a.Queued == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("service never went idle")
}

func TestLibraryPageAndSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/")
	h.mustContain(rec, 200, "Channel One", `href="/feed/UCone"`, "Check all feeds now", `hx-get="/status?v=`)
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing security headers: %v", rec.Header())
	}
	h.mustContain(h.get("/static/htmx.min.js"), 200)
	h.mustContain(h.get("/nope"), 404)
	h.mustContain(h.get("/healthz"), 200, "ok")
}

func TestCrossSiteWritesAreBlocked(t *testing.T) {
	h := newHarness(t)
	for _, hdr := range [][]string{{"Sec-Fetch-Site", "cross-site"}, {"Origin", "http://evil.example"}} {
		rec := h.do("POST", "/feed/UCone/delete", url.Values{"delete_files": {"on"}}, hdr...)
		if rec.Code != 403 {
			t.Fatalf("%v: got %d", hdr, rec.Code)
		}
	}
	if _, ok := h.cfg.Get().Feed("UCone"); !ok {
		t.Fatal("feed must survive a blocked request")
	}
	// Same-origin requests pass.
	h.mustContain(h.do("POST", "/status/dismiss", url.Values{"key": {"x"}}, "Origin", "http://example.com", "Sec-Fetch-Site", "same-origin"), 200)
}

func TestPullFeedThenBrowseEpisodes(t *testing.T) {
	h := newHarness(t)
	h.mustContain(h.do("POST", "/feed/UCone/pull", nil), 200)
	h.waitIdle()

	rec := h.get("/feed/UCone")
	h.mustContain(rec, 200, "Channel One", "Title vid0000000", "Title vid0000002", "http://pod.example:8080/feeds/UCone.xml", `hx-get="/feed/UCone/older?offset=3"`, "Load more")
	if strings.Contains(rec.Body.String(), "vid0000003") {
		t.Fatal("only page_size episodes should be downloaded")
	}
	h.mustContain(h.get("/feed/UCone/episodes?q=0000001"), 200, "Title vid0000001")
	if body := h.get("/feed/UCone/episodes?q=zzz").Body.String(); !strings.Contains(body, "No episodes match") {
		t.Fatalf("empty filter: %s", body)
	}

	// Media and thumbnails live on syncr's public address; the UI links to them there.
	h.mustContain(rec, 200, `href="http://pod.example:8080/media/UCone/vid0000000.mp4"`, `src="http://pod.example:8080/media/UCone/vid0000000.jpg"`)

	h.mustContain(h.get("/search?q=0000002"), 200, "Title vid0000002", "Episodes matching")
	h.mustContain(h.get("/search?q=channel+one"), 200, "Channel One")
	h.mustContain(h.get("/search?q="), 200, "Channel One")
}

func TestThumbnailsAndMetadataInUI(t *testing.T) {
	h := newHarness(t)
	h.svc.PullFeed("UCone")
	page := h.get("/feed/UCone")
	h.mustContain(page, 200,
		`<img class="thumb" src="http://pod.example:8080/media/UCone/vid0000000.jpg"`, "1.2M views", "480p", "<summary>Details</summary>",
		"Line one\nLine &lt;two&gt;", `href="https://youtu.be/vid0000000"`)

	// Thumbnails survive cleanup; the media does not.
	h.cfg.Update(func(c *podsync.Config) error { c.Defaults.CleanupKeepLast = 1; return nil })
	if _, err := h.sync.Apply(bg); err != nil { // the sync service re-reads the config the web UI wrote
		t.Fatal(err)
	}
	h.svc.Cleanup("UCone")
	cleaned := h.get("/feed/UCone").Body.String()
	if !strings.Contains(cleaned, `src="http://pod.example:8080/media/UCone/vid0000002.jpg"`) || !strings.Contains(cleaned, "removed by cleanup") {
		t.Fatalf("cleaned row should keep its thumbnail:\n%s", cleaned)
	}
	// Load More rows show the remote thumbnail, views and channel metadata; downloading keeps the hint.
	older := h.get("/feed/UCone/older?offset=3")
	h.mustContain(older, 200, `src="https://i.ytimg.com/vi/vid0000003/hq.jpg"`, "2.5K views")
	gate := h.holdDownloads()
	dl := h.do("POST", "/feed/UCone/episodes/vid0000003/download", url.Values{
		"title": {"Video 3"}, "thumb": {"https://i.ytimg.com/vi/vid0000003/hq.jpg"}, "views": {"2500"}, "channel": {"Remote Chan"},
	})
	h.mustContain(dl, 200, `src="https://i.ytimg.com/vi/vid0000003/hq.jpg"`, "downloading", `hx-trigger="every 2s"`,
		// dynamic values in hx-get must be URL-encoded (html/template does not treat hx-* as URL attributes)
		`row?title=Video&#43;3&amp;duration=0&amp;thumb=https%3A%2F%2Fi.ytimg.com%2Fvi%2Fvid0000003%2Fhq.jpg&amp;channel=Remote&#43;Chan&amp;views=2500"`)
	bad := h.do("POST", "/feed/UCone/episodes/vid0000004/download", url.Values{"title": {"Video 4"}, "thumb": {"javascript:alert(1)"}})
	if strings.Contains(bad.Body.String(), "javascript:") {
		t.Fatalf("non-https thumbnail hint must be ignored: %s", bad.Body.String())
	}
	h.releaseDownloads(gate)
}

func TestLoadMoreShowsFadedRowsAndDownloadWorks(t *testing.T) {
	h := newHarness(t)
	h.svc.PullFeed("UCone")

	rec := h.get("/feed/UCone/older?offset=3")
	h.mustContain(rec, 200, "faded", "Video 3", "Video 5", "Download</button>", `hx-get="/feed/UCone/older?offset=6"`)
	if strings.Contains(rec.Body.String(), "Video 0") {
		t.Fatal("already-downloaded episodes must not be offered")
	}
	last := h.get("/feed/UCone/older?offset=6")
	h.mustContain(last, 200, "Video 6", "Video 7", "the end of the feed")
	if strings.Contains(last.Body.String(), "Load more") {
		t.Fatal("no Load more button at the end of the feed")
	}

	gate := h.holdDownloads()
	dl := h.do("POST", "/feed/UCone/episodes/vid0000004/download", url.Values{"title": {"Video 4"}, "duration": {"60"}})
	h.mustContain(dl, 200, "Video 4", "faded", "downloading")
	h.releaseDownloads(gate)
	row := h.get("/feed/UCone/episodes/vid0000004/row?title=Video+4")
	h.mustContain(row, 200, "Title vid0000004", `href="http://pod.example:8080/media/UCone/vid0000004.mp4"`, "kept")
	if strings.Contains(row.Body.String(), "faded") {
		t.Fatal("downloaded row should not be faded")
	}
	// It is now part of the main list and no longer offered by Load More.
	if strings.Contains(h.get("/feed/UCone/older?offset=3").Body.String(), "Video 4") {
		t.Fatal("downloaded episode still offered")
	}
	h.mustContain(h.do("POST", "/feed/UCone/episodes/..%2Fx/download", url.Values{}), 400)
	h.mustContain(h.get("/feed/UCone/older?offset=abc"), 200)

	h.src.With(func(m *mock.Source) { m.ListErr = errors.New("HTTP Error 429") })
	h.mustContain(h.get("/feed/UCone/older?offset=3"), 200, "Could not load more", "429", "Try again")
	h.mustContain(h.get("/status"), 200, "Could not list episodes", "429")
}

func TestStatusBannerDismissAndBusy(t *testing.T) {
	h := newHarness(t)
	h.st.Report("health:youtube", podsync.LevelError, "yt-dlp not found at \"/x\"")
	h.st.Report("selfupdate:youtube", podsync.LevelWarning, "Self-update failed")
	rec := h.get("/status")
	h.mustContain(rec, 200, `class="banner error"`, `class="banner warning"`, "yt-dlp not found")
	h.mustContain(h.do("POST", "/status/dismiss", url.Values{"key": {"health:youtube"}}), 200, "Self-update failed")
	if strings.Contains(h.get("/status").Body.String(), "yt-dlp not found") {
		t.Fatal("dismissed issue still shown")
	}
	h.mustContain(h.get("/"), 200, "Self-update failed")
}

func TestStatusTriggersRefreshOnlyWhenLibraryChanged(t *testing.T) {
	h := newHarness(t)
	h.get("/status") // let the web UI's library catch up with the sync service's startup
	cur := h.lib.Version()
	rec := h.get(fmt.Sprintf("/status?v=%d", cur))
	if rec.Header().Get("HX-Trigger") != "" {
		t.Fatal("unchanged library must not trigger a refresh")
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprintf(`hx-get="/status?v=%d"`, cur)) {
		t.Fatalf("poll URL must carry the current version: %s", rec.Body.String())
	}
	h.svc.PullFeed("UCone")
	if rec := h.get(fmt.Sprintf("/status?v=%d", cur)); rec.Header().Get("HX-Trigger") != "episodes-changed" {
		t.Fatalf("stale version should trigger a refresh, headers: %v", rec.Header())
	}
	if rec := h.get("/status"); rec.Header().Get("HX-Trigger") != "" {
		t.Fatal("no version param means no trigger")
	}
}

func TestAddFlow(t *testing.T) {
	h := newHarness(t)
	h.mustContain(h.get("/add"), 200, `name="q"`, "Playlists need their link")
	if page := h.get("/add").Body.String(); strings.Contains(page, `name="kind"`) || strings.Contains(page, "/add/search") || strings.Contains(page, "/add/preview") {
		t.Fatalf("there is one lookup box now:\n%s", page)
	}

	// Several matches are listed; Select looks the chosen one up again by its ID.
	list := h.do("POST", "/add/find", url.Values{"q": {"found"}})
	h.mustContain(list, 200, "Found One", "Found Two", "@found", "1.5M subscribers", `hx-post="/add/find"`, `name="q" value="UCnew"`)
	if strings.Contains(list.Body.String(), `name="id" value=`) {
		t.Fatal("a list is not a preview")
	}
	// One match goes straight to the preview, whether it came from a reference or a name search.
	for _, q := range []string{"@new", "UCnew", "lonely"} {
		h.mustContain(h.do("POST", "/add/find", url.Values{"q": {q}}), 200, "New &lt;Channel&gt;", "@new · 1.5M subscribers", `name="id" value="UCnew"`, "Add feed")
	}
	h.mustContain(h.do("POST", "/add/find", url.Values{"q": {"zero"}}), 200, "No channels found for “zero”")

	// Mistyped references are shown inline; service problems also reach the banner and clear on success.
	h.mustContain(h.do("POST", "/add/find", url.Values{"q": {"nope"}}), 200, "not a recognized channel URL")
	h.mustContain(h.do("POST", "/add/find", url.Values{"q": {""}}), 200, "type a channel name")
	if strings.Contains(h.get("/status").Body.String(), "Looking up feeds failed") {
		t.Fatal("a mistyped link must not raise a banner")
	}
	h.mustContain(h.do("POST", "/add/find", url.Values{"q": {"boom"}}), 200, "boom")
	h.mustContain(h.get("/status"), 200, "Looking up feeds failed")
	h.do("POST", "/add/find", url.Values{"q": {"found"}})
	if strings.Contains(h.get("/status").Body.String(), "Looking up feeds failed") {
		t.Fatal("the issue should clear after a success")
	}

	// A forged id with no preview is refused.
	h.mustContain(h.do("POST", "/add", url.Values{"id": {"UCforged"}}), 200, "preview expired")
	if _, ok := h.cfg.Get().Feed("UCforged"); ok {
		t.Fatal("forged feed added")
	}

	h.mustContain(h.do("POST", "/add", url.Values{"id": {"UCnew"}, "page_size": {"x"}}), 200, "must be a number")
	rec := h.do("POST", "/add", url.Values{"id": {"UCnew"}, "page_size": {"2"}}, "HX-Request", "true")
	if rec.Code != 204 || rec.Header().Get("HX-Redirect") != "/feed/UCnew" {
		t.Fatalf("add result: %d %v", rec.Code, rec.Header())
	}
	f, ok := h.cfg.Get().Feed("UCnew")
	if !ok || f.Name != "New <Channel>" || f.PageSize == nil || *f.PageSize != 2 || f.Source != "youtube" {
		t.Fatalf("saved feed: %+v ok=%v", f, ok)
	}
	h.waitIdle()
	if n := len(h.syncrLib.Episodes("UCnew")); n != 2 {
		t.Fatalf("first pull should fetch page_size=2 episodes, got %d", n)
	}
	sum, _ := h.syncrLib.Feed("UCnew")
	if sum.Meta.ImageURL != "https://img/x.jpg" || sum.Meta.Description != "about" {
		t.Fatalf("meta: %+v", sum.Meta)
	}
	h.mustContain(h.do("POST", "/add/find", url.Values{"q": {"@new"}}), 200, "Already in your library")
	h.mustContain(h.do("POST", "/add", url.Values{"id": {"UCnew"}}), 200, "already in your library")
}

func TestSettingsSaveValidationAndApply(t *testing.T) {
	h := newHarness(t)
	h.mustContain(h.get("/settings"), 200, `name="update_interval" value="30m"`, `name="min_duration" value="61s"`, `name="max_resolution" min="0" value="720"`)

	form := func() url.Values {
		return url.Values{
			"concurrency": {"3"}, "update_interval": {"15m"},
			"format": {"audio"}, "max_resolution": {"480"}, "min_duration": {"90s"},
			"page_size": {"4"}, "cleanup_keep_last": {"20"},
		}
	}
	rec := h.do("POST", "/settings", form())
	h.mustContain(rec, 200, "Saved. Changes are applied.")
	got := h.cfg.Get()
	if got.Download.Concurrency != 3 || got.Download.UpdateInterval.Std() != 15*time.Minute || got.Defaults.Format != "audio" || got.Defaults.PageSize != 4 ||
		got.Defaults.CleanupKeepLast != 20 || got.Defaults.MaxResolution != 480 || got.Defaults.MinDuration.Std() != 90*time.Second || got.Download.SelfUpdate {
		t.Fatalf("not applied: %+v %+v", got.Download, got.Defaults)
	}
	h.mustContain(h.get("/settings"), 200, `name="min_duration" value="90s"`)

	// Durations may be bare seconds; 0 means no minimum.
	f := form()
	f.Set("min_duration", "45")
	h.mustContain(h.do("POST", "/settings", f), 200, "Saved")
	if h.cfg.Get().Defaults.MinDuration.Std() != 45*time.Second {
		t.Fatalf("%v", h.cfg.Get().Defaults.MinDuration)
	}
	f.Set("min_duration", "0")
	h.mustContain(h.do("POST", "/settings", f), 200, "Saved")
	if h.cfg.Get().Defaults.MinDuration != 0 {
		t.Fatal("0 = no minimum")
	}

	f = form()
	f.Set("page_size", "0")
	f.Set("update_interval", "soon")
	f.Set("min_duration", "short")
	rec = h.do("POST", "/settings", f)
	h.mustContain(rec, 200, "Not saved", "page_size", "update_interval", "min_duration", `name="page_size" min="1" max="500" value="0"`)
	if h.cfg.Get().Defaults.PageSize != 4 {
		t.Fatal("invalid save must not change config")
	}
	f = form()
	f.Del("min_duration")
	h.mustContain(h.do("POST", "/settings", f), 200, "Not saved", "min_duration is required")

	// The sync service picked the new values up through Apply.
	if sc := h.syncrCfg.Get(); sc.Download.Concurrency != 3 || sc.Defaults.PageSize != 4 {
		t.Fatalf("the sync service must have applied the web UI's settings: %+v", sc.Download)
	}
}

// What belongs to whoever runs the service is shown, never editable: not by form, not by a forged post.
func TestSettingsPageDoesNotEditOperatorSettings(t *testing.T) {
	h := newHarness(t)
	h.cfg.Update(func(c *podsync.Config) error { c.Download.CookiesFile = "/config/cookies.txt"; return nil })
	page := h.get("/settings").Body.String()
	for _, field := range []string{"base_url", "yt_dlp_path", "cookies_file", "listen_addr", "api_addr", "data_dir", "title_include", "title_exclude", "cleanup_after_days"} {
		if strings.Contains(page, `name="`+field+`"`) {
			t.Errorf("%s must not be an editable field", field)
		}
	}
	for _, shown := range []string{"<code>:8080</code>", "<code>127.0.0.1:8081</code>", "<code>http://pod.example:8080</code>", "<code>yt-dlp</code>", "<code>/config/cookies.txt</code>"} {
		if !strings.Contains(page, shown) {
			t.Errorf("settings page should show %s read-only:\n%s", shown, page)
		}
	}

	before := h.cfg.Get()
	forged := url.Values{
		"base_url": {"http://evil.example"}, "yt_dlp_path": {"/bin/sh"}, "cookies_file": {"/etc/passwd"},
		"listen_addr": {":1"}, "api_addr": {":2"}, "data_dir": {"/tmp/elsewhere"},
		"concurrency": {"2"}, "update_interval": {"30m"}, "format": {"video"}, "max_resolution": {"720"}, "min_duration": {"61s"},
		"page_size": {"3"}, "cleanup_keep_last": {"0"}, "cleanup_after_days": {"30"}, "title_include": {"x"}, "title_exclude": {"y"},
	}
	h.mustContain(h.do("POST", "/settings", forged), 200, "Saved")
	if raw, _ := os.ReadFile(h.cfgPath); strings.Contains(string(raw), "cleanup_after_days") || strings.Contains(string(raw), "title_") {
		t.Fatalf("there are no global patterns or age-based cleanup; a forged post must not create them:\n%s", raw)
	}
	after := h.cfg.Get()
	if after.Server != before.Server || after.Download.YtDlpPath != before.Download.YtDlpPath || after.Download.CookiesFile != before.Download.CookiesFile {
		t.Fatalf("a forged post changed operator settings: %+v -> %+v", before.Server, after.Server)
	}
}

func TestSettingsSavedEvenWhenSyncServiceIsDown(t *testing.T) {
	h := newHarness(t)
	dead := rest.NewClient("http://127.0.0.1:1", nil)
	ui, err := newServer(options{Config: h.cfg, Library: h.lib, Syncer: dead})
	if err != nil {
		t.Fatal(err)
	}
	h = &harness{t: t, h: ui.Handler(), cfg: h.cfg, lib: h.lib}
	rec := h.do("POST", "/settings", url.Values{
		"concurrency": {"5"}, "update_interval": {"30m"},
		"format": {"video"}, "max_resolution": {"720"}, "min_duration": {"60s"}, "page_size": {"3"}, "cleanup_keep_last": {"0"},
	})
	h.mustContain(rec, 200, "Saved, but the sync service could not be reached")
	if h.cfg.Get().Download.Concurrency != 5 {
		t.Fatal("the change must be saved even though the sync service could not be told")
	}
}

func TestFeedSettingsOverridesAndInherit(t *testing.T) {
	h := newHarness(t)
	page := h.get("/feed/UCone/settings")
	h.mustContain(page, 200, "Default (video)", `placeholder="3"`, `name="min_duration" value="" placeholder="61s"`, `name="title_include"`, `name="title_exclude"`)
	if strings.Contains(page.Body.String(), "cleanup_after_days") {
		t.Fatal("age-based cleanup no longer exists")
	}
	form := url.Values{"name": {"Renamed"}, "format": {"audio"}, "max_resolution": {""}, "min_duration": {"0"}, "title_include": {" review "}, "title_exclude": {"(?i)live"},
		"page_size": {"7"}, "cleanup_keep_last": {""}}
	h.mustContain(h.do("POST", "/feed/UCone/settings", form), 200, "Saved", `name="min_duration" value="0s"`, `name="title_exclude" value="(?i)live"`)
	f, _ := h.cfg.Get().Feed("UCone")
	eff := f.Effective(h.cfg.Get().Defaults)
	if f.Name != "Renamed" || eff.Format != "audio" || f.MinDuration == nil || eff.MinDuration != 0 || eff.PageSize != 7 || eff.MaxResolution != 720 || f.MaxResolution != nil ||
		f.TitleInclude != "review" || f.TitleExclude != "(?i)live" {
		t.Fatalf("%+v / %+v", f, eff)
	}
	// The patterns are the feed's own: they live on the feed entry in the file, never in [defaults].
	raw, _ := os.ReadFile(h.cfgPath)
	if before, _, _ := strings.Cut(string(raw), "[[feeds]]"); strings.Contains(before, "title_") || !strings.Contains(string(raw), `title_exclude = "(?i)live"`) {
		t.Fatalf("title patterns must be written on the feed only:\n%s", raw)
	}
	// The sync service applies them.
	if sf, _ := h.syncrCfg.Get().Feed("UCone"); sf.TitleExclude != "(?i)live" {
		t.Fatalf("syncr must have applied the feed's patterns: %+v", sf)
	}
	// Blank patterns clear them; blank min_duration inherits again; bare numbers are seconds.
	form.Set("min_duration", "")
	form.Set("title_include", "")
	form.Set("title_exclude", "")
	h.mustContain(h.do("POST", "/feed/UCone/settings", form), 200, "Saved")
	if f, _ := h.cfg.Get().Feed("UCone"); f.MinDuration != nil || f.TitleInclude != "" || f.TitleExclude != "" {
		t.Fatalf("blank must inherit/clear: %+v", f)
	}
	form.Set("min_duration", "45")
	h.mustContain(h.do("POST", "/feed/UCone/settings", form), 200, "Saved")
	if f, _ := h.cfg.Get().Feed("UCone"); f.MinDuration == nil || f.MinDuration.Std() != 45*time.Second {
		t.Fatalf("%+v", f.MinDuration)
	}
	form.Set("min_duration", "x")
	h.mustContain(h.do("POST", "/feed/UCone/settings", form), 200, "Not saved", "min_duration")
	bad := url.Values{"name": {"X"}, "title_include": {"("}}
	h.mustContain(h.do("POST", "/feed/UCone/settings", bad), 200, "Not saved", "title_include", "regular expression")
	h.mustContain(h.get("/feed/UCnone/settings"), 404)
}

func TestDeleteFeedKeepsOrDeletesFiles(t *testing.T) {
	h := newHarness(t)
	h.svc.PullFeed("UCone")
	h.mustContain(h.get("/feed/UCone/delete"), 200, "Delete “Channel One”?", "3 downloaded episodes")
	h.mustContain(h.do("POST", "/feed/UCone/delete", url.Values{}), 303)
	if _, ok := h.cfg.Get().Feed("UCone"); ok {
		t.Fatal("feed still configured")
	}
	// Files remain and show as orphaned.
	h.mustContain(h.get("/"), 200, "Not in config", "Delete files")
	dir, _ := h.syncrLib.FeedDir("UCone")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("files should remain")
	}
	h.mustContain(h.do("POST", "/orphans/UCone/delete", nil), 303)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("orphan files should be deleted")
	}

	h2 := newHarness(t)
	h2.svc.PullFeed("UCone")
	dir2, _ := h2.syncrLib.FeedDir("UCone")
	h2.do("POST", "/feed/UCone/delete", url.Values{"delete_files": {"on"}})
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Fatal("files should be deleted with the checkbox")
	}
	h2.do("POST", "/orphans/UCone/delete", nil) // no-op on missing
	h3 := newHarness(t)
	h3.mustContain(h3.do("POST", "/orphans/UCone/delete", nil), 400)
}

func TestForcePullNotices(t *testing.T) {
	h := newHarness(t)
	h.mustContain(h.do("POST", "/pull", nil), 200, "Checking all feeds")
	h.waitIdle()
	h.mustContain(h.do("POST", "/feed/UCnone/pull", nil), 404)
}

func TestSyncServiceOutageIsVisibleButBrowsingStillWorks(t *testing.T) {
	h := newHarness(t)
	h.svc.PullFeed("UCone")
	dead := rest.NewClient("http://127.0.0.1:1", nil)
	ui, err := newServer(options{Config: h.cfg, Library: h.lib, Syncer: dead})
	if err != nil {
		t.Fatal(err)
	}
	d := &harness{t: t, h: ui.Handler()}
	// The library and feeds come from the shared files, so they keep working...
	d.mustContain(d.get("/"), 200, "Channel One", "Cannot reach the sync service")
	d.mustContain(d.get("/feed/UCone"), 200, "Title vid0000000")
	// ...the banner explains why nothing new arrives, and cannot be dismissed...
	rec := d.get("/status")
	d.mustContain(rec, 200, `class="banner error"`, "Cannot reach the sync service")
	if strings.Contains(rec.Body.String(), "/status/dismiss") {
		t.Fatal("an outage banner must not offer Dismiss")
	}
	// ...and actions that need the service say so instead of failing silently.
	d.mustContain(d.do("POST", "/pull", nil), 200, "unreachable")
	d.mustContain(d.get("/feed/UCone/older?offset=3"), 200, "Could not load more", "unreachable")
	d.mustContain(d.do("POST", "/add/find", url.Values{"q": {"@x"}}), 200, "unreachable")
}

// The web UI is for managing the library; syncr publishes the feeds and media.
func TestWebUIDoesNotPublishFeedsOrMedia(t *testing.T) {
	h := newHarness(t)
	h.svc.PullFeed("UCone")
	for _, p := range []string{"/feeds/UCone.xml", "/media/UCone/vid0000000.mp4", "/media/UCone/vid0000000.jpg", "/api/v1/status", "/api/v1/apply"} {
		if rec := h.get(p); rec.Code != 404 {
			t.Errorf("%s must not be served by the web UI, got %d", p, rec.Code)
		}
	}
	// What it shows points at syncr's public address (server.base_url), and the CSP allows loading from there.
	rec := h.get("/feed/UCone")
	h.mustContain(rec, 200, `value="http://pod.example:8080/feeds/UCone.xml"`)
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' https: data: http://pod.example:8080") || !strings.Contains(csp, "media-src 'self' http://pod.example:8080") {
		t.Fatalf("CSP must allow images and media from syncr's public address: %s", csp)
	}
	// Changing the public address (by editing config.toml; Settings doesn't) moves the links and the CSP with it.
	if err := h.cfg.Update(func(c *podsync.Config) error { c.Server.BaseURL = "http://towhee:8080"; return nil }); err != nil {
		t.Fatal(err)
	}
	page := h.get("/feed/UCone")
	h.mustContain(page, 200, `src="http://towhee:8080/media/UCone/vid0000000.jpg"`, `value="http://towhee:8080/feeds/UCone.xml"`)
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "http://towhee:8080") {
		t.Fatal("CSP should follow the public address")
	}
}

func TestChangesFromTheOtherProgramAreSeenWithoutRestart(t *testing.T) {
	h := newHarness(t)
	h.mustContain(h.get("/feed/UCone"), 200, "No episodes yet")
	h.svc.PullFeed("UCone") // the sync service downloads while the web UI keeps running
	h.mustContain(h.get("/feed/UCone"), 200, "Title vid0000000", "Title vid0000002")
	h.mustContain(h.get("/"), 200, "3 episodes")

	// Someone edits config.toml by hand (or the web UI's other tab/instance does).
	hand := h.cfg.Get()
	hand.Feeds = append(hand.Feeds, podsync.Feed{ID: "UChand", Name: "Hand Added", Kind: "channel", Source: "youtube"})
	raw, err := toml.Marshal(hand)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustContain(h.get("/"), 200, "Hand Added")

	// A broken edit is reported (not dismissible) and the last good settings stay.
	if err := os.WriteFile(h.cfgPath, []byte("[defaults]\npage_size = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := h.get("/")
	h.mustContain(rec, 200, "Hand Added", "configuration file cannot be read", "page_size")
	if strings.Count(rec.Body.String(), "/status/dismiss") != 0 {
		t.Fatal("a broken config file banner must not be dismissible")
	}
	if err := os.WriteFile(h.cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.get("/").Body.String(), "configuration file cannot be read") {
		t.Fatal("banner should clear once the file is valid again")
	}
}

func TestDismissedIssueReturnsWhenReportedAgain(t *testing.T) {
	h := newHarness(t)
	h.st.Report("k", podsync.LevelWarning, "first")
	h.mustContain(h.get("/status"), 200, "first")
	h.mustContain(h.do("POST", "/status/dismiss", url.Values{"key": {"k"}}), 200)
	if strings.Contains(h.get("/status").Body.String(), "first") {
		t.Fatal("dismissed")
	}
	h.st.Report("k", podsync.LevelWarning, "second")
	h.mustContain(h.get("/status"), 200, "second")
}
