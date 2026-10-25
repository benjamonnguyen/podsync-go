package main

import (
	"net/http"
	"strings"

	"podsync"
)

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "add", map[string]any{"Title": "Add feed", "DefaultPageSize": s.cfg.Get().Defaults.PageSize})
}

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
		s.renderAddPreview(w, r, feeds[0], FeedSettings{}, "")
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

// renderAddPreview shows the found feed with its full set of per-feed options
// (the same fields the feed settings page edits), prefilled with fs — empty
// on first preview, or whatever the user just submitted if it failed to add.
// A feed id may be reused across formats (see config.go's duplicate check),
// so "already in your library" only reflects the format currently selected
// and does not block adding a different one.
func (s *Server) renderAddPreview(w http.ResponseWriter, r *http.Request, info podsync.FeedInfo, fs FeedSettings, errText string) {
	cfg := s.cfg.Get()
	format := cfg.Defaults.Format
	if fs.Format != nil {
		format = *fs.Format
	}
	_, exists := cfg.Feed(info.ID, format)
	s.fragment(w, "add_preview", map[string]any{
		"Info": info, "Err": errText,
		"F":            s.feedSettingsFrom(info.ID, info.Name, info.Kind, "", fs, cfg.Defaults),
		"Exists":       exists,
		"ExistsFormat": format,
	})
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
	fs, parseErr := feedSettingsFromForm(r)
	if parseErr != nil {
		s.renderAddPreview(w, r, pv.info, fs, parseErr.Error())
		return
	}
	sum, err := s.AddFeed(r.Context(), AddFeedRequest{Info: pv.info, Settings: fs})
	if err != nil {
		s.renderAddPreview(w, r, pv.info, fs, err.Error())
		return
	}
	redirect(w, r, "/feed/"+sum.ID+"/"+sum.Format)
}
