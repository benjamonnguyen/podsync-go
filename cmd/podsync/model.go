package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"podsync"
)

// The web UI shares the config file and the data directory with the sync
// service, so most of what it shows and changes is done here directly against
// podsync.ConfigStore and podsync.Library. It asks the sync service (podsync.Syncer)
// only for what needs the running service: applying changes, pulls, live
// status and source lookups.
//
// A feed's identity is the pair (ID, Format), not ID alone: the same ID may
// be configured twice, once per format (e.g. an audio and a video feed off
// one channel), each with its own directory, settings page and download
// history.

// FeedSummary is a feed as shown in lists.
type FeedSummary struct {
	ID           string
	Format       string
	Source       string
	Kind         string
	Name         string
	Description  string
	ImageURL     string
	RSSURL       string
	Episodes     int // downloaded
	Bytes        int64
	LastPull     time.Time
	HasOverrides bool
	Orphan       bool
}

type FeedList struct {
	Feeds   []FeedSummary
	Orphans []FeedSummary
}

// FeedDetail adds the feed's settings to its summary.
type FeedDetail struct {
	FeedSummary
	Overrides podsync.Feed    // the configured feed: name and nil-able overrides
	Effective podsync.Options // overrides applied over Defaults
	Defaults  podsync.Options
}

// FeedSettings is what UpdateFeed replaces. Nil option fields inherit the defaults.
type FeedSettings struct {
	Format          *string
	MaxVideoHeight  *int
	AudioQuality    *string
	MinDuration     *podsync.Duration
	TitleInclude    string // regex, empty = off (per feed only; there are no global patterns)
	TitleExclude    string
	CleanupKeepLast *int
	PageSize        *int
	PlaylistSort    string // "" or "oldest"; only meaningful for playlist feeds
	OPMLExclude     bool
}

// Episode states shown to users: the library status plus live and listing-only states.
const (
	StateAvailable   = "available" // listed by the source, not in the library
	StateQueued      = "queued"
	StateDownloading = "downloading"
	StateDownloaded  = string(podsync.StatusDownloaded)
	StateCleaned     = string(podsync.StatusCleaned)
	StateFailed      = string(podsync.StatusFailed)
)

// EpisodeView is an episode together with its live state.
type EpisodeView struct {
	podsync.Episode
	State string
}

type EpisodeSearchHit struct {
	FeedID     string
	FeedFormat string
	FeedName   string
	EpisodeView
}

type SearchHits struct {
	Feeds     []FeedSummary
	Episodes  []EpisodeSearchHit
	Truncated bool
}

// OlderPage is one Load More window.
type OlderPage struct {
	Episodes   []EpisodeView
	NextOffset int
	More       bool
}

type AddFeedRequest struct {
	Info     podsync.FeedInfo
	Settings FeedSettings
}

// Settings is the configuration as the settings page shows it. Only some of it
// is editable there: the concurrency, update interval, self-update switch and
// the feed defaults. Everything belonging to the deployment (the listen
// address, the public address, the data directory, the yt-dlp cookies file)
// is an environment variable, not shown or edited here.
type Settings struct {
	Download   podsync.Download
	Defaults   podsync.Options
	ConfigPath string
}

type SettingsResult struct {
	Settings
	// ApplyErr is set when the settings were saved but the sync service could
	// not be told (it applies them the next time it is asked to, or restarts).
	ApplyErr error
}

const searchLibraryLimit = 100

var errSyncDown = "Saved, but the sync service could not be reached, so it will use this the next time it is told to apply changes or restarts: "

// ---- feeds ----

func (s *Server) rssURL(id, format string) string {
	return s.baseURL + "/feeds/" + format + "/" + id + ".xml"
}

func (s *Server) summary(cfg podsync.Config, f podsync.Feed) FeedSummary {
	format := f.Effective(cfg.Defaults).Format
	lf, _ := s.lib.Feed(f.ID, format)
	name := lf.Meta.Name
	if name == "" {
		name = f.ID
	}
	return FeedSummary{
		ID: f.ID, Format: format, Source: f.Source, Kind: f.Kind, Name: name,
		Description: lf.Meta.Description, ImageURL: lf.Meta.ImageURL, RSSURL: s.rssURL(f.ID, format),
		Episodes: lf.Downloaded, Bytes: lf.Bytes, LastPull: lf.Meta.LastPull, HasOverrides: f.HasOverrides(),
	}
}

func (s *Server) orphanSummary(lf podsync.LibraryFeed) FeedSummary {
	name := lf.Meta.Name
	if name == "" {
		name = lf.ID
	}
	return FeedSummary{
		ID: lf.ID, Format: lf.Format, Source: lf.Source, Name: name, ImageURL: lf.Meta.ImageURL,
		Episodes: lf.Downloaded, Bytes: lf.Bytes, Orphan: true,
	}
}

// feedIdentKey is the (id, format) identity key used to match configured
// feeds against library directories (e.g. to tell orphans apart).
func feedIdentKey(id, format string) string { return id + "\x00" + format }

func (s *Server) Feeds(ctx context.Context) FeedList {
	cfg := s.cfg.Get()
	list := FeedList{Feeds: []FeedSummary{}, Orphans: []FeedSummary{}}
	known := map[string]bool{}
	for _, f := range cfg.Feeds {
		known[feedIdentKey(f.ID, f.Effective(cfg.Defaults).Format)] = true
		list.Feeds = append(list.Feeds, s.summary(cfg, f))
	}
	for _, lf := range s.lib.Feeds() {
		if !known[feedIdentKey(lf.ID, lf.Format)] {
			list.Orphans = append(list.Orphans, s.orphanSummary(lf))
		}
	}
	return list
}

func (s *Server) feed(id, format string) (podsync.Config, podsync.Feed, error) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(id, format)
	if !ok {
		return cfg, f, podsync.NotFound("no such feed %q", id)
	}
	return cfg, f, nil
}

func (s *Server) detail(cfg podsync.Config, f podsync.Feed) FeedDetail {
	return FeedDetail{FeedSummary: s.summary(cfg, f), Overrides: f, Effective: f.Effective(cfg.Defaults), Defaults: cfg.Defaults}
}

func (s *Server) Feed(ctx context.Context, id, format string) (FeedDetail, error) {
	cfg, f, err := s.feed(id, format)
	if err != nil {
		return FeedDetail{}, err
	}
	return s.detail(cfg, f), nil
}

// ---- episodes ----

type jobStates map[string]string // jobKey(feedID,format,episodeID) -> queued | downloading

func jobKey(feedID, format, epID string) string { return feedID + "/" + format + "/" + epID }

// syncTimeout bounds the status calls made on every page and poll, so a hung
// sync service cannot hang the web UI.
const syncTimeout = 3 * time.Second

// jobs asks the sync service which downloads are in flight. If it cannot be
// reached the banner says so; rows simply show their stored state.
func (s *Server) jobs(ctx context.Context) jobStates {
	out := jobStates{}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	if st, err := s.sync.Status(ctx); err == nil {
		for _, j := range st.Jobs {
			out[jobKey(j.FeedID, j.Format, j.EpisodeID)] = j.State
		}
	}
	return out
}

func episodeFromInfo(ep podsync.EpisodeInfo) podsync.Episode {
	return podsync.Episode{
		ID: ep.ID, Title: ep.Title, Description: ep.Description, URL: ep.URL, Thumbnail: ep.Thumbnail,
		Published: ep.Published, DurationSec: ep.DurationSec, Channel: ep.Channel, ViewCount: ep.ViewCount,
		Height: ep.Height,
	}
}

// view overlays what the library and the download queue know onto hint.
func (s *Server) view(feedID, format string, hint podsync.Episode, jobs jobStates) EpisodeView {
	v := EpisodeView{Episode: hint, State: StateAvailable}
	if ep, ok := s.lib.Episode(feedID, format, hint.ID); ok {
		v.Episode = ep
		if v.Title == "" {
			v.Title = hint.Title
		}
		if v.Thumbnail == "" {
			v.Thumbnail = hint.Thumbnail
		}
		if v.Channel == "" {
			v.Channel = hint.Channel
		}
		if v.ViewCount == 0 {
			v.ViewCount = hint.ViewCount
		}
		if v.DurationSec == 0 {
			v.DurationSec = hint.DurationSec
		}
		v.State = string(ep.Status)
	}
	if js := jobs[jobKey(feedID, format, hint.ID)]; js != "" {
		v.State = js
	}
	if v.Title == "" {
		v.Title = hint.ID
	}
	return v
}

func matchWords(title, q string) bool {
	low := strings.ToLower(title)
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if !strings.Contains(low, w) {
			return false
		}
	}
	return true
}

func (s *Server) Episodes(ctx context.Context, feedID, format, q string) ([]EpisodeView, error) {
	if _, _, err := s.feed(feedID, format); err != nil {
		return nil, err
	}
	jobs := s.jobs(ctx)
	views := []EpisodeView{}
	for _, ep := range s.lib.Episodes(feedID, format) {
		if q != "" && !matchWords(ep.Title, q) {
			continue
		}
		views = append(views, s.view(feedID, format, ep, jobs))
	}
	// Episodes without a date (e.g. failed before metadata was fetched) are
	// almost always recent, so list them first rather than burying them.
	sort.SliceStable(views, func(i, j int) bool { return views[i].Published.IsZero() && !views[j].Published.IsZero() })
	return views, nil
}

// Episode returns one episode's current view; hint supplies listing data for
// episodes the library does not know yet.
func (s *Server) Episode(ctx context.Context, feedID, format string, hint podsync.EpisodeInfo) (EpisodeView, error) {
	if _, _, err := s.feed(feedID, format); err != nil {
		return EpisodeView{}, err
	}
	return s.view(feedID, format, episodeFromInfo(hint), s.jobs(ctx)), nil
}

// Older lists the window [offset, offset+page_size) of a feed's source (asking
// the sync service) and returns the episodes that pass the feed's filters and
// are not already in the library.
func (s *Server) Older(ctx context.Context, feedID, format string, offset int) (OlderPage, error) {
	cfg, f, err := s.feed(feedID, format)
	if err != nil {
		return OlderPage{}, err
	}
	opts := f.Effective(cfg.Defaults)
	filter, err := f.Filter(cfg.Defaults)
	if err != nil {
		return OlderPage{}, err
	}
	eps, err := s.sync.List(ctx, feedID, format, offset, opts.PageSize)
	if err != nil {
		return OlderPage{}, err
	}
	jobs := s.jobs(ctx)
	page := OlderPage{Episodes: []EpisodeView{}, NextOffset: offset + len(eps), More: len(eps) >= opts.PageSize}
	for _, ep := range eps {
		if ep.Unavailable || ep.IsLive || !filter.Allows(ep) {
			continue
		}
		if _, known := s.lib.Episode(feedID, format, ep.ID); known {
			continue
		}
		page.Episodes = append(page.Episodes, s.view(feedID, format, episodeFromInfo(ep), jobs))
	}
	return page, nil
}

func (s *Server) SearchLibrary(ctx context.Context, q string) SearchHits {
	q = strings.TrimSpace(q)
	hits := SearchHits{Feeds: []FeedSummary{}, Episodes: []EpisodeSearchHit{}}
	if q == "" {
		return hits
	}
	list := s.Feeds(ctx)
	names := map[string]string{}
	for _, f := range append(append([]FeedSummary{}, list.Feeds...), list.Orphans...) {
		if matchWords(f.Name, q) {
			hits.Feeds = append(hits.Feeds, f)
		}
	}
	for _, f := range list.Feeds {
		names[feedIdentKey(f.ID, f.Format)] = f.Name
	}
	found := s.lib.Search(q)
	if len(found) > searchLibraryLimit {
		found, hits.Truncated = found[:searchLibraryLimit], true
	}
	jobs := s.jobs(ctx)
	for _, h := range found {
		name, ok := names[feedIdentKey(h.FeedID, h.Format)]
		if !ok {
			name = h.FeedID + " (orphaned)"
		}
		hits.Episodes = append(hits.Episodes, EpisodeSearchHit{
			FeedID: h.FeedID, FeedFormat: h.Format, FeedName: name, EpisodeView: s.view(h.FeedID, h.Format, h.Episode, jobs),
		})
	}
	return hits
}

// ---- mutations ----

// applyBestEffort tells the sync service to re-read the config and library.
// The change itself is already saved, so a failure is reported, not fatal.
func (s *Server) applyBestEffort(ctx context.Context) (podsync.ApplyResult, error) {
	res, err := s.sync.Apply(ctx)
	if err != nil {
		return res, fmt.Errorf("%s%w", errSyncDown, err)
	}
	return res, nil
}

// AddFeed saves the feed (with the full set of per-feed options req.Settings
// carries — the add page offers the same options the feed settings page
// does), creates its folder and cache, tells the sync service and asks it to
// pull the feed right away. An id may already be configured under a
// different format (see config.go's duplicate check): req.Settings.Format
// decides which (id, format) slot this add claims, defaulting to the
// current global default when left unset, same as any other feed.
func (s *Server) AddFeed(ctx context.Context, req AddFeedRequest) (FeedSummary, error) {
	info, upd := req.Info, req.Settings
	if info.Source == "" {
		info.Source = podsync.DefaultSource
	}
	if strings.TrimSpace(info.ID) == "" {
		return FeedSummary{}, podsync.Invalid("feed id is required")
	}
	if strings.TrimSpace(info.URL) == "" {
		return FeedSummary{}, podsync.Invalid("feed url is required")
	}
	var format string
	err := s.cfg.Update(func(c *podsync.Config) error {
		format = c.Defaults.Format
		if upd.Format != nil {
			format = *upd.Format
		}
		if _, dup := c.Feed(info.ID, format); dup {
			return podsync.Invalid("That feed is already in your library.")
		}
		if _, dup := c.FeedByURL(info.URL, format); dup {
			return podsync.Invalid("That feed is already in your library.")
		}
		c.Feeds = append(c.Feeds, podsync.Feed{
			ID: info.ID, URL: info.URL,
			Format: upd.Format, MaxVideoHeight: upd.MaxVideoHeight, AudioQuality: upd.AudioQuality, MinDuration: upd.MinDuration,
			TitleInclude: upd.TitleInclude, TitleExclude: upd.TitleExclude,
			CleanupKeepLast: upd.CleanupKeepLast, PageSize: upd.PageSize,
			PlaylistSort: upd.PlaylistSort, OPMLExclude: upd.OPMLExclude,
		})
		return nil
	})
	if err != nil {
		return FeedSummary{}, err
	}
	if err := s.lib.EnsureFeed(info.Source, info.ID, format); err == nil {
		_ = s.lib.UpdateMeta(info.ID, format, func(m *podsync.FeedMeta) {
			m.Name, m.Description, m.ImageURL = info.Name, info.Description, info.ImageURL
		})
	}
	if _, err := s.applyBestEffort(ctx); err == nil {
		_ = s.sync.Pull(ctx, podsync.PullRequest{Feed: info.ID, Format: format}) // busy is fine: the scheduler picks it up
	}
	cfg, f, err := s.feed(info.ID, format)
	if err != nil {
		return FeedSummary{}, err
	}
	return s.summary(cfg, f), nil
}

// UpdateFeed saves settings for the feed at (id, format). If upd.Format
// changes the feed's effective format, this moves it to a new (id, format)
// identity; the caller is responsible for deciding what happens to the old
// one's library entry (see handleFeedSettingsSave).
func (s *Server) UpdateFeed(ctx context.Context, id, format string, upd FeedSettings) (FeedDetail, error) {
	if _, _, err := s.feed(id, format); err != nil {
		return FeedDetail{}, err
	}
	var newFormat string
	err := s.cfg.Update(func(c *podsync.Config) error {
		for i := range c.Feeds {
			if c.Feeds[i].ID != id || c.Feeds[i].Effective(c.Defaults).Format != format {
				continue
			}
			f := &c.Feeds[i]
			f.Format, f.MaxVideoHeight, f.AudioQuality, f.MinDuration = upd.Format, upd.MaxVideoHeight, upd.AudioQuality, upd.MinDuration
			f.TitleInclude, f.TitleExclude = upd.TitleInclude, upd.TitleExclude
			f.CleanupKeepLast, f.PageSize = upd.CleanupKeepLast, upd.PageSize
			f.PlaylistSort = upd.PlaylistSort
			f.OPMLExclude = upd.OPMLExclude
			newFormat = f.Effective(c.Defaults).Format
			return nil
		}
		return podsync.NotFound("no such feed %q", id)
	})
	if err != nil {
		return FeedDetail{}, err
	}
	_, _ = s.applyBestEffort(ctx)
	return s.Feed(ctx, id, newFormat)
}

func (s *Server) DeleteFeed(ctx context.Context, id, format string, deleteFiles bool) error {
	if _, _, err := s.feed(id, format); err != nil {
		return err
	}
	err := s.cfg.Update(func(c *podsync.Config) error {
		kept := c.Feeds[:0:0]
		for _, x := range c.Feeds {
			if x.ID != id || x.Effective(c.Defaults).Format != format {
				kept = append(kept, x)
			}
		}
		c.Feeds = kept
		return nil
	})
	if err != nil {
		return err
	}
	if deleteFiles {
		if err := s.lib.DeleteFeed(id, format); err != nil {
			return fmt.Errorf("feed removed from config but deleting files failed: %w", err)
		}
	}
	_, _ = s.applyBestEffort(ctx)
	return nil
}

// DeleteMedia removes the downloaded files of the given episodes. Each record
// stays, as "cleaned", so the episode is not downloaded again. It returns the
// IDs it removed; episodes that are not downloaded are skipped.
func (s *Server) DeleteMedia(ctx context.Context, feedID, format string, ids []string) ([]string, error) {
	if _, _, err := s.feed(feedID, format); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, podsync.Invalid("no episodes selected")
	}
	var removed []string
	var errs []error
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		ep, ok := s.lib.Episode(feedID, format, id)
		if !ok || ep.Status != podsync.StatusDownloaded {
			continue
		}
		if err := s.lib.RemoveMedia(feedID, format, ep); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ep.Title, err))
			continue
		}
		ep.Status, ep.File, ep.Size = podsync.StatusCleaned, "", 0
		if err := s.lib.PutEpisode(feedID, format, ep); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ep.Title, err))
			continue
		}
		removed = append(removed, id)
	}
	if len(removed) > 0 {
		_, _ = s.applyBestEffort(ctx)
	}
	return removed, errors.Join(errs...)
}

func (s *Server) DeleteOrphan(ctx context.Context, id, format string) error {
	if _, configured := s.cfg.Get().Feed(id, format); configured {
		return podsync.Invalid("feed is still configured")
	}
	if _, ok := s.lib.Feed(id, format); !ok {
		return podsync.NotFound("no such folder %q", id)
	}
	return s.lib.DeleteFeed(id, format)
}

// ---- settings ----

func (s *Server) settings(c podsync.Config) Settings {
	return Settings{Download: c.Download, Defaults: c.Defaults, ConfigPath: s.cfg.Path()}
}

func (s *Server) Settings(ctx context.Context) Settings { return s.settings(s.cfg.Get()) }

// UpdateSettings saves the editable settings (concurrency, update interval,
// self-update, feed defaults) and tells the sync service to apply them. Anything
// else in the submitted Settings is ignored, so a form post cannot change
// deployment settings, which are not part of this file at all.
func (s *Server) UpdateSettings(ctx context.Context, in Settings) (SettingsResult, error) {
	err := s.cfg.Update(func(c *podsync.Config) error {
		c.Download.Concurrency = in.Download.Concurrency
		c.Download.UpdateInterval = in.Download.UpdateInterval
		c.Download.SelfUpdate = in.Download.SelfUpdate
		c.Defaults = in.Defaults
		return nil
	})
	if err != nil {
		return SettingsResult{}, err
	}
	res := SettingsResult{Settings: s.settings(s.cfg.Get())}
	_, res.ApplyErr = s.applyBestEffort(ctx)
	return res, nil
}

// ---- operations ----

// Pull starts an update of one feed, or of all feeds when feedID is empty.
func (s *Server) Pull(ctx context.Context, feedID, format string) error {
	if feedID != "" {
		if _, _, err := s.feed(feedID, format); err != nil {
			return err
		}
	}
	return s.sync.Pull(ctx, podsync.PullRequest{Feed: feedID, Format: format})
}

// DownloadEpisode asks the sync service to download one episode now.
func (s *Server) DownloadEpisode(ctx context.Context, feedID, format string, ep podsync.EpisodeInfo) error {
	if _, _, err := s.feed(feedID, format); err != nil {
		return err
	}
	return s.sync.Pull(ctx, podsync.PullRequest{Feed: feedID, Format: format, Episode: &ep})
}
