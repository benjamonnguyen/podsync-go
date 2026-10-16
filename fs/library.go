// Package fs implements podsync.Library on the local filesystem: media files
// plus small JSON sidecars, with an in-memory index rebuilt by scanning the
// data directory.
package fs

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"podsync"
)

var _ podsync.Library = (*Library)(nil)

const metaFile = "_meta.json"

type feedState struct {
	source   string
	meta     podsync.FeedMeta
	episodes map[string]*podsync.Episode
}

type Library struct {
	root    string
	mu      sync.RWMutex
	feed    map[string]*feedState
	version atomic.Uint64
}

func New(root string) *Library {
	return &Library{root: root, feed: map[string]*feedState{}}
}

func (l *Library) Root() string { return l.root }

// Version increases whenever anything in the library changes, so UI clients
// can cheaply tell whether to refresh.
func (l *Library) Version() uint64 { return l.version.Load() }

// TempDir is where downloads are staged before being moved into place.
func (l *Library) TempDir() string { return filepath.Join(l.root, ".tmp") }

func (l *Library) feedDir(source, id string) string { return filepath.Join(l.root, source, id) }

// FeedDir returns the directory for a known feed.
func (l *Library) FeedDir(id string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	fs, ok := l.feed[id]
	if !ok {
		return "", false
	}
	return l.feedDir(fs.source, id), true
}

// Load scans the data dir and rebuilds the index.
func (l *Library) Load() error {
	if err := os.MkdirAll(l.root, 0o755); err != nil {
		return err
	}
	_ = os.RemoveAll(l.TempDir()) // leftovers from an interrupted run
	sources, err := os.ReadDir(l.root)
	if err != nil {
		return err
	}
	feeds := map[string]*feedState{}
	for _, src := range sources {
		if !src.IsDir() || strings.HasPrefix(src.Name(), ".") {
			continue
		}
		dirs, err := os.ReadDir(filepath.Join(l.root, src.Name()))
		if err != nil {
			return err
		}
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			fs := loadFeed(filepath.Join(l.root, src.Name(), d.Name()), src.Name())
			if _, dup := feeds[d.Name()]; dup {
				log.Printf("library: feed id %q exists under more than one source; ignoring %s/%s", d.Name(), src.Name(), d.Name())
				continue
			}
			feeds[d.Name()] = fs
		}
	}
	l.mu.Lock()
	l.feed = feeds
	l.mu.Unlock()
	l.version.Add(1)
	return nil
}

func loadFeed(dir, source string) *feedState {
	fs := &feedState{source: source, episodes: map[string]*podsync.Episode{}}
	if b, err := os.ReadFile(filepath.Join(dir, metaFile)); err == nil {
		if err := json.Unmarshal(b, &fs.meta); err != nil {
			log.Printf("library: ignoring corrupt %s: %v", filepath.Join(dir, metaFile), err)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == metaFile || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var ep podsync.Episode
		if err := json.Unmarshal(b, &ep); err != nil || ep.ID == "" {
			log.Printf("library: ignoring corrupt sidecar %s: %v", filepath.Join(dir, name), err)
			continue
		}
		if ep.Status == podsync.StatusDownloaded {
			if _, err := os.Stat(filepath.Join(dir, ep.File)); err != nil {
				// Media was deleted behind our back; treat as cleaned so it is not re-downloaded.
				ep.Status, ep.File, ep.Size = podsync.StatusCleaned, "", 0
			}
		}
		fs.episodes[ep.ID] = &ep
	}
	return fs
}

// EnsureFeed creates the feed directory and index entry if missing.
func (l *Library) EnsureFeed(source, id string) error {
	if err := os.MkdirAll(l.feedDir(source, id), 0o755); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.feed[id]; !ok {
		l.feed[id] = &feedState{source: source, episodes: map[string]*podsync.Episode{}}
		l.version.Add(1)
	}
	return nil
}

func (l *Library) Feeds() []podsync.LibraryFeed {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]podsync.LibraryFeed, 0, len(l.feed))
	for id, fs := range l.feed {
		out = append(out, summarize(id, fs))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (l *Library) Feed(id string) (podsync.LibraryFeed, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	fs, ok := l.feed[id]
	if !ok {
		return podsync.LibraryFeed{}, false
	}
	return summarize(id, fs), true
}

func summarize(id string, fs *feedState) podsync.LibraryFeed {
	s := podsync.LibraryFeed{Source: fs.source, ID: id, Meta: fs.meta}
	for _, ep := range fs.episodes {
		if ep.Status == podsync.StatusDownloaded {
			s.Downloaded++
			s.Bytes += ep.Size
		}
	}
	return s
}

// UpdateMeta mutates and persists a feed's cache metadata.
func (l *Library) UpdateMeta(id string, fn func(*podsync.FeedMeta)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	fs, ok := l.feed[id]
	if !ok {
		return fmt.Errorf("unknown feed %q", id)
	}
	next := fs.meta
	fn(&next)
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(l.feedDir(fs.source, id), metaFile), b, 0o644); err != nil {
		return err
	}
	fs.meta = next
	l.version.Add(1)
	return nil
}

// Episodes returns copies sorted newest first (episodes without a date last).
func (l *Library) Episodes(feedID string) []podsync.Episode {
	l.mu.RLock()
	defer l.mu.RUnlock()
	fs, ok := l.feed[feedID]
	if !ok {
		return nil
	}
	out := make([]podsync.Episode, 0, len(fs.episodes))
	for _, ep := range fs.episodes {
		out = append(out, *ep)
	}
	podsync.SortNewestFirst(out)
	return out
}

func (l *Library) Episode(feedID, epID string) (podsync.Episode, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if fs, ok := l.feed[feedID]; ok {
		if ep, ok := fs.episodes[epID]; ok {
			return *ep, true
		}
	}
	return podsync.Episode{}, false
}

// PutEpisode persists the sidecar and updates the index.
func (l *Library) PutEpisode(feedID string, ep podsync.Episode) error {
	if ep.ID == "" || strings.ContainsAny(ep.ID, `/\`) || strings.HasPrefix(ep.ID, ".") {
		return fmt.Errorf("invalid episode id %q", ep.ID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fs, ok := l.feed[feedID]
	if !ok {
		return fmt.Errorf("unknown feed %q", feedID)
	}
	b, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(l.feedDir(fs.source, feedID), ep.ID+".json"), b, 0o644); err != nil {
		return err
	}
	cp := ep
	fs.episodes[ep.ID] = &cp
	l.version.Add(1)
	return nil
}

// RemoveMedia deletes an episode's media file (not its sidecar or thumbnail).
func (l *Library) RemoveMedia(feedID string, ep podsync.Episode) error {
	if ep.File == "" {
		return nil
	}
	path, err := l.plainPath(feedID, ep.File)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// DeleteFeed removes the feed directory and its index entry.
func (l *Library) DeleteFeed(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	fs, ok := l.feed[id]
	if !ok {
		return nil
	}
	if err := os.RemoveAll(l.feedDir(fs.source, id)); err != nil {
		return err
	}
	delete(l.feed, id)
	l.version.Add(1)
	return nil
}

// plainPath resolves a file name inside a feed directory, refusing anything
// that is not a plain file name.
func (l *Library) plainPath(feedID, file string) (string, error) {
	if file == "" || file != filepath.Base(file) || strings.HasPrefix(file, ".") {
		return "", fmt.Errorf("invalid file name %q", file)
	}
	dir, ok := l.FeedDir(feedID)
	if !ok {
		return "", fmt.Errorf("unknown feed %q", feedID)
	}
	return filepath.Join(dir, file), nil
}

// MediaPath returns the path of a media or thumbnail file owned by one of the
// feed's episodes. The thumbnail outlives the media: cleanup removes only the
// media file. Sidecars and unknown names are not found.
func (l *Library) MediaPath(feedID, file string) (string, error) {
	l.mu.RLock()
	owned := false
	if fs, ok := l.feed[feedID]; ok {
		for _, ep := range fs.episodes {
			if (ep.Status == podsync.StatusDownloaded && ep.File == file) || (ep.ThumbFile != "" && ep.ThumbFile == file) {
				owned = true
				break
			}
		}
	}
	l.mu.RUnlock()
	if !owned {
		return "", podsync.NotFound("no such file")
	}
	path, err := l.plainPath(feedID, file)
	if err != nil {
		return "", podsync.NotFound("no such file")
	}
	return path, nil
}

// Search matches episode titles; every whitespace-separated word must appear
// (case-insensitive). Only episodes with media on disk are returned.
func (l *Library) Search(query string) []podsync.EpisodeHit {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []podsync.EpisodeHit
	for id, fs := range l.feed {
		for _, ep := range fs.episodes {
			if ep.Status != podsync.StatusDownloaded {
				continue
			}
			title := strings.ToLower(ep.Title)
			match := true
			for _, w := range words {
				if !strings.Contains(title, w) {
					match = false
					break
				}
			}
			if match {
				out = append(out, podsync.EpisodeHit{FeedID: id, Episode: *ep})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Episode.Published.Equal(out[j].Episode.Published) {
			return out[i].Episode.Published.After(out[j].Episode.Published)
		}
		return out[i].Episode.ID < out[j].Episode.ID
	})
	return out
}
