package syncer

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"podsync"
)

var _ podsync.Syncer = (*Service)(nil)

var episodeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{6,20}$`)

const searchLibraryLimit = 100

func (s *Service) rssURL(cfg podsync.Config, id string) string {
	return cfg.Server.BaseURL + "/feeds/" + id + ".xml"
}

func (s *Service) summary(cfg podsync.Config, f podsync.Feed) podsync.FeedSummary {
	lf, _ := s.lib.Feed(f.ID)
	name := f.Name
	if name == "" {
		name = lf.Meta.Name
	}
	return podsync.FeedSummary{
		ID: f.ID, Source: f.Source, Kind: f.Kind, Name: name,
		Description: lf.Meta.Description, ImageURL: lf.Meta.ImageURL, RSSURL: s.rssURL(cfg, f.ID),
		Episodes: lf.Downloaded, Bytes: lf.Bytes, LastPull: lf.Meta.LastPull, HasOverrides: f.HasOverrides(),
	}
}

func (s *Service) orphanSummary(lf podsync.LibraryFeed) podsync.FeedSummary {
	name := lf.Meta.Name
	if name == "" {
		name = lf.ID
	}
	return podsync.FeedSummary{
		ID: lf.ID, Source: lf.Source, Name: name, ImageURL: lf.Meta.ImageURL,
		Episodes: lf.Downloaded, Bytes: lf.Bytes, Orphan: true,
	}
}

func (s *Service) Feeds(ctx context.Context) (podsync.FeedList, error) {
	cfg := s.cfg.Get()
	list := podsync.FeedList{Feeds: []podsync.FeedSummary{}, Orphans: []podsync.FeedSummary{}}
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
	return list, nil
}

func (s *Service) feed(id string) (podsync.Config, podsync.Feed, error) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(id)
	if !ok {
		return cfg, f, podsync.NotFound("no such feed %q", id)
	}
	return cfg, f, nil
}

func (s *Service) detail(cfg podsync.Config, f podsync.Feed) podsync.FeedDetail {
	return podsync.FeedDetail{
		FeedSummary: s.summary(cfg, f), Overrides: f,
		Effective: f.Effective(cfg.Defaults), Defaults: cfg.Defaults,
	}
}

func (s *Service) Feed(ctx context.Context, id string) (podsync.FeedDetail, error) {
	cfg, f, err := s.feed(id)
	if err != nil {
		return podsync.FeedDetail{}, err
	}
	return s.detail(cfg, f), nil
}

// ---- episodes ----

func episodeFromInfo(ep podsync.EpisodeInfo) podsync.Episode {
	return podsync.Episode{
		ID: ep.ID, Title: ep.Title, Description: ep.Description, URL: ep.URL, Thumbnail: ep.Thumbnail,
		Published: ep.Published, DurationSec: ep.DurationSec, Channel: ep.Channel, ViewCount: ep.ViewCount,
		Height: ep.Height, IsShort: ep.IsShort,
	}
}

// sanitize limits what a client can inject into the library through listing
// hints: only an https thumbnail URL, bounded text.
func sanitize(ep podsync.EpisodeInfo) podsync.EpisodeInfo {
	if !strings.HasPrefix(ep.Thumbnail, "https://") || len(ep.Thumbnail) > 2000 {
		ep.Thumbnail = ""
	}
	ep.Title = clip(strings.TrimSpace(ep.Title), 500)
	ep.Channel = clip(strings.TrimSpace(ep.Channel), 200)
	ep.Description, ep.URL = "", ""
	return ep
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// view overlays what the library and the download queue know onto hint.
func (s *Service) view(feedID string, hint podsync.Episode) podsync.EpisodeView {
	v := podsync.EpisodeView{Episode: hint, State: podsync.StateAvailable}
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
	if js := s.JobState(feedID, hint.ID); js != "" {
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

func (s *Service) Episodes(ctx context.Context, feedID, q string) ([]podsync.EpisodeView, error) {
	if _, _, err := s.feed(feedID); err != nil {
		return nil, err
	}
	views := []podsync.EpisodeView{}
	for _, ep := range s.lib.Episodes(feedID) {
		if q != "" && !matchWords(ep.Title, q) {
			continue
		}
		views = append(views, s.view(feedID, ep))
	}
	// Episodes without a date (e.g. failed before metadata was fetched) are
	// almost always recent, so list them first rather than burying them.
	sort.SliceStable(views, func(i, j int) bool { return views[i].Published.IsZero() && !views[j].Published.IsZero() })
	return views, nil
}

func (s *Service) Episode(ctx context.Context, feedID string, hint podsync.EpisodeInfo) (podsync.EpisodeView, error) {
	if _, _, err := s.feed(feedID); err != nil {
		return podsync.EpisodeView{}, err
	}
	if !episodeIDRe.MatchString(hint.ID) {
		return podsync.EpisodeView{}, podsync.Invalid("invalid episode id")
	}
	return s.view(feedID, episodeFromInfo(sanitize(hint))), nil
}

// Older lists the window [offset, offset+page_size) of a feed's source and
// returns the episodes that pass the feed's filters and are not already in
// the library (those are shown in the feed's main list).
func (s *Service) Older(ctx context.Context, feedID string, offset int) (podsync.OlderPage, error) {
	cfg, f, err := s.feed(feedID)
	if err != nil {
		return podsync.OlderPage{}, err
	}
	if offset < 0 || offset > 100000 {
		return podsync.OlderPage{}, podsync.Invalid("offset out of range")
	}
	src, err := s.reg.Get(f.Source)
	if err != nil {
		return podsync.OlderPage{}, err
	}
	opts := f.Effective(cfg.Defaults)
	filter, err := podsync.NewEpisodeFilter(opts)
	if err != nil {
		return podsync.OlderPage{}, err
	}
	info := podsync.FeedInfo{Source: f.Source, Kind: f.Kind, ID: f.ID, Name: f.Name}
	eps, err := src.ListEpisodes(ctx, info, offset, opts.PageSize)
	if err != nil {
		if ctx.Err() == nil {
			s.st.Report("list:"+feedID, podsync.LevelError, fmt.Sprintf("Could not list episodes for %q: %v", f.Name, err))
		}
		return podsync.OlderPage{}, err
	}
	s.st.Clear("list:" + feedID)
	page := podsync.OlderPage{Episodes: []podsync.EpisodeView{}, NextOffset: offset + len(eps), More: len(eps) >= opts.PageSize}
	for _, ep := range eps {
		if ep.Unavailable || ep.IsLive || !filter.Allows(ep) {
			continue
		}
		if _, known := s.lib.Episode(feedID, ep.ID); known {
			continue
		}
		page.Episodes = append(page.Episodes, s.view(feedID, episodeFromInfo(ep)))
	}
	return page, nil
}

func (s *Service) SearchLibrary(ctx context.Context, q string) (podsync.SearchHits, error) {
	q = strings.TrimSpace(q)
	hits := podsync.SearchHits{Feeds: []podsync.FeedSummary{}, Episodes: []podsync.EpisodeSearchHit{}}
	if q == "" {
		return hits, nil
	}
	list, _ := s.Feeds(ctx)
	names := map[string]string{}
	for _, f := range append(append([]podsync.FeedSummary{}, list.Feeds...), list.Orphans...) {
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
	for _, h := range found {
		name, ok := names[h.FeedID]
		if !ok {
			name = h.FeedID + " (orphaned)"
		}
		hits.Episodes = append(hits.Episodes, podsync.EpisodeSearchHit{FeedID: h.FeedID, FeedName: name, EpisodeView: s.view(h.FeedID, h.Episode)})
	}
	return hits, nil
}

// ---- discovery ----

func (s *Service) defaultSource() (podsync.Source, error) { return s.reg.Get(podsync.DefaultSource) }

func (s *Service) Resolve(ctx context.Context, ref string) (podsync.FeedInfo, error) {
	src, err := s.defaultSource()
	if err != nil {
		return podsync.FeedInfo{}, err
	}
	return src.Resolve(ctx, strings.TrimSpace(ref))
}

func (s *Service) SearchSource(ctx context.Context, query string, kind podsync.SearchKind) ([]podsync.SearchResult, error) {
	src, err := s.defaultSource()
	if err != nil {
		return nil, err
	}
	results, err := src.Search(ctx, query, kind)
	if err != nil {
		if ctx.Err() == nil {
			s.st.Report("search", podsync.LevelWarning, "Channel search failed: "+err.Error())
		}
		return nil, err
	}
	s.st.Clear("search")
	if results == nil {
		results = []podsync.SearchResult{}
	}
	return results, nil
}

// ---- mutations ----

func (s *Service) AddFeed(ctx context.Context, req podsync.AddFeedRequest) (podsync.FeedSummary, error) {
	info := req.Info
	if info.Source == "" {
		info.Source = podsync.DefaultSource
	}
	if info.Kind == "" {
		info.Kind = podsync.FeedChannel
	}
	if strings.TrimSpace(info.ID) == "" {
		return podsync.FeedSummary{}, podsync.Invalid("feed id is required")
	}
	err := s.cfg.Update(func(c *podsync.Config) error {
		if _, dup := c.Feed(info.ID); dup {
			return podsync.Invalid("That feed is already in your library.")
		}
		c.Feeds = append(c.Feeds, podsync.Feed{ID: info.ID, Source: info.Source, Kind: info.Kind, Name: info.Name, PageSize: req.PageSize})
		return nil
	})
	if err != nil {
		return podsync.FeedSummary{}, err
	}
	s.Reconcile()
	_ = s.lib.UpdateMeta(info.ID, func(m *podsync.FeedMeta) {
		m.Name, m.Description, m.ImageURL = info.Name, info.Description, info.ImageURL
	})
	_ = s.TriggerFeed(info.ID) // busy is fine: the scheduler will pick it up
	cfg, f, err := s.feed(info.ID)
	if err != nil {
		return podsync.FeedSummary{}, err
	}
	return s.summary(cfg, f), nil
}

func (s *Service) UpdateFeed(ctx context.Context, id string, upd podsync.FeedSettings) (podsync.FeedDetail, error) {
	if _, _, err := s.feed(id); err != nil {
		return podsync.FeedDetail{}, err
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
		return podsync.FeedDetail{}, err
	}
	return s.Feed(ctx, id)
}

func (s *Service) DeleteFeed(ctx context.Context, id string, deleteFiles bool) error {
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
	s.ForgetFeed(id)
	if deleteFiles {
		if err := s.lib.DeleteFeed(id); err != nil {
			return fmt.Errorf("feed removed from config but deleting files failed: %w", err)
		}
	}
	return nil
}

func (s *Service) DeleteOrphan(ctx context.Context, id string) error {
	if _, configured := s.cfg.Get().Feed(id); configured {
		return podsync.Invalid("feed is still configured")
	}
	if _, ok := s.lib.Feed(id); !ok {
		return podsync.NotFound("no such folder %q", id)
	}
	return s.lib.DeleteFeed(id)
}

// ---- settings ----

func (s *Service) settings(c podsync.Config) podsync.Settings {
	return podsync.Settings{Server: c.Server, Download: c.Download, Defaults: c.Defaults, ConfigPath: s.cfg.Path()}
}

func (s *Service) Settings(ctx context.Context) (podsync.Settings, error) {
	return s.settings(s.cfg.Get()), nil
}

func (s *Service) UpdateSettings(ctx context.Context, in podsync.Settings) (podsync.SettingsResult, error) {
	before := s.cfg.Get()
	err := s.cfg.Update(func(c *podsync.Config) error {
		c.Server, c.Download, c.Defaults = in.Server, in.Download, in.Defaults
		return nil
	})
	if err != nil {
		return podsync.SettingsResult{}, err
	}
	after := s.cfg.Get()
	return podsync.SettingsResult{
		Settings:        s.settings(after),
		RestartRequired: before.Server.ListenAddr != after.Server.ListenAddr || before.Server.DataDir != after.Server.DataDir,
	}, nil
}

// ---- operations ----

func (s *Service) Pull(ctx context.Context, feedID string) error {
	if feedID == "" {
		return s.TriggerAll()
	}
	return s.TriggerFeed(feedID)
}

func (s *Service) Download(ctx context.Context, feedID string, ep podsync.EpisodeInfo) error {
	if !episodeIDRe.MatchString(ep.ID) {
		return podsync.Invalid("invalid episode id")
	}
	return s.RequestDownload(feedID, sanitize(ep))
}

func (s *Service) Status(ctx context.Context) (podsync.Status, error) {
	return podsync.Status{
		Activity: s.Activity(), Issues: append([]podsync.Issue{}, s.st.Active()...),
		Version: s.lib.Version(), Versions: s.Versions(),
	}, nil
}

func (s *Service) DismissIssue(ctx context.Context, key string) error {
	s.st.Dismiss(key)
	return nil
}
