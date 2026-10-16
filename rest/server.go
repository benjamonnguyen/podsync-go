// Package rest exposes podsync.Syncer over HTTP+JSON (Server, which also serves
// the RSS feeds and media files) and implements podsync.Syncer on top of that
// API (Client), so the web UI can run as a separate process.
package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"podsync"
	"podsync/rss"
)

const maxBody = 1 << 20

// Server is the HTTP API in front of a podsync.Syncer.
type Server struct {
	svc   podsync.Syncer
	media podsync.MediaStore
	now   func() time.Time
}

func NewServer(svc podsync.Syncer, media podsync.MediaStore) *Server {
	return &Server{svc: svc, media: media, now: time.Now}
}

type errorBody struct {
	Error struct {
		Kind    podsync.ErrorKind `json:"kind,omitempty"`
		Message string            `json:"message"`
	} `json:"error"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })

	const v1 = "/api/v1"
	mux.HandleFunc("GET "+v1+"/feeds", s.listFeeds)
	mux.HandleFunc("POST "+v1+"/feeds", s.addFeed)
	mux.HandleFunc("GET "+v1+"/feeds/{id}", s.getFeed)
	mux.HandleFunc("PUT "+v1+"/feeds/{id}", s.updateFeed)
	mux.HandleFunc("DELETE "+v1+"/feeds/{id}", s.deleteFeed)
	mux.HandleFunc("POST "+v1+"/feeds/{id}/pull", s.pullFeed)
	mux.HandleFunc("GET "+v1+"/feeds/{id}/episodes", s.listEpisodes)
	mux.HandleFunc("GET "+v1+"/feeds/{id}/episodes/{ep}", s.getEpisode)
	mux.HandleFunc("POST "+v1+"/feeds/{id}/episodes/{ep}/download", s.downloadEpisode)
	mux.HandleFunc("GET "+v1+"/feeds/{id}/older", s.older)
	mux.HandleFunc("DELETE "+v1+"/orphans/{id}", s.deleteOrphan)
	mux.HandleFunc("POST "+v1+"/pull", s.pullAll)
	mux.HandleFunc("GET "+v1+"/search", s.searchLibrary)
	mux.HandleFunc("POST "+v1+"/resolve", s.resolve)
	mux.HandleFunc("GET "+v1+"/discover", s.discover)
	mux.HandleFunc("GET "+v1+"/settings", s.getSettings)
	mux.HandleFunc("PUT "+v1+"/settings", s.putSettings)
	mux.HandleFunc("GET "+v1+"/status", s.status)
	mux.HandleFunc("POST "+v1+"/status/dismiss", s.dismiss)

	mux.HandleFunc("GET /feeds/{file}", s.serveRSS)
	mux.HandleFunc("GET /media/{id}/{file}", s.serveMedia)
	return CrossSiteGuard(requireJSON(mux))
}

// requireJSON makes body-carrying writes declare application/json, which a
// cross-site HTML form cannot do.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && r.ContentLength != 0 && strings.HasPrefix(r.URL.Path, "/api/") {
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				writeError(w, podsync.Invalid("Content-Type must be application/json"))
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("rest: encode response: %v", err)
		}
	}
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, podsync.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, podsync.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, podsync.ErrBusy):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func writeError(w http.ResponseWriter, err error) {
	var body errorBody
	body.Error.Message = err.Error()
	var pe *podsync.Error
	if errors.As(err, &pe) {
		body.Error.Kind = pe.Kind
	}
	writeJSON(w, statusFor(err), body)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, podsync.Invalid("bad request body: %v", err))
		return false
	}
	return true
}

// ---- feeds ----

func (s *Server) listFeeds(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Feeds(r.Context())
	reply(w, http.StatusOK, out, err)
}

func reply(w http.ResponseWriter, code int, v any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, code, v)
}

func (s *Server) addFeed(w http.ResponseWriter, r *http.Request) {
	var req podsync.AddFeedRequest
	if !readJSON(w, r, &req) {
		return
	}
	out, err := s.svc.AddFeed(r.Context(), req)
	reply(w, http.StatusCreated, out, err)
}

func (s *Server) getFeed(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Feed(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, out, err)
}

func (s *Server) updateFeed(w http.ResponseWriter, r *http.Request) {
	var upd podsync.FeedSettings
	if !readJSON(w, r, &upd) {
		return
	}
	out, err := s.svc.UpdateFeed(r.Context(), r.PathValue("id"), upd)
	reply(w, http.StatusOK, out, err)
}

func (s *Server) deleteFeed(w http.ResponseWriter, r *http.Request) {
	del, _ := strconv.ParseBool(r.URL.Query().Get("delete_files"))
	if err := s.svc.DeleteFeed(r.Context(), r.PathValue("id"), del); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteOrphan(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteOrphan(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pullFeed(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Pull(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) pullAll(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Pull(r.Context(), ""); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ---- episodes ----

func (s *Server) listEpisodes(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Episodes(r.Context(), r.PathValue("id"), r.URL.Query().Get("q"))
	reply(w, http.StatusOK, out, err)
}

func hintFromQuery(r *http.Request) podsync.EpisodeInfo {
	q := r.URL.Query()
	h := podsync.EpisodeInfo{ID: r.PathValue("ep"), Title: q.Get("title"), Thumbnail: q.Get("thumb"), Channel: q.Get("channel")}
	h.DurationSec, _ = strconv.Atoi(q.Get("duration"))
	h.ViewCount, _ = strconv.ParseInt(q.Get("views"), 10, 64)
	return h
}

func (s *Server) getEpisode(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Episode(r.Context(), r.PathValue("id"), hintFromQuery(r))
	reply(w, http.StatusOK, out, err)
}

func (s *Server) downloadEpisode(w http.ResponseWriter, r *http.Request) {
	ep := podsync.EpisodeInfo{}
	if !readJSON(w, r, &ep) {
		return
	}
	ep.ID = r.PathValue("ep") // the path is authoritative
	id := r.PathValue("id")
	if err := s.svc.Download(r.Context(), id, ep); err != nil {
		writeError(w, err)
		return
	}
	out, err := s.svc.Episode(r.Context(), id, ep)
	reply(w, http.StatusAccepted, out, err)
}

func (s *Server) older(w http.ResponseWriter, r *http.Request) {
	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if err != nil {
		offset = 0
	}
	out, err := s.svc.Older(r.Context(), r.PathValue("id"), offset)
	reply(w, http.StatusOK, out, err)
}

// ---- search / discovery ----

func (s *Server) searchLibrary(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.SearchLibrary(r.Context(), r.URL.Query().Get("q"))
	reply(w, http.StatusOK, out, err)
}

type resolveRequest struct {
	Ref string `json:"ref"`
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	var req resolveRequest
	if !readJSON(w, r, &req) {
		return
	}
	out, err := s.svc.Resolve(r.Context(), req.Ref)
	reply(w, http.StatusOK, out, err)
}

func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	kind := podsync.SearchChannel
	if r.URL.Query().Get("kind") == string(podsync.SearchPlaylist) {
		kind = podsync.SearchPlaylist
	}
	out, err := s.svc.SearchSource(r.Context(), r.URL.Query().Get("q"), kind)
	reply(w, http.StatusOK, out, err)
}

// ---- settings & status ----

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Settings(r.Context())
	reply(w, http.StatusOK, out, err)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var in podsync.Settings
	if !readJSON(w, r, &in) {
		return
	}
	out, err := s.svc.UpdateSettings(r.Context(), in)
	reply(w, http.StatusOK, out, err)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Status(r.Context())
	reply(w, http.StatusOK, out, err)
}

type dismissRequest struct {
	Key string `json:"key"`
}

func (s *Server) dismiss(w http.ResponseWriter, r *http.Request) {
	var req dismissRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.svc.DismissIssue(r.Context(), req.Key); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- public: RSS and media ----

func (s *Server) serveRSS(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".xml")
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	detail, err := s.svc.Feed(ctx, id)
	if errors.Is(err, podsync.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not build feed", http.StatusInternalServerError)
		return
	}
	views, err := s.svc.Episodes(ctx, id, "")
	if err != nil {
		http.Error(w, "could not build feed", http.StatusInternalServerError)
		return
	}
	settings, err := s.svc.Settings(ctx)
	if err != nil {
		http.Error(w, "could not build feed", http.StatusInternalServerError)
		return
	}
	eps := make([]podsync.Episode, 0, len(views))
	for _, v := range views {
		eps = append(eps, v.Episode)
	}
	base := settings.Server.BaseURL
	out, err := rss.Build(base, rss.Feed{
		ID: id, Title: detail.Name, Description: detail.Description, ImageURL: detail.ImageURL,
		Link: base + "/feed/" + id, Self: detail.RSSURL,
	}, eps, s.now())
	if err != nil {
		http.Error(w, "could not build feed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write(out)
}

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	path, err := s.media.MediaPath(r.PathValue("id"), file)
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
