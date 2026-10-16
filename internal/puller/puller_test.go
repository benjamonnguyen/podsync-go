package puller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"podsync-go/internal/config"
	"podsync-go/internal/library"
	"podsync-go/internal/source"
	"podsync-go/internal/status"
)

var base = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

type fakeSource struct {
	mu        sync.Mutex
	eps       []source.EpisodeInfo // newest first
	listErr   error
	dlErr     map[string]error
	gate      chan struct{} // when non-nil, Download blocks until closed
	active    int
	maxActive int
	downloads map[string]int
	health    source.Health
	updates   int
	updateErr error
}

func (f *fakeSource) SelfUpdate(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	return "updated", f.updateErr
}

func (f *fakeSource) Kind() string { return "youtube" }
func (f *fakeSource) Check(context.Context) source.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health
}
func (f *fakeSource) Resolve(_ context.Context, ref string) (source.FeedInfo, error) {
	return source.FeedInfo{Source: "youtube", Kind: "channel", ID: ref, Name: "Fake", ImageURL: "http://img/x.jpg"}, nil
}
func (f *fakeSource) Search(context.Context, string, source.SearchKind) ([]source.SearchResult, error) {
	return nil, nil
}
func (f *fakeSource) ListEpisodes(_ context.Context, _ source.FeedInfo, offset, limit int) ([]source.EpisodeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if offset >= len(f.eps) {
		return nil, nil
	}
	end := offset + limit
	if end > len(f.eps) {
		end = len(f.eps)
	}
	return append([]source.EpisodeInfo(nil), f.eps[offset:end]...), nil
}
func (f *fakeSource) Download(ctx context.Context, ep source.EpisodeInfo, _ source.DownloadOptions, dest string) (source.DownloadResult, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	gate := f.gate
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return source.DownloadResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	f.downloads[ep.ID]++
	err := f.dlErr[ep.ID]
	f.mu.Unlock()
	if err != nil {
		return source.DownloadResult{}, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return source.DownloadResult{}, err
	}
	if err := os.WriteFile(filepath.Join(dest, ep.ID+".mp4"), []byte("media"), 0o644); err != nil {
		return source.DownloadResult{}, err
	}
	idx := 0
	for i, e := range f.eps {
		if e.ID == ep.ID {
			idx = i
		}
	}
	return source.DownloadResult{File: ep.ID + ".mp4", Size: 5, Episode: source.EpisodeInfo{
		ID: ep.ID, Title: "T " + ep.ID, Published: base.AddDate(0, 0, -idx), URL: "http://x/" + ep.ID, DurationSec: 100,
	}}, nil
}

func (f *fakeSource) downloadCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downloads[id]
}

func mkEps(n int) []source.EpisodeInfo {
	var out []source.EpisodeInfo
	for i := 0; i < n; i++ {
		out = append(out, source.EpisodeInfo{ID: fmt.Sprintf("ep%02d", i), Title: fmt.Sprintf("Episode %d", i), DurationSec: 600})
	}
	return out
}

type env struct {
	svc *Service
	src *fakeSource
	lib *library.Library
	st  *status.Tracker
	cfg *config.Store
}

func setup(t *testing.T, eps []source.EpisodeInfo, mutate func(*config.Config)) *env {
	t.Helper()
	c := config.Default()
	c.Defaults.PageSize = 5
	c.Defaults.CleanupKeepLast = 0
	c.Feeds = []config.Feed{{ID: "UC1", Name: "One", Kind: "channel", Source: "youtube"}}
	if mutate != nil {
		mutate(&c)
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewMemoryStore(c)
	lib := library.New(t.TempDir())
	if err := lib.Load(); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{eps: eps, downloads: map[string]int{}, dlErr: map[string]error{}}
	st := status.New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc := New(ctx, cfg, lib, source.NewRegistry(src), st)
	svc.Reconcile()
	return &env{svc, src, lib, st, cfg}
}

func TestPullDownloadsNewestPageAndIsIdempotent(t *testing.T) {
	e := setup(t, mkEps(8), nil)
	if err := e.svc.PullFeed("UC1"); err != nil {
		t.Fatal(err)
	}
	eps := e.lib.Episodes("UC1")
	if len(eps) != 5 {
		t.Fatalf("want page_size=5 episodes, got %d", len(eps))
	}
	if eps[0].ID != "ep00" || eps[0].Status != library.StatusDownloaded || eps[0].File != "ep00.mp4" || eps[0].Manual {
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
	if sum, _ := e.lib.Feed("UC1"); sum.Meta.LastPull.IsZero() || sum.Meta.ImageURL != "http://img/x.jpg" {
		t.Fatalf("meta: %+v", sum.Meta)
	}
	e.svc.PullFeed("UC1")
	for _, ep := range eps {
		if n := e.src.downloadCount(ep.ID); n != 1 {
			t.Fatalf("%s downloaded %d times", ep.ID, n)
		}
	}
}

func TestFiltersAndSkippedEntries(t *testing.T) {
	eps := mkEps(7)
	eps[0].IsShort = true
	eps[1].IsLive = true
	eps[2].Unavailable = true
	eps[3].Title = "Sponsored: thing"
	eps[4].Title = "Keep me"
	e := setup(t, eps, func(c *config.Config) {
		c.Defaults.PageSize = 7
		c.Defaults.TitleExclude = "^Sponsored"
		c.Defaults.TitleInclude = "(?i)keep|episode"
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
	e := setup(t, mkEps(2), nil)
	e.src.dlErr["ep00"] = errors.New("Video unavailable")
	e.svc.PullFeed("UC1")
	ep, _ := e.lib.Episode("UC1", "ep00")
	if ep.Status != library.StatusFailed || ep.Attempts != 1 || ep.LastError != "Video unavailable" {
		t.Fatalf("%+v", ep)
	}
	if iss := e.st.Active(); len(iss) != 1 || iss[0].Key != "download:UC1" {
		t.Fatalf("banner: %+v", iss)
	}
	e.svc.PullFeed("UC1")
	e.svc.PullFeed("UC1")
	e.svc.PullFeed("UC1") // attempts exhausted: no 4th try
	if n := e.src.downloadCount("ep00"); n != MaxAttempts {
		t.Fatalf("want %d attempts, got %d", MaxAttempts, n)
	}
	delete(e.src.dlErr, "ep00")
	if err := e.svc.RequestDownload("UC1", source.EpisodeInfo{ID: "ep00"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return e.svc.JobState("UC1", "ep00") == "" && e.lib.Episodes("UC1")[0].Status == library.StatusDownloaded
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
	e := setup(t, mkEps(2), nil)
	e.src.listErr = errors.New("HTTP Error 429")
	e.svc.PullAll()
	iss := e.st.Active()
	if len(iss) != 1 || iss[0].Key != "list:UC1" {
		t.Fatalf("%+v", iss)
	}
	e.src.listErr = nil
	e.svc.PullAll()
	if len(e.st.Active()) != 0 {
		t.Fatalf("%+v", e.st.Active())
	}
}

func TestHealthErrorsBecomeBanner(t *testing.T) {
	e := setup(t, nil, nil)
	e.src.health = source.Health{Errors: []string{"yt-dlp not found"}, Warnings: []string{"ffmpeg not found"}}
	e.svc.CheckHealth()
	if len(e.st.Active()) != 2 {
		t.Fatalf("%+v", e.st.Active())
	}
	e.src.health = source.Health{Version: "1.0"}
	e.svc.CheckHealth()
	if len(e.st.Active()) != 0 || e.svc.Versions()["youtube"] != "1.0" {
		t.Fatalf("%+v", e.st.Active())
	}
}

func TestCleanupKeepLastTombstonesAndManualExempt(t *testing.T) {
	e := setup(t, mkEps(5), func(c *config.Config) { c.Defaults.CleanupKeepLast = 2 })
	e.svc.PullFeed("UC1")
	// ep00,ep01 kept; ep02..04 cleaned.
	dir, _ := e.lib.FeedDir("UC1")
	for id, wantKept := range map[string]bool{"ep00": true, "ep01": true, "ep02": false, "ep03": false, "ep04": false} {
		_, err := os.Stat(filepath.Join(dir, id+".mp4"))
		if (err == nil) != wantKept {
			t.Errorf("%s: media present=%v want %v", id, err == nil, wantKept)
		}
		ep, _ := e.lib.Episode("UC1", id)
		if wantKept != (ep.Status == library.StatusDownloaded) {
			t.Errorf("%s status %s", id, ep.Status)
		}
	}
	// Next pull must not resurrect cleaned episodes.
	e.svc.PullFeed("UC1")
	if n := e.src.downloadCount("ep04"); n != 1 {
		t.Fatalf("cleaned episode re-downloaded: %d", n)
	}
	// Manually downloading a cleaned episode pins it.
	e.svc.RequestDownload("UC1", source.EpisodeInfo{ID: "ep04"})
	waitFor(t, func() bool { ep, _ := e.lib.Episode("UC1", "ep04"); return ep.Status == library.StatusDownloaded })
	e.svc.Cleanup("UC1")
	if ep, _ := e.lib.Episode("UC1", "ep04"); ep.Status != library.StatusDownloaded || !ep.Manual {
		t.Fatalf("manual episode must survive cleanup: %+v", ep)
	}
}

func TestCleanupAfterDays(t *testing.T) {
	e := setup(t, mkEps(3), func(c *config.Config) { c.Defaults.CleanupAfterDays = 1 })
	e.svc.now = func() time.Time { return base.AddDate(0, 0, 1).Add(12 * time.Hour) } // ep00 is 1.5d old
	e.svc.PullFeed("UC1")
	if ep, _ := e.lib.Episode("UC1", "ep00"); ep.Status != library.StatusCleaned {
		t.Fatalf("ep00: %+v", ep)
	}
	e.svc.now = func() time.Time { return base }
	e.svc.Cleanup("UC1") // not relevant now; everything already cleaned or fresh
}

func TestConcurrencyCapAndBusy(t *testing.T) {
	e := setup(t, mkEps(6), func(c *config.Config) { c.Defaults.PageSize = 6; c.Download.Concurrency = 2 })
	e.src.gate = make(chan struct{})
	if err := e.svc.TriggerAll(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a := e.svc.Activity(); return a.Downloading == 2 && a.Queued == 4 && a.Pulling })
	if err := e.svc.TriggerAll(); !errors.Is(err, ErrBusy) {
		t.Fatalf("second trigger should be busy, got %v", err)
	}
	// Raise the cap live.
	e.cfg.OnChange(e.svc.ConfigChanged)
	if err := e.cfg.Update(func(c *config.Config) error { c.Download.Concurrency = 4; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { e.src.mu.Lock(); defer e.src.mu.Unlock(); return e.src.active == 4 })
	close(e.src.gate)
	waitFor(t, func() bool { a := e.svc.Activity(); return !a.Pulling && a.Downloading == 0 && a.Queued == 0 })
	if e.src.maxActive != 4 {
		t.Fatalf("max concurrent downloads = %d, want 4", e.src.maxActive)
	}
	if got := len(e.lib.Episodes("UC1")); got != 6 {
		t.Fatalf("episodes: %d", got)
	}
}

func TestOlderWindowSkipsKnownAndFiltered(t *testing.T) {
	eps := mkEps(14)
	eps[6].IsShort = true
	e := setup(t, eps, nil) // page_size 5, shorts excluded
	e.svc.PullFeed("UC1")   // ep00..ep04
	p, err := e.svc.Older(context.Background(), "UC1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !p.More || p.NextOffset != 10 || len(p.Episodes) != 4 || p.Episodes[0].ID != "ep05" {
		t.Fatalf("%+v", p)
	}
	p2, _ := e.svc.Older(context.Background(), "UC1", 10)
	if p2.More || len(p2.Episodes) != 4 || p2.NextOffset != 14 {
		t.Fatalf("last page: %+v", p2)
	}
	// Episodes already in the library (any status) are not offered again.
	e.svc.RequestDownload("UC1", source.EpisodeInfo{ID: "ep05"})
	waitFor(t, func() bool { return e.svc.JobState("UC1", "ep05") == "" })
	p3, _ := e.svc.Older(context.Background(), "UC1", 5)
	if len(p3.Episodes) != 3 {
		t.Fatalf("known episode offered again: %+v", p3.Episodes)
	}
	e.src.listErr = errors.New("nope")
	if _, err := e.svc.Older(context.Background(), "UC1", 5); err == nil {
		t.Fatal("expected error")
	}
	if len(e.st.Active()) != 1 {
		t.Fatal("list failure should reach the banner")
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
	if e.src.updates != 0 {
		t.Fatal("must not self-update while disabled")
	}
	e.src.updateErr = errors.New("installed with a package manager")
	if err := e.cfg.Update(func(c *config.Config) error { c.Download.SelfUpdate = true; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		e.src.mu.Lock()
		defer e.src.mu.Unlock()
		return e.src.updates == 1
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
	e.src.mu.Lock()
	e.src.updateErr = nil
	e.src.mu.Unlock()
	e.svc.selfUpdateOnce()
	for _, is := range e.st.Active() {
		if is.Key == "selfupdate:youtube" {
			t.Fatal("warning should clear after a successful update")
		}
	}
}
