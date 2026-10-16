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
	srv := httptest.NewServer(rest.NewServer(svc).Handler())
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

func TestClientServerRoundTripOfEveryOperation(t *testing.T) {
	e := setup(t)

	if res, err := e.client.Apply(bg); err != nil || res.Changed || res.RestartRequired {
		t.Fatalf("%v %+v", err, res)
	}

	if err := e.client.Pull(bg, podsync.PullRequest{Feed: "UCone"}); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if n := len(e.lib.Episodes("UCone")); n != 3 {
		t.Fatalf("pull should download page_size episodes, got %d", n)
	}
	if err := e.client.Pull(bg, podsync.PullRequest{}); err != nil {
		t.Fatal(err)
	}
	e.idle()

	hint := &podsync.EpisodeInfo{ID: "ep04xxxxxx", Title: "Manual"}
	if err := e.client.Pull(bg, podsync.PullRequest{Feed: "UCone", Episode: hint}); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if ep, ok := e.lib.Episode("UCone", "ep04xxxxxx"); !ok || !ep.Manual || ep.Status != podsync.StatusDownloaded {
		t.Fatalf("%+v %v", ep, ok)
	}

	e.src.With(func(s *mock.Source) {
		s.FindFn = func(q string) ([]podsync.FeedInfo, error) {
			return []podsync.FeedInfo{{Source: "youtube", Kind: "channel", ID: "UCnew", Name: "Found <" + q + ">", Handle: "@f", Subscribers: 5, ImageURL: "https://img/x.jpg"}}, nil
		}
	})
	found, err := e.client.Find(bg, "https://youtube.com/@new?x=1&y=2")
	if err != nil || len(found) != 1 || found[0].ID != "UCnew" || found[0].Name != "Found <https://youtube.com/@new?x=1&y=2>" || found[0].Handle != "@f" || found[0].Subscribers != 5 {
		t.Fatalf("%v %+v", err, found)
	}

	eps, err := e.client.List(bg, "UCone", 5, 2)
	if err != nil || len(eps) != 2 || eps[0].ID != "ep05" {
		t.Fatalf("%v %+v", err, eps)
	}

	st, err := e.client.Status(bg)
	if err != nil || st.Pulling || st.Issues == nil || st.Jobs == nil || st.Versions == nil {
		t.Fatalf("empty collections must be [] / {} not null: %v %+v", err, st)
	}
	e.src.With(func(s *mock.Source) { s.ListErr = errors.New("HTTP Error 429") })
	if _, err := e.client.List(bg, "UCone", 0, 3); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("%v", err)
	}
	if st, _ := e.client.Status(bg); len(st.Issues) != 1 || st.Issues[0].Key != "list:UCone" {
		t.Fatalf("%+v", st)
	}
}

func TestErrorKindsSurviveTheWire(t *testing.T) {
	e := setup(t)
	if _, err := e.client.List(bg, "nope", 0, 3); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	if _, err := e.client.List(bg, "UCone", 0, 0); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("400: %v", err)
	}
	e.src.With(func(s *mock.Source) { s.FindErr = podsync.Invalid("not a recognized channel URL") })
	if _, err := e.client.Find(bg, "https://youtube.com/nope"); !errors.Is(err, podsync.ErrInvalid) || !strings.Contains(err.Error(), "recognized") {
		t.Fatalf("400: %v", err)
	}
	if err := e.client.Pull(bg, podsync.PullRequest{Feed: "UCone", Episode: &podsync.EpisodeInfo{ID: "../x"}}); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("400: %v", err)
	}
	e.src.With(func(s *mock.Source) { s.Gate = make(chan struct{}) })
	if err := e.client.Pull(bg, podsync.PullRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := e.client.Pull(bg, podsync.PullRequest{}); !errors.Is(err, podsync.ErrBusy) {
		t.Fatalf("409: %v", err)
	}
	e.src.With(func(s *mock.Source) { close(s.Gate); s.Gate = nil })
	e.idle()
	// Odd characters in ids must not break routing.
	if _, err := e.client.List(bg, "a b/c?d", 0, 3); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	dead := rest.NewClient("http://127.0.0.1:1", nil)
	if _, err := dead.Status(bg); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("%v", err)
	}
}

func TestRequestGuardsAndSurface(t *testing.T) {
	e := setup(t)
	if resp, _ := e.raw("POST", "/api/v1/pull", `{"feed":"UCone"}`, "Content-Type", "text/plain"); resp.StatusCode != 400 {
		t.Fatalf("a body without a JSON content type must be refused, got %d", resp.StatusCode)
	}
	if resp, _ := e.raw("POST", "/api/v1/pull", `{}`, "Content-Type", "application/json", "Sec-Fetch-Site", "cross-site"); resp.StatusCode != 403 {
		t.Fatalf("cross-site write: %d", resp.StatusCode)
	}
	if resp, _ := e.raw("POST", "/api/v1/apply", "", "Origin", "http://evil.example"); resp.StatusCode != 403 {
		t.Fatalf("cross-origin write: %d", resp.StatusCode)
	}
	if resp, _ := e.raw("POST", "/api/v1/pull", `{not json`, "Content-Type", "application/json"); resp.StatusCode != 400 {
		t.Fatalf("bad json: %d", resp.StatusCode)
	}
	if resp, body := e.raw("GET", "/api/v1/feeds/UCnope/listing?limit=3", ""); resp.StatusCode != 404 || !strings.Contains(body, `"kind":"not_found"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp, _ := e.raw("GET", "/healthz", ""); resp.StatusCode != 200 {
		t.Fatal("healthz")
	}
	// The API is deliberately small: nothing else is served.
	for _, p := range []string{"/api/v1/feeds", "/api/v1/settings", "/api/v1/resolve", "/api/v1/search", "/feeds/UCone.xml", "/media/UCone/x.mp4"} {
		if resp, _ := e.raw("GET", p, ""); resp.StatusCode != 404 && resp.StatusCode != 405 {
			t.Errorf("%s should not exist, got %d", p, resp.StatusCode)
		}
	}
}
