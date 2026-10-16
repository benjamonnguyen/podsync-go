package webui

import (
	"net/http"
	"os"
	"strings"

	"podsync/rss"
)

// The podcast feeds and media files are served straight from the shared
// library; podcast apps never talk to the sync service.

func (s *Server) handleRSS(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".xml")
	if !ok {
		http.NotFound(w, r)
		return
	}
	cfg, f, err := s.feed(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sum := s.summary(cfg, f)
	base := cfg.Server.BaseURL
	out, err := rss.Build(base, rss.Feed{
		ID: id, Title: sum.Name, Description: sum.Description, ImageURL: sum.ImageURL,
		Link: base + "/feed/" + id, Self: sum.RSSURL,
	}, s.lib.Episodes(id), s.now())
	if err != nil {
		http.Error(w, "could not build feed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write(out)
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	path, err := s.lib.MediaPath(r.PathValue("id"), file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", rss.MimeType(file))
	http.ServeContent(w, r, file, st.ModTime(), f)
}
