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
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"podsync"
)

var _ podsync.Library = (*Library)(nil)

const metaFile = "_meta.json"

// feedKey is a feed's identity in the in-memory index: the same id may be
// configured twice, once per format, each with its own directory.
func feedKey(id, format string) string { return id + "\x00" + format }

type feedState struct {
	source   string
	id       string
	format   string
	meta     podsync.FeedMeta
	episodes map[string]*podsync.Episode
	dirMod   time.Time // modification time of the feed directory when it was last scanned
}

type Library struct {
	root    string
	mu      sync.RWMutex
	feed    map[string]*feedState // keyed by feedKey(id, format)
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

func (l *Library) feedDir(source, id, format string) string {
	return filepath.Join(l.root, source, id, format)
}

// FeedDir returns the directory for a known feed.
func (l *Library) FeedDir(id, format string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	fs, ok := l.feed[feedKey(id, format)]
	if !ok {
		return "", false
	}
	return l.feedDir(fs.source, id, format), true
}

// Load scans the data dir and rebuilds the index.
func (l *Library) Load() error {
	if err := os.MkdirAll(l.root, 0o755); err != nil {
		return err
	}
	sources, err := os.ReadDir(l.root)
	if err != nil {
		return err
	}
	feeds := map[string]*feedState{}
	for _, src := range sources {
		if !src.IsDir() || strings.HasPrefix(src.Name(), ".") {
			continue
		}
		ids, err := os.ReadDir(filepath.Join(l.root, src.Name()))
		if err != nil {
			return err
		}
		for _, idEntry := range ids {
			if !idEntry.IsDir() {
				continue
			}
			formats, err := os.ReadDir(filepath.Join(l.root, src.Name(), idEntry.Name()))
			if err != nil {
				return err
			}
			for _, formatEntry := range formats {
				if !formatEntry.IsDir() {
					continue
				}
				key := feedKey(idEntry.Name(), formatEntry.Name())
				if _, dup := feeds[key]; dup {
					log.Printf("library: feed %q/%q exists under more than one source; ignoring %s", idEntry.Name(), formatEntry.Name(), src.Name())
					continue
				}
				dir := filepath.Join(l.root, src.Name(), idEntry.Name(), formatEntry.Name())
				feeds[key] = loadFeed(dir, src.Name(), idEntry.Name(), formatEntry.Name())
			}
		}
	}
	l.mu.Lock()
	l.feed = feeds
	l.mu.Unlock()
	l.version.Add(1)
	return nil
}

// settleWindow is how long after a directory's last modification Refresh keeps
// rescanning it, so a write that lands within the filesystem's timestamp
// granularity right after a scan is not missed.
const settleWindow = 3 * time.Second

// Refresh picks up changes made by other processes (the sync service writes
// episodes while the web UI reads them and deletes feeds). A feed directory is
// rescanned only when its modification time moved; every sidecar write goes
// through a rename, which updates it.
func (l *Library) Refresh() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sources, err := os.ReadDir(l.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	changed := false
	seen := map[string]bool{}
	for _, src := range sources {
		if !src.IsDir() || strings.HasPrefix(src.Name(), ".") {
			continue
		}
		ids, err := os.ReadDir(filepath.Join(l.root, src.Name()))
		if err != nil {
			return changed, err
		}
		for _, idEntry := range ids {
			if !idEntry.IsDir() {
				continue
			}
			formats, err := os.ReadDir(filepath.Join(l.root, src.Name(), idEntry.Name()))
			if err != nil {
				return changed, err
			}
			for _, formatEntry := range formats {
				if !formatEntry.IsDir() {
					continue
				}
				id, format := idEntry.Name(), formatEntry.Name()
				key := feedKey(id, format)
				if seen[key] {
					continue
				}
				seen[key] = true
				dir := filepath.Join(l.root, src.Name(), id, format)
				st, err := os.Stat(dir)
				if err != nil {
					continue
				}
				cur := l.feed[key]
				if cur != nil && cur.source == src.Name() && st.ModTime().Equal(cur.dirMod) && time.Since(st.ModTime()) > settleWindow {
					continue
				}
				next := loadFeed(dir, src.Name(), id, format)
				if cur == nil || cur.source != next.source || !reflect.DeepEqual(cur.meta, next.meta) || !reflect.DeepEqual(cur.episodes, next.episodes) {
					changed = true
				}
				l.feed[key] = next
			}
		}
	}
	for key := range l.feed {
		if !seen[key] {
			delete(l.feed, key)
			changed = true
		}
	}
	if changed {
		l.version.Add(1)
	}
	return changed, nil
}

// CleanTemp removes staged partial downloads left by an interrupted run.
func (l *Library) CleanTemp() error { return os.RemoveAll(l.TempDir()) }

func loadFeed(dir, source, id, format string) *feedState {
	fs := &feedState{source: source, id: id, format: format, episodes: map[string]*podsync.Episode{}}
	if st, err := os.Stat(dir); err == nil {
		fs.dirMod = st.ModTime()
	}
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
func (l *Library) EnsureFeed(source, id, format string) error {
	if err := os.MkdirAll(l.feedDir(source, id, format), 0o755); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := feedKey(id, format)
	if _, ok := l.feed[key]; !ok {
		l.feed[key] = &feedState{source: source, id: id, format: format, episodes: map[string]*podsync.Episode{}}
		l.version.Add(1)
	}
	return nil
}

func (l *Library) Feeds() []podsync.LibraryFeed {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]podsync.LibraryFeed, 0, len(l.feed))
	for _, fs := range l.feed {
		out = append(out, summarize(fs))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Format < out[j].Format
	})
	return out
}

func (l *Library) Feed(id, format string) (podsync.LibraryFeed, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	fs, ok := l.feed[feedKey(id, format)]
	if !ok {
		return podsync.LibraryFeed{}, false
	}
	return summarize(fs), true
}

func summarize(fs *feedState) podsync.LibraryFeed {
	s := podsync.LibraryFeed{Source: fs.source, ID: fs.id, Format: fs.format, Meta: fs.meta}
	for _, ep := range fs.episodes {
		if ep.Status == podsync.StatusDownloaded {
			s.Downloaded++
			s.Bytes += ep.Size
		}
	}
	return s
}

// UpdateMeta mutates and persists a feed's cache metadata.
func (l *Library) UpdateMeta(id, format string, fn func(*podsync.FeedMeta)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := feedKey(id, format)
	fs, ok := l.feed[key]
	if !ok {
		return fmt.Errorf("unknown feed %q/%q", id, format)
	}
	next := fs.meta
	fn(&next)
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(l.feedDir(fs.source, id, format), metaFile), b, 0o644); err != nil {
		return err
	}
	fs.meta = next
	l.version.Add(1)
	return nil
}

// Episodes returns copies sorted newest first (episodes without a date last).
func (l *Library) Episodes(feedID, format string) []podsync.Episode {
	l.mu.RLock()
	defer l.mu.RUnlock()
	fs, ok := l.feed[feedKey(feedID, format)]
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

func (l *Library) Episode(feedID, format, epID string) (podsync.Episode, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if fs, ok := l.feed[feedKey(feedID, format)]; ok {
		if ep, ok := fs.episodes[epID]; ok {
			return *ep, true
		}
	}
	return podsync.Episode{}, false
}

// PutEpisode persists the sidecar and updates the index.
func (l *Library) PutEpisode(feedID, format string, ep podsync.Episode) error {
	if ep.ID == "" || strings.ContainsAny(ep.ID, `/\`) || strings.HasPrefix(ep.ID, ".") {
		return fmt.Errorf("invalid episode id %q", ep.ID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := feedKey(feedID, format)
	fs, ok := l.feed[key]
	if !ok {
		return fmt.Errorf("unknown feed %q/%q", feedID, format)
	}
	b, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(l.feedDir(fs.source, feedID, format), ep.ID+".json"), b, 0o644); err != nil {
		return err
	}
	cp := ep
	fs.episodes[ep.ID] = &cp
	l.version.Add(1)
	return nil
}

// RemoveMedia deletes an episode's media file (not its sidecar or thumbnail).
func (l *Library) RemoveMedia(feedID, format string, ep podsync.Episode) error {
	if ep.File == "" {
		return nil
	}
	path, err := l.plainPath(feedID, format, ep.File)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// DeleteFeed removes the feed directory and its index entry.
func (l *Library) DeleteFeed(id, format string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := feedKey(id, format)
	fs, ok := l.feed[key]
	if !ok {
		return nil
	}
	if err := os.RemoveAll(l.feedDir(fs.source, id, format)); err != nil {
		return err
	}
	delete(l.feed, key)
	l.version.Add(1)
	return nil
}

// ClearFeed removes a feed's bookkeeping (sidecars and _meta.json)
// unconditionally; if deleteMedia is true, every other file (media,
// thumbnails) is removed too, and the whole directory with it. Used when a
// feed's format changes: the old (id, format) entry is actively cleared
// rather than left as a passive orphan, since the sidecars no longer
// correspond to anything the config still points at.
func (l *Library) ClearFeed(id, format string, deleteMedia bool) error {
	if deleteMedia {
		return l.DeleteFeed(id, format)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := feedKey(id, format)
	fs, ok := l.feed[key]
	if !ok {
		return nil
	}
	dir := l.feedDir(fs.source, id, format)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == metaFile || strings.HasSuffix(name, ".json") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	l.feed[key] = &feedState{source: fs.source, id: id, format: format, episodes: map[string]*podsync.Episode{}}
	l.version.Add(1)
	return nil
}

// plainPath resolves a file name inside a feed directory, refusing anything
// that is not a plain file name.
func (l *Library) plainPath(feedID, format, file string) (string, error) {
	if file == "" || file != filepath.Base(file) || strings.HasPrefix(file, ".") {
		return "", fmt.Errorf("invalid file name %q", file)
	}
	dir, ok := l.FeedDir(feedID, format)
	if !ok {
		return "", fmt.Errorf("unknown feed %q/%q", feedID, format)
	}
	return filepath.Join(dir, file), nil
}

// MediaPath returns the path of a media or thumbnail file owned by one of the
// feed's episodes. The thumbnail outlives the media: cleanup removes only the
// media file. Sidecars and unknown names are not found.
func (l *Library) MediaPath(feedID, format, file string) (string, error) {
	l.mu.RLock()
	owned := false
	if fs, ok := l.feed[feedKey(feedID, format)]; ok {
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
	path, err := l.plainPath(feedID, format, file)
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
	for _, fs := range l.feed {
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
				out = append(out, podsync.EpisodeHit{FeedID: fs.id, Format: fs.format, Episode: *ep})
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
