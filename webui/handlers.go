package webui

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"podsync"
)

func isKind(err error, target error) bool { return errors.Is(err, target) }

// rowView is one episode row: the service's view of the episode plus the
// presentation context the template needs.
type rowView struct {
	podsync.EpisodeView
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

func makeRows(feedID, feedName string, playlist bool, views []podsync.EpisodeView) []rowView {
	rows := make([]rowView, 0, len(views))
	for _, v := range views {
		rows = append(rows, rowView{EpisodeView: v, FeedID: feedID, FeedName: feedName, ShowChannel: playlist})
	}
	return rows
}

// hintFromRequest reads the listing data a row carries in hidden inputs or
// query parameters, so a row can be re-rendered before the library knows the
// episode. The service sanitizes it.
func hintFromRequest(r *http.Request, epID string) podsync.EpisodeInfo {
	get := func(k string) string {
		if v := r.PostFormValue(k); v != "" {
			return v
		}
		return r.URL.Query().Get(k)
	}
	h := podsync.EpisodeInfo{ID: epID, Title: strings.TrimSpace(get("title")), Channel: strings.TrimSpace(get("channel")), Thumbnail: get("thumb")}
	h.DurationSec, _ = strconv.Atoi(get("duration"))
	h.ViewCount, _ = strconv.ParseInt(get("views"), 10, 64)
	return h
}

// ---- library ----

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	list, err := s.api.Feeds(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "library", map[string]any{"Title": "Library", "Cards": list.Feeds, "Orphans": list.Orphans})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		list, err := s.api.Feeds(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.fragment(w, "library_body", map[string]any{"Cards": list.Feeds, "Orphans": list.Orphans})
		return
	}
	hits, err := s.api.SearchLibrary(r.Context(), q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows := make([]rowView, 0, len(hits.Episodes))
	for _, h := range hits.Episodes {
		rows = append(rows, rowView{EpisodeView: h.EpisodeView, FeedID: h.FeedID, FeedName: h.FeedName, ShowFeed: true})
	}
	s.fragment(w, "search_results", map[string]any{"Q": q, "Feeds": hits.Feeds, "Rows": rows, "Truncated": hits.Truncated})
}

// ---- feed detail ----

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.api.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views, err := s.api.Episodes(r.Context(), id, "")
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
	detail, err := s.api.Feed(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.fragment(w, "feed_stats", map[string]any{"Card": detail.FeedSummary, "Format": detail.Effective.Format})
}

func (s *Server) handleEpisodes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	detail, err := s.api.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views, err := s.api.Episodes(r.Context(), id, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.fragment(w, "episode_list", map[string]any{"FeedID": id, "Rows": makeRows(id, detail.Name, detail.Kind == podsync.FeedPlaylist, views), "Q": q})
}

func (s *Server) handleOlder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.api.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if err != nil || offset < 0 || offset > 100000 {
		offset = 0
	}
	page, err := s.api.Older(r.Context(), id, offset)
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
	detail, err := s.api.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hint := hintFromRequest(r, epID)
	if err := s.api.Download(r.Context(), id, hint); err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderRow(w, r, detail, hint)
}

func (s *Server) handleRow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.api.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderRow(w, r, detail, hintFromRequest(r, r.PathValue("ep")))
}

func (s *Server) renderRow(w http.ResponseWriter, r *http.Request, detail podsync.FeedDetail, hint podsync.EpisodeInfo) {
	v, err := s.api.Episode(r.Context(), detail.ID, hint)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.fragment(w, "episode_row", rowView{EpisodeView: v, FeedID: detail.ID, FeedName: detail.Name, ShowChannel: detail.Kind == podsync.FeedPlaylist})
}

// ---- pulling & status ----

type statusView struct {
	Pulling     bool
	Downloading int
	Queued      int
	Busy        bool
	Version     uint64
	Issues      []podsync.Issue
	Notice      string
}

// statusView asks the sync service for its status. If the service cannot be
// reached that is itself shown as an error in the banner.
func (s *Server) statusView(r *http.Request, notice string) statusView {
	st, err := s.api.Status(r.Context())
	if err != nil {
		now := s.now()
		return statusView{Notice: notice, Issues: []podsync.Issue{{
			Key: "syncr", Level: podsync.LevelError, Message: "Cannot reach the sync service: " + err.Error(), Since: now, Last: now, Count: 1,
		}}}
	}
	return statusView{
		Pulling: st.Pulling, Downloading: st.Downloading, Queued: st.Queued, Busy: st.Pulling || st.Downloading > 0 || st.Queued > 0,
		Version: st.Version, Issues: st.Issues, Notice: notice,
	}
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

func (s *Server) handleDismiss(w http.ResponseWriter, r *http.Request) {
	_ = s.api.DismissIssue(r.Context(), r.FormValue("key"))
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
	err := s.api.Pull(r.Context(), "")
	s.fragment(w, "status", s.statusView(r, pullNotice(err, "Checking all feeds for new episodes…")))
}

func (s *Server) handlePullFeed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.api.Feed(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	err = s.api.Pull(r.Context(), id)
	s.fragment(w, "status", s.statusView(r, pullNotice(err, "Checking "+detail.Name+" for new episodes…")))
}
