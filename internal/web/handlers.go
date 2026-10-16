package web

import (
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"podsync-go/internal/config"
	"podsync-go/internal/puller"
	"podsync-go/internal/source"
)

var episodeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{6,20}$`)

type feedCard struct {
	ID, Name, Kind, ImageURL, Description, RSSURL string
	Episodes                                      int
	Bytes                                         int64
	LastPull                                      time.Time
	Orphan                                        bool
	HasOverrides                                  bool
}

type rowView struct {
	FeedID, FeedName string
	ShowFeed         bool
	ID, Title        string
	Published        time.Time
	DurationSec      int
	Size             int64
	File             string
	State            string // downloaded | cleaned | failed | available | queued | downloading
	Error            string
	Manual           bool
}

func (s *Server) rssURL(cfg config.Config, id string) string {
	return cfg.Server.BaseURL + "/feeds/" + id + ".xml"
}

func (s *Server) card(cfg config.Config, f config.Feed) feedCard {
	sum, _ := s.lib.Feed(f.ID)
	name := f.Name
	if name == "" {
		name = sum.Meta.Name
	}
	return feedCard{
		ID: f.ID, Name: name, Kind: f.Kind, ImageURL: sum.Meta.ImageURL, Description: sum.Meta.Description,
		RSSURL: s.rssURL(cfg, f.ID), Episodes: sum.Downloaded, Bytes: sum.Bytes, LastPull: sum.Meta.LastPull,
		HasOverrides: f.HasOverrides(),
	}
}

func (s *Server) cards() (cards, orphans []feedCard) {
	cfg := s.cfg.Get()
	known := map[string]bool{}
	for _, f := range cfg.Feeds {
		known[f.ID] = true
		cards = append(cards, s.card(cfg, f))
	}
	for _, sum := range s.lib.Feeds() {
		if known[sum.ID] {
			continue
		}
		name := sum.Meta.Name
		if name == "" {
			name = sum.ID
		}
		orphans = append(orphans, feedCard{ID: sum.ID, Name: name, ImageURL: sum.Meta.ImageURL, Episodes: sum.Downloaded, Bytes: sum.Bytes, Orphan: true})
	}
	return cards, orphans
}

func (s *Server) rowFor(feedID, feedName, epID, title string, dur int) rowView {
	r := rowView{FeedID: feedID, FeedName: feedName, ID: epID, Title: title, DurationSec: dur, State: "available"}
	if ep, ok := s.lib.Episode(feedID, epID); ok {
		r.Title, r.Published, r.Size, r.File, r.Manual = ep.Title, ep.Published, ep.Size, ep.File, ep.Manual
		if ep.DurationSec > 0 {
			r.DurationSec = ep.DurationSec
		}
		r.State, r.Error = string(ep.Status), ep.LastError
	}
	if js := s.svc.JobState(feedID, epID); js != "" {
		r.State = js
	}
	if r.Title == "" {
		r.Title = epID
	}
	return r
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

func (s *Server) episodeRows(feedID, feedName, q string) []rowView {
	var rows []rowView
	for _, ep := range s.lib.Episodes(feedID) {
		if q != "" && !matchWords(ep.Title, q) {
			continue
		}
		rows = append(rows, s.rowFor(feedID, feedName, ep.ID, ep.Title, ep.DurationSec))
	}
	// Episodes without a date (e.g. failed before metadata was fetched) are
	// almost always recent, so show them first rather than burying them.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Published.IsZero() && !rows[j].Published.IsZero() })
	return rows
}

// ---- library ----

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	cards, orphans := s.cards()
	s.render(w, "library", map[string]any{"Title": "Library", "Cards": cards, "Orphans": orphans})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cards, orphans := s.cards()
	if q == "" {
		s.fragment(w, "library_body", map[string]any{"Cards": cards, "Orphans": orphans})
		return
	}
	var matched []feedCard
	for _, c := range append(append([]feedCard{}, cards...), orphans...) {
		if matchWords(c.Name, q) {
			matched = append(matched, c)
		}
	}
	names := map[string]string{}
	for _, c := range cards {
		names[c.ID] = c.Name
	}
	hits := s.lib.Search(q)
	truncated := false
	if len(hits) > 100 {
		hits, truncated = hits[:100], true
	}
	var rows []rowView
	for _, h := range hits {
		name, ok := names[h.FeedID]
		if !ok {
			name = h.FeedID + " (orphaned)"
		}
		row := s.rowFor(h.FeedID, name, h.Episode.ID, h.Episode.Title, h.Episode.DurationSec)
		row.ShowFeed = true
		rows = append(rows, row)
	}
	s.fragment(w, "search_results", map[string]any{"Q": q, "Feeds": matched, "Rows": rows, "Truncated": truncated})
}

// ---- feed detail ----

func (s *Server) feedOr404(w http.ResponseWriter, r *http.Request) (config.Config, config.Feed, bool) {
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
	}
	return cfg, f, ok
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	cfg, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	card := s.card(cfg, f)
	eff := f.Effective(cfg.Defaults)
	s.render(w, "feed", map[string]any{
		"Title": card.Name, "Card": card, "Rows": s.episodeRows(f.ID, card.Name, ""),
		"PageSize": eff.PageSize, "Offset": eff.PageSize, "Format": eff.Format, "FeedID": f.ID, "Q": "",
	})
}

func (s *Server) handleEpisodes(w http.ResponseWriter, r *http.Request) {
	_, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	s.fragment(w, "episode_list", map[string]any{"FeedID": f.ID, "Rows": s.episodeRows(f.ID, f.Name, q), "Q": q})
}

func (s *Server) handleOlder(w http.ResponseWriter, r *http.Request) {
	_, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if err != nil || offset < 0 || offset > 100000 {
		offset = 0
	}
	page, err := s.svc.Older(r.Context(), f.ID, offset)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.fragment(w, "older", map[string]any{"FeedID": f.ID, "Err": err.Error(), "More": true, "NextOffset": offset})
		return
	}
	var rows []rowView
	for _, ep := range page.Episodes {
		row := s.rowFor(f.ID, f.Name, ep.ID, ep.Title, ep.DurationSec)
		row.Published = ep.Published
		rows = append(rows, row)
	}
	s.fragment(w, "older", map[string]any{"FeedID": f.ID, "Rows": rows, "More": page.More, "NextOffset": page.NextOffset, "Empty": len(rows) == 0 && !page.More})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	_, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	epID := r.PathValue("ep")
	if !episodeIDRe.MatchString(epID) {
		http.Error(w, "invalid episode id", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	dur, _ := strconv.Atoi(r.FormValue("duration"))
	if err := s.svc.RequestDownload(f.ID, source.EpisodeInfo{ID: epID, Title: title, DurationSec: dur}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.fragment(w, "episode_row", s.rowFor(f.ID, f.Name, epID, title, dur))
}

func (s *Server) handleRow(w http.ResponseWriter, r *http.Request) {
	_, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	dur, _ := strconv.Atoi(r.URL.Query().Get("duration"))
	s.fragment(w, "episode_row", s.rowFor(f.ID, f.Name, r.PathValue("ep"), r.URL.Query().Get("title"), dur))
}

// ---- pulling & status ----

type issueView struct {
	Key, Level, Message string
	Count               int
	Since               time.Time
}

type statusView struct {
	Pulling     bool
	Downloading int
	Queued      int
	Busy        bool
	Issues      []issueView
	Notice      string
}

func (s *Server) statusView(notice string) statusView {
	act := s.st.Activity()
	v := statusView{Pulling: act.Pulling, Downloading: act.Downloading, Queued: act.Queued, Notice: notice}
	v.Busy = act.Pulling || act.Downloading > 0 || act.Queued > 0
	for _, is := range s.st.Active() {
		v.Issues = append(v.Issues, issueView{Key: is.Key, Level: string(is.Level), Message: is.Message, Count: is.Count, Since: is.Since})
	}
	return v
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	v := s.statusView("")
	s.mu.Lock()
	changed := v.Busy || s.wasBusy
	s.wasBusy = v.Busy
	s.mu.Unlock()
	if changed {
		w.Header().Set("HX-Trigger", "episodes-changed")
	}
	s.fragment(w, "status", v)
}

func (s *Server) handleDismiss(w http.ResponseWriter, r *http.Request) {
	s.st.Dismiss(r.FormValue("key"))
	s.fragment(w, "status", s.statusView(""))
}

func (s *Server) pullNotice(err error, started string) string {
	switch {
	case err == nil:
		return started
	case errors.Is(err, puller.ErrBusy):
		return "An update is already running."
	}
	return err.Error()
}

func (s *Server) handlePullAll(w http.ResponseWriter, r *http.Request) {
	s.fragment(w, "status", s.statusView(s.pullNotice(s.svc.TriggerAll(), "Checking all feeds for new episodes…")))
}

func (s *Server) handlePullFeed(w http.ResponseWriter, r *http.Request) {
	_, f, ok := s.feedOr404(w, r)
	if !ok {
		return
	}
	s.fragment(w, "status", s.statusView(s.pullNotice(s.svc.TriggerFeed(f.ID), "Checking "+f.Name+" for new episodes…")))
}
