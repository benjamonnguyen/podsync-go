package syncer

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"podsync"
	"podsync/mock"
)

var bg = context.Background()

func ptr[T any](v T) *T { return &v }

func TestFeedsListsConfiguredAndOrphans(t *testing.T) {
	e := setup(t, mock.Episodes(3), nil)
	e.svc.PullFeed("UC1")
	if err := e.lib.EnsureFeed("youtube", "UCorphan"); err != nil {
		t.Fatal(err)
	}
	list, _ := e.svc.Feeds(bg)
	if len(list.Feeds) != 1 || list.Feeds[0].ID != "UC1" || list.Feeds[0].Episodes != 3 || list.Feeds[0].RSSURL != "http://localhost:8080/feeds/UC1.xml" {
		t.Fatalf("%+v", list.Feeds)
	}
	if len(list.Orphans) != 1 || list.Orphans[0].ID != "UCorphan" || !list.Orphans[0].Orphan {
		t.Fatalf("%+v", list.Orphans)
	}
	d, err := e.svc.Feed(bg, "UC1")
	if err != nil || d.Effective.PageSize != 5 || d.Defaults.PageSize != 5 || d.Overrides.ID != "UC1" {
		t.Fatalf("%v %+v", err, d)
	}
	if _, err := e.svc.Feed(bg, "nope"); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestEpisodesViewsStatesAndFilter(t *testing.T) {
	e := setup(t, mock.Episodes(3), nil)
	e.src.DownloadErr["ep01"] = errors.New("boom")
	e.svc.PullFeed("UC1")
	views, err := e.svc.Episodes(bg, "UC1", "")
	if err != nil || len(views) != 3 {
		t.Fatalf("%v %d", err, len(views))
	}
	states := map[string]string{}
	for _, v := range views {
		states[v.ID] = v.State
	}
	if states["ep00"] != podsync.StateDownloaded || states["ep01"] != podsync.StateFailed {
		t.Fatalf("%v", states)
	}
	if views[0].ID != "ep01" { // the failed one has no date, so it is listed first
		t.Fatalf("undated episodes should lead: %+v", views[0])
	}
	if got, _ := e.svc.Episodes(bg, "UC1", "ep02"); len(got) != 1 || got[0].ID != "ep02" {
		t.Fatalf("filter: %+v", got)
	}
	if _, err := e.svc.Episodes(bg, "nope", ""); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatal("unknown feed")
	}
}

func TestEpisodeHintOverlayAndSanitizing(t *testing.T) {
	e := setup(t, nil, nil)
	v, err := e.svc.Episode(bg, "UC1", podsync.EpisodeInfo{
		ID: "abcdEFGhij1", Title: "  Hello  ", Thumbnail: "javascript:alert(1)", Channel: "C", ViewCount: 5, Description: "<script>", URL: "http://evil",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != podsync.StateAvailable || v.Title != "Hello" || v.Thumbnail != "" || v.Description != "" || v.URL != "" || v.ViewCount != 5 {
		t.Fatalf("%+v", v)
	}
	ok, _ := e.svc.Episode(bg, "UC1", podsync.EpisodeInfo{ID: "abcdEFGhij1", Thumbnail: "https://i.ytimg.com/x.jpg"})
	if ok.Thumbnail != "https://i.ytimg.com/x.jpg" {
		t.Fatalf("https thumbnails are kept: %+v", ok)
	}
	if _, err := e.svc.Episode(bg, "UC1", podsync.EpisodeInfo{ID: "../x"}); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatal("bad id must be invalid")
	}
}

func TestDownloadValidatesAndQueues(t *testing.T) {
	e := setup(t, mock.Episodes(8), nil)
	if err := e.svc.Download(bg, "UC1", podsync.EpisodeInfo{ID: "../x"}); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("%v", err)
	}
	if err := e.svc.Download(bg, "nope", podsync.EpisodeInfo{ID: "ep05"}); !errors.Is(err, podsync.ErrNotFound) {
		// ep05 is shorter than 6 chars -> invalid id is reported first; use a valid id below
		t.Logf("short id reports %v", err)
	}
	if err := e.svc.Download(bg, "nope", podsync.EpisodeInfo{ID: "abcdEFGhij1"}); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("unknown feed: %v", err)
	}
	if err := e.svc.Download(bg, "UC1", podsync.EpisodeInfo{ID: "abcdEFGhij1", Title: "x"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		v, _ := e.svc.Episode(bg, "UC1", podsync.EpisodeInfo{ID: "abcdEFGhij1"})
		return v.State == podsync.StateDownloaded
	})
}

func TestSearchLibraryFindsFeedsAndEpisodes(t *testing.T) {
	e := setup(t, mock.Episodes(3), nil)
	e.svc.PullFeed("UC1")
	hits, _ := e.svc.SearchLibrary(bg, "title ep01")
	if len(hits.Episodes) != 1 || hits.Episodes[0].FeedName != "One" || hits.Episodes[0].State != podsync.StateDownloaded {
		t.Fatalf("%+v", hits.Episodes)
	}
	hits, _ = e.svc.SearchLibrary(bg, "one")
	if len(hits.Feeds) != 1 {
		t.Fatalf("%+v", hits.Feeds)
	}
	if hits, _ := e.svc.SearchLibrary(bg, "  "); len(hits.Feeds)+len(hits.Episodes) != 0 {
		t.Fatal("blank search finds nothing")
	}
}

func TestSearchSourceReportsAndClearsBanner(t *testing.T) {
	e := setup(t, nil, nil)
	e.src.SearchErr = errors.New("boom")
	if _, err := e.svc.SearchSource(bg, "x", podsync.SearchChannel); err == nil {
		t.Fatal("expected error")
	}
	if a := e.st.Active(); len(a) != 1 || a[0].Key != "search" {
		t.Fatalf("%+v", a)
	}
	e.src.SearchErr = nil
	e.src.SearchResults = []podsync.SearchResult{{ID: "UC1", Name: "n"}}
	res, err := e.svc.SearchSource(bg, "x", podsync.SearchChannel)
	if err != nil || len(res) != 1 || len(e.st.Active()) != 0 {
		t.Fatalf("%v %+v %+v", err, res, e.st.Active())
	}
}

func TestAddUpdateDeleteFeedLifecycle(t *testing.T) {
	e := setup(t, mock.Episodes(5), nil)
	e.cfg.OnChange(e.svc.ConfigChanged)
	info := podsync.FeedInfo{ID: "UCnew", Name: "New", Description: "about", ImageURL: "https://img/x.jpg"}
	sum, err := e.svc.AddFeed(bg, podsync.AddFeedRequest{Info: info, PageSize: ptr(2)})
	if err != nil || sum.ID != "UCnew" || sum.Kind != "channel" {
		t.Fatalf("%v %+v", err, sum)
	}
	waitFor(t, func() bool { return !e.svc.Activity().Pulling && len(e.lib.Episodes("UCnew")) == 2 })
	if lf, _ := e.lib.Feed("UCnew"); lf.Meta.ImageURL != "https://img/x.jpg" || lf.Meta.Description != "about" {
		t.Fatalf("%+v", lf.Meta)
	}
	if _, err := e.svc.AddFeed(bg, podsync.AddFeedRequest{Info: info}); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := e.svc.AddFeed(bg, podsync.AddFeedRequest{Info: podsync.FeedInfo{ID: "../x"}}); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("unsafe id: %v", err)
	}
	if _, err := e.svc.AddFeed(bg, podsync.AddFeedRequest{Info: podsync.FeedInfo{ID: "UCx"}, PageSize: ptr(0)}); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("bad page size: %v", err)
	}

	d, err := e.svc.UpdateFeed(bg, "UCnew", podsync.FeedSettings{Name: "Renamed", Format: ptr("audio"), TitleExclude: ptr("x"), PageSize: ptr(3)})
	if err != nil || d.Name != "Renamed" || d.Effective.Format != "audio" || d.Effective.PageSize != 3 || d.Overrides.MaxResolution != nil {
		t.Fatalf("%v %+v", err, d)
	}
	if _, err := e.svc.UpdateFeed(bg, "UCnew", podsync.FeedSettings{Name: "x", TitleInclude: ptr("(")}); !errors.Is(err, podsync.ErrInvalid) || !strings.Contains(err.Error(), "title_include") {
		t.Fatalf("regex: %v", err)
	}
	if _, err := e.svc.UpdateFeed(bg, "nope", podsync.FeedSettings{}); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}

	dir, _ := e.lib.FeedDir("UCnew")
	if err := e.svc.DeleteFeed(bg, "UCnew", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("files should be kept")
	}
	list, _ := e.svc.Feeds(bg)
	if len(list.Orphans) != 1 || list.Orphans[0].ID != "UCnew" {
		t.Fatalf("%+v", list)
	}
	if err := e.svc.DeleteOrphan(bg, "UC1"); !errors.Is(err, podsync.ErrInvalid) {
		t.Fatalf("configured feed is not an orphan: %v", err)
	}
	if err := e.svc.DeleteOrphan(bg, "UCnew"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("orphan files should be deleted")
	}
	if err := e.svc.DeleteOrphan(bg, "UCnew"); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := e.svc.DeleteFeed(bg, "UC1", true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteFeed(bg, "UC1", true); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestSettingsUpdateAndRestartFlag(t *testing.T) {
	e := setup(t, nil, nil)
	s, _ := e.svc.Settings(bg)
	s.Download.Concurrency = 3
	s.Defaults.Format = "audio"
	res, err := e.svc.UpdateSettings(bg, s)
	if err != nil || res.RestartRequired || e.cfg.Get().Download.Concurrency != 3 || e.cfg.Get().Defaults.Format != "audio" {
		t.Fatalf("%v %+v", err, res)
	}
	s.Server.ListenAddr = ":9999"
	if res, err := e.svc.UpdateSettings(bg, s); err != nil || !res.RestartRequired {
		t.Fatalf("%v %+v", err, res)
	}
	s.Defaults.PageSize = 0
	s.Defaults.TitleExclude = "("
	_, err = e.svc.UpdateSettings(bg, s)
	if !errors.Is(err, podsync.ErrInvalid) || !strings.Contains(err.Error(), "page_size") || !strings.Contains(err.Error(), "title_exclude") {
		t.Fatalf("%v", err)
	}
	if e.cfg.Get().Defaults.PageSize == 0 {
		t.Fatal("invalid update leaked")
	}
}

func TestStatusIncludesIssuesVersionAndDismiss(t *testing.T) {
	e := setup(t, nil, nil)
	e.st.Report("k", podsync.LevelError, "bad")
	st, _ := e.svc.Status(bg)
	if len(st.Issues) != 1 || st.Version == 0 {
		t.Fatalf("%+v", st)
	}
	v0 := st.Version
	e.svc.DismissIssue(bg, "k")
	st, _ = e.svc.Status(bg)
	if len(st.Issues) != 0 || st.Version != v0 {
		t.Fatalf("%+v", st)
	}
	e.svc.PullFeed("UC1")
	if st, _ = e.svc.Status(bg); st.Version == v0 {
		t.Fatal("version must change when the library changes")
	}
}

func TestPullByFeedAndAll(t *testing.T) {
	e := setup(t, mock.Episodes(2), nil)
	if err := e.svc.Pull(bg, "nope"); !errors.Is(err, podsync.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := e.svc.Pull(bg, "UC1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !e.svc.Activity().Pulling && len(e.lib.Episodes("UC1")) == 2 })
	if err := e.svc.Pull(bg, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !e.svc.Activity().Pulling })
}
