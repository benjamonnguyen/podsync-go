package webui

import (
	"context"
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

// FeedSummary is a feed as shown in lists.
type FeedSummary struct {
	ID           string
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
	Name             string
	Format           *string
	MaxResolution    *int
	ExcludeShorts    *bool
	TitleInclude     *string
	TitleExclude     *string
	CleanupAfterDays *int
	CleanupKeepLast  *int
	PageSize         *int
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
	FeedID   string
	FeedName string
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
	PageSize *int
}

// Settings is the part of the configuration the settings page edits. The
// listen address and data directory are shown but not editable: both programs
// have to agree on them, so they are changed in config.toml with a restart.
type Settings struct {
	Server     podsync.Server
	Download   podsync.Download
	Defaults   podsync.Options
	ConfigPath string
}

type SettingsResult struct {
	Settings
	RestartRequired bool
	// ApplyErr is set when the settings were saved but the sync service could
	// not be told (it applies them the next time it is asked to, or restarts).
	ApplyErr error
}

const searchLibraryLimit = 100

var errSyncDown = "Saved, but the sync service could not be reached, so it will use this the next time it is told to apply changes or restarts: "

// ---- feeds ----

func (s *Server) rssURL(cfg podsync.Config, id string) string {
	return cfg.Server.BaseURL + "/feeds/" + id + ".xml"
}

func (s *Server) summary(cfg podsync.Config, f podsync.Feed) FeedSummary {
	lf, _ := s.lib.Feed(f.ID)
	name := f.Name
	if name == "" {
		name = lf.Meta.Name
	}
	return FeedSummary{
		ID: f.ID, Source: f.Source, Kind: f.Kind, Name: name,
		Description: lf.Meta.Description, ImageURL: lf.Meta.ImageURL, RSSURL: s.rssURL(cfg, f.ID),
		Episodes: lf.Downloaded, Bytes: lf.Bytes, LastPull: lf.Meta.LastPull, HasOverrides: f.HasOverrides(),
	}
}

func (s *Server) orphanSummary(lf podsync.LibraryFeed) FeedSummary {
	name := lf.Meta.Name
	if name == "" {
		name = lf.ID
	}
	return FeedSummary{ID: lf.ID, Source: lf.Source, Name: name, ImageURL: lf.Meta.ImageURL, Episodes: lf.Downloaded, Bytes: lf.Bytes, Orphan: true}
}

func (s *Server) Feeds(ctx context.Context) FeedList {
	cfg := s.cfg.Get()
	list := FeedList{Feeds: []FeedSummary{}, Orphans: []FeedSummary{}}
	known := map[string]bool{}
	for _, f := range cfg.Feeds {
		known[f.ID] = true
		list.Feeds = append(list.Feeds, s.summary(cfg, f))
	}
	for _, lf := range s.lib.Feeds() {
		if !known[lf.ID] {
			list.Orphans = append(list.Orphans, s.orphanSummary(lf))
		}
	}
	return list
}

func (s *Server) feed(id string) (podsync.Config, podsync.Feed, error) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(id)
	if !ok {
		return cfg, f, podsync.NotFound("no such feed %q", id)
	}
	return cfg, f, nil
}

func (s *Server) detail(cfg podsync.Config, f podsync.Feed) FeedDetail {
	return FeedDetail{FeedSummary: s.summary(cfg, f), Overrides: f, Effective: f.Effective(cfg.Defaults), Defaults: cfg.Defaults}
}

func (s *Server) Feed(ctx context.Context, id string) (FeedDetail, error) {
	cfg, f, err := s.feed(id)
	if err != nil {
		return FeedDetail{}, err
	}
	return s.detail(cfg, f), nil
}

// ---- episodes ----

type jobStates map[string]string // "feedID/episodeID" -> queued | downloading

func jobKey(feedID, epID string) string { return feedID + "/" + epID }

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
			out[jobKey(j.FeedID, j.EpisodeID)] = j.State
		}
	}
	return out
}

func episodeFromInfo(ep podsync.EpisodeInfo) podsync.Episode {
	return podsync.Episode{
		ID: ep.ID, Title: ep.Title, Description: ep.Description, URL: ep.URL, Thumbnail: ep.Thumbnail,
		Published: ep.Published, DurationSec: ep.DurationSec, Channel: ep.Channel, ViewCount: ep.ViewCount,
		Height: ep.Height, IsShort: ep.IsShort,
	}
}

// view overlays what the library and the download queue know onto hint.
func (s *Server) view(feedID string, hint podsync.Episode, jobs jobStates) EpisodeView {
	v := EpisodeView{Episode: hint, State: StateAvailable}
	if ep, ok := s.lib.Episode(feedID, hint.ID); ok {
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
	if js := jobs[jobKey(feedID, hint.ID)]; js != "" {
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

func (s *Server) Episodes(ctx context.Context, feedID, q string) ([]EpisodeView, error) {
	if _, _, err := s.feed(feedID); err != nil {
		return nil, err
	}
	jobs := s.jobs(ctx)
	views := []EpisodeView{}
	for _, ep := range s.lib.Episodes(feedID) {
		if q != "" && !matchWords(ep.Title, q) {
			continue
		}
		views = append(views, s.view(feedID, ep, jobs))
	}
	// Episodes without a date (e.g. failed before metadata was fetched) are
	// almost always recent, so list them first rather than burying them.
	sort.SliceStable(views, func(i, j int) bool { return views[i].Published.IsZero() && !views[j].Published.IsZero() })
	return views, nil
}

// Episode returns one episode's current view; hint supplies listing data for
// episodes the library does not know yet.
func (s *Server) Episode(ctx context.Context, feedID string, hint podsync.EpisodeInfo) (EpisodeView, error) {
	if _, _, err := s.feed(feedID); err != nil {
		return EpisodeView{}, err
	}
	return s.view(feedID, episodeFromInfo(hint), s.jobs(ctx)), nil
}

// Older lists the window [offset, offset+page_size) of a feed's source (asking
// the sync service) and returns the episodes that pass the feed's filters and
// are not already in the library.
func (s *Server) Older(ctx context.Context, feedID string, offset int) (OlderPage, error) {
	cfg, f, err := s.feed(feedID)
	if err != nil {
		return OlderPage{}, err
	}
	opts := f.Effective(cfg.Defaults)
	filter, err := podsync.NewEpisodeFilter(opts)
	if err != nil {
		return OlderPage{}, err
	}
	eps, err := s.sync.List(ctx, feedID, offset, opts.PageSize)
	if err != nil {
		return OlderPage{}, err
	}
	jobs := s.jobs(ctx)
	page := OlderPage{Episodes: []EpisodeView{}, NextOffset: offset + len(eps), More: len(eps) >= opts.PageSize}
	for _, ep := range eps {
		if ep.Unavailable || ep.IsLive || !filter.Allows(ep) {
			continue
		}
		if _, known := s.lib.Episode(feedID, ep.ID); known {
			continue
		}
		page.Episodes = append(page.Episodes, s.view(feedID, episodeFromInfo(ep), jobs))
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
		names[f.ID] = f.Name
	}
	found := s.lib.Search(q)
	if len(found) > searchLibraryLimit {
		found, hits.Truncated = found[:searchLibraryLimit], true
	}
	jobs := s.jobs(ctx)
	for _, h := range found {
		name, ok := names[h.FeedID]
		if !ok {
			name = h.FeedID + " (orphaned)"
		}
		hits.Episodes = append(hits.Episodes, EpisodeSearchHit{FeedID: h.FeedID, FeedName: name, EpisodeView: s.view(h.FeedID, h.Episode, jobs)})
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

// AddFeed saves the feed, creates its folder and cache, tells the sync service
// and asks it to pull the feed right away.
func (s *Server) AddFeed(ctx context.Context, req AddFeedRequest) (FeedSummary, error) {
	info := req.Info
	if info.Source == "" {
		info.Source = podsync.DefaultSource
	}
	if info.Kind == "" {
		info.Kind = podsync.FeedChannel
	}
	if strings.TrimSpace(info.ID) == "" {
		return FeedSummary{}, podsync.Invalid("feed id is required")
	}
	err := s.cfg.Update(func(c *podsync.Config) error {
		if _, dup := c.Feed(info.ID); dup {
			return podsync.Invalid("That feed is already in your library.")
		}
		c.Feeds = append(c.Feeds, podsync.Feed{ID: info.ID, Source: info.Source, Kind: info.Kind, Name: info.Name, PageSize: req.PageSize})
		return nil
	})
	if err != nil {
		return FeedSummary{}, err
	}
	if err := s.lib.EnsureFeed(info.Source, info.ID); err == nil {
		_ = s.lib.UpdateMeta(info.ID, func(m *podsync.FeedMeta) {
			m.Name, m.Description, m.ImageURL = info.Name, info.Description, info.ImageURL
		})
	}
	if _, err := s.applyBestEffort(ctx); err == nil {
		_ = s.sync.Pull(ctx, podsync.PullRequest{Feed: info.ID}) // busy is fine: the scheduler picks it up
	}
	cfg, f, err := s.feed(info.ID)
	if err != nil {
		return FeedSummary{}, err
	}
	return s.summary(cfg, f), nil
}

func (s *Server) UpdateFeed(ctx context.Context, id string, upd FeedSettings) (FeedDetail, error) {
	if _, _, err := s.feed(id); err != nil {
		return FeedDetail{}, err
	}
	err := s.cfg.Update(func(c *podsync.Config) error {
		for i := range c.Feeds {
			if c.Feeds[i].ID != id {
				continue
			}
			f := &c.Feeds[i]
			if n := strings.TrimSpace(upd.Name); n != "" {
				f.Name = n
			}
			f.Format, f.MaxResolution, f.ExcludeShorts = upd.Format, upd.MaxResolution, upd.ExcludeShorts
			f.TitleInclude, f.TitleExclude = upd.TitleInclude, upd.TitleExclude
			f.CleanupAfterDays, f.CleanupKeepLast, f.PageSize = upd.CleanupAfterDays, upd.CleanupKeepLast, upd.PageSize
			return nil
		}
		return podsync.NotFound("no such feed %q", id)
	})
	if err != nil {
		return FeedDetail{}, err
	}
	_, _ = s.applyBestEffort(ctx)
	return s.Feed(ctx, id)
}

func (s *Server) DeleteFeed(ctx context.Context, id string, deleteFiles bool) error {
	if _, _, err := s.feed(id); err != nil {
		return err
	}
	err := s.cfg.Update(func(c *podsync.Config) error {
		kept := c.Feeds[:0:0]
		for _, x := range c.Feeds {
			if x.ID != id {
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
		if err := s.lib.DeleteFeed(id); err != nil {
			return fmt.Errorf("feed removed from config but deleting files failed: %w", err)
		}
	}
	_, _ = s.applyBestEffort(ctx)
	return nil
}

func (s *Server) DeleteOrphan(ctx context.Context, id string) error {
	if _, configured := s.cfg.Get().Feed(id); configured {
		return podsync.Invalid("feed is still configured")
	}
	if _, ok := s.lib.Feed(id); !ok {
		return podsync.NotFound("no such folder %q", id)
	}
	return s.lib.DeleteFeed(id)
}

// ---- settings ----

func (s *Server) settings(c podsync.Config) Settings {
	return Settings{Server: c.Server, Download: c.Download, Defaults: c.Defaults, ConfigPath: s.cfg.Path()}
}

func (s *Server) Settings(ctx context.Context) Settings { return s.settings(s.cfg.Get()) }

// UpdateSettings saves the editable settings (base URL, download options,
// defaults) and tells the sync service to apply them.
func (s *Server) UpdateSettings(ctx context.Context, in Settings) (SettingsResult, error) {
	err := s.cfg.Update(func(c *podsync.Config) error {
		c.Server.BaseURL, c.Download, c.Defaults = in.Server.BaseURL, in.Download, in.Defaults
		return nil
	})
	if err != nil {
		return SettingsResult{}, err
	}
	res := SettingsResult{Settings: s.settings(s.cfg.Get())}
	applied, err := s.applyBestEffort(ctx)
	res.ApplyErr, res.RestartRequired = err, applied.RestartRequired
	return res, nil
}

// ---- operations ----

// Pull starts an update of one feed, or of all feeds when feedID is empty.
func (s *Server) Pull(ctx context.Context, feedID string) error {
	if feedID != "" {
		if _, _, err := s.feed(feedID); err != nil {
			return err
		}
	}
	return s.sync.Pull(ctx, podsync.PullRequest{Feed: feedID})
}

// DownloadEpisode asks the sync service to download one episode now.
func (s *Server) DownloadEpisode(ctx context.Context, feedID string, ep podsync.EpisodeInfo) error {
	if _, _, err := s.feed(feedID); err != nil {
		return err
	}
	return s.sync.Pull(ctx, podsync.PullRequest{Feed: feedID, Episode: &ep})
}
