package podsync

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// FeedInfo is the canonical description of a channel or playlist.
type FeedInfo struct {
	Source      string `json:"source"`
	Kind        string `json:"kind"` // "channel" | "playlist"
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
	// Display extras filled in by lookups (not stored).
	Handle      string `json:"handle,omitempty"`      // e.g. "@LinusTechTips"
	Subscribers int64  `json:"subscribers,omitempty"` // when the source reports it
	// Reverse is set from the feed's playlist_sort = "oldest": the playlist
	// itself is ordered oldest first, so ListEpisodes must reverse it before
	// applying offset/limit to still return newest first.
	Reverse bool `json:"reverse,omitempty"`
}

// EpisodeInfo describes one episode as reported by a source. Listings are
// cheap "flat" results, so Published is usually zero until the episode has
// been downloaded.
type EpisodeInfo struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	URL         string    `json:"url,omitempty"`
	Thumbnail   string    `json:"thumbnail,omitempty"`
	Published   time.Time `json:"published"`
	DurationSec int       `json:"duration_sec,omitempty"`
	Channel     string    `json:"channel,omitempty"` // uploader; useful for playlists that mix channels
	ViewCount   int64     `json:"view_count,omitempty"`
	Height      int       `json:"height,omitempty"`      // video height once downloaded; 0 for audio/unknown
	IsLive      bool      `json:"is_live,omitempty"`     // live or upcoming stream; cannot be downloaded yet
	Unavailable bool      `json:"unavailable,omitempty"` // private/deleted placeholder entries
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

// Health reports whether a source's external dependencies work.
type Health struct {
	Version  string
	Errors   []string // the source cannot function
	Warnings []string // degraded (e.g. ffmpeg missing)
}

// Source is a provider of feeds and episodes (YouTube today).
type Source interface {
	Kind() string
	Check(ctx context.Context) Health
	// Find answers what a user typed when adding a feed, guided by hints: a URL
	// or an @handle (or a channel ID) resolves to exactly that feed; anything
	// else is searched by name among channels. Playlists are only found by
	// URL. Errors from resolving a reference the user typed are ErrInvalid.
	Find(ctx context.Context, query string) ([]FeedInfo, error)
	// Resolve turns an exact reference (URL, @handle or ID of a channel or
	// playlist) into its canonical feed; used when a feed's ID is already known.
	Resolve(ctx context.Context, ref string) (FeedInfo, error)
	// ListEpisodes returns the window [offset, offset+limit) of a feed's
	// episodes, newest first (reversing the feed's own order first if
	// feed.Reverse is set). Unavailable entries are included (flagged) so
	// callers can tell whether a short page means the end of the feed.
	ListEpisodes(ctx context.Context, feed FeedInfo, offset, limit int) ([]EpisodeInfo, error)
	// Download fetches one episode into destDir.
	Download(ctx context.Context, ep EpisodeInfo, opts DownloadOptions, destDir string) (DownloadResult, error)
}

// SourceUpdater is implemented by sources that can update their own tooling.
type SourceUpdater interface {
	SelfUpdate(ctx context.Context) (string, error)
}

// Registry maps a source kind to its implementation.
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
