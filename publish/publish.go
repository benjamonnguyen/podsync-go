// Package publish serves what podcast apps fetch from the sync service: the
// RSS feeds and the media and thumbnail files they point to. It is the only
// public surface of syncr; the control API (package rest) listens separately.
package publish

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"podsync"
	"podsync/rss"
)

// Server publishes feeds and media from a Library.
type Server struct {
	cfg podsync.ConfigStore
	lib podsync.Library
	now func() time.Time
}

func NewServer(cfg podsync.ConfigStore, lib podsync.Library) *Server {
	return &Server{cfg: cfg, lib: lib, now: time.Now}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /feeds/{file}", s.serveRSS)
	mux.HandleFunc("GET /media/{id}/{file}", s.serveMedia)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) serveRSS(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".xml")
	if !ok {
		http.NotFound(w, r)
		return
	}
	cfg := s.cfg.Get()
	f, ok := cfg.Feed(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	lf, _ := s.lib.Feed(id)
	title := f.Name
	if title == "" {
		title = lf.Meta.Name
	}
	base := cfg.Server.BaseURL
	out, err := rss.Build(base, rss.Feed{
		ID: id, Title: title, Description: lf.Meta.Description, ImageURL: lf.Meta.ImageURL,
		Link: base, Self: base + "/feeds/" + id + ".xml",
	}, s.lib.Episodes(id), s.now())
	if err != nil {
		http.Error(w, "could not build feed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write(out)
}

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
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
