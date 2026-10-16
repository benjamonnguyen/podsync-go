package web

import (
	"net/http"
	"os"
	"strings"

	"podsync-go/internal/rss"
)

func (s *Server) handleRSS(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	id, ok := strings.CutSuffix(file, ".xml")
	if !ok {
		s.notFound(w, r)
		return
	}
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(id)
	if !ok {
		s.notFound(w, r)
		return
	}
	sum, _ := s.lib.Feed(id)
	title := f.Name
	if title == "" {
		title = sum.Meta.Name
	}
	out, err := rss.Build(cfg.Server.BaseURL, rss.Feed{
		ID: id, Title: title, Description: sum.Meta.Description, ImageURL: sum.Meta.ImageURL,
		Link: cfg.Server.BaseURL + "/feed/" + id, Self: s.rssURL(cfg, id),
	}, s.lib.Episodes(id), s.now())
	if err != nil {
		http.Error(w, "could not build feed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write(out)
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	id, file := r.PathValue("id"), r.PathValue("file")
	_, isMedia := s.lib.EpisodeByFile(id, file)
	_, isThumb := s.lib.EpisodeByThumb(id, file)
	if !isMedia && !isThumb {
		s.notFound(w, r)
		return
	}
	path, err := s.lib.MediaPath(id, file)
	if err != nil {
		s.notFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.notFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", rss.MimeType(file))
	http.ServeContent(w, r, file, st.ModTime(), f)
}
