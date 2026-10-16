package webui_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
	"podsync/webui"
)

type harness struct {
	t    *testing.T
	h    http.Handler
	cfg  *toml.Store
	lib  *fs.Library
	svc  *syncer.Service
	src  *mock.Source
	st   *status.Tracker
	rest *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	c := podsync.Default()
	c.Defaults.PageSize = 3
	c.Defaults.CleanupKeepLast = 0
	c.Server.BaseURL = "http://pod.example:8080"
	c.Feeds = []podsync.Feed{{ID: "UCone", Name: "Channel One", Kind: "channel", Source: "youtube"}}
	c.Normalize()
	cfg := toml.NewMemoryStore(c)
	lib := fs.New(t.TempDir())
	if err := lib.Load(); err != nil {
		t.Fatal(err)
	}
	src := mock.NewSource()
	for i := 0; i < 8; i++ {
		src.Episodes = append(src.Episodes, podsync.EpisodeInfo{
			ID: fmt.Sprintf("vid%07d", i), Title: fmt.Sprintf("Video %d", i),
			Thumbnail: fmt.Sprintf("https://i.ytimg.com/vi/vid%07d/hq.jpg", i), ViewCount: 2500, Channel: "Remote Chan",
		})
	}
	src.SearchResults = []podsync.SearchResult{{Kind: "channel", ID: "UCnew", Name: "Found One", Handle: "@found", Subscribers: 1500000}}
	src.ResolveFn = func(ref string) (podsync.FeedInfo, error) {
		if ref == "nope" {
			return podsync.FeedInfo{}, errors.New("not a recognized channel")
		}
		return podsync.FeedInfo{Source: "youtube", Kind: "channel", ID: "UCnew", Name: "New <Channel>", Description: "about", ImageURL: "https://img/x.jpg"}, nil
	}
	st := status.New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc := syncer.New(ctx, cfg, lib, podsync.NewRegistry(src), st)
	cfg.OnChange(svc.ConfigChanged)
	svc.Reconcile()
	api := httptest.NewServer(rest.NewServer(svc, lib).Handler())
	t.Cleanup(api.Close)
	upstream, _ := url.Parse(api.URL)
	ui, err := webui.New(rest.NewClient(api.URL, nil), upstream)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t, ui.Handler(), cfg, lib, svc, src, st, api}
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

func TestPullFeedThenBrowseEpisodesRSSAndMedia(t *testing.T) {
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

	rss := h.get("/feeds/UCone.xml")
	h.mustContain(rss, 200, "<enclosure url=\"http://pod.example:8080/media/UCone/vid0000000.mp4\"", `type="video/mp4"`)
	if ct := rss.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Fatalf("content type %q", ct)
	}
	media := h.get("/media/UCone/vid0000000.mp4")
	h.mustContain(media, 200, "0123456789")
	rng := h.do("GET", "/media/UCone/vid0000000.mp4", nil, "Range", "bytes=2-4")
	h.mustContain(rng, 206, "234")

	for _, bad := range []string{"/media/UCone/_meta.json", "/media/UCone/vid0000000.json", "/media/UCone/..%2F..%2Fetc%2Fpasswd", "/media/UCnope/vid0000000.mp4", "/feeds/UCnope.xml", "/feeds/UCone"} {
		if rec := h.get(bad); rec.Code != 404 {
			t.Errorf("%s -> %d, want 404", bad, rec.Code)
		}
	}

	h.mustContain(h.get("/search?q=0000002"), 200, "Title vid0000002", "Episodes matching")
	h.mustContain(h.get("/search?q=channel+one"), 200, "Channel One")
	h.mustContain(h.get("/search?q="), 200, "Channel One")
}

func TestThumbnailsAndMetadataInUIAndMedia(t *testing.T) {
	h := newHarness(t)
	h.svc.PullFeed("UCone")
	page := h.get("/feed/UCone")
	h.mustContain(page, 200,
		`<img class="thumb" src="/media/UCone/vid0000000.jpg"`, "1.2M views", "480p", "<summary>Details</summary>",
		"Line one\nLine &lt;two&gt;", `href="https://youtu.be/vid0000000"`)
	h.mustContain(h.get("/media/UCone/vid0000000.jpg"), 200, "JPEGDATA")
	h.mustContain(h.get("/feeds/UCone.xml"), 200, `<itunes:image href="http://pod.example:8080/media/UCone/vid0000000.jpg">`)

	// Thumbnails survive cleanup; the media does not.
	h.cfg.Update(func(c *podsync.Config) error { c.Defaults.CleanupKeepLast = 1; return nil })
	h.svc.Cleanup("UCone")
	h.mustContain(h.get("/media/UCone/vid0000002.jpg"), 200, "JPEGDATA")
	h.mustContain(h.get("/media/UCone/vid0000002.mp4"), 404)
	cleaned := h.get("/feed/UCone").Body.String()
	if !strings.Contains(cleaned, `src="/media/UCone/vid0000002.jpg"`) || !strings.Contains(cleaned, "removed by cleanup") {
		t.Fatalf("cleaned row should keep its thumbnail:\n%s", cleaned)
	}
	// Only known thumbnails are served.
	h.mustContain(h.get("/media/UCone/unknown.jpg"), 404)

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
	h.mustContain(row, 200, "Title vid0000004", `href="/media/UCone/vid0000004.mp4"`, "kept")
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
	h.mustContain(h.get("/add"), 200, "Paste a link", "search by name")

	h.mustContain(h.do("POST", "/add/search", url.Values{"q": {"fo"}}), 200, "at least 3")
	h.mustContain(h.do("POST", "/add/search", url.Values{"q": {"found"}, "kind": {"channel"}}), 200, "Found One", "@found", "1.5M subscribers")
	h.src.With(func(m *mock.Source) { m.SearchErr = errors.New("boom") })
	h.mustContain(h.do("POST", "/add/search", url.Values{"q": {"found"}}), 200, "Search failed: boom")
	h.mustContain(h.get("/status"), 200, "Channel search failed")
	h.src.With(func(m *mock.Source) { m.SearchErr = nil })
	h.do("POST", "/add/search", url.Values{"q": {"found"}})
	if strings.Contains(h.get("/status").Body.String(), "Channel search failed") {
		t.Fatal("search issue should clear after a success")
	}

	h.mustContain(h.do("POST", "/add/preview", url.Values{"ref": {"nope"}}), 200, "not a recognized channel")
	prev := h.do("POST", "/add/preview", url.Values{"ref": {"@new"}})
	h.mustContain(prev, 200, "New &lt;Channel&gt;", `name="id" value="UCnew"`, "Add feed")

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
	if n := len(h.lib.Episodes("UCnew")); n != 2 {
		t.Fatalf("first pull should fetch page_size=2 episodes, got %d", n)
	}
	sum, _ := h.lib.Feed("UCnew")
	if sum.Meta.ImageURL != "https://img/x.jpg" || sum.Meta.Description != "about" {
		t.Fatalf("meta: %+v", sum.Meta)
	}
	h.mustContain(h.do("POST", "/add/preview", url.Values{"ref": {"@new"}}), 200, "Already in your library")
	h.mustContain(h.do("POST", "/add", url.Values{"id": {"UCnew"}}), 200, "already in your library")
}

func TestSettingsSaveValidationAndRestartNotice(t *testing.T) {
	h := newHarness(t)
	h.mustContain(h.get("/settings"), 200, `name="base_url" value="http://pod.example:8080"`, `name="update_interval" value="30m"`)

	form := func() url.Values {
		c := h.cfg.Get()
		return url.Values{
			"base_url": {c.Server.BaseURL}, "listen_addr": {c.Server.ListenAddr}, "data_dir": {c.Server.DataDir},
			"yt_dlp_path": {"yt-dlp"}, "concurrency": {"3"}, "update_interval": {"15m"}, "cookies_file": {""},
			"format": {"audio"}, "max_resolution": {"720"}, "exclude_shorts": {"on"}, "title_include": {""}, "title_exclude": {"(?i)sponsor"},
			"page_size": {"4"}, "cleanup_keep_last": {"20"}, "cleanup_after_days": {"0"},
		}
	}
	f := form()
	rec := h.do("POST", "/settings", f)
	h.mustContain(rec, 200, "Saved. Changes are applied.")
	got := h.cfg.Get()
	if got.Download.Concurrency != 3 || got.Download.UpdateInterval.Std() != 15*time.Minute || got.Defaults.Format != "audio" || got.Defaults.PageSize != 4 || got.Download.SelfUpdate {
		t.Fatalf("not applied: %+v %+v", got.Download, got.Defaults)
	}

	f = form()
	f.Set("title_exclude", "(")
	f.Set("page_size", "0")
	f.Set("update_interval", "soon")
	rec = h.do("POST", "/settings", f)
	h.mustContain(rec, 200, "Not saved", "title_exclude", "page_size", "update_interval", `name="title_exclude" value="("`)
	if h.cfg.Get().Defaults.TitleExclude != "(?i)sponsor" {
		t.Fatal("invalid save must not change config")
	}

	f = form()
	f.Set("listen_addr", ":9999")
	h.mustContain(h.do("POST", "/settings", f), 200, "restart")
}

func TestFeedSettingsOverridesAndInherit(t *testing.T) {
	h := newHarness(t)
	h.mustContain(h.get("/feed/UCone/settings"), 200, "Default (video)", `placeholder="3"`)
	form := url.Values{"name": {"Renamed"}, "format": {"audio"}, "max_resolution": {""}, "exclude_shorts": {"no"}, "title_include": {""}, "title_exclude": {"live"},
		"page_size": {"7"}, "cleanup_keep_last": {""}, "cleanup_after_days": {"30"}}
	h.mustContain(h.do("POST", "/feed/UCone/settings", form), 200, "Saved")
	f, _ := h.cfg.Get().Feed("UCone")
	eff := f.Effective(h.cfg.Get().Defaults)
	if f.Name != "Renamed" || eff.Format != "audio" || eff.ExcludeShorts || eff.PageSize != 7 || eff.CleanupAfterDays != 30 || eff.MaxResolution != 1080 || f.MaxResolution != nil {
		t.Fatalf("%+v / %+v", f, eff)
	}
	bad := url.Values{"name": {"X"}, "title_include": {"("}}
	h.mustContain(h.do("POST", "/feed/UCone/settings", bad), 200, "Not saved", "regular expression")
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
	if h.get("/feeds/UCone.xml").Code != 404 {
		t.Fatal("rss should be gone")
	}
	// Files remain and show as orphaned.
	h.mustContain(h.get("/"), 200, "Not in config", "Delete files")
	dir, _ := h.lib.FeedDir("UCone")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("files should remain")
	}
	h.mustContain(h.do("POST", "/orphans/UCone/delete", nil), 303)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("orphan files should be deleted")
	}

	h2 := newHarness(t)
	h2.svc.PullFeed("UCone")
	dir2, _ := h2.lib.FeedDir("UCone")
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

func TestSyncServiceOutageIsVisible(t *testing.T) {
	dead, _ := url.Parse("http://127.0.0.1:1")
	ui, err := webui.New(rest.NewClient(dead.String(), nil), dead)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, h: ui.Handler()}
	rec := h.get("/status")
	h.mustContain(rec, 200, `class="banner error"`, "Cannot reach the sync service")
	if rec := h.get("/"); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "unreachable") {
		t.Fatalf("library page should report the outage, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.get("/media/UC1/x.mp4"); rec.Code != http.StatusBadGateway {
		t.Fatalf("proxy must answer 502 when syncr is down, got %d", rec.Code)
	}
	// Dismissing the synthetic issue must not crash and the banner stays (the service is still down).
	h.mustContain(h.do("POST", "/status/dismiss", url.Values{"key": {"syncr"}}), 200, "Cannot reach the sync service")
}

func TestPublicFeedAndMediaAreProxiedFromSyncr(t *testing.T) {
	h := newHarness(t)
	h.svc.PullFeed("UCone")
	rec := h.get("/feeds/UCone.xml")
	h.mustContain(rec, 200, "<enclosure", "Channel One")
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/rss+xml") {
		t.Fatalf("%v", rec.Header())
	}
	h.mustContain(h.do("GET", "/media/UCone/vid0000000.mp4", nil, "Range", "bytes=2-4"), 206, "234")
	// The API itself is not exposed through the UI server.
	for _, p := range []string{"/api/v1/feeds", "/api/v1/settings"} {
		if h.get(p).Code != 404 {
			t.Errorf("%s must not be reachable through the web UI", p)
		}
	}
}
