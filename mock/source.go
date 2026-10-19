// Package mock provides test doubles for podsync interfaces.
package mock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"podsync"
)

var _ podsync.Source = (*Source)(nil)
var _ podsync.SourceUpdater = (*Source)(nil)

// Source is a controllable in-memory podsync.Source. Fields are guarded by Mu;
// use the helper methods or lock Mu when changing them while a test is running.
type Source struct {
	Mu sync.Mutex

	Episodes     []podsync.EpisodeInfo // newest first
	ListErr      error
	FindErr      error
	FindResults  []podsync.FeedInfo // returned by Find when FindFn is nil
	FindFn       func(query string) ([]podsync.FeedInfo, error)
	ResolveFn    func(ref string) (podsync.FeedInfo, error)
	DownloadErr  map[string]error
	Gate         chan struct{} // when non-nil, Download blocks until it is closed
	HealthResult podsync.Health
	UpdateErr    error
	Published    time.Time // base publish date for downloaded episodes (newest = Published)

	Downloads  map[string]int
	Active     int
	MaxActive  int
	UpdateRuns int
	ListCalls  int // number of ListEpisodes calls, for asserting how many pages a test fetched
}

func NewSource() *Source {
	return &Source{
		Downloads:   map[string]int{},
		DownloadErr: map[string]error{},
		Published:   time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Episodes generates n episodes ep00, ep01, ... newest first.
func Episodes(n int) []podsync.EpisodeInfo {
	var out []podsync.EpisodeInfo
	for i := 0; i < n; i++ {
		out = append(out, podsync.EpisodeInfo{ID: fmt.Sprintf("ep%02d", i), Title: fmt.Sprintf("Episode %d", i), DurationSec: 600})
	}
	return out
}

func (s *Source) Kind() string { return "youtube" }

func (s *Source) Check(context.Context) podsync.Health {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.HealthResult
}

func (s *Source) Resolve(_ context.Context, ref string) (podsync.FeedInfo, error) {
	s.Mu.Lock()
	fn := s.ResolveFn
	s.Mu.Unlock()
	if fn != nil {
		return fn(ref)
	}
	return podsync.FeedInfo{Source: "youtube", Kind: "channel", ID: ref, Name: "Fake", ImageURL: "http://img/x.jpg"}, nil
}

func (s *Source) Find(_ context.Context, query string) ([]podsync.FeedInfo, error) {
	s.Mu.Lock()
	fn, res, err := s.FindFn, s.FindResults, s.FindErr
	s.Mu.Unlock()
	if fn != nil {
		return fn(query)
	}
	return res, err
}

func (s *Source) ListEpisodes(_ context.Context, _ podsync.FeedInfo, offset, limit int) ([]podsync.EpisodeInfo, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.ListCalls++
	if s.ListErr != nil {
		return nil, s.ListErr
	}
	if offset >= len(s.Episodes) {
		return nil, nil
	}
	end := min(offset+limit, len(s.Episodes))
	return append([]podsync.EpisodeInfo(nil), s.Episodes[offset:end]...), nil
}

func (s *Source) Download(ctx context.Context, ep podsync.EpisodeInfo, _ podsync.DownloadOptions, dest string) (podsync.DownloadResult, error) {
	s.Mu.Lock()
	s.Active++
	if s.Active > s.MaxActive {
		s.MaxActive = s.Active
	}
	gate := s.Gate
	s.Mu.Unlock()
	defer func() { s.Mu.Lock(); s.Active--; s.Mu.Unlock() }()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return podsync.DownloadResult{}, ctx.Err()
		}
	}
	s.Mu.Lock()
	s.Downloads[ep.ID]++
	err := s.DownloadErr[ep.ID]
	idx := 0
	for i, e := range s.Episodes {
		if e.ID == ep.ID {
			idx = i
		}
	}
	published := s.Published.AddDate(0, 0, -idx)
	s.Mu.Unlock()
	if err != nil {
		return podsync.DownloadResult{}, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return podsync.DownloadResult{}, err
	}
	if err := os.WriteFile(filepath.Join(dest, ep.ID+".mp4"), []byte("0123456789"), 0o644); err != nil {
		return podsync.DownloadResult{}, err
	}
	if err := os.WriteFile(filepath.Join(dest, ep.ID+".jpg"), []byte("JPEGDATA"), 0o644); err != nil {
		return podsync.DownloadResult{}, err
	}
	return podsync.DownloadResult{File: ep.ID + ".mp4", Size: 10, Thumb: ep.ID + ".jpg", Episode: podsync.EpisodeInfo{
		ID: ep.ID, Title: "Title " + ep.ID, Published: published, DurationSec: 75, URL: "https://youtu.be/" + ep.ID,
		Description: "Line one\nLine <two>", Channel: "The Channel", ViewCount: 1234567, Height: 480,
		Thumbnail: "https://remote/" + ep.ID + ".jpg",
	}}, nil
}

func (s *Source) SelfUpdate(context.Context) (string, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.UpdateRuns++
	return "updated", s.UpdateErr
}

// DownloadCount reports how many times an episode was downloaded.
func (s *Source) DownloadCount(id string) int {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.Downloads[id]
}

// With runs fn while holding the lock, for mutating fields mid-test.
func (s *Source) With(fn func(*Source)) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	fn(s)
}
