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

func (s *Server) handleAddSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	if len([]rune(q)) < 3 {
		s.fragment(w, "add_results", map[string]any{"Hint": "Type at least 3 characters."})
		return
	}
	kind := podsync.SearchChannel
	if r.FormValue("kind") == "playlist" {
		kind = podsync.SearchPlaylist
	}
	results, err := s.sync.Search(r.Context(), q, kind)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.fragment(w, "add_results", map[string]any{"Err": err.Error()})
		return
	}
	s.fragment(w, "add_results", map[string]any{"Results": results, "Q": q})
}

func (s *Server) defaultPageSize(r *http.Request) int { return s.cfg.Get().Defaults.PageSize }

func (s *Server) handleAddPreview(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.FormValue("ref"))
	info, err := s.sync.Resolve(r.Context(), ref)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.fragment(w, "add_preview", map[string]any{"Err": err.Error()})
		return
	}
	s.mu.Lock()
	for k, p := range s.previews {
		if s.now().Sub(p.at) > previewTTL {
			delete(s.previews, k)
		}
	}
	s.previews[info.ID] = preview{info: info, at: s.now()}
	s.mu.Unlock()
	_, exists := s.cfg.Get().Feed(info.ID)
	s.fragment(w, "add_preview", map[string]any{"Info": info, "Exists": exists, "DefaultPageSize": s.defaultPageSize(r)})
}

// handleAddCommit only adds feeds that were just previewed, so a forged form
// post cannot add arbitrary IDs or names.
func (s *Server) handleAddCommit(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.FormValue("id"))
	s.mu.Lock()
	pv, ok := s.previews[id]
	s.mu.Unlock()
	if !ok || s.now().Sub(pv.at) > previewTTL {
		s.fragment(w, "add_preview", map[string]any{"Err": "That preview expired — look the channel up again."})
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
