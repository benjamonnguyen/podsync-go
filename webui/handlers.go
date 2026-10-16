package webui

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"podsync"
)

func isKind(err error, target error) bool { return errors.Is(err, target) }

// rowView is one episode row: the episode's view plus the presentation
// context the template needs.
type rowView struct {
	EpisodeView
	FeedID, FeedName string
	ShowFeed         bool // show the feed name (cross-feed search results)
	ShowChannel      bool // show the uploader (playlists mix channels)
}

// Thumb is the local thumbnail when the episode has one, else the remote URL.
func (r rowView) Thumb() string {
	if r.ThumbFile != "" {
		return "/media/" + r.FeedID + "/" + r.ThumbFile
	}
	return r.Thumbnail
}

func makeRows(feedID, feedName string, playlist bool, views []EpisodeView) []rowView {
	rows := make([]rowView, 0, len(views))
	for _, v := range views {
		rows = append(rows, rowView{EpisodeView: v, FeedID: feedID, FeedName: feedName, ShowChannel: playlist})
	}
	return rows
}

// hintFromRequest reads the listing data a row carries in hidden inputs or
// query parameters, so a row can be re-rendered before the library knows the
// episode. Only a remote https thumbnail is accepted; the sync service
// re-sanitizes whatever it receives.
func hintFromRequest(r *http.Request, epID string) podsync.EpisodeInfo {
	get := func(k string) string {
		if v := r.PostFormValue(k); v != "" {
			return v
		}
		return r.URL.Query().Get(k)
	}
	h := podsync.EpisodeInfo{ID: epID, Title: strings.TrimSpace(get("title")), Channel: strings.TrimSpace(get("channel"))}
	h.DurationSec, _ = strconv.Atoi(get("duration"))
	h.ViewCount, _ = strconv.ParseInt(get("views"), 10, 64)
	if t := get("thumb"); strings.HasPrefix(t, "https://") {
		h.Thumbnail = t
	}
	return h
}

// ---- library ----

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	list := s.Feeds(r.Context())
	s.render(w, r, "library", map[string]any{"Title": "Library", "Cards": list.Feeds, "Orphans": list.Orphans})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		list := s.Feeds(r.Context())
		s.fragment(w, "library_body", map[string]any{"Cards": list.Feeds, "Orphans": list.Orphans})
		return
	}
	hits := s.SearchLibrary(r.Context(), q)
	rows := make([]rowView, 0, len(hits.Episodes))
	for _, h := range hits.Episodes {
		rows = append(rows, rowView{EpisodeView: h.EpisodeView, FeedID: h.FeedID, FeedName: h.FeedName, ShowFeed: true})
	}
	s.fragment(w, "search_results", map[string]any{"Q": q, "Feeds": hits.Feeds, "Rows": rows, "Truncated": hits.Truncated})
}

// ---- feed detail ----

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views, err := s.Episodes(r.Context(), id, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "feed", map[string]any{
		"Title": detail.Name, "Card": detail.FeedSummary, "Stats": map[string]any{"Card": detail.FeedSummary, "Format": detail.Effective.Format},
		"Rows":     makeRows(id, detail.Name, detail.Kind == podsync.FeedPlaylist, views),
		"PageSize": detail.Effective.PageSize, "Offset": detail.Effective.PageSize, "Format": detail.Effective.Format, "FeedID": id, "Q": "",
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	detail, err := s.Feed(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.fragment(w, "feed_stats", map[string]any{"Card": detail.FeedSummary, "Format": detail.Effective.Format})
}

func (s *Server) handleEpisodes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	detail, err := s.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views, err := s.Episodes(r.Context(), id, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.fragment(w, "episode_list", map[string]any{"FeedID": id, "Rows": makeRows(id, detail.Name, detail.Kind == podsync.FeedPlaylist, views), "Q": q})
}

func (s *Server) handleOlder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if err != nil || offset < 0 || offset > 100000 {
		offset = 0
	}
	page, err := s.Older(r.Context(), id, offset)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.fragment(w, "older", map[string]any{"FeedID": id, "Err": err.Error(), "More": true, "NextOffset": offset})
		return
	}
	rows := makeRows(id, detail.Name, detail.Kind == podsync.FeedPlaylist, page.Episodes)
	s.fragment(w, "older", map[string]any{
		"FeedID": id, "Rows": rows, "More": page.More, "NextOffset": page.NextOffset, "Empty": len(rows) == 0 && !page.More,
	})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, epID := r.PathValue("id"), r.PathValue("ep")
	detail, err := s.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hint := hintFromRequest(r, epID)
	if err := s.DownloadEpisode(r.Context(), id, hint); err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderRow(w, r, detail, hint)
}

func (s *Server) handleRow(w http.ResponseWriter, r *http.Request) {
	detail, err := s.Feed(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderRow(w, r, detail, hintFromRequest(r, r.PathValue("ep")))
}

func (s *Server) renderRow(w http.ResponseWriter, r *http.Request, detail FeedDetail, hint podsync.EpisodeInfo) {
	v, err := s.Episode(r.Context(), detail.ID, hint)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.fragment(w, "episode_row", rowView{EpisodeView: v, FeedID: detail.ID, FeedName: detail.Name, ShowChannel: detail.Kind == podsync.FeedPlaylist})
}

// ---- pulling & status ----

type issueView struct {
	podsync.Issue
	Dismissible bool
}

type statusView struct {
	Pulling     bool
	Downloading int
	Queued      int
	Busy        bool
	Version     uint64
	Issues      []issueView
	Notice      string
}

// statusView combines the sync service's live status with problems the web UI
// detects itself: the sync service being unreachable and a config file that
// no longer parses. Those two are not dismissible.
func (s *Server) statusView(r *http.Request, notice string) statusView {
	v := statusView{Notice: notice, Version: s.lib.Version()}
	now := s.now()
	st, err := s.sync.Status(r.Context())
	if err != nil {
		v.Issues = append(v.Issues, issueView{Issue: podsync.Issue{
			Key: "syncr", Level: podsync.LevelError, Message: "Cannot reach the sync service, so nothing is being downloaded: " + err.Error(), Since: now, Last: now, Count: 1,
		}})
	} else {
		v.Pulling, v.Downloading, v.Queued = st.Pulling, st.Downloading, st.Queued
		v.Busy = st.Pulling || st.Downloading > 0 || st.Queued > 0
		s.mu.Lock()
		for _, is := range st.Issues {
			if d, ok := s.dismissed[is.Key]; ok && !is.Last.After(d) {
				continue
			}
			v.Issues = append(v.Issues, issueView{Issue: is, Dismissible: true})
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	cfgErr := s.cfgErr
	s.mu.Unlock()
	if cfgErr != nil {
		v.Issues = append(v.Issues, issueView{Issue: podsync.Issue{
			Key: "config-file", Level: podsync.LevelError, Message: "The configuration file cannot be read, so the last good settings are in use: " + cfgErr.Error(), Since: now, Last: now, Count: 1,
		}})
	}
	return v
}

// handleStatus serves the banner. It also tells the page to refresh its
// episode list/stats when the library changed since the version the client
// last saw (sent as ?v=), so short downloads between polls are never missed.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	v := s.statusView(r, "")
	if seen, err := strconv.ParseUint(r.URL.Query().Get("v"), 10, 64); err == nil && seen != v.Version {
		w.Header().Set("HX-Trigger", "episodes-changed")
	}
	s.fragment(w, "status", v)
}

// handleDismiss hides an issue until the sync service reports it again.
func (s *Server) handleDismiss(w http.ResponseWriter, r *http.Request) {
	key := r.FormValue("key")
	if st, err := s.sync.Status(r.Context()); err == nil {
		for _, is := range st.Issues {
			if is.Key == key {
				s.mu.Lock()
				s.dismissed[key] = is.Last
				s.mu.Unlock()
			}
		}
	}
	s.fragment(w, "status", s.statusView(r, ""))
}

func pullNotice(err error, started string) string {
	switch {
	case err == nil:
		return started
	case errors.Is(err, podsync.ErrBusy):
		return "An update is already running."
	}
	return err.Error()
}

func (s *Server) handlePullAll(w http.ResponseWriter, r *http.Request) {
	err := s.Pull(r.Context(), "")
	s.fragment(w, "status", s.statusView(r, pullNotice(err, "Checking all feeds for new episodes…")))
}

func (s *Server) handlePullFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	err = s.Pull(r.Context(), id)
	s.fragment(w, "status", s.statusView(r, pullNotice(err, "Checking "+detail.Name+" for new episodes…")))
}
