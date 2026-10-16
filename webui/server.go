// Package webui serves the htmx-driven web interface and the public podcast
// feeds and media files. It shares the config file and data directory with the
// sync service (cmd/syncr): it reads and writes those directly and talks to the
// sync service, through podsync.Syncer, only for what needs the running
// service (applying changes, pulls, live status, source lookups).
package webui

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"podsync"
	"podsync/rest"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Options are the web UI's dependencies, wired together by cmd/webui.
type Options struct {
	Config  podsync.ConfigStore // the config file shared with the sync service
	Library podsync.Library     // the data directory shared with the sync service
	Syncer  podsync.Syncer      // client of the sync service's control API
}

type Server struct {
	cfg  podsync.ConfigStore
	lib  podsync.Library
	sync podsync.Syncer
	now  func() time.Time

	pages map[string]*template.Template
	frag  *template.Template

	mu        sync.Mutex
	previews  map[string]preview
	dismissed map[string]time.Time // issue key -> Last of the issue instance that was dismissed
	cfgErr    error                // last failure to re-read the config file
}

type preview struct {
	info podsync.FeedInfo
	at   time.Time
}

const previewTTL = 15 * time.Minute

var pageNames = []string{"library", "feed", "add", "settings", "feed_settings", "delete"}

func New(o Options) (*Server, error) {
	s := &Server{
		cfg: o.Config, lib: o.Library, sync: o.Syncer, now: time.Now,
		previews: map[string]preview{}, dismissed: map[string]time.Time{},
	}
	funcs := template.FuncMap{
		"size":    humanSize,
		"dur":     humanDuration,
		"date":    func(t time.Time) string { return formatDate(t) },
		"ago":     func(t time.Time) string { return ago(t, time.Now()) },
		"plural":  plural,
		"compact": compactNumber,
		"count":   func(n int, one, many string) string { return fmt.Sprintf("%d %s", n, plural(n, one, many)) },
	}
	frag, err := template.New("fragments.html").Funcs(funcs).ParseFS(assets, "templates/fragments.html")
	if err != nil {
		return nil, err
	}
	s.frag = frag
	s.pages = map[string]*template.Template{}
	for _, name := range pageNames {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/fragments.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServer(http.FS(staticFS)), time.Hour)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })

	mux.HandleFunc("GET /{$}", s.handleLibrary)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("POST /pull", s.handlePullAll)

	mux.HandleFunc("GET /feed/{id}", s.handleFeed)
	mux.HandleFunc("GET /feed/{id}/stats", s.handleStats)
	mux.HandleFunc("GET /feed/{id}/episodes", s.handleEpisodes)
	mux.HandleFunc("GET /feed/{id}/older", s.handleOlder)
	mux.HandleFunc("POST /feed/{id}/pull", s.handlePullFeed)
	mux.HandleFunc("POST /feed/{id}/episodes/{ep}/download", s.handleDownload)
	mux.HandleFunc("GET /feed/{id}/episodes/{ep}/row", s.handleRow)
	mux.HandleFunc("GET /feed/{id}/settings", s.handleFeedSettings)
	mux.HandleFunc("POST /feed/{id}/settings", s.handleFeedSettingsSave)
	mux.HandleFunc("GET /feed/{id}/delete", s.handleDeleteConfirm)
	mux.HandleFunc("POST /feed/{id}/delete", s.handleDelete)
	mux.HandleFunc("POST /orphans/{id}/delete", s.handleOrphanDelete)

	mux.HandleFunc("GET /add", s.handleAdd)
	mux.HandleFunc("POST /add/search", s.handleAddSearch)
	mux.HandleFunc("POST /add/preview", s.handleAddPreview)
	mux.HandleFunc("POST /add", s.handleAddCommit)

	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings", s.handleSettingsSave)

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /status/dismiss", s.handleDismiss)

	mux.HandleFunc("GET /feeds/{file}", s.handleRSS)
	mux.HandleFunc("GET /media/{id}/{file}", s.handleMedia)

	return secure(s.refresh(mux))
}

// refresh picks up what the sync service (and hand edits) changed in the
// shared config file and data directory since the previous request. Both
// checks are cheap when nothing changed.
func (s *Server) refresh(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/static/") && r.URL.Path != "/healthz" {
			_, err := s.cfg.Reload()
			s.mu.Lock()
			s.cfgErr = err
			s.mu.Unlock()
			if _, err := s.lib.Refresh(); err != nil {
				log.Printf("refresh library: %v", err)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// secure adds security headers and the cross-site write guard.
func secure(next http.Handler) http.Handler {
	guarded := rest.CrossSiteGuard(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; media-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		guarded.ServeHTTP(w, r)
	})
}

func cacheFor(next http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		next.ServeHTTP(w, r)
	})
}

// ---- rendering ----

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Status"] = s.statusView(r, "")
	data["Page"] = page
	var buf strings.Builder
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, buf.String())
}

func (s *Server) fragment(w http.ResponseWriter, name string, data any) {
	var buf strings.Builder
	if err := s.frag.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("render fragment %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, buf.String())
}

// fail renders an error response for failures the user cannot fix from the page itself.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	code := http.StatusInternalServerError
	switch {
	case isKind(err, podsync.ErrNotFound):
		code = http.StatusNotFound
	case isKind(err, podsync.ErrInvalid):
		code = http.StatusBadRequest
	case isKind(err, podsync.ErrBusy):
		code = http.StatusConflict
	}
	http.Error(w, err.Error(), code)
}

// redirect works for both full-page form posts and htmx requests.
func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}
