// Package puller runs the update loop: list each feed's newest episodes,
// download the new ones through a bounded queue, then apply cleanup.
package puller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"podsync-go/internal/config"
	"podsync-go/internal/library"
	"podsync-go/internal/source"
	"podsync-go/internal/status"
)

const (
	// MaxAttempts is how many times the scheduler tries a failing episode
	// before leaving it for a manual retry.
	MaxAttempts  = 3
	startupDelay = 3 * time.Second
)

var ErrBusy = errors.New("an update is already running")

type job struct {
	feedID string
	ep     source.EpisodeInfo
	manual bool
	state  string // "queued" | "downloading"
	done   chan struct{}
}

type Service struct {
	ctx context.Context
	cfg *config.Store
	lib *library.Library
	reg *source.Registry
	st  *status.Tracker
	now func() time.Time

	pullMu sync.Mutex // held for the duration of a pull

	mu       sync.Mutex
	jobs     map[string]*job // key feedID/epID, queued or running
	queue    []*job
	active   int
	pulling  bool
	failures map[string]map[string]string // feedID -> epID -> message
	versions map[string]string
	wake     chan struct{}
}

func New(ctx context.Context, cfg *config.Store, lib *library.Library, reg *source.Registry, st *status.Tracker) *Service {
	s := &Service{
		ctx: ctx, cfg: cfg, lib: lib, reg: reg, st: st, now: time.Now,
		jobs:     map[string]*job{},
		failures: map[string]map[string]string{},
		versions: map[string]string{},
		wake:     make(chan struct{}, 1),
	}
	st.SetActivity(s.Activity)
	return s
}

func jobKey(feedID, epID string) string { return feedID + "/" + epID }

// Activity reports live background work for the status banner.
func (s *Service) Activity() status.Activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return status.Activity{Pulling: s.pulling, Downloading: s.active, Queued: len(s.queue)}
}

// Versions returns the tool versions found by the last health check.
func (s *Service) Versions() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.versions))
	for k, v := range s.versions {
		out[k] = v
	}
	return out
}

// JobState reports whether an episode is queued or downloading ("" if not).
func (s *Service) JobState(feedID, epID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[jobKey(feedID, epID)]; ok {
		return j.state
	}
	return ""
}

// ---- scheduler ----

// Reconcile makes sure every configured feed has a directory in the library.
func (s *Service) Reconcile() {
	for _, f := range s.cfg.Get().Feeds {
		if err := s.lib.EnsureFeed(f.Source, f.ID); err != nil {
			s.st.Report("library:"+f.ID, status.Error, fmt.Sprintf("Cannot create storage for %q: %v", f.Name, err))
		} else {
			s.st.Clear("library:" + f.ID)
		}
	}
}

// ConfigChanged should be called after the configuration is updated.
func (s *Service) ConfigChanged(old, cur config.Config) {
	s.Reconcile()
	s.mu.Lock()
	s.scheduleLocked() // concurrency may have changed
	s.mu.Unlock()
	if old.Download.UpdateInterval != cur.Download.UpdateInterval {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	go s.CheckHealth()
	if !old.Download.SelfUpdate && cur.Download.SelfUpdate {
		go s.selfUpdateOnce()
	}
}

// Run blocks until the service context is cancelled.
func (s *Service) Run() {
	s.Reconcile()
	s.CheckHealth()
	go s.selfUpdateLoop()
	timer := time.NewTimer(startupDelay)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
			timer.Reset(s.cfg.Get().Download.UpdateInterval.Std())
		case <-timer.C:
			if err := s.PullAll(); err != nil && !errors.Is(err, ErrBusy) {
				log.Printf("pull: %v", err)
			}
			timer.Reset(s.cfg.Get().Download.UpdateInterval.Std())
		}
	}
}

// CheckHealth verifies each source's tooling and updates the status banner.
func (s *Service) CheckHealth() {
	for _, src := range s.reg.All() {
		h := src.Check(s.ctx)
		key := "health:" + src.Kind()
		s.mu.Lock()
		s.versions[src.Kind()] = h.Version
		s.mu.Unlock()
		s.st.Clear(key)
		s.st.Clear(key + ":warn")
		if len(h.Errors) > 0 {
			s.st.Report(key, status.Error, fmt.Sprintf("%s is not working: %s", src.Kind(), h.Errors[0]))
		}
		if len(h.Warnings) > 0 {
			s.st.Report(key+":warn", status.Warning, h.Warnings[0])
		}
	}
}

func (s *Service) selfUpdateOnce() {
	if !s.cfg.Get().Download.SelfUpdate {
		return
	}
	for _, src := range s.reg.All() {
		u, ok := src.(source.Updater)
		if !ok {
			continue
		}
		key := "selfupdate:" + src.Kind()
		out, err := u.SelfUpdate(s.ctx)
		if err != nil {
			s.st.Report(key, status.Warning, fmt.Sprintf("Self-update of %s tooling failed: %v", src.Kind(), err))
			continue
		}
		s.st.Clear(key)
		log.Printf("self-update (%s): %s", src.Kind(), out)
	}
	s.CheckHealth()
}

func (s *Service) selfUpdateLoop() {
	s.selfUpdateOnce()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.selfUpdateOnce()
		}
	}
}

// ---- pulling ----

// TriggerAll starts an update of every feed in the background.
func (s *Service) TriggerAll() error {
	if !s.pullMu.TryLock() {
		return ErrBusy
	}
	s.setPulling(true) // visible immediately, not only once the goroutine is scheduled
	go func() {
		defer s.pullMu.Unlock()
		s.pullAllLocked()
	}()
	return nil
}

// TriggerFeed starts an update of one feed in the background.
func (s *Service) TriggerFeed(id string) error {
	if _, ok := s.cfg.Get().Feed(id); !ok {
		return fmt.Errorf("unknown feed %q", id)
	}
	if !s.pullMu.TryLock() {
		return ErrBusy
	}
	s.setPulling(true)
	go func() {
		defer s.pullMu.Unlock()
		s.pullFeedsLocked([]string{id})
	}()
	return nil
}

// PullAll updates every feed and waits for the downloads to finish.
func (s *Service) PullAll() error {
	if !s.pullMu.TryLock() {
		return ErrBusy
	}
	defer s.pullMu.Unlock()
	s.pullAllLocked()
	return nil
}

// PullFeed updates one feed and waits for its downloads to finish.
func (s *Service) PullFeed(id string) error {
	if _, ok := s.cfg.Get().Feed(id); !ok {
		return fmt.Errorf("unknown feed %q", id)
	}
	if !s.pullMu.TryLock() {
		return ErrBusy
	}
	defer s.pullMu.Unlock()
	s.pullFeedsLocked([]string{id})
	return nil
}

func (s *Service) pullAllLocked() {
	var ids []string
	for _, f := range s.cfg.Get().Feeds {
		ids = append(ids, f.ID)
	}
	s.CheckHealth()
	s.pullFeedsLocked(ids)
}

func (s *Service) setPulling(v bool) {
	s.mu.Lock()
	s.pulling = v
	s.mu.Unlock()
}

func (s *Service) pullFeedsLocked(ids []string) {
	s.setPulling(true)
	defer s.setPulling(false)
	var all []*job
	for _, id := range ids {
		if s.ctx.Err() != nil {
			return
		}
		all = append(all, s.listAndEnqueue(id)...)
	}
	for _, j := range all {
		select {
		case <-j.done:
		case <-s.ctx.Done():
			return
		}
	}
	for _, id := range ids {
		s.Cleanup(id)
	}
}

// listAndEnqueue lists a feed's newest page_size episodes and enqueues the
// ones that are new and pass the feed's filters.
func (s *Service) listAndEnqueue(id string) []*job {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(id)
	if !ok {
		return nil
	}
	key := "list:" + id
	src, err := s.reg.Get(f.Source)
	if err != nil {
		s.st.Report(key, status.Error, fmt.Sprintf("Feed %q: %v", f.Name, err))
		return nil
	}
	opts := f.Effective(cfg.Defaults)
	filter, err := NewFilter(opts)
	if err != nil {
		s.st.Report(key, status.Error, fmt.Sprintf("Feed %q has an invalid title filter: %v", f.Name, err))
		return nil
	}
	info := source.FeedInfo{Source: f.Source, Kind: f.Kind, ID: f.ID, Name: f.Name}
	eps, err := src.ListEpisodes(s.ctx, info, 0, opts.PageSize)
	if err != nil {
		if s.ctx.Err() == nil {
			s.st.Report(key, status.Error, fmt.Sprintf("Could not list episodes for %q: %v", f.Name, err))
		}
		return nil
	}
	s.st.Clear(key)
	s.refreshMeta(id, src, info)

	var jobs []*job
	for _, ep := range eps {
		if ep.Unavailable || ep.IsLive || !filter.Allows(ep) {
			continue
		}
		if known, ok := s.lib.Episode(id, ep.ID); ok {
			retry := known.Status == library.StatusFailed && known.Attempts < MaxAttempts
			if !retry {
				continue
			}
		}
		if j := s.enqueue(id, ep, false); j != nil {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

// refreshMeta stamps the last pull time and fills in missing image/description once.
func (s *Service) refreshMeta(id string, src source.Source, info source.FeedInfo) {
	sum, _ := s.lib.Feed(id)
	var fresh source.FeedInfo
	if sum.Meta.ImageURL == "" {
		if r, err := src.Resolve(s.ctx, info.ID); err == nil {
			fresh = r
		}
	}
	_ = s.lib.UpdateMeta(id, func(m *library.FeedMeta) {
		m.LastPull = s.now().UTC()
		if m.Name == "" {
			m.Name = info.Name
		}
		if fresh.ID != "" {
			m.ImageURL, m.Description = fresh.ImageURL, fresh.Description
		}
	})
}

// ---- download queue ----

// RequestDownload queues a user-requested download (Load More / Retry).
func (s *Service) RequestDownload(feedID string, ep source.EpisodeInfo) error {
	if _, ok := s.cfg.Get().Feed(feedID); !ok {
		return fmt.Errorf("unknown feed %q", feedID)
	}
	if known, ok := s.lib.Episode(feedID, ep.ID); ok && known.Status == library.StatusDownloaded {
		return nil
	}
	s.enqueue(feedID, ep, true)
	return nil
}

func (s *Service) enqueue(feedID string, ep source.EpisodeInfo, manual bool) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := jobKey(feedID, ep.ID)
	if j, ok := s.jobs[key]; ok {
		if manual {
			j.manual = true
		}
		return j
	}
	j := &job{feedID: feedID, ep: ep, manual: manual, state: "queued", done: make(chan struct{})}
	s.jobs[key] = j
	s.queue = append(s.queue, j)
	s.scheduleLocked()
	return j
}

func (s *Service) scheduleLocked() {
	limit := s.cfg.Get().Download.Concurrency
	for s.active < limit && len(s.queue) > 0 {
		j := s.queue[0]
		s.queue = s.queue[1:]
		s.active++
		j.state = "downloading"
		go s.runJob(j)
	}
}

func (s *Service) runJob(j *job) {
	s.download(j)
	s.mu.Lock()
	s.active--
	delete(s.jobs, jobKey(j.feedID, j.ep.ID))
	s.scheduleLocked()
	s.mu.Unlock()
	close(j.done)
}

func (s *Service) download(j *job) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(j.feedID)
	if !ok {
		return // feed was deleted while queued
	}
	src, err := s.reg.Get(f.Source)
	if err != nil {
		s.recordFailure(j, f, err)
		return
	}
	s.mu.Lock()
	manual := j.manual
	s.mu.Unlock()
	opts := f.Effective(cfg.Defaults)
	tmp := filepath.Join(s.lib.TempDir(), j.feedID, j.ep.ID) // per-episode: concurrent jobs must not share a staging dir
	res, err := src.Download(s.ctx, j.ep, source.DownloadOptions{
		Audio:     opts.Format == config.FormatAudio,
		MaxHeight: opts.MaxResolution,
	}, tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		s.recordFailure(j, f, err)
		return
	}
	feedDir, ok := s.lib.FeedDir(j.feedID)
	if !ok {
		_ = os.RemoveAll(tmp)
		return // feed deleted mid-download
	}
	if err := os.MkdirAll(feedDir, 0o755); err != nil {
		s.recordFailure(j, f, err)
		return
	}
	if err := os.Rename(filepath.Join(tmp, res.File), filepath.Join(feedDir, res.File)); err != nil {
		s.recordFailure(j, f, fmt.Errorf("move into library: %w", err))
		return
	}
	thumb := ""
	if res.Thumb != "" {
		if err := os.Rename(filepath.Join(tmp, res.Thumb), filepath.Join(feedDir, res.Thumb)); err == nil {
			thumb = res.Thumb
		} else {
			log.Printf("keeping %s/%s without thumbnail: %v", j.feedID, j.ep.ID, err)
		}
	}
	_ = os.RemoveAll(tmp)
	now := s.now().UTC()
	ep := library.Episode{
		ID: j.ep.ID, Title: res.Episode.Title, Description: res.Episode.Description, URL: res.Episode.URL,
		Thumbnail: firstNonEmpty(res.Episode.Thumbnail, j.ep.Thumbnail), ThumbFile: thumb,
		Published: res.Episode.Published, DurationSec: res.Episode.DurationSec,
		Channel: firstNonEmpty(res.Episode.Channel, j.ep.Channel), ViewCount: maxInt64(res.Episode.ViewCount, j.ep.ViewCount), Height: res.Episode.Height,
		IsShort: j.ep.IsShort, Status: library.StatusDownloaded, File: res.File, Size: res.Size,
		DownloadedAt: &now, Manual: manual,
	}
	if ep.Published.IsZero() {
		ep.Published = now
	}
	if prev, ok := s.lib.Episode(j.feedID, j.ep.ID); ok && prev.Manual {
		ep.Manual = true
	}
	if err := s.lib.PutEpisode(j.feedID, ep); err != nil {
		s.recordFailure(j, f, err)
		return
	}
	s.clearFailure(j.feedID, j.ep.ID)
}

func (s *Service) recordFailure(j *job, f config.Feed, err error) {
	if s.ctx.Err() != nil {
		return // shutting down; do not poison the episode
	}
	prev, _ := s.lib.Episode(j.feedID, j.ep.ID)
	attempts := prev.Attempts + 1
	if j.manual {
		attempts = 1
	}
	title := firstNonEmpty(prev.Title, j.ep.Title, j.ep.ID)
	_ = s.lib.PutEpisode(j.feedID, library.Episode{
		ID: j.ep.ID, Title: title, URL: firstNonEmpty(prev.URL, j.ep.URL), Thumbnail: firstNonEmpty(prev.Thumbnail, j.ep.Thumbnail),
		Channel: firstNonEmpty(prev.Channel, j.ep.Channel), ViewCount: maxInt64(prev.ViewCount, j.ep.ViewCount),
		Published: prev.Published, DurationSec: maxInt(prev.DurationSec, j.ep.DurationSec), IsShort: j.ep.IsShort,
		Status: library.StatusFailed, Attempts: attempts, LastError: err.Error(), Manual: j.manual || prev.Manual,
	})
	s.mu.Lock()
	if s.failures[j.feedID] == nil {
		s.failures[j.feedID] = map[string]string{}
	}
	s.failures[j.feedID][j.ep.ID] = fmt.Sprintf("%q: %v", title, err)
	n, msg := len(s.failures[j.feedID]), s.failures[j.feedID][j.ep.ID]
	s.mu.Unlock()
	text := fmt.Sprintf("Download failed in %q — %s", f.Name, msg)
	if n > 1 {
		text = fmt.Sprintf("%d downloads failed in %q; latest: %s", n, f.Name, msg)
	}
	s.st.Report("download:"+j.feedID, status.Error, text)
}

func (s *Service) clearFailure(feedID, epID string) {
	s.mu.Lock()
	delete(s.failures[feedID], epID)
	empty := len(s.failures[feedID]) == 0
	if empty {
		delete(s.failures, feedID)
	}
	s.mu.Unlock()
	if empty {
		s.st.Clear("download:" + feedID)
	}
}

// ForgetFeed drops in-memory state for a deleted feed.
func (s *Service) ForgetFeed(feedID string) {
	s.mu.Lock()
	delete(s.failures, feedID)
	s.mu.Unlock()
	s.st.ClearPrefix("download:" + feedID)
	s.st.ClearPrefix("list:" + feedID)
	s.st.ClearPrefix("cleanup:" + feedID)
	s.st.ClearPrefix("library:" + feedID)
}

// ---- cleanup ----

// Cleanup removes media beyond the feed's retention settings. Episodes the
// user downloaded manually are exempt. The sidecar stays (as "cleaned") so
// the scheduler does not download the episode again.
func (s *Service) Cleanup(feedID string) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(feedID)
	if !ok {
		return
	}
	opts := f.Effective(cfg.Defaults)
	if opts.CleanupKeepLast == 0 && opts.CleanupAfterDays == 0 {
		return
	}
	var eligible []library.Episode
	for _, ep := range s.lib.Episodes(feedID) {
		if ep.Status == library.StatusDownloaded && !ep.Manual {
			eligible = append(eligible, ep) // already newest first
		}
	}
	doomed := map[string]library.Episode{}
	if k := opts.CleanupKeepLast; k > 0 && len(eligible) > k {
		for _, ep := range eligible[k:] {
			doomed[ep.ID] = ep
		}
	}
	if d := opts.CleanupAfterDays; d > 0 {
		cutoff := s.now().AddDate(0, 0, -d)
		for _, ep := range eligible {
			if !ep.Published.IsZero() && ep.Published.Before(cutoff) {
				doomed[ep.ID] = ep
			}
		}
	}
	ids := make([]string, 0, len(doomed))
	for id := range doomed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	key := "cleanup:" + feedID
	failed := false
	for _, id := range ids {
		ep := doomed[id]
		if err := s.lib.RemoveMedia(feedID, ep); err != nil {
			s.st.Report(key, status.Warning, fmt.Sprintf("Cleanup of %q failed: %v", f.Name, err))
			failed = true
			continue
		}
		ep.Status, ep.File, ep.Size = library.StatusCleaned, "", 0
		if err := s.lib.PutEpisode(feedID, ep); err != nil {
			s.st.Report(key, status.Warning, fmt.Sprintf("Cleanup of %q failed: %v", f.Name, err))
			failed = true
		}
	}
	if !failed {
		s.st.Clear(key)
	}
}

// ---- Load More ----

// OlderPage is one window of a feed's older, not-yet-known episodes.
type OlderPage struct {
	Episodes   []source.EpisodeInfo
	NextOffset int
	More       bool
}

// Older lists the window [offset, offset+page_size) of a feed's source and
// returns the episodes that pass the feed's filters and are not already in
// the library (those are shown in the feed's main list).
func (s *Service) Older(ctx context.Context, feedID string, offset int) (OlderPage, error) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(feedID)
	if !ok {
		return OlderPage{}, fmt.Errorf("unknown feed %q", feedID)
	}
	src, err := s.reg.Get(f.Source)
	if err != nil {
		return OlderPage{}, err
	}
	opts := f.Effective(cfg.Defaults)
	filter, err := NewFilter(opts)
	if err != nil {
		return OlderPage{}, err
	}
	info := source.FeedInfo{Source: f.Source, Kind: f.Kind, ID: f.ID, Name: f.Name}
	eps, err := src.ListEpisodes(ctx, info, offset, opts.PageSize)
	if err != nil {
		s.st.Report("list:"+feedID, status.Error, fmt.Sprintf("Could not list episodes for %q: %v", f.Name, err))
		return OlderPage{}, err
	}
	s.st.Clear("list:" + feedID)
	page := OlderPage{NextOffset: offset + len(eps), More: len(eps) >= opts.PageSize}
	for _, ep := range eps {
		if ep.Unavailable || ep.IsLive || !filter.Allows(ep) {
			continue
		}
		if _, known := s.lib.Episode(feedID, ep.ID); known {
			continue
		}
		page.Episodes = append(page.Episodes, ep)
	}
	return page, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
