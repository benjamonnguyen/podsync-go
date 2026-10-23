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
	"podsync/opml"
	"podsync/rss"
)

// Server publishes feeds and media from a Library. baseURL is syncr's public
// address (how podcast apps and browsers reach it); it is fixed for the
// process lifetime, set by a flag/environment variable, not the config file.
type Server struct {
	cfg     podsync.ConfigStore
	lib     podsync.Library
	baseURL string
	now     func() time.Time
}

func NewServer(cfg podsync.ConfigStore, lib podsync.Library, baseURL string) *Server {
	return &Server{cfg: cfg, lib: lib, baseURL: baseURL, now: time.Now}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /feeds/{file}", s.serveRSS)
	mux.HandleFunc("GET /media/{id}/{file}", s.serveMedia)
	mux.HandleFunc("GET /opml.xml", s.serveOPML)
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
	f, ok := s.cfg.Get().Feed(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	lf, _ := s.lib.Feed(id)
	title := lf.Meta.Name
	if title == "" {
		title = f.ID
	}
	base := s.baseURL
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

func (s *Server) serveOPML(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL
	cfg := s.cfg.Get()
	var feeds []opml.Feed
	for _, f := range cfg.Feeds {
		if f.OPMLExclude {
			continue
		}
		lf, _ := s.lib.Feed(f.ID)
		title := lf.Meta.Name
		if title == "" {
			title = f.ID
		}
		feeds = append(feeds, opml.Feed{Title: title, XMLURL: base + "/feeds/" + f.ID + ".xml", HTMLURL: base})
	}
	out, err := opml.Build("podsync", feeds)
	if err != nil {
		http.Error(w, "could not build OPML", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
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
