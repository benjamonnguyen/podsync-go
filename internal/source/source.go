// Package source defines the adapter interface that every media source
// (YouTube today) implements, plus the registry that maps kinds to adapters.
package source

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// FeedInfo is the canonical description of a channel or playlist.
type FeedInfo struct {
	Source      string
	Kind        string // "channel" | "playlist"
	ID          string
	Name        string
	Description string
	ImageURL    string
}

// EpisodeInfo describes one episode. Listings are cheap "flat" results, so
// Published is usually zero until the episode has been downloaded.
type EpisodeInfo struct {
	ID          string
	Title       string
	Description string
	URL         string
	Thumbnail   string
	Published   time.Time
	DurationSec int
	Channel     string // uploader; useful for playlists that mix channels
	ViewCount   int64
	Height      int // video height in pixels once downloaded; 0 for audio/unknown
	IsShort     bool
	IsLive      bool // live or upcoming stream; cannot be downloaded yet
	Unavailable bool // private/deleted placeholder entries
}

type DownloadOptions struct {
	Audio     bool
	MaxHeight int // 0 = no cap
}

type DownloadResult struct {
	File    string // base name inside destDir
	Size    int64
	Thumb   string      // base name of the saved jpg thumbnail inside destDir; empty if none
	Episode EpisodeInfo // full metadata discovered during download
}

type SearchKind string

const (
	SearchChannel  SearchKind = "channel"
	SearchPlaylist SearchKind = "playlist"
)

type SearchResult struct {
	Kind        string
	ID          string
	Name        string
	Handle      string
	Description string
	ImageURL    string
	Subscribers int64
	Items       int // playlist length when known
}

// Health reports whether the adapter's external dependencies work.
type Health struct {
	Version  string
	Errors   []string // adapter cannot function
	Warnings []string // degraded (e.g. ffmpeg missing)
}

type Source interface {
	Kind() string
	Check(ctx context.Context) Health
	// Resolve turns a user-supplied URL, @handle or ID into a canonical feed.
	Resolve(ctx context.Context, ref string) (FeedInfo, error)
	// Search finds channels or playlists by name.
	Search(ctx context.Context, query string, kind SearchKind) ([]SearchResult, error)
	// ListEpisodes returns the window [offset, offset+limit) of a feed's
	// episodes, newest first. Unavailable entries are included (flagged) so
	// callers can tell whether a short page means the end of the feed.
	ListEpisodes(ctx context.Context, feed FeedInfo, offset, limit int) ([]EpisodeInfo, error)
	// Download fetches one episode into destDir.
	Download(ctx context.Context, ep EpisodeInfo, opts DownloadOptions, destDir string) (DownloadResult, error)
}

// Updater is implemented by sources that can update their own tooling.
type Updater interface {
	SelfUpdate(ctx context.Context) (string, error)
}

type Registry struct {
	sources map[string]Source
}

func NewRegistry(srcs ...Source) *Registry {
	r := &Registry{sources: map[string]Source{}}
	for _, s := range srcs {
		r.sources[s.Kind()] = s
	}
	return r
}

func (r *Registry) Get(kind string) (Source, error) {
	s, ok := r.sources[kind]
	if !ok {
		return nil, fmt.Errorf("unknown source %q", kind)
	}
	return s, nil
}

func (r *Registry) All() []Source {
	kinds := make([]string, 0, len(r.sources))
	for k := range r.sources {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	out := make([]Source, len(kinds))
	for i, k := range kinds {
		out[i] = r.sources[k]
	}
	return out
}
