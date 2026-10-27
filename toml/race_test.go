package toml

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"podsync"
)

// A Reload that read the file just before an Update wrote it must not swap
// the stale copy in: the next Update would then write the feed back out.
func TestReloadDuringUpdateKeepsFeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = s.Reload()
				}
			}
		}()
	}
	const n = 200
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("UC%d", i)
		if err := s.Update(func(c *podsync.Config) error {
			c.Feeds = append(c.Feeds, podsync.Feed{ID: id, URL: "https://www.youtube.com/channel/" + id + "/videos"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Feeds) != n {
		t.Fatalf("file has %d feeds, want %d", len(cfg.Feeds), n)
	}
}

// An Update must build on a hand edit made since the last Reload instead of
// overwriting it with the stale in-memory copy.
func TestUpdateKeepsUnreloadedHandEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	data = append(data, "\n[[feeds]]\nid = \"hand\"\nurl = \"https://www.youtube.com/channel/UChand/videos\"\n"...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(c *podsync.Config) error {
		c.Feeds = append(c.Feeds, podsync.Feed{ID: "ui", URL: "https://www.youtube.com/channel/UCui/videos"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.Get().Feeds; len(got) != 2 {
		t.Fatalf("feeds = %+v, want the hand-edited one and the new one", got)
	}
}
