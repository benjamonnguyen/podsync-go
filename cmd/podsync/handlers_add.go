package main

import (
	"net/http"
	"strconv"
	"strings"

	"podsync"
)

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "add", map[string]any{"Title": "Add feed", "DefaultPageSize": s.cfg.Get().Defaults.PageSize})
}

func (s *Server) defaultPageSize(r *http.Request) int { return s.cfg.Get().Defaults.PageSize }

// handleAddFind is the one lookup behind the Add page. The sync service decides
// from hints what was typed: a link or @handle resolves to that feed, anything
// else is searched as a channel name. One match goes straight to the preview;
// several are listed, and picking one looks it up again by ID.
func (s *Server) handleAddFind(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	feeds, err := s.sync.Find(r.Context(), q)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.fragment(w, "add_results", map[string]any{"Err": err.Error()})
		return
	}
	switch len(feeds) {
	case 0:
		s.fragment(w, "add_results", map[string]any{"Empty": true, "Q": q})
	case 1:
		s.remember(feeds[0])
		_, exists := s.cfg.Get().Feed(feeds[0].ID)
		s.fragment(w, "add_preview", map[string]any{"Info": feeds[0], "Exists": exists, "DefaultPageSize": s.defaultPageSize(r)})
	default:
		s.fragment(w, "add_results", map[string]any{"Results": feeds, "Q": q})
	}
}

// remember keeps what the user was just shown, so that adding can only add a
// feed that was previewed (a forged form post cannot add arbitrary IDs or names).
func (s *Server) remember(info podsync.FeedInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.previews {
		if s.now().Sub(p.at) > previewTTL {
			delete(s.previews, k)
		}
	}
	s.previews[info.ID] = preview{info: info, at: s.now()}
}

// handleAddCommit only adds feeds that were just previewed, so a forged form
// post cannot add arbitrary IDs or names.
func (s *Server) handleAddCommit(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.FormValue("id"))
	s.mu.Lock()
	pv, ok := s.previews[id]
	s.mu.Unlock()
	if !ok || s.now().Sub(pv.at) > previewTTL {
		s.fragment(w, "add_preview", map[string]any{"Err": "That preview expired — find the feed again."})
		return
	}
	var pageSize *int
	if v := strings.TrimSpace(r.FormValue("page_size")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.fragment(w, "add_preview", map[string]any{"Info": pv.info, "DefaultPageSize": s.defaultPageSize(r), "Err": "Episodes per update must be a number."})
			return
		}
		pageSize = &n
	}
	sum, err := s.AddFeed(r.Context(), AddFeedRequest{Info: pv.info, PageSize: pageSize})
	if err != nil {
		s.fragment(w, "add_preview", map[string]any{"Info": pv.info, "DefaultPageSize": s.defaultPageSize(r), "Err": err.Error()})
		return
	}
	redirect(w, r, "/feed/"+sum.ID)
}
