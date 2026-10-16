package podsync

import "context"

// Syncer is the control surface of the sync service (cmd/syncr), the part of
// the system that runs yt-dlp and downloads in the background. It is
// deliberately small: the web UI shares the config file and the data
// directory with the sync service and reads and writes those directly, so it
// only needs the sync service for what cannot be done through files.
//
// syncer.Service implements it inside the sync service; rest.Client implements
// it for the web UI by calling the REST API.
//
// Errors of kind ErrNotFound, ErrInvalid and ErrBusy are preserved across the
// REST boundary.
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

	// Resolve turns a URL, @handle or ID into a feed description.
	Resolve(ctx context.Context, ref string) (FeedInfo, error)
	// Search searches the default source by name.
	Search(ctx context.Context, query string, kind SearchKind) ([]SearchResult, error)
	// List returns the window [offset, offset+limit) of a configured feed's
	// episodes straight from its source, newest first. Nothing is stored.
	List(ctx context.Context, feedID string, offset, limit int) ([]EpisodeInfo, error)
}

// ApplyResult is what Apply reports.
type ApplyResult struct {
	// Changed is true when the configuration on disk differed from what was running.
	Changed bool `json:"changed"`
	// RestartRequired is true when a changed value only takes effect after a
	// restart of the sync service (its listen address or data directory).
	RestartRequired bool `json:"restart_required"`
}

// PullRequest selects what Pull does.
type PullRequest struct {
	Feed    string       `json:"feed,omitempty"`    // empty = every feed
	Episode *EpisodeInfo `json:"episode,omitempty"` // download this episode of Feed now
}

// Job is one queued or running download.
type Job struct {
	FeedID    string `json:"feed_id"`
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
