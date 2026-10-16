package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"podsync"
	"podsync/fs"
	"podsync/mock"
	"podsync/status"
	"podsync/toml"
)

var bg = context.Background()

func TestPullRequestVariants(t *testing.T) {
	e := setup(t, mock.Episodes(8), nil)
	if err := e.svc.Pull(bg, podsync.PullRequest{Feed: "nope"}); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("unknown feed: %v", err)
	}
	if err := e.svc.Pull(bg, podsync.PullRequest{Feed: "UC1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !e.svc.Activity().Pulling && len(e.lib.Episodes("UC1")) == 5 })
	if err := e.svc.Pull(bg, podsync.PullRequest{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !e.svc.Activity().Pulling })

	ep := &podsync.EpisodeInfo{ID: "abcdEFGhij1", Title: "  Manual  ", Thumbnail: "javascript:x", Description: "<b>", URL: "http://evil"}
	if err := e.svc.Pull(bg, podsync.PullRequest{Episode: ep}); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("an episode download needs a feed: %v", err)
	}
	if err := e.svc.Pull(bg, podsync.PullRequest{Feed: "UC1", Episode: &podsync.EpisodeInfo{ID: "../x"}}); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	if err := e.svc.Pull(bg, podsync.PullRequest{Feed: "nope", Episode: ep}); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("unknown feed: %v", err)
	}
	if err := e.svc.Pull(bg, podsync.PullRequest{Feed: "UC1", Episode: ep}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		v, ok := e.lib.Episode("UC1", "abcdEFGhij1")
		return ok && v.Status == podsync.StatusDownloaded
	})
	got, _ := e.lib.Episode("UC1", "abcdEFGhij1")
	if !got.Manual || got.Thumbnail == "javascript:x" || got.URL == "http://evil" || got.Description == "<b>" {
		t.Fatalf("client-supplied hints must be sanitized and real metadata used: %+v", got)
	}
}

func TestStatusListsJobsIssuesAndVersions(t *testing.T) {
	e := setup(t, mock.Episodes(3), nil)
	e.st.Report("k", podsync.LevelError, "bad")
	e.src.With(func(s *mock.Source) { s.Gate = make(chan struct{}) })
	if err := e.svc.Pull(bg, podsync.PullRequest{Feed: "UC1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a := e.svc.Activity(); return a.Downloading == 2 && a.Queued == 1 })
	st, err := e.svc.Status(bg)
	if err != nil || !st.Pulling || len(st.Issues) != 1 || len(st.Jobs) != 3 {
		t.Fatalf("%v %+v", err, st)
	}
	states := map[string]int{}
	for _, j := range st.Jobs {
		if j.FeedID != "UC1" {
			t.Fatalf("%+v", j)
		}
		states[j.State]++
	}
	if states["downloading"] != 2 || states["queued"] != 1 {
		t.Fatalf("%v", states)
	}
	e.src.With(func(s *mock.Source) { close(s.Gate); s.Gate = nil })
	waitFor(t, func() bool { return !e.svc.Activity().Pulling })
	if st, _ := e.svc.Status(bg); len(st.Jobs) != 0 || st.Jobs == nil {
		t.Fatalf("jobs should be an empty list when idle: %+v", st.Jobs)
	}
}

func TestFindBannerOnlyForServiceProblems(t *testing.T) {
	e := setup(t, nil, nil)
	e.src.FindErr = errors.New("boom")
	if _, err := e.svc.Find(bg, "some name"); err == nil {
		t.Fatal("expected error")
	}
	if a := e.st.Active(); len(a) != 1 || a[0].Key != "find" {
		t.Fatalf("a failed search is a service problem and belongs in the banner: %+v", a)
	}
	e.src.FindErr = nil
	e.src.FindResults = []podsync.FeedInfo{{ID: "UC1", Name: "n"}}
	res, err := e.svc.Find(bg, "  some name ")
	if err != nil || len(res) != 1 || len(e.st.Active()) != 0 {
		t.Fatalf("%v %+v %+v", err, res, e.st.Active())
	}
	// A reference the user mistyped is just shown to them.
	e.src.FindErr = podsync.Invalid("not a recognized channel URL")
	if _, err := e.svc.Find(bg, "https://youtube.com/nope"); !errors.Is(err, podsync.ErrInvalid) || len(e.st.Active()) != 0 {
		t.Fatalf("%v %+v", err, e.st.Active())
	}
	// No results is an empty list, not null.
	e.src.FindErr, e.src.FindResults = nil, nil
	if res, err := e.svc.Find(bg, "zzz"); err != nil || res == nil || len(res) != 0 {
		t.Fatalf("%v %#v", err, res)
	}
}

func TestListWindowValidationAndBanner(t *testing.T) {
	e := setup(t, mock.Episodes(8), nil)
	eps, err := e.svc.List(bg, "UC1", 5, 3)
	if err != nil || len(eps) != 3 || eps[0].ID != "ep05" {
		t.Fatalf("%v %+v", err, eps)
	}
	if eps, _ := e.svc.List(bg, "UC1", 50, 3); eps == nil || len(eps) != 0 {
		t.Fatalf("past the end is an empty (non-nil) list: %#v", eps)
	}
	for name, call := range map[string]func() error{
		"unknown feed": func() error { _, err := e.svc.List(bg, "nope", 0, 3); return err },
		"neg offset":   func() error { _, err := e.svc.List(bg, "UC1", -1, 3); return err },
		"zero limit":   func() error { _, err := e.svc.List(bg, "UC1", 0, 0); return err },
		"huge limit":   func() error { _, err := e.svc.List(bg, "UC1", 0, 100000); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	e.src.ListErr = errors.New("HTTP Error 429")
	if _, err := e.svc.List(bg, "UC1", 0, 3); err == nil {
		t.Fatal("expected error")
	}
	if a := e.st.Active(); len(a) != 1 || a[0].Key != "list:UC1" || !strings.Contains(a[0].Message, "429") {
		t.Fatalf("%+v", a)
	}
	e.src.ListErr = nil
	e.svc.List(bg, "UC1", 0, 3)
	if len(e.st.Active()) != 0 {
		t.Fatal("banner should clear after a successful listing")
	}
}

// The sync service and the web UI are separate processes sharing a config file
// and a data directory; Apply is how the sync service learns about the web UI's edits.
func TestApplyPicksUpConfigAndLibraryChangesMadeByAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	c := podsync.Default()
	c.Server.DataDir = filepath.Join(dir, "data")
	c.Defaults.PageSize = 3
	c.Defaults.CleanupKeepLast = 0
	c.Feeds = []podsync.Feed{{ID: "UC1", Name: "One", Kind: "channel", Source: "youtube"}}
	c.Normalize()
	web, err := toml.Open(cfgPath) // the web UI's store
	if err != nil {
		t.Fatal(err)
	}
	if err := web.Update(func(x *podsync.Config) error { *x = c; return nil }); err != nil {
		t.Fatal(err)
	}
	syncrCfg, err := toml.Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	lib := fs.New(c.Server.DataDir)
	if err := lib.Load(); err != nil {
		t.Fatal(err)
	}
	src := mock.NewSource()
	src.Episodes = mock.Episodes(6)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc := New(ctx, syncrCfg, lib, podsync.NewRegistry(src), status.New())
	syncrCfg.OnChange(svc.ConfigChanged)
	svc.Reconcile()

	// Nothing changed yet.
	if res, err := svc.Apply(bg); err != nil || res.Changed || res.RestartRequired {
		t.Fatalf("%v %+v", err, res)
	}

	// The web UI adds a feed (and its folder), raises concurrency, and edits the restart-only server settings.
	webLib := fs.New(c.Server.DataDir)
	webLib.Load()
	err = web.Update(func(x *podsync.Config) error {
		x.Feeds = append(x.Feeds, podsync.Feed{ID: "UC2", Name: "Two", Kind: "channel", Source: "youtube"})
		x.Download.Concurrency = 4
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	webLib.EnsureFeed("youtube", "UC2")
	res, err := svc.Apply(bg)
	if err != nil || !res.Changed || res.RestartRequired {
		t.Fatalf("%v %+v", err, res)
	}
	if got := syncrCfg.Get(); len(got.Feeds) != 2 || got.Download.Concurrency != 4 {
		t.Fatalf("%+v", got.Download)
	}
	if err := svc.Pull(bg, podsync.PullRequest{Feed: "UC2"}); err != nil {
		t.Fatalf("a feed added by the web UI is pullable after Apply: %v", err)
	}
	waitFor(t, func() bool { return !svc.Activity().Pulling && len(lib.Episodes("UC2")) == 3 })

	// Removing a feed and deleting its files (web UI) is picked up, and its banner entries are dropped.
	svc.st.Report("download:UC2", podsync.LevelError, "x")
	web.Update(func(x *podsync.Config) error { x.Feeds = x.Feeds[:1]; return nil })
	webLib.Refresh()
	webLib.DeleteFeed("UC2")
	if _, err := svc.Apply(bg); err != nil {
		t.Fatal(err)
	}
	if _, ok := lib.Feed("UC2"); ok {
		t.Fatal("the deleted feed's folder should leave the sync service's index")
	}
	for _, is := range svc.st.Active() {
		if strings.HasSuffix(is.Key, "UC2") {
			t.Fatalf("issues of a removed feed must be dropped: %+v", is)
		}
	}

	// Restart-only settings are flagged.
	web.Update(func(x *podsync.Config) error { x.Server.APIAddr = "127.0.0.1:9999"; return nil })
	if res, err := svc.Apply(bg); err != nil || !res.RestartRequired {
		t.Fatalf("%v %+v", err, res)
	}

	// A hand-edited, broken file is refused and shown in the banner; the old settings stay.
	writeFile(t, cfgPath, "[defaults]\npage_size = 0\n")
	if _, err := svc.Apply(bg); !errors.Is(err, podsync.ErrInvalid) || !strings.Contains(err.Error(), "page_size") {
		t.Fatalf("%v", err)
	}
	if syncrCfg.Get().Defaults.PageSize != 3 {
		t.Fatal("the running config must not change")
	}
	found := false
	for _, is := range svc.st.Active() {
		found = found || is.Key == "config"
	}
	if !found {
		t.Fatal("a broken config file must be reported in the banner")
	}

	// A missed Apply is corrected by the next pull: the web UI saves valid settings again and a
	// scheduled pull (nobody calls Apply) re-reads the file and clears the banner.
	err = web.Update(func(x *podsync.Config) error {
		x.Feeds = append(x.Feeds, podsync.Feed{ID: "UC3", Name: "Three", Kind: "channel", Source: "youtube"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.PullAll(); err != nil {
		t.Fatal(err)
	}
	if _, ok := syncrCfg.Get().Feed("UC3"); !ok || syncrCfg.Get().Server.APIAddr != "127.0.0.1:9999" {
		t.Fatalf("the scheduled pull must re-read the config file: %+v", syncrCfg.Get().Feeds)
	}
	for _, is := range svc.st.Active() {
		if is.Key == "config" {
			t.Fatal("the config banner should clear once the file applies again")
		}
	}
	if n := len(lib.Episodes("UC3")); n != 3 {
		t.Fatalf("the feed added by the file edit should be pulled in the same run, got %d episodes", n)
	}
}
