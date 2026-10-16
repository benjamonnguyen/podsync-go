// Package rest exposes the sync service's small control API over HTTP+JSON
// (Server) and implements podsync.Syncer on top of that API (Client) for the
// web UI. The web UI shares the config file and data directory with the sync
// service, so the API only covers what needs the running service: applying
// changes, triggering pulls, live status, and source lookups (which need
// yt-dlp).
package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"podsync"
)

const maxBody = 1 << 20

// Server is the HTTP API in front of a podsync.Syncer.
type Server struct {
	svc podsync.Syncer
}

func NewServer(svc podsync.Syncer) *Server { return &Server{svc: svc} }

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
	mux.HandleFunc("POST "+v1+"/apply", s.apply)
	mux.HandleFunc("POST "+v1+"/pull", s.pull)
	mux.HandleFunc("GET "+v1+"/status", s.status)
	mux.HandleFunc("GET "+v1+"/find", s.find)
	mux.HandleFunc("GET "+v1+"/feeds/{id}/listing", s.listing)
	return CrossSiteGuard(requireJSON(mux))
}

// requireJSON makes body-carrying writes declare application/json, which a
// cross-site HTML form cannot do.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && r.ContentLength != 0 {
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

func reply(w http.ResponseWriter, code int, v any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, code, v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, podsync.Invalid("bad request body: %v", err))
		return false
	}
	return true
}

// ---- endpoints ----

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Apply(r.Context())
	reply(w, http.StatusOK, out, err)
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request) {
	var req podsync.PullRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.svc.Pull(r.Context(), req); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Status(r.Context())
	reply(w, http.StatusOK, out, err)
}

func (s *Server) find(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Find(r.Context(), r.URL.Query().Get("q"))
	reply(w, http.StatusOK, out, err)
}

func (s *Server) listing(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, err := strconv.Atoi(q.Get("offset"))
	if err != nil {
		offset = 0
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		limit = 0
	}
	out, err := s.svc.List(r.Context(), r.PathValue("id"), offset, limit)
	reply(w, http.StatusOK, out, err)
}
