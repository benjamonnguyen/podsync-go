package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"podsync"
	"podsync/fs"
	"podsync/mock"
	"podsync/status"
	"podsync/toml"
)

var base = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

type env struct {
	svc *Service
	src *mock.Source
	lib *fs.Library
	st  *status.Tracker
	cfg *toml.Store
}

func setup(t *testing.T, eps []podsync.EpisodeInfo, mutate func(*podsync.Config)) *env {
	t.Helper()
	c := podsync.Default()
	c.Defaults.PageSize = 5
	c.Defaults.CleanupKeepLast = 0
	c.Feeds = []podsync.Feed{{ID: "UC1", Name: "One", Kind: "channel", Source: "youtube"}}
	if mutate != nil {
		mutate(&c)
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg := toml.NewMemoryStore(c)
	lib := fs.New(t.TempDir())
	if err := lib.Load(); err != nil {
		t.Fatal(err)
	}
	src := mock.NewSource()
	src.Episodes = eps
	src.Published = base
	st := status.New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc := New(ctx, cfg, lib, podsync.NewRegistry(src), st)
	svc.Reconcile()
	return &env{svc, src, lib, st, cfg}
}

func TestPullDownloadsNewestPageAndIsIdempotent(t *testing.T) {
	e := setup(t, mock.Episodes(8), nil)
	if err := e.svc.PullFeed("UC1"); err != nil {
		t.Fatal(err)
	}
	eps := e.lib.Episodes("UC1")
	if len(eps) != 5 {
		t.Fatalf("want page_size=5 episodes, got %d", len(eps))
	}
	if eps[0].ID != "ep00" || eps[0].Status != podsync.StatusDownloaded || eps[0].File != "ep00.mp4" || eps[0].Manual {
		t.Fatalf("%+v", eps[0])
	}
	dir, _ := e.lib.FeedDir("UC1")
	if _, err := os.Stat(filepath.Join(dir, "ep00.mp4")); err != nil {
		t.Fatal("media not moved into the feed dir")
	}
	filepath.WalkDir(e.lib.TempDir(), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("staging file left behind: %s", path)
		}
		return nil
	})
	if eps[0].ThumbFile != "ep00.jpg" || eps[0].Channel != "The Channel" || eps[0].ViewCount != 1234567 || eps[0].Height != 480 || eps[0].Thumbnail != "https://remote/ep00.jpg" {
		t.Fatalf("metadata not stored: %+v", eps[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "ep00.jpg")); err != nil {
		t.Fatal("thumbnail not moved into the feed dir")
	}
	if sum, _ := e.lib.Feed("UC1"); sum.Meta.LastPull.IsZero() || sum.Meta.ImageURL != "http://img/x.jpg" {
		t.Fatalf("meta: %+v", sum.Meta)
	}
	e.svc.PullFeed("UC1")
	for _, ep := range eps {
		if n := e.src.DownloadCount(ep.ID); n != 1 {
			t.Fatalf("%s downloaded %d times", ep.ID, n)
		}
	}
}

// TestPullCatchesUpOnPublishBurst covers the case where more than page_size
// episodes were published between two checks: the first page after the burst
// is entirely new, which must not mean the older half of the burst (still
// outside page_size) is silently skipped forever.
func TestPullCatchesUpOnPublishBurst(t *testing.T) {
	e := setup(t, mock.Episodes(8), nil) // page_size=5
	if err := e.svc.PullFeed("UC1"); err != nil {
		t.Fatal(err)
	}
	if n := len(e.lib.Episodes("UC1")); n != 5 {
		t.Fatalf("first pull: want 5, got %d", n)
	}

	// A burst of 10 new episodes lands in front of everything already known.
	var burst []podsync.EpisodeInfo
	for i := 0; i < 10; i++ {
		burst = append(burst, podsync.EpisodeInfo{ID: fmt.Sprintf("new%02d", i), Title: fmt.Sprintf("New %d", i), DurationSec: 600})
	}
	e.src.With(func(s *mock.Source) { s.Episodes = append(burst, s.Episodes...) })
	e.src.ListCalls = 0
	if err := e.svc.PullFeed("UC1"); err != nil {
		t.Fatal(err)
	}

	got := e.lib.Episodes("UC1")
	if len(got) != 15 { // the original 5 + all 10 from the burst
		t.Fatalf("want 15 episodes after catch-up, got %d: %+v", len(got), got)
	}
	for _, ep := range burst {
		if _, ok := e.lib.Episode("UC1", ep.ID); !ok {
			t.Fatalf("%s from the burst was never downloaded: catch-up did not reach it", ep.ID)
		}
	}
	// 10 new / page_size 5 = 2 full unseen pages before the 3rd page (offset
	// 10) reaches ep00..ep04, which are already known: 1 initial + 2 extra.
	if e.src.ListCalls != 3 {
		t.Fatalf("want 3 ListEpisodes calls (1 initial + 2 catch-up), got %d", e.src.ListCalls)
	}
}

// TestPullCatchUpIsCappedAndSkippedOnFirstPull covers the two safety rails:
// catch-up never runs on a feed's very first check (nothing can be "known"
// yet, so it would otherwise walk the entire back catalogue every time), and
// even on a later check it stops after maxCatchUpPages extra queries rather
// than paging forever through a feed where nothing ever matches.
func TestPullCatchUpIsCappedAndSkippedOnFirstPull(t *testing.T) {
	e := setup(t, mock.Episodes(500), func(c *podsync.Config) { c.Defaults.PageSize = 5 })
	if err := e.svc.PullFeed("UC1"); err != nil {
		t.Fatal(err)
	}
	if n := len(e.lib.Episodes("UC1")); n != 5 {
		t.Fatalf("first pull must still be capped at page_size, got %d", n)
	}
	if e.src.ListCalls != 1 {
		t.Fatalf("no catch-up on a feed's first pull, got %d ListEpisodes calls", e.src.ListCalls)
	}

	// Replace the whole catalogue with episodes that will never match anything known.
	e.src.With(func(s *mock.Source) {
		s.Episodes = nil
		for i := 0; i < 500; i++ {
			s.Episodes = append(s.Episodes, podsync.EpisodeInfo{ID: fmt.Sprintf("zz%03d", i), Title: fmt.Sprintf("Z %d", i), DurationSec: 600})
		}
	})
	e.src.ListCalls = 0
	if err := e.svc.PullFeed("UC1"); err != nil {
		t.Fatal(err)
	}
	// 1 initial page + maxCatchUpPages (10) extra, never finding a known episode.
	if e.src.ListCalls != 11 {
		t.Fatalf("want catch-up capped at 11 ListEpisodes calls, got %d", e.src.ListCalls)
	}
}

func TestFiltersAndSkippedEntries(t *testing.T) {
	eps := mock.Episodes(7)
	eps[0].DurationSec = 60 // exactly 60s is a Short: the default minimum is 61s
	eps[1].IsLive = true
	eps[2].Unavailable = true
	eps[3].Title = "Sponsored: thing"
	eps[4].Title = "Keep me"
	e := setup(t, eps, func(c *podsync.Config) {
		c.Defaults.PageSize = 7
		c.Feeds[0].TitleExclude = "^Sponsored"
		c.Feeds[0].TitleInclude = "(?i)keep|episode"
	})
	e.svc.PullFeed("UC1")
	var got []string
	for _, ep := range e.lib.Episodes("UC1") {
		got = append(got, ep.ID)
	}
	want := map[string]bool{"ep04": true, "ep05": true, "ep06": true}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected %s in %v", id, got)
		}
	}
}

func TestFailureRetryBannerAndManualReset(t *testing.T) {
	e := setup(t, mock.Episodes(2), nil)
	e.src.DownloadErr["ep00"] = errors.New("Video unavailable")
	e.svc.PullFeed("UC1")
	ep, _ := e.lib.Episode("UC1", "ep00")
	if ep.Status != podsync.StatusFailed || ep.Attempts != 1 || ep.LastError != "Video unavailable" {
		t.Fatalf("%+v", ep)
	}
	if iss := e.st.Active(); len(iss) != 1 || iss[0].Key != "download:UC1" {
		t.Fatalf("banner: %+v", iss)
	}
	e.svc.PullFeed("UC1")
	e.svc.PullFeed("UC1")
	e.svc.PullFeed("UC1") // attempts exhausted: no 4th try
	if n := e.src.DownloadCount("ep00"); n != MaxAttempts {
		t.Fatalf("want %d attempts, got %d", MaxAttempts, n)
	}
	delete(e.src.DownloadErr, "ep00")
	if err := e.svc.RequestDownload("UC1", podsync.EpisodeInfo{ID: "ep00"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return e.svc.JobState("UC1", "ep00") == "" && e.lib.Episodes("UC1")[0].Status == podsync.StatusDownloaded
	})
	ep, _ = e.lib.Episode("UC1", "ep00")
	if !ep.Manual || ep.Attempts != 0 {
		t.Fatalf("manual retry result: %+v", ep)
	}
	if len(e.st.Active()) != 0 {
		t.Fatalf("banner should clear after success: %+v", e.st.Active())
	}
}

func TestListFailureReportedThenCleared(t *testing.T) {
	e := setup(t, mock.Episodes(2), nil)
	e.src.ListErr = errors.New("HTTP Error 429")
	e.svc.PullAll()
	iss := e.st.Active()
	if len(iss) != 1 || iss[0].Key != "list:UC1" {
		t.Fatalf("%+v", iss)
	}
	e.src.ListErr = nil
	e.svc.PullAll()
	if len(e.st.Active()) != 0 {
		t.Fatalf("%+v", e.st.Active())
	}
}

func TestHealthErrorsBecomeBanner(t *testing.T) {
	e := setup(t, nil, nil)
	e.src.HealthResult = podsync.Health{Errors: []string{"yt-dlp not found"}, Warnings: []string{"ffmpeg not found"}}
	e.svc.CheckHealth()
	if len(e.st.Active()) != 2 {
		t.Fatalf("%+v", e.st.Active())
	}
	e.src.HealthResult = podsync.Health{Version: "1.0"}
	e.svc.CheckHealth()
	if len(e.st.Active()) != 0 || e.svc.Versions()["youtube"] != "1.0" {
		t.Fatalf("%+v", e.st.Active())
	}
}

func TestCleanupKeepLastTombstonesAndManualExempt(t *testing.T) {
	e := setup(t, mock.Episodes(5), func(c *podsync.Config) { c.Defaults.CleanupKeepLast = 2 })
	e.svc.PullFeed("UC1")
	// ep00,ep01 kept; ep02..04 cleaned.
	dir, _ := e.lib.FeedDir("UC1")
	for id, wantKept := range map[string]bool{"ep00": true, "ep01": true, "ep02": false, "ep03": false, "ep04": false} {
		_, err := os.Stat(filepath.Join(dir, id+".mp4"))
		if (err == nil) != wantKept {
			t.Errorf("%s: media present=%v want %v", id, err == nil, wantKept)
		}
		ep, _ := e.lib.Episode("UC1", id)
		if wantKept != (ep.Status == podsync.StatusDownloaded) {
			t.Errorf("%s status %s", id, ep.Status)
		}
		if _, err := os.Stat(filepath.Join(dir, id+".jpg")); err != nil {
			t.Errorf("%s: thumbnail must survive cleanup: %v", id, err)
		}
		if ep.ThumbFile != id+".jpg" {
			t.Errorf("%s: thumb_file lost: %+v", id, ep)
		}
	}
	// Next pull must not resurrect cleaned episodes.
	e.svc.PullFeed("UC1")
	if n := e.src.DownloadCount("ep04"); n != 1 {
		t.Fatalf("cleaned episode re-downloaded: %d", n)
	}
	// Manually downloading a cleaned episode pins it.
	e.svc.RequestDownload("UC1", podsync.EpisodeInfo{ID: "ep04"})
	waitFor(t, func() bool { ep, _ := e.lib.Episode("UC1", "ep04"); return ep.Status == podsync.StatusDownloaded })
	e.svc.Cleanup("UC1")
	if ep, _ := e.lib.Episode("UC1", "ep04"); ep.Status != podsync.StatusDownloaded || !ep.Manual {
		t.Fatalf("manual episode must survive cleanup: %+v", ep)
	}
}

func TestConcurrencyCapAndBusy(t *testing.T) {
	e := setup(t, mock.Episodes(6), func(c *podsync.Config) { c.Defaults.PageSize = 6; c.Download.Concurrency = 2 })
	e.src.Gate = make(chan struct{})
	if err := e.svc.TriggerAll(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a := e.svc.Activity(); return a.Downloading == 2 && a.Queued == 4 && a.Pulling })
	if err := e.svc.TriggerAll(); !errors.Is(err, podsync.ErrBusy) {
		t.Fatalf("second trigger should be busy, got %v", err)
	}
	// Raise the cap live.
	e.cfg.OnChange(e.svc.ConfigChanged)
	if err := e.cfg.Update(func(c *podsync.Config) error { c.Download.Concurrency = 4; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { e.src.Mu.Lock(); defer e.src.Mu.Unlock(); return e.src.Active == 4 })
	close(e.src.Gate)
	waitFor(t, func() bool { a := e.svc.Activity(); return !a.Pulling && a.Downloading == 0 && a.Queued == 0 })
	if e.src.MaxActive != 4 {
		t.Fatalf("max concurrent downloads = %d, want 4", e.src.MaxActive)
	}
	if got := len(e.lib.Episodes("UC1")); got != 6 {
		t.Fatalf("episodes: %d", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func TestSelfUpdateRunsWhenEnabledAndReportsFailure(t *testing.T) {
	e := setup(t, nil, nil)
	e.cfg.OnChange(e.svc.ConfigChanged)
	e.svc.selfUpdateOnce()
	if e.src.UpdateRuns != 0 {
		t.Fatal("must not self-update while disabled")
	}
	e.src.UpdateErr = errors.New("installed with a package manager")
	if err := e.cfg.Update(func(c *podsync.Config) error { c.Download.SelfUpdate = true; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		e.src.Mu.Lock()
		defer e.src.Mu.Unlock()
		return e.src.UpdateRuns == 1
	})
	waitFor(t, func() bool {
		for _, is := range e.st.Active() {
			if is.Key == "selfupdate:youtube" {
				return true
			}
		}
		return false
	})
	// A later success clears the warning.
	e.src.Mu.Lock()
	e.src.UpdateErr = nil
	e.src.Mu.Unlock()
	e.svc.selfUpdateOnce()
	for _, is := range e.st.Active() {
		if is.Key == "selfupdate:youtube" {
			t.Fatal("warning should clear after a successful update")
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
