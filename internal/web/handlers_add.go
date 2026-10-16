package web

import (
	"net/http"
	"strconv"
	"strings"

	"podsync-go/internal/config"
	"podsync-go/internal/library"
	"podsync-go/internal/source"
	"podsync-go/internal/status"
)

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	s.render(w, "add", map[string]any{"Title": "Add feed", "DefaultPageSize": s.cfg.Get().Defaults.PageSize})
}

func (s *Server) defaultSource() (source.Source, error) { return s.reg.Get(config.DefaultSource) }

func (s *Server) handleAddSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	if len([]rune(q)) < 3 {
		s.fragment(w, "add_results", map[string]any{"Hint": "Type at least 3 characters."})
		return
	}
	kind := source.SearchChannel
	if r.FormValue("kind") == "playlist" {
		kind = source.SearchPlaylist
	}
	src, err := s.defaultSource()
	if err != nil {
		s.fragment(w, "add_results", map[string]any{"Err": err.Error()})
		return
	}
	results, err := src.Search(r.Context(), q, kind)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.st.Report("search", status.Warning, "Channel search failed: "+err.Error())
		s.fragment(w, "add_results", map[string]any{"Err": err.Error()})
		return
	}
	s.st.Clear("search")
	s.fragment(w, "add_results", map[string]any{"Results": results, "Q": q})
}

func (s *Server) handleAddPreview(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.FormValue("ref"))
	src, err := s.defaultSource()
	if err != nil {
		s.fragment(w, "add_preview", map[string]any{"Err": err.Error()})
		return
	}
	info, err := src.Resolve(r.Context(), ref)
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
	cfg := s.cfg.Get()
	_, exists := cfg.Feed(info.ID)
	s.fragment(w, "add_preview", map[string]any{"Info": info, "Exists": exists, "DefaultPageSize": cfg.Defaults.PageSize})
}

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
			s.fragment(w, "add_preview", map[string]any{"Info": pv.info, "DefaultPageSize": s.cfg.Get().Defaults.PageSize, "Err": "Episodes per update must be a number."})
			return
		}
		pageSize = &n
	}
	info := pv.info
	err := s.cfg.Update(func(c *config.Config) error {
		if _, dup := c.Feed(info.ID); dup {
			return errExists
		}
		c.Feeds = append(c.Feeds, config.Feed{ID: info.ID, Source: info.Source, Kind: info.Kind, Name: info.Name, PageSize: pageSize})
		return nil
	})
	if err != nil {
		s.fragment(w, "add_preview", map[string]any{"Info": info, "DefaultPageSize": s.cfg.Get().Defaults.PageSize, "Err": err.Error()})
		return
	}
	s.svc.Reconcile()
	_ = s.lib.UpdateMeta(info.ID, func(m *library.FeedMeta) {
		m.Name, m.Description, m.ImageURL = info.Name, info.Description, info.ImageURL
	})
	_ = s.svc.TriggerFeed(info.ID) // busy is fine: the scheduler will pick it up
	redirect(w, r, "/feed/"+info.ID)
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

const errExists = simpleError("That feed is already in your library.")
