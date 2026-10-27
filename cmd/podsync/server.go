package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"podsync"
)

//go:embed templates/*.html static/*
var assets embed.FS

// assetVersion changes whenever an embedded static file does, so browsers
// refetch CSS and JS after a deploy instead of using the cached copy.
var assetVersion = func() string {
	h := sha256.New()
	files, _ := fs.Glob(assets, "static/*")
	for _, f := range files {
		b, _ := assets.ReadFile(f)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

// options are the web UI's dependencies, wired together in main.
type options struct {
	Config      podsync.ConfigStore // the config file, shared with the sync loop
	Library     podsync.Library     // the data directory, shared with the sync loop
	Syncer      podsync.Syncer      // the sync service, called in-process
	BaseURL     string              // the public address, for the links and thumbnails the UI shows
	MP3Encoding bool                // PODSYNC_MP3_ENCODING_DISABLED, inverted; just for the "audio" format label
	CookiesFile string              // where the settings page stores cookies.txt for yt-dlp (may not exist yet)
}

type Server struct {
	cfg         podsync.ConfigStore
	lib         podsync.Library
	sync        podsync.Syncer
	baseURL     string
	mp3Encoding bool
	cookiesFile string
	now         func() time.Time

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

func newServer(o options) (*Server, error) {
	s := &Server{
		cfg: o.Config, lib: o.Library, sync: o.Syncer, baseURL: o.BaseURL, mp3Encoding: o.MP3Encoding, cookiesFile: o.CookiesFile, now: time.Now,
		previews: map[string]preview{}, dismissed: map[string]time.Time{},
	}
	funcs := template.FuncMap{
		"size":    humanSize,
		"dur":     humanDuration,
		"date":    func(t time.Time) string { return formatDate(t) },
		"ago":     func(t time.Time) string { return ago(t, time.Now()) },
		"plural":  plural,
		"compact": compactNumber,
		"secs":    seconds,
		"count":   func(n int, one, many string) string { return fmt.Sprintf("%d %s", n, plural(n, one, many)) },
		"dot":     joinDot,
		"subscribers": func(n int64) string {
			if n <= 0 {
				return ""
			}
			return compactNumber(n) + " subscribers"
		},
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

	mux.HandleFunc("GET /feed/{id}/{format}", s.handleFeed)
	mux.HandleFunc("GET /feed/{id}/{format}/stats", s.handleStats)
	mux.HandleFunc("GET /feed/{id}/{format}/episodes", s.handleEpisodes)
	mux.HandleFunc("GET /feed/{id}/{format}/older", s.handleOlder)
	mux.HandleFunc("POST /feed/{id}/{format}/episodes/{ep}/download", s.handleDownload)
	mux.HandleFunc("GET /feed/{id}/{format}/episodes/{ep}/row", s.handleRow)
	mux.HandleFunc("POST /feed/{id}/{format}/episodes/delete", s.handleDeleteMedia)
	mux.HandleFunc("GET /feed/{id}/{format}/settings", s.handleFeedSettings)
	mux.HandleFunc("POST /feed/{id}/{format}/settings", s.handleFeedSettingsSave)
	mux.HandleFunc("GET /feed/{id}/{format}/delete", s.handleDeleteConfirm)
	mux.HandleFunc("POST /feed/{id}/{format}/delete", s.handleDelete)
	mux.HandleFunc("POST /orphans/{id}/{format}/delete", s.handleOrphanDelete)

	mux.HandleFunc("GET /add", s.handleAdd)
	mux.HandleFunc("POST /add/find", s.handleAddFind)
	mux.HandleFunc("POST /add", s.handleAddCommit)

	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings", s.handleSettingsSave)
	mux.HandleFunc("POST /settings/cookies", s.handleCookiesUpload)
	mux.HandleFunc("POST /settings/cookies/delete", s.handleCookiesDelete)
	mux.HandleFunc("POST /settings/members-retry", s.handleMembersRetry)

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /status/dismiss", s.handleDismiss)

	return s.secure(s.refresh(mux))
}

// refresh picks up what the sync loop (and hand edits) changed in the shared
// config file and data directory since the previous request. Both checks are
// cheap when nothing changed.
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

// secure adds security headers and the cross-site write guard. Thumbnails and
// media are loaded from the public address (server.base_url), so that origin
// is allowed for images and media.
func (s *Server) secure(next http.Handler) http.Handler {
	guarded := crossSiteGuard(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		origin := s.mediaOrigin()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data: "+origin+"; media-src 'self' "+origin+"; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		guarded.ServeHTTP(w, r)
	})
}

// crossSiteGuard rejects state-changing requests that a browser marks as
// coming from another site. Plain non-browser clients (curl, podcast apps)
// send neither header and pass.
func crossSiteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// mediaBase is the public address where feeds, media and thumbnails are served.
func (s *Server) mediaBase() string { return s.baseURL }

// audioExt names the file extension "audio" format downloads actually land
// as, for the settings pages' format labels: PODSYNC_MP3_ENCODING_DISABLED
// changes this deployment-wide, so a hardcoded "(m4a)" would lie about it.
func (s *Server) audioExt() string {
	if s.mp3Encoding {
		return "mp3"
	}
	return "m4a"
}

// mediaOrigin is mediaBase reduced to scheme://host[:port] for the CSP.
func (s *Server) mediaOrigin() string {
	u, err := url.Parse(s.mediaBase())
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
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
	data["AssetV"] = assetVersion
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
