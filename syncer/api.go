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

const (
	maxListOffset = 100000
	maxListLimit  = 500
)

// Apply re-reads the configuration file and rescans the library, because the
// web UI (which writes both) changed them. Config changes take effect through
// the OnChange hook (ConfigChanged); the library is reconciled either way.
func (s *Service) Apply(ctx context.Context) (podsync.ApplyResult, error) {
	changed, err := s.cfg.Reload()
	if err != nil {
		s.st.Report("config", podsync.LevelError, "The configuration file could not be applied (the previous settings stay in effect): "+err.Error())
		return podsync.ApplyResult{}, podsync.InvalidErr(err)
	}
	s.st.Clear("config")
	if _, err := s.lib.Refresh(); err != nil {
		return podsync.ApplyResult{}, err
	}
	s.Reconcile()
	cur := s.cfg.Get().Server
	return podsync.ApplyResult{
		Changed:         changed,
		RestartRequired: cur.ListenAddr != s.started.ListenAddr || cur.DataDir != s.started.DataDir,
	}, nil
}

// Pull queues an update (one feed or all) or a manual episode download.
func (s *Service) Pull(ctx context.Context, req podsync.PullRequest) error {
	if req.Episode != nil {
		if req.Feed == "" {
			return podsync.Invalid("feed is required to download an episode")
		}
		if !episodeIDRe.MatchString(req.Episode.ID) {
			return podsync.Invalid("invalid episode id")
		}
		return s.RequestDownload(req.Feed, sanitize(*req.Episode))
	}
	if req.Feed == "" {
		return s.TriggerAll()
	}
	return s.TriggerFeed(req.Feed)
}

// sanitize limits what a client can inject into the library through a
// download request: only an https thumbnail URL and bounded text. The real
// metadata is fetched from the source during the download.
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

func (s *Service) Status(ctx context.Context) (podsync.SyncStatus, error) {
	s.mu.Lock()
	jobs := make([]podsync.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, podsync.Job{FeedID: j.feedID, EpisodeID: j.ep.ID, State: j.state})
	}
	s.mu.Unlock()
	sort.Slice(jobs, func(i, k int) bool {
		if jobs[i].FeedID != jobs[k].FeedID {
			return jobs[i].FeedID < jobs[k].FeedID
		}
		return jobs[i].EpisodeID < jobs[k].EpisodeID
	})
	return podsync.SyncStatus{
		Activity: s.Activity(), Issues: append([]podsync.Issue{}, s.st.Active()...), Jobs: jobs, Versions: s.Versions(),
	}, nil
}

func (s *Service) defaultSource() (podsync.Source, error) { return s.reg.Get(podsync.DefaultSource) }

func (s *Service) Resolve(ctx context.Context, ref string) (podsync.FeedInfo, error) {
	src, err := s.defaultSource()
	if err != nil {
		return podsync.FeedInfo{}, err
	}
	return src.Resolve(ctx, strings.TrimSpace(ref))
}

func (s *Service) Search(ctx context.Context, query string, kind podsync.SearchKind) ([]podsync.SearchResult, error) {
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

func (s *Service) List(ctx context.Context, feedID string, offset, limit int) ([]podsync.EpisodeInfo, error) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(feedID)
	if !ok {
		return nil, podsync.NotFound("no such feed %q", feedID)
	}
	if offset < 0 || offset > maxListOffset || limit < 1 || limit > maxListLimit {
		return nil, podsync.Invalid("offset or limit out of range")
	}
	src, err := s.reg.Get(f.Source)
	if err != nil {
		return nil, err
	}
	info := podsync.FeedInfo{Source: f.Source, Kind: f.Kind, ID: f.ID, Name: f.Name}
	eps, err := src.ListEpisodes(ctx, info, offset, limit)
	if err != nil {
		if ctx.Err() == nil {
			s.st.Report("list:"+feedID, podsync.LevelError, fmt.Sprintf("Could not list episodes for %q: %v", f.Name, err))
		}
		return nil, err
	}
	s.st.Clear("list:" + feedID)
	if eps == nil {
		eps = []podsync.EpisodeInfo{}
	}
	return eps, nil
}
