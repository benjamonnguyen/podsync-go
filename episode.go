package podsync

import (
	"sort"
	"time"
)

// EpisodeStatus is the persisted state of an episode in the library.
type EpisodeStatus string

const (
	StatusDownloaded EpisodeStatus = "downloaded"
	// StatusCleaned means retention removed the media. The record is kept so
	// the scheduler does not download the episode again.
	StatusCleaned EpisodeStatus = "cleaned"
	StatusFailed  EpisodeStatus = "failed"
)

// Episode is the library's record of one episode (stored as a sidecar JSON
// file next to the media).
type Episode struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Description  string        `json:"description,omitempty"`
	URL          string        `json:"url,omitempty"`
	Thumbnail    string        `json:"thumbnail,omitempty"`  // remote thumbnail URL
	ThumbFile    string        `json:"thumb_file,omitempty"` // local jpg next to the media
	Published    time.Time     `json:"published"`
	DurationSec  int           `json:"duration_sec,omitempty"`
	Channel      string        `json:"channel,omitempty"`
	ViewCount    int64         `json:"view_count,omitempty"`
	Height       int           `json:"height,omitempty"`
	IsShort      bool          `json:"is_short,omitempty"`
	Status       EpisodeStatus `json:"status"`
	File         string        `json:"file,omitempty"`
	Size         int64         `json:"size,omitempty"`
	DownloadedAt *time.Time    `json:"downloaded_at,omitempty"`
	Manual       bool          `json:"manual,omitempty"` // downloaded on request; exempt from cleanup
	Attempts     int           `json:"attempts,omitempty"`
	LastError    string        `json:"last_error,omitempty"`
}

// FeedMeta is cache-only information about a feed; losing it is harmless.
type FeedMeta struct {
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	ImageURL    string    `json:"image_url,omitempty"`
	LastPull    time.Time `json:"last_pull,omitempty"`
}

// LibraryFeed describes a feed directory found in the library.
type LibraryFeed struct {
	Source     string
	ID         string
	Meta       FeedMeta
	Downloaded int
	Bytes      int64
}

// EpisodeHit is one library search result.
type EpisodeHit struct {
	FeedID  string
	Episode Episode
}

// SortNewestFirst orders episodes by publish date, newest first (episodes
// without a date last), then by ID for stability.
func SortNewestFirst(eps []Episode) {
	sort.Slice(eps, func(i, j int) bool {
		if !eps[i].Published.Equal(eps[j].Published) {
			return eps[i].Published.After(eps[j].Published)
		}
		return eps[i].ID < eps[j].ID
	})
}
