package rest_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

var bg = context.Background()

type env struct {
	t      *testing.T
	svc    *syncer.Service
	src    *mock.Source
	lib    *fs.Library
	srv    *httptest.Server
	client *rest.Client
}

func setup(t *testing.T) *env {
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
	src.Episodes = mock.Episodes(8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc := syncer.New(ctx, cfg, lib, podsync.NewRegistry(src), status.New())
	cfg.OnChange(svc.ConfigChanged)
	svc.Reconcile()
	srv := httptest.NewServer(rest.NewServer(svc, lib).Handler())
	t.Cleanup(srv.Close)
	return &env{t, svc, src, lib, srv, rest.NewClient(srv.URL, nil)}
}

func (e *env) idle() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := e.svc.Activity(); !a.Pulling && a.Downloading == 0 && a.Queued == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatal("never idle")
}

func (e *env) raw(method, path, body string, hdr ...string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func ptr[T any](v T) *T { return &v }

func TestClientServerRoundTripOfEveryOperation(t *testing.T) {
	e := setup(t)

	if err := e.client.Pull(bg, "UCone"); err != nil {
		t.Fatal(err)
	}
	e.idle()

	list, err := e.client.Feeds(bg)
	if err != nil || len(list.Feeds) != 1 || list.Feeds[0].Episodes != 3 || list.Feeds[0].RSSURL != "http://pod.example:8080/feeds/UCone.xml" {
		t.Fatalf("%v %+v", err, list)
	}
	d, err := e.client.Feed(bg, "UCone")
	if err != nil || d.Effective.PageSize != 3 || d.Overrides.ID != "UCone" {
		t.Fatalf("%v %+v", err, d)
	}
	views, err := e.client.Episodes(bg, "UCone", "")
	if err != nil || len(views) != 3 || views[0].State != podsync.StateDownloaded || views[0].ThumbFile == "" || views[0].Height != 480 {
		t.Fatalf("%v %+v", err, views)
	}
	if got, _ := e.client.Episodes(bg, "UCone", "ep01"); len(got) != 1 {
		t.Fatalf("q filter: %+v", got)
	}

	page, err := e.client.Older(bg, "UCone", 3)
	if err != nil || len(page.Episodes) != 3 || !page.More || page.NextOffset != 6 || page.Episodes[0].State != podsync.StateAvailable {
		t.Fatalf("%v %+v", err, page)
	}

	hint := podsync.EpisodeInfo{ID: "ep04xxxxxx", Title: "Hinted", Thumbnail: "https://i.ytimg.com/x.jpg", Channel: "Ch", ViewCount: 9, DurationSec: 61}
	v, err := e.client.Episode(bg, "UCone", hint)
	if err != nil || v.State != podsync.StateAvailable || v.Title != "Hinted" || v.ViewCount != 9 || v.DurationSec != 61 || v.Thumbnail != hint.Thumbnail {
		t.Fatalf("%v %+v", err, v)
	}
	if err := e.client.Download(bg, "UCone", podsync.EpisodeInfo{ID: "ep04xxxxxx", Title: "Hinted"}); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if v, _ := e.client.Episode(bg, "UCone", hint); v.State != podsync.StateDownloaded || !v.Manual {
		t.Fatalf("%+v", v)
	}

	hits, err := e.client.SearchLibrary(bg, "title ep01")
	if err != nil || len(hits.Episodes) != 1 || hits.Episodes[0].FeedName != "Channel One" {
		t.Fatalf("%v %+v", err, hits)
	}

	e.src.With(func(s *mock.Source) {
		s.SearchResults = []podsync.SearchResult{{ID: "UCnew", Name: "Found", Handle: "@f", Subscribers: 5}}
		s.ResolveFn = func(ref string) (podsync.FeedInfo, error) {
			return podsync.FeedInfo{Source: "youtube", Kind: "channel", ID: "UCnew", Name: "New <Channel>", Description: "d", ImageURL: "https://img/x.jpg"}, nil
		}
	})
	res, err := e.client.SearchSource(bg, "found", podsync.SearchChannel)
	if err != nil || len(res) != 1 || res[0].Handle != "@f" {
		t.Fatalf("%v %+v", err, res)
	}
	info, err := e.client.Resolve(bg, "@new")
	if err != nil || info.ID != "UCnew" || info.Name != "New <Channel>" {
		t.Fatalf("%v %+v", err, info)
	}
	sum, err := e.client.AddFeed(bg, podsync.AddFeedRequest{Info: info, PageSize: ptr(2)})
	if err != nil || sum.ID != "UCnew" {
		t.Fatalf("%v %+v", err, sum)
	}
	e.idle()
	upd, err := e.client.UpdateFeed(bg, "UCnew", podsync.FeedSettings{Name: "Renamed", Format: ptr("audio"), PageSize: ptr(4)})
	if err != nil || upd.Name != "Renamed" || upd.Effective.Format != "audio" || upd.Effective.PageSize != 4 || upd.Overrides.MaxResolution != nil {
		t.Fatalf("%v %+v", err, upd)
	}

	set, err := e.client.Settings(bg)
	if err != nil || set.Download.Concurrency != 2 || set.Defaults.PageSize != 3 {
		t.Fatalf("%v %+v", err, set)
	}
	set.Download.Concurrency = 4
	set.Download.UpdateInterval = podsync.Duration(45 * time.Minute)
	sr, err := e.client.UpdateSettings(bg, set)
	if err != nil || sr.RestartRequired || sr.Download.Concurrency != 4 || sr.Download.UpdateInterval.Std() != 45*time.Minute {
		t.Fatalf("%v %+v", err, sr)
	}

	e.src.With(func(s *mock.Source) { s.ListErr = errors.New("HTTP Error 429") })
	if _, err := e.client.Older(bg, "UCone", 3); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("%v", err)
	}
	st, err := e.client.Status(bg)
	if err != nil || len(st.Issues) != 1 || st.Issues[0].Key != "list:UCone" || st.Version == 0 || st.Versions == nil {
		t.Fatalf("%v %+v", err, st)
	}
	if err := e.client.DismissIssue(bg, "list:UCone"); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.client.Status(bg); len(st.Issues) != 0 {
		t.Fatalf("%+v", st)
	}

	if err := e.client.DeleteFeed(bg, "UCnew", false); err != nil {
		t.Fatal(err)
	}
	list, _ = e.client.Feeds(bg)
	if len(list.Orphans) != 1 || list.Orphans[0].ID != "UCnew" {
		t.Fatalf("%+v", list)
	}
	if err := e.client.DeleteOrphan(bg, "UCnew"); err != nil {
		t.Fatal(err)
	}
	if err := e.client.DeleteFeed(bg, "UCone", true); err != nil {
		t.Fatal(err)
	}
}

func TestErrorKindsSurviveTheWire(t *testing.T) {
	e := setup(t)
	if _, err := e.client.Feed(bg, "nope"); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	if _, err := e.client.AddFeed(bg, podsync.AddFeedRequest{Info: podsync.FeedInfo{ID: "UCone"}}); !errors.Is(err, podsync.ErrInvalid) || !strings.Contains(err.Error(), "already") {
		t.Fatalf("400: %v", err)
	}
	set, _ := e.client.Settings(bg)
	set.Defaults.PageSize = 0
	set.Defaults.TitleInclude = "("
	_, err := e.client.UpdateSettings(bg, set)
	if !errors.Is(err, podsync.ErrInvalid) || !strings.Contains(err.Error(), "page_size") || !strings.Contains(err.Error(), "title_include") {
		t.Fatalf("validation text must reach the client: %v", err)
	}
	e.src.With(func(s *mock.Source) { s.Gate = make(chan struct{}) })
	if err := e.client.Pull(bg, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.client.Pull(bg, ""); !errors.Is(err, podsync.ErrBusy) {
		t.Fatalf("409: %v", err)
	}
	e.src.With(func(s *mock.Source) { close(s.Gate); s.Gate = nil })
	e.idle()
	if _, err := e.client.Episodes(bg, "nope", ""); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// Odd characters in ids must not break routing.
	if _, err := e.client.Feed(bg, "a b/c?d"); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	dead := rest.NewClient("http://127.0.0.1:1", nil)
	if _, err := dead.Feeds(bg); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("%v", err)
	}
}

func TestRequestGuards(t *testing.T) {
	e := setup(t)
	if resp, _ := e.raw("POST", "/api/v1/resolve", `{"ref":"x"}`, "Content-Type", "text/plain"); resp.StatusCode != 400 {
		t.Fatalf("a body without a JSON content type must be refused, got %d", resp.StatusCode)
	}
	if resp, _ := e.raw("POST", "/api/v1/resolve", `{"ref":"x"}`, "Content-Type", "application/json", "Sec-Fetch-Site", "cross-site"); resp.StatusCode != 403 {
		t.Fatalf("cross-site write: %d", resp.StatusCode)
	}
	if resp, _ := e.raw("DELETE", "/api/v1/feeds/UCone?delete_files=true", "", "Origin", "http://evil.example"); resp.StatusCode != 403 {
		t.Fatalf("cross-origin delete: %d", resp.StatusCode)
	}
	if _, ok := e.svc.Feed(bg, "UCone"); ok != nil {
		t.Fatal("feed must survive blocked requests")
	}
	if resp, _ := e.raw("POST", "/api/v1/resolve", `{not json`, "Content-Type", "application/json"); resp.StatusCode != 400 {
		t.Fatalf("bad json: %d", resp.StatusCode)
	}
	if resp, body := e.raw("GET", "/api/v1/feeds/UCnope", ""); resp.StatusCode != 404 || !strings.Contains(body, `"kind":"not_found"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp, _ := e.raw("GET", "/healthz", ""); resp.StatusCode != 200 {
		t.Fatal("healthz")
	}
}

func TestRSSAndMedia(t *testing.T) {
	e := setup(t)
	e.svc.PullFeed("UCone")

	resp, body := e.raw("GET", "/feeds/UCone.xml", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/rss+xml") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	for _, want := range []string{
		`<enclosure url="http://pod.example:8080/media/UCone/ep00.mp4"`, `type="video/mp4"`,
		`<itunes:image href="http://pod.example:8080/media/UCone/ep00.jpg">`, "<title>Channel One</title>",
		`href="http://pod.example:8080/feeds/UCone.xml"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "ep03") {
		t.Fatal("only downloaded episodes belong in the feed")
	}

	resp, body = e.raw("GET", "/media/UCone/ep00.mp4", "")
	if resp.StatusCode != 200 || body != "0123456789" || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("%d %q %v", resp.StatusCode, body, resp.Header)
	}
	resp, body = e.raw("GET", "/media/UCone/ep00.mp4", "", "Range", "bytes=2-4")
	if resp.StatusCode != 206 || body != "234" {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}
	resp, body = e.raw("GET", "/media/UCone/ep00.jpg", "")
	if resp.StatusCode != 200 || body != "JPEGDATA" || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	for _, bad := range []string{"/media/UCone/_meta.json", "/media/UCone/ep00.json", "/media/UCone/..%2F..%2Fetc%2Fpasswd", "/media/UCnope/ep00.mp4", "/media/UCone/ep03.mp4", "/feeds/UCnope.xml", "/feeds/UCone", "/feeds/UCone.txt"} {
		if resp, _ := e.raw("GET", bad, ""); resp.StatusCode != 404 {
			t.Errorf("%s -> %d, want 404", bad, resp.StatusCode)
		}
	}
}
