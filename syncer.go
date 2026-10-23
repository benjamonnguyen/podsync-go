package podsync

import "context"

// Syncer is the control surface of the sync loop, the part of the system
// that runs yt-dlp and downloads in the background. It is deliberately
// small: the web UI shares the config file and the data directory with the
// sync loop and reads and writes those directly, so it only needs this
// interface for what cannot be done through files.
//
// syncer.Service implements it and is called directly by the web UI, in the
// same process (see cmd/podsync).
type Syncer interface {
	// Apply makes the sync service re-read the configuration file (and rescan
	// the library) after someone else changed them.
	Apply(ctx context.Context) (ApplyResult, error)

	// Pull starts an update of one feed, or of every feed when req.Feed is
	// empty; or, when req.Episode is set, downloads that one episode now.
	// It returns once the work is queued. ErrBusy means an update is running.
	Pull(ctx context.Context, req PullRequest) error

	// Status reports live activity, current problems and in-flight downloads.
	Status(ctx context.Context) (SyncStatus, error)

	// Find looks up what a user typed to add a feed: a URL or @handle resolves
	// to that feed, anything else is a channel name search (see Source.Find).
	Find(ctx context.Context, query string) ([]FeedInfo, error)
	// List returns the window [offset, offset+limit) of a configured feed's
	// episodes straight from its source, newest first. Nothing is stored.
	List(ctx context.Context, feedID, format string, offset, limit int) ([]EpisodeInfo, error)
}

// ApplyResult is what Apply reports.
type ApplyResult struct {
	// Changed is true when the configuration on disk differed from what was running.
	Changed bool `json:"changed"`
}

// PullRequest selects what Pull does. Feed and Format together identify one
// feed; Feed empty means every feed (Format is then ignored).
type PullRequest struct {
	Feed    string       `json:"feed,omitempty"`    // empty = every feed
	Format  string       `json:"format,omitempty"`  // required when Feed is set
	Episode *EpisodeInfo `json:"episode,omitempty"` // download this episode of Feed now
}

// Job is one queued or running download.
type Job struct {
	FeedID    string `json:"feed_id"`
	Format    string `json:"format"`
	EpisodeID string `json:"episode_id"`
	State     string `json:"state"` // "queued" | "downloading"
}

// SyncStatus is the sync service's live state.
type SyncStatus struct {
	Activity
	Issues   []Issue           `json:"issues"`
	Jobs     []Job             `json:"jobs"`
	Versions map[string]string `json:"versions,omitempty"` // tool versions, e.g. yt-dlp
}
