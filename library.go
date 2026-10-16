package podsync

// Library stores feeds and their episodes. The implementation (package fs)
// keeps media files plus a small JSON sidecar per episode on disk.
type Library interface {
	// Load scans storage and rebuilds the in-memory index.
	Load() error
	Root() string
	// TempDir is where downloads are staged before being moved into place.
	TempDir() string
	// Version increases whenever anything in the library changes.
	Version() uint64

	EnsureFeed(source, id string) error
	Feeds() []LibraryFeed
	Feed(id string) (LibraryFeed, bool)
	FeedDir(id string) (string, bool)
	UpdateMeta(id string, fn func(*FeedMeta)) error
	DeleteFeed(id string) error

	// Episodes returns copies, newest first.
	Episodes(feedID string) []Episode
	Episode(feedID, epID string) (Episode, bool)
	PutEpisode(feedID string, ep Episode) error
	// RemoveMedia deletes an episode's media file (not its record or thumbnail).
	RemoveMedia(feedID string, ep Episode) error
	// Search matches downloaded episodes whose title contains every word of query.
	Search(query string) []EpisodeHit

	MediaStore
}

// MediaStore resolves the files a feed's episodes own (media and thumbnails).
type MediaStore interface {
	// MediaPath returns the on-disk path of a media or thumbnail file that
	// belongs to an episode of the feed, or an ErrNotFound error. It never
	// returns paths for sidecars or anything outside the feed directory.
	MediaPath(feedID, file string) (string, error)
}
