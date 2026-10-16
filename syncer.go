package podsync

import (
	"context"
	"time"
)

// Syncer is the application service: everything a client (the web UI, or any
// other tool) can ask the sync service to do. The syncer package implements
// it, rest.Server exposes it over HTTP, and rest.Client implements it again on
// the client side so the web UI can run in a separate process.
//
// Errors of kind ErrNotFound, ErrInvalid and ErrBusy are preserved across the
// REST boundary.
type Syncer interface {
	// Feeds lists configured feeds (in config order) and library folders
	// whose feed is no longer configured.
	Feeds(ctx context.Context) (FeedList, error)
	Feed(ctx context.Context, id string) (FeedDetail, error)
	// Episodes lists the episodes known for a feed (downloaded, cleaned,
	// failed, in flight), optionally filtered by title words in q.
	Episodes(ctx context.Context, feedID, q string) ([]EpisodeView, error)
	// Episode returns one episode's current view. hint supplies what the
	// caller already knows (listing data) for episodes not yet in the library.
	Episode(ctx context.Context, feedID string, hint EpisodeInfo) (EpisodeView, error)
	// Older lists the window of the feed's source starting at offset
	// (Load More): episodes passing the feed's filters that are not in the library.
	Older(ctx context.Context, feedID string, offset int) (OlderPage, error)
	// SearchLibrary finds feeds by name and downloaded episodes by title.
	SearchLibrary(ctx context.Context, q string) (SearchHits, error)

	// Resolve turns a URL, @handle or ID into a feed description (for previews).
	Resolve(ctx context.Context, ref string) (FeedInfo, error)
	// SearchSource searches the default source by name.
	SearchSource(ctx context.Context, query string, kind SearchKind) ([]SearchResult, error)

	AddFeed(ctx context.Context, req AddFeedRequest) (FeedSummary, error)
	// UpdateFeed replaces a feed's name and option overrides (nil = inherit).
	UpdateFeed(ctx context.Context, id string, upd FeedSettings) (FeedDetail, error)
	DeleteFeed(ctx context.Context, id string, deleteFiles bool) error
	// DeleteOrphan deletes the files of a library folder whose feed is not configured.
	DeleteOrphan(ctx context.Context, id string) error

	Settings(ctx context.Context) (Settings, error)
	UpdateSettings(ctx context.Context, s Settings) (SettingsResult, error)

	// Pull starts an update of one feed, or of every feed when feedID is "".
	// It returns ErrBusy if an update is already running.
	Pull(ctx context.Context, feedID string) error
	// Download queues a user-requested download (Load More / Retry).
	Download(ctx context.Context, feedID string, ep EpisodeInfo) error

	Status(ctx context.Context) (Status, error)
	DismissIssue(ctx context.Context, key string) error
}

// FeedSummary is a feed as shown in lists.
type FeedSummary struct {
	ID           string    `json:"id"`
	Source       string    `json:"source"`
	Kind         string    `json:"kind"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	ImageURL     string    `json:"image_url,omitempty"`
	RSSURL       string    `json:"rss_url"`
	Episodes     int       `json:"episodes"` // downloaded
	Bytes        int64     `json:"bytes"`
	LastPull     time.Time `json:"last_pull"`
	HasOverrides bool      `json:"has_overrides"`
	Orphan       bool      `json:"orphan,omitempty"`
}

type FeedList struct {
	Feeds   []FeedSummary `json:"feeds"`
	Orphans []FeedSummary `json:"orphans"`
}

// FeedDetail adds the feed's settings to its summary.
type FeedDetail struct {
	FeedSummary
	Overrides Feed    `json:"overrides"` // the configured feed: name and nil-able overrides
	Effective Options `json:"effective"` // overrides applied over Defaults
	Defaults  Options `json:"defaults"`
}

// FeedSettings is what UpdateFeed replaces. Nil option fields inherit the defaults.
type FeedSettings struct {
	Name             string  `json:"name"`
	Format           *string `json:"format,omitempty"`
	MaxResolution    *int    `json:"max_resolution,omitempty"`
	ExcludeShorts    *bool   `json:"exclude_shorts,omitempty"`
	TitleInclude     *string `json:"title_include,omitempty"`
	TitleExclude     *string `json:"title_exclude,omitempty"`
	CleanupAfterDays *int    `json:"cleanup_after_days,omitempty"`
	CleanupKeepLast  *int    `json:"cleanup_keep_last,omitempty"`
	PageSize         *int    `json:"page_size,omitempty"`
}

// Episode states shown to users: the library status plus live and listing-only states.
const (
	StateAvailable   = "available" // listed by the source, not in the library
	StateQueued      = "queued"
	StateDownloading = "downloading"
	StateDownloaded  = string(StatusDownloaded)
	StateCleaned     = string(StatusCleaned)
	StateFailed      = string(StatusFailed)
)

// EpisodeView is an episode together with its live state.
type EpisodeView struct {
	Episode
	State string `json:"state"`
}

type EpisodeSearchHit struct {
	FeedID   string `json:"feed_id"`
	FeedName string `json:"feed_name"`
	EpisodeView
}

type SearchHits struct {
	Feeds     []FeedSummary      `json:"feeds"`
	Episodes  []EpisodeSearchHit `json:"episodes"`
	Truncated bool               `json:"truncated,omitempty"`
}

// OlderPage is one Load More window.
type OlderPage struct {
	Episodes   []EpisodeView `json:"episodes"`
	NextOffset int           `json:"next_offset"`
	More       bool          `json:"more"`
}

type AddFeedRequest struct {
	Info     FeedInfo `json:"info"`
	PageSize *int     `json:"page_size,omitempty"`
}

// Settings is the global configuration (everything except the feed list).
type Settings struct {
	Server     Server   `json:"server"`
	Download   Download `json:"download"`
	Defaults   Options  `json:"defaults"`
	ConfigPath string   `json:"config_path,omitempty"`
}

type SettingsResult struct {
	Settings
	// RestartRequired is set when a changed value only takes effect after a restart.
	RestartRequired bool `json:"restart_required"`
}
