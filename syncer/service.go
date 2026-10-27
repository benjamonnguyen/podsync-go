// Package syncer is the application service: it runs the update loop (list each
// feed's newest episodes, download the new ones through a bounded queue, apply
// cleanup) and implements podsync.Syncer for clients. It depends only on the
// interfaces in package podsync.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"podsync"
	"sort"
	"sync"
	"time"
)

const (
	// MaxAttempts is how many times the scheduler tries a failing episode
	// before leaving it for a manual retry.
	MaxAttempts  = 3
	startupDelay = 3 * time.Second

	// maxCatchUpPages bounds how far listAndEnqueue will page back looking for
	// an episode it already has a record of (see catchUpPages). Without a cap,
	// a feed whose episodes never get a library record (every one filtered
	// out, never attempted) would page back this far on every single check.
	maxCatchUpPages = 10
)

// feedRef identifies one configured feed: the same id may be configured
// twice, once per format, so id alone is ambiguous.
type feedRef struct{ id, format string }

// refKey is the internal map/status key for a feedRef (ids cannot contain
// "/", so this cannot collide).
func refKey(id, format string) string { return id + "/" + format }

type job struct {
	feedID string
	format string
	ep     podsync.EpisodeInfo
	manual bool
	state  string // "queued" | "downloading"
	done   chan struct{}
}

type Service struct {
	ctx context.Context
	cfg podsync.ConfigStore
	lib podsync.Library
	reg *podsync.Registry
	st  podsync.StatusTracker
	now func() time.Time

	pullMu sync.Mutex // held for the duration of a pull

	mu       sync.Mutex
	jobs     map[string]*job // key refKey(feedID,format)/epID, queued or running
	queue    []*job
	active   int
	pulling  bool
	failures map[string]map[string]string // refKey(feedID,format) -> epID -> message
	versions map[string]string
	wake     chan struct{}
}

func New(ctx context.Context, cfg podsync.ConfigStore, lib podsync.Library, reg *podsync.Registry, st podsync.StatusTracker) *Service {
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

func jobKey(feedID, format, epID string) string { return refKey(feedID, format) + "/" + epID }

// Activity reports live background work for the status banner.
func (s *Service) Activity() podsync.Activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return podsync.Activity{Pulling: s.pulling, Downloading: s.active, Queued: len(s.queue)}
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
func (s *Service) JobState(feedID, format, epID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[jobKey(feedID, format, epID)]; ok {
		return j.state
	}
	return ""
}

// ---- scheduler ----

// configuredRefs lists every feed currently configured, as (id, effective format) pairs.
func configuredRefs(cfg podsync.Config) []feedRef {
	refs := make([]feedRef, 0, len(cfg.Feeds))
	for _, f := range cfg.Feeds {
		refs = append(refs, feedRef{f.ID, f.Effective(cfg.Defaults).Format})
	}
	return refs
}

// Reconcile makes sure every configured feed has a directory in the library.
func (s *Service) Reconcile() {
	cfg := s.cfg.Get()
	for _, f := range cfg.Feeds {
		format := f.Effective(cfg.Defaults).Format
		key := refKey(f.ID, format)
		if err := s.lib.EnsureFeed(f.Source, f.ID, format); err != nil {
			s.st.Report("library:"+key, podsync.LevelError, fmt.Sprintf("Cannot create storage for %q: %v", f.ID, err))
		} else {
			s.st.Clear("library:" + key)
		}
	}
}

// ConfigChanged should be called after the configuration is updated.
func (s *Service) ConfigChanged(old, cur podsync.Config) {
	for _, f := range old.Feeds {
		oldFormat := f.Effective(old.Defaults).Format
		if _, still := cur.Feed(f.ID, oldFormat); !still {
			// Removed from config, or its format changed: either way this
			// (id, format) identity is no longer configured. The library
			// directory is left as an orphan; only the status banners for it
			// are cleared here.
			s.ForgetFeed(f.ID, oldFormat)
		}
	}
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
			if err := s.PullAll(); err != nil && !errors.Is(err, podsync.ErrBusy) {
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
			s.st.Report(key, podsync.LevelError, fmt.Sprintf("%s is not working: %s", src.Kind(), h.Errors[0]))
		}
		if len(h.Warnings) > 0 {
			s.st.Report(key+":warn", podsync.LevelWarning, h.Warnings[0])
		}
	}
}

func (s *Service) selfUpdateOnce() {
	if !s.cfg.Get().Download.SelfUpdate {
		return
	}
	for _, src := range s.reg.All() {
		u, ok := src.(podsync.SourceUpdater)
		if !ok {
			continue
		}
		key := "selfupdate:" + src.Kind()
		out, err := u.SelfUpdate(s.ctx)
		if err != nil {
			s.st.Report(key, podsync.LevelWarning, fmt.Sprintf("Self-update of %s tooling failed: %v", src.Kind(), err))
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
		return podsync.Busy()
	}
	s.setPulling(true) // visible immediately, not only once the goroutine is scheduled
	go func() {
		defer s.pullMu.Unlock()
		s.pullAllLocked()
	}()
	return nil
}

// TriggerFeed starts an update of one feed in the background.
func (s *Service) TriggerFeed(id, format string) error {
	if _, ok := s.cfg.Get().Feed(id, format); !ok {
		return podsync.NotFound("no such feed %q", id)
	}
	if !s.pullMu.TryLock() {
		return podsync.Busy()
	}
	s.setPulling(true)
	go func() {
		defer s.pullMu.Unlock()
		s.pullFeedsLocked([]feedRef{{id, format}})
	}()
	return nil
}

// PullAll updates every feed and waits for the downloads to finish.
func (s *Service) PullAll() error {
	if !s.pullMu.TryLock() {
		return podsync.Busy()
	}
	defer s.pullMu.Unlock()
	s.pullAllLocked()
	return nil
}

// PullFeed updates one feed and waits for its downloads to finish.
func (s *Service) PullFeed(id, format string) error {
	if _, ok := s.cfg.Get().Feed(id, format); !ok {
		return podsync.NotFound("no such feed %q", id)
	}
	if !s.pullMu.TryLock() {
		return podsync.Busy()
	}
	defer s.pullMu.Unlock()
	s.pullFeedsLocked([]feedRef{{id, format}})
	return nil
}

func (s *Service) pullAllLocked() {
	s.reloadFromDisk()
	refs := configuredRefs(s.cfg.Get())
	s.CheckHealth()
	s.pullFeedsLocked(refs)
}

func (s *Service) setPulling(v bool) {
	s.mu.Lock()
	s.pulling = v
	s.mu.Unlock()
}

func (s *Service) pullFeedsLocked(refs []feedRef) {
	s.setPulling(true)
	defer s.setPulling(false)
	_, _ = s.lib.Refresh() // the web UI may have added or deleted feeds
	var all []*job
	for _, ref := range refs {
		if s.ctx.Err() != nil {
			return
		}
		all = append(all, s.listAndEnqueue(ref)...)
	}
	for _, j := range all {
		select {
		case <-j.done:
		case <-s.ctx.Done():
			return
		}
	}
	for _, ref := range refs {
		s.Cleanup(ref.id, ref.format)
	}
}

// reloadFromDisk picks up what the web UI (or a hand edit) changed since the
// last Apply, so a missed Apply is corrected by the next pull at the latest.
func (s *Service) reloadFromDisk() {
	if _, err := s.cfg.Reload(); err != nil {
		s.st.Report("config", podsync.LevelError, "The configuration file could not be applied (the previous settings stay in effect): "+err.Error())
	} else {
		s.st.Clear("config")
	}
	_, _ = s.lib.Refresh() // the web UI may have added or deleted feeds
}

// listAndEnqueue lists a feed's newest page_size episodes and enqueues the
// ones that are new and pass the feed's filters.
func (s *Service) listAndEnqueue(ref feedRef) []*job {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(ref.id, ref.format)
	if !ok {
		return nil
	}
	key := "list:" + refKey(ref.id, ref.format)
	src, err := s.reg.Get(f.Source)
	if err != nil {
		s.st.Report(key, podsync.LevelError, fmt.Sprintf("Feed %q: %v", f.ID, err))
		return nil
	}
	opts := f.Effective(cfg.Defaults)
	filter, err := f.Filter(cfg.Defaults)
	if err != nil {
		s.st.Report(key, podsync.LevelError, fmt.Sprintf("Feed %q has an invalid title filter: %v", f.ID, err))
		return nil
	}
	info := podsync.FeedInfo{Source: f.Source, Kind: f.Kind, ID: f.ID, URL: f.URL, Reverse: f.PlaylistSort == podsync.PlaylistSortDesc}
	eps, err := src.ListEpisodes(s.ctx, info, 0, opts.PageSize)
	if err != nil {
		if s.ctx.Err() == nil {
			s.st.Report(key, podsync.LevelError, fmt.Sprintf("Could not list episodes for %q: %v", f.ID, err))
		}
		return nil
	}
	s.st.Clear(key)
	s.refreshMeta(ref, src, info)

	// If the page we just got is entirely episodes we've never seen, a
	// publish burst may have pushed older-but-still-unseen episodes past
	// page_size: on a fixed-size, offset-0 window, those would otherwise be
	// skipped forever (never downloaded, and never looked at again, since
	// every future check re-samples the same "newest page_size" window).
	// Keep paging back until a page contains an episode we already have a
	// record for (the boundary with what the previous check already saw),
	// the feed runs out, or maxCatchUpPages caps the extra queries.
	//
	// Skipped entirely on a feed's very first check (nothing downloaded yet,
	// so nothing can be "known"): otherwise this would walk back through the
	// whole back catalogue on every brand-new feed, which page_size exists
	// to prevent in the first place.
	all := eps
	everPulled := len(s.lib.Episodes(ref.id, ref.format)) > 0
	for pages, offset := 1, opts.PageSize; everPulled && len(eps) == opts.PageSize && !s.hasKnownEpisode(ref, eps) && pages <= maxCatchUpPages; pages, offset = pages+1, offset+opts.PageSize {
		if s.ctx.Err() != nil {
			return nil
		}
		more, err := src.ListEpisodes(s.ctx, info, offset, opts.PageSize)
		if err != nil {
			if s.ctx.Err() == nil {
				s.st.Report(key, podsync.LevelWarning, fmt.Sprintf("Could not fully check %q for missed episodes: %v", f.ID, err))
			}
			break
		}
		if len(more) == 0 {
			break
		}
		all = append(all, more...)
		eps = more
	}

	var jobs []*job
	for _, ep := range all {
		if ep.Unavailable || ep.IsLive || !filter.Allows(ep) {
			continue
		}
		if known, ok := s.lib.Episode(ref.id, ref.format, ep.ID); ok {
			retry := known.Status == podsync.StatusFailed && !known.MembersOnly && known.Attempts < MaxAttempts
			if !retry {
				continue
			}
		}
		if j := s.enqueue(ref, ep, false); j != nil {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

// hasKnownEpisode reports whether the library already has a record for any
// of eps — downloaded, cleaned or failed all count, since the sidecar JSON
// survives even after the media file is deleted. Used by listAndEnqueue to
// find where a previous check's listing left off.
func (s *Service) hasKnownEpisode(ref feedRef, eps []podsync.EpisodeInfo) bool {
	for _, ep := range eps {
		if _, ok := s.lib.Episode(ref.id, ref.format, ep.ID); ok {
			return true
		}
	}
	return false
}

// refreshMeta stamps the last pull time and fills in missing image/description once.
func (s *Service) refreshMeta(ref feedRef, src podsync.Source, info podsync.FeedInfo) {
	sum, _ := s.lib.Feed(ref.id, ref.format)
	var fresh podsync.FeedInfo
	if sum.Meta.ImageURL == "" {
		if r, err := src.Resolve(s.ctx, info.URL); err == nil {
			fresh = r
		}
	}
	_ = s.lib.UpdateMeta(ref.id, ref.format, func(m *podsync.FeedMeta) {
		m.LastPull = s.now().UTC()
		if fresh.ID != "" {
			m.Name, m.ImageURL, m.Description = fresh.Name, fresh.ImageURL, fresh.Description
		}
	})
}

// ---- download queue ----

// RequestDownload queues a user-requested download (Load More / Retry).
func (s *Service) RequestDownload(feedID, format string, ep podsync.EpisodeInfo) error {
	if _, ok := s.cfg.Get().Feed(feedID, format); !ok {
		return podsync.NotFound("no such feed %q", feedID)
	}
	if known, ok := s.lib.Episode(feedID, format, ep.ID); ok && known.Status == podsync.StatusDownloaded {
		return nil
	}
	s.enqueue(feedRef{feedID, format}, ep, true)
	return nil
}

// RetryMembersOnly queues every members-only episode of a feed that failed
// (the scheduler never retries those), as if never tried: the user has added
// cookies that may belong to a member. It returns how many it queued.
func (s *Service) RetryMembersOnly(feedID, format string) (int, error) {
	if _, ok := s.cfg.Get().Feed(feedID, format); !ok {
		return 0, podsync.NotFound("no such feed %q", feedID)
	}
	n := 0
	for _, ep := range s.lib.Episodes(feedID, format) {
		if !ep.BlockedMembersOnly() {
			continue
		}
		ep.Attempts = 0
		_ = s.lib.PutEpisode(feedID, format, ep)
		s.enqueue(feedRef{feedID, format}, podsync.EpisodeInfo{
			ID: ep.ID, Title: ep.Title, URL: ep.URL, Thumbnail: ep.Thumbnail, Published: ep.Published,
			DurationSec: ep.DurationSec, Channel: ep.Channel, ViewCount: ep.ViewCount, MembersOnly: true,
		}, false)
		n++
	}
	return n, nil
}

func (s *Service) enqueue(ref feedRef, ep podsync.EpisodeInfo, manual bool) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := jobKey(ref.id, ref.format, ep.ID)
	if j, ok := s.jobs[key]; ok {
		if manual {
			j.manual = true
		}
		return j
	}
	j := &job{feedID: ref.id, format: ref.format, ep: ep, manual: manual, state: "queued", done: make(chan struct{})}
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
	delete(s.jobs, jobKey(j.feedID, j.format, j.ep.ID))
	s.scheduleLocked()
	s.mu.Unlock()
	close(j.done)
}

func (s *Service) download(j *job) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(j.feedID, j.format)
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
	tmp := filepath.Join(s.lib.TempDir(), refKey(j.feedID, j.format), j.ep.ID) // per-episode: concurrent jobs must not share a staging dir
	res, err := src.Download(s.ctx, j.ep, podsync.DownloadOptions{
		Audio:          opts.Format == podsync.FormatAudio,
		MaxVideoHeight: opts.MaxVideoHeight,
		AudioQuality:   opts.AudioQuality,
	}, tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		s.recordFailure(j, f, err)
		return
	}
	feedDir, ok := s.lib.FeedDir(j.feedID, j.format)
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
			log.Printf("keeping %s/%s without thumbnail: %v", refKey(j.feedID, j.format), j.ep.ID, err)
		}
	}
	_ = os.RemoveAll(tmp)
	now := s.now().UTC()
	ep := podsync.Episode{
		ID: j.ep.ID, Title: res.Episode.Title, Description: res.Episode.Description, URL: res.Episode.URL,
		Thumbnail: firstNonEmpty(res.Episode.Thumbnail, j.ep.Thumbnail), ThumbFile: thumb,
		Published: res.Episode.Published, DurationSec: res.Episode.DurationSec,
		Channel: firstNonEmpty(res.Episode.Channel, j.ep.Channel), ViewCount: maxInt64(res.Episode.ViewCount, j.ep.ViewCount), Height: res.Episode.Height,
		Status: podsync.StatusDownloaded, File: res.File, Size: res.Size,
		DownloadedAt: &now, Manual: manual, MembersOnly: j.ep.MembersOnly,
	}
	if ep.Published.IsZero() {
		ep.Published = now
	}
	if prev, ok := s.lib.Episode(j.feedID, j.format, j.ep.ID); ok {
		ep.Manual = ep.Manual || prev.Manual
		ep.MembersOnly = ep.MembersOnly || prev.MembersOnly
	}
	if err := s.lib.PutEpisode(j.feedID, j.format, ep); err != nil {
		s.recordFailure(j, f, err)
		return
	}
	s.clearFailure(j.feedID, j.format, j.ep.ID)
}

func (s *Service) recordFailure(j *job, f podsync.Feed, err error) {
	if s.ctx.Err() != nil {
		return // shutting down; do not poison the episode
	}
	prev, _ := s.lib.Episode(j.feedID, j.format, j.ep.ID)
	attempts := prev.Attempts + 1
	if j.manual {
		attempts = 1
	}
	title := firstNonEmpty(prev.Title, j.ep.Title, j.ep.ID)
	membersOnly := errors.Is(err, podsync.ErrMembersOnly)
	_ = s.lib.PutEpisode(j.feedID, j.format, podsync.Episode{
		ID: j.ep.ID, Title: title, URL: firstNonEmpty(prev.URL, j.ep.URL), Thumbnail: firstNonEmpty(prev.Thumbnail, j.ep.Thumbnail),
		Channel: firstNonEmpty(prev.Channel, j.ep.Channel), ViewCount: maxInt64(prev.ViewCount, j.ep.ViewCount),
		Published: firstNonZero(prev.Published, j.ep.Published), DurationSec: maxInt(prev.DurationSec, j.ep.DurationSec),
		Status: podsync.StatusFailed, Attempts: attempts, LastError: err.Error(), Manual: j.manual || prev.Manual,
		MembersOnly: membersOnly,
	})
	if membersOnly {
		// Expected, not a problem to report: the row shows it, and it is not retried.
		s.clearFailure(j.feedID, j.format, j.ep.ID)
		return
	}
	key := refKey(j.feedID, j.format)
	s.mu.Lock()
	if s.failures[key] == nil {
		s.failures[key] = map[string]string{}
	}
	s.failures[key][j.ep.ID] = fmt.Sprintf("%q: %v", title, err)
	n, msg := len(s.failures[key]), s.failures[key][j.ep.ID]
	s.mu.Unlock()
	text := fmt.Sprintf("Download failed in %q — %s", f.ID, msg)
	if n > 1 {
		text = fmt.Sprintf("%d downloads failed in %q; latest: %s", n, f.ID, msg)
	}
	s.st.Report("download:"+key, podsync.LevelError, text)
}

func (s *Service) clearFailure(feedID, format, epID string) {
	key := refKey(feedID, format)
	s.mu.Lock()
	delete(s.failures[key], epID)
	empty := len(s.failures[key]) == 0
	if empty {
		delete(s.failures, key)
	}
	s.mu.Unlock()
	if empty {
		s.st.Clear("download:" + key)
	}
}

// ForgetFeed drops in-memory state for a feed no longer configured under
// this (id, format) identity.
func (s *Service) ForgetFeed(feedID, format string) {
	key := refKey(feedID, format)
	s.mu.Lock()
	delete(s.failures, key)
	s.mu.Unlock()
	s.st.ClearPrefix("download:" + key)
	s.st.ClearPrefix("list:" + key)
	s.st.ClearPrefix("cleanup:" + key)
	s.st.ClearPrefix("library:" + key)
}

// ---- cleanup ----

// Cleanup removes media beyond the newest cleanup_keep_last downloaded episodes. Episodes the
// user downloaded manually are exempt. The sidecar stays (as "cleaned") so
// the scheduler does not download the episode again.
func (s *Service) Cleanup(feedID, format string) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(feedID, format)
	if !ok {
		return
	}
	opts := f.Effective(cfg.Defaults)
	if opts.CleanupKeepLast == 0 {
		return
	}
	var eligible []podsync.Episode
	for _, ep := range s.lib.Episodes(feedID, format) {
		if ep.Status == podsync.StatusDownloaded && !ep.Manual {
			eligible = append(eligible, ep) // already newest first
		}
	}
	doomed := map[string]podsync.Episode{}
	if k := opts.CleanupKeepLast; k > 0 && len(eligible) > k {
		for _, ep := range eligible[k:] {
			doomed[ep.ID] = ep
		}
	}
	ids := make([]string, 0, len(doomed))
	for id := range doomed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	key := "cleanup:" + refKey(feedID, format)
	failed := false
	for _, id := range ids {
		ep := doomed[id]
		if err := s.lib.RemoveMedia(feedID, format, ep); err != nil {
			s.st.Report(key, podsync.LevelWarning, fmt.Sprintf("Cleanup of %q failed: %v", f.ID, err))
			failed = true
			continue
		}
		ep.Status, ep.File, ep.Size = podsync.StatusCleaned, "", 0
		if err := s.lib.PutEpisode(feedID, format, ep); err != nil {
			s.st.Report(key, podsync.LevelWarning, fmt.Sprintf("Cleanup of %q failed: %v", f.ID, err))
			failed = true
		}
	}
	if !failed {
		s.st.Clear(key)
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func firstNonZero(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
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
