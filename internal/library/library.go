// Package library stores feeds and episodes on disk: media files plus small
// JSON sidecars, with an in-memory index rebuilt by scanning the data dir.
package library

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
	"time"

	"podsync-go/internal/fsutil"
)

type Status string

const (
	StatusDownloaded Status = "downloaded"
	StatusCleaned    Status = "cleaned" // media removed by cleanup; sidecar kept so it is not re-downloaded
	StatusFailed     Status = "failed"
)

const metaFile = "_meta.json"

// Episode is the sidecar record for one episode.
type Episode struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Description  string     `json:"description,omitempty"`
	URL          string     `json:"url,omitempty"`
	Thumbnail    string     `json:"thumbnail,omitempty"`
	Published    time.Time  `json:"published"`
	DurationSec  int        `json:"duration_sec,omitempty"`
	IsShort      bool       `json:"is_short,omitempty"`
	Status       Status     `json:"status"`
	File         string     `json:"file,omitempty"`
	Size         int64      `json:"size,omitempty"`
	DownloadedAt *time.Time `json:"downloaded_at,omitempty"`
	Manual       bool       `json:"manual,omitempty"`
	Attempts     int        `json:"attempts,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
}

// FeedMeta is cache-only information; losing it is harmless.
type FeedMeta struct {
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	ImageURL    string    `json:"image_url,omitempty"`
	LastPull    time.Time `json:"last_pull,omitempty"`
}

// FeedSummary describes a feed directory found on disk.
type FeedSummary struct {
	Source     string
	ID         string
	Meta       FeedMeta
	Downloaded int
	Bytes      int64
}

type feedState struct {
	source   string
	meta     FeedMeta
	episodes map[string]*Episode
}

type Library struct {
	root string
	mu   sync.RWMutex
	feed map[string]*feedState
}

func New(root string) *Library {
	return &Library{root: root, feed: map[string]*feedState{}}
}

func (l *Library) Root() string { return l.root }

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
	return nil
}

func loadFeed(dir, source string) *feedState {
	fs := &feedState{source: source, episodes: map[string]*Episode{}}
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
		var ep Episode
		if err := json.Unmarshal(b, &ep); err != nil || ep.ID == "" {
			log.Printf("library: ignoring corrupt sidecar %s: %v", filepath.Join(dir, name), err)
			continue
		}
		if ep.Status == StatusDownloaded {
			if _, err := os.Stat(filepath.Join(dir, ep.File)); err != nil {
				// Media was deleted behind our back; treat as cleaned so it is not re-downloaded.
				ep.Status, ep.File, ep.Size = StatusCleaned, "", 0
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
		l.feed[id] = &feedState{source: source, episodes: map[string]*Episode{}}
	}
	return nil
}

func (l *Library) Feeds() []FeedSummary {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]FeedSummary, 0, len(l.feed))
	for id, fs := range l.feed {
		out = append(out, summarize(id, fs))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (l *Library) Feed(id string) (FeedSummary, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	fs, ok := l.feed[id]
	if !ok {
		return FeedSummary{}, false
	}
	return summarize(id, fs), true
}

func summarize(id string, fs *feedState) FeedSummary {
	s := FeedSummary{Source: fs.source, ID: id, Meta: fs.meta}
	for _, ep := range fs.episodes {
		if ep.Status == StatusDownloaded {
			s.Downloaded++
			s.Bytes += ep.Size
		}
	}
	return s
}

// UpdateMeta mutates and persists a feed's cache metadata.
func (l *Library) UpdateMeta(id string, fn func(*FeedMeta)) error {
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
	if err := fsutil.WriteFileAtomic(filepath.Join(l.feedDir(fs.source, id), metaFile), b, 0o644); err != nil {
		return err
	}
	fs.meta = next
	return nil
}

// Episodes returns copies sorted newest first (episodes without a date last).
func (l *Library) Episodes(feedID string) []Episode {
	l.mu.RLock()
	defer l.mu.RUnlock()
	fs, ok := l.feed[feedID]
	if !ok {
		return nil
	}
	out := make([]Episode, 0, len(fs.episodes))
	for _, ep := range fs.episodes {
		out = append(out, *ep)
	}
	SortNewestFirst(out)
	return out
}

func SortNewestFirst(eps []Episode) {
	sort.Slice(eps, func(i, j int) bool {
		if !eps[i].Published.Equal(eps[j].Published) {
			return eps[i].Published.After(eps[j].Published)
		}
		return eps[i].ID < eps[j].ID
	})
}

func (l *Library) Episode(feedID, epID string) (Episode, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if fs, ok := l.feed[feedID]; ok {
		if ep, ok := fs.episodes[epID]; ok {
			return *ep, true
		}
	}
	return Episode{}, false
}

// PutEpisode persists the sidecar and updates the index.
func (l *Library) PutEpisode(feedID string, ep Episode) error {
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
	if err := fsutil.WriteFileAtomic(filepath.Join(l.feedDir(fs.source, feedID), ep.ID+".json"), b, 0o644); err != nil {
		return err
	}
	cp := ep
	fs.episodes[ep.ID] = &cp
	return nil
}

// RemoveMedia deletes an episode's media file (not its sidecar).
func (l *Library) RemoveMedia(feedID string, ep Episode) error {
	if ep.File == "" {
		return nil
	}
	path, err := l.MediaPath(feedID, ep.File)
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
	return nil
}

// MediaPath resolves a media file name inside a feed directory, refusing
// anything that is not a plain file name.
func (l *Library) MediaPath(feedID, file string) (string, error) {
	if file == "" || file != filepath.Base(file) || strings.HasPrefix(file, ".") {
		return "", fmt.Errorf("invalid file name %q", file)
	}
	dir, ok := l.FeedDir(feedID)
	if !ok {
		return "", fmt.Errorf("unknown feed %q", feedID)
	}
	return filepath.Join(dir, file), nil
}

// Hit is one search result.
type Hit struct {
	FeedID  string
	Episode Episode
}

// Search matches episode titles; every whitespace-separated word must appear
// (case-insensitive). Only episodes with media on disk are returned.
func (l *Library) Search(query string) []Hit {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []Hit
	for id, fs := range l.feed {
		for _, ep := range fs.episodes {
			if ep.Status != StatusDownloaded {
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
				out = append(out, Hit{FeedID: id, Episode: *ep})
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

// EpisodeByFile finds the downloaded episode that owns a media file name.
// Used to make sure only real media (not sidecars) is ever served.
func (l *Library) EpisodeByFile(feedID, file string) (Episode, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if fs, ok := l.feed[feedID]; ok {
		for _, ep := range fs.episodes {
			if ep.Status == StatusDownloaded && ep.File == file {
				return *ep, true
			}
		}
	}
	return Episode{}, false
}
