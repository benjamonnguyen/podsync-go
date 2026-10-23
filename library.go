package podsync

// Library stores feeds and their episodes. The implementation (package fs)
// keeps media files plus a small JSON sidecar per episode on disk. A feed's
// identity is the pair (id, format): the same id may be configured twice,
// once per format (e.g. an audio and a video feed off one channel), each
// getting its own directory and download history. Changing a feed's format
// moves it to a different (id, format) directory; the old one is not deleted
// automatically — it shows up as an orphan, same as any other feed removed
// from the config.
type Library interface {
	// Load scans storage and rebuilds the in-memory index.
	Load() error
	// Refresh picks up changes other processes made to storage since the last
	// scan (the sync service and the web UI share one data directory). It
	// reports whether anything changed.
	Refresh() (changed bool, err error)
	// CleanTemp removes staged partial downloads. Only the process that
	// downloads (the sync service) should call it, at startup.
	CleanTemp() error
	Root() string
	// TempDir is where downloads are staged before being moved into place.
	TempDir() string
	// Version increases whenever anything in the library changes.
	Version() uint64

	EnsureFeed(source, id, format string) error
	Feeds() []LibraryFeed
	Feed(id, format string) (LibraryFeed, bool)
	FeedDir(id, format string) (string, bool)
	UpdateMeta(id, format string, fn func(*FeedMeta)) error
	DeleteFeed(id, format string) error
	// ClearFeed removes a feed's bookkeeping (sidecars and _meta.json)
	// unconditionally; if deleteMedia is true, every other file (media,
	// thumbnails) is removed too. Used when a feed's format changes: the old
	// (id, format) entry is cleared rather than left as a passive orphan.
	ClearFeed(id, format string, deleteMedia bool) error

	// Episodes returns copies, newest first.
	Episodes(feedID, format string) []Episode
	Episode(feedID, format, epID string) (Episode, bool)
	PutEpisode(feedID, format string, ep Episode) error
	// RemoveMedia deletes an episode's media file (not its record or thumbnail).
	RemoveMedia(feedID, format string, ep Episode) error
	// Search matches downloaded episodes whose title contains every word of query.
	Search(query string) []EpisodeHit

	MediaStore
}

// MediaStore resolves the files a feed's episodes own (media and thumbnails).
type MediaStore interface {
	// MediaPath returns the on-disk path of a media or thumbnail file that
	// belongs to an episode of the feed, or an ErrNotFound error. It never
	// returns paths for sidecars or anything outside the feed directory.
	MediaPath(feedID, format, file string) (string, error)
}
