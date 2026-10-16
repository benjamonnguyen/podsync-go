// Package web serves the htmx-driven UI, the RSS feeds and the media files.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"podsync-go/internal/config"
	"podsync-go/internal/library"
	"podsync-go/internal/puller"
	"podsync-go/internal/source"
	"podsync-go/internal/status"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	cfg *config.Store
	lib *library.Library
	svc *puller.Service
	reg *source.Registry
	st  *status.Tracker
	now func() time.Time

	pages map[string]*template.Template
	frag  *template.Template

	mu       sync.Mutex
	previews map[string]preview
}

type preview struct {
	info source.FeedInfo
	at   time.Time
}

const previewTTL = 15 * time.Minute

var pageNames = []string{"library", "feed", "add", "settings", "feed_settings", "delete"}

func New(cfg *config.Store, lib *library.Library, svc *puller.Service, reg *source.Registry, st *status.Tracker) (*Server, error) {
	s := &Server{cfg: cfg, lib: lib, svc: svc, reg: reg, st: st, now: time.Now, previews: map[string]preview{}}
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

	return secure(mux)
}

// secure adds security headers and rejects cross-site state-changing
// requests. There is no login, so without this any web page the user visits
// could silently POST to the server on their LAN.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; media-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "cross-site request blocked", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "cross-origin request blocked", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func cacheFor(next http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		next.ServeHTTP(w, r)
	})
}

// ---- rendering ----

func (s *Server) render(w http.ResponseWriter, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Status"] = s.statusView("")
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

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
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
