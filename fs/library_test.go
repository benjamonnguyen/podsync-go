package fs

import (
	"errors"
	"os"
	"path/filepath"
	"podsync"
	"testing"
	"time"
)

func newLib(t *testing.T) *Library {
	t.Helper()
	l := New(t.TempDir())
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPutLoadRoundTripAndMissingMedia(t *testing.T) {
	l := newLib(t)
	if err := l.EnsureFeed("youtube", "UC1"); err != nil {
		t.Fatal(err)
	}
	dir, _ := l.FeedDir("UC1")
	os.WriteFile(filepath.Join(dir, "a.mp4"), []byte("12345"), 0o644)
	now := time.Now().UTC().Truncate(time.Second)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(l.PutEpisode("UC1", podsync.Episode{ID: "a", Title: "Alpha Video", Status: podsync.StatusDownloaded, File: "a.mp4", Size: 5, Published: now}))
	must(l.PutEpisode("UC1", podsync.Episode{ID: "b", Title: "Beta", Status: podsync.StatusDownloaded, File: "b.mp4", Size: 9, Published: now.Add(-time.Hour)}))
	must(l.UpdateMeta("UC1", func(m *podsync.FeedMeta) { m.Name = "Chan" }))

	l2 := New(l.Root())
	must(l2.Load())
	f, ok := l2.Feed("UC1")
	if !ok || f.Meta.Name != "Chan" || f.Downloaded != 1 || f.Bytes != 5 {
		t.Fatalf("feed after reload: %+v ok=%v", f, ok)
	}
	b, _ := l2.Episode("UC1", "b")
	if b.Status != podsync.StatusCleaned {
		t.Fatalf("episode with missing media should load as cleaned, got %q", b.Status)
	}
	eps := l2.Episodes("UC1")
	if len(eps) != 2 || eps[0].ID != "a" {
		t.Fatalf("order: %+v", eps)
	}
}

func TestSearchRequiresAllWordsAndDownloadedMedia(t *testing.T) {
	l := newLib(t)
	l.EnsureFeed("youtube", "UC1")
	l.PutEpisode("UC1", podsync.Episode{ID: "a", Title: "Rust vs Go benchmark", Status: podsync.StatusDownloaded, File: "a.mp4"})
	l.PutEpisode("UC1", podsync.Episode{ID: "b", Title: "Go tutorial", Status: podsync.StatusCleaned})
	if got := l.Search("go BENCH"); len(got) != 1 || got[0].Episode.ID != "a" {
		t.Fatalf("got %+v", got)
	}
	if got := l.Search("go"); len(got) != 1 {
		t.Fatalf("cleaned episodes must not match: %+v", got)
	}
	if got := l.Search("  "); got != nil {
		t.Fatal("blank query should match nothing")
	}
}

func TestMediaPathOnlyServesFilesOwnedByEpisodes(t *testing.T) {
	l := newLib(t)
	l.EnsureFeed("youtube", "UC1")
	l.PutEpisode("UC1", podsync.Episode{ID: "a", Status: podsync.StatusDownloaded, File: "a.mp4", ThumbFile: "a.jpg"})
	l.PutEpisode("UC1", podsync.Episode{ID: "b", Status: podsync.StatusCleaned, File: "b.mp4", ThumbFile: "b.jpg"})
	for _, ok := range []string{"a.mp4", "a.jpg", "b.jpg"} {
		if _, err := l.MediaPath("UC1", ok); err != nil {
			t.Errorf("%q should be served: %v", ok, err)
		}
	}
	for _, bad := range []string{"b.mp4", "a.json", "_meta.json", "../x", "a/b", "", ".hidden", "/etc/passwd", "unknown.jpg"} {
		_, err := l.MediaPath("UC1", bad)
		if !errors.Is(err, podsync.ErrNotFound) {
			t.Errorf("%q should be not found, got %v", bad, err)
		}
	}
	if _, err := l.MediaPath("UCnope", "a.mp4"); !errors.Is(err, podsync.ErrNotFound) {
		t.Error("unknown feed should be not found")
	}
	if err := l.PutEpisode("UC1", podsync.Episode{ID: "../evil"}); err == nil {
		t.Fatal("episode id with separator should be rejected")
	}
}

func TestDeleteFeed(t *testing.T) {
	l := newLib(t)
	l.EnsureFeed("youtube", "UC1")
	dir, _ := l.FeedDir("UC1")
	if err := l.DeleteFeed("UC1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("dir should be gone")
	}
	if _, ok := l.Feed("UC1"); ok {
		t.Fatal("index entry should be gone")
	}
}

// Two Library values over one directory model the sync service and the web UI,
// which share the data directory.
func TestRefreshSeesChangesFromAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	writer, reader := New(dir), New(dir)
	for _, l := range []*Library{writer, reader} {
		if err := l.Load(); err != nil {
			t.Fatal(err)
		}
	}
	v0 := reader.Version()
	if changed, err := reader.Refresh(); err != nil || changed || reader.Version() != v0 {
		t.Fatalf("nothing changed yet: %v %v", changed, err)
	}

	writer.EnsureFeed("youtube", "UC1")
	writer.UpdateMeta("UC1", func(m *podsync.FeedMeta) { m.Name = "Chan" })
	writer.PutEpisode("UC1", podsync.Episode{ID: "a", Title: "A", Status: podsync.StatusFailed})
	if changed, _ := reader.Refresh(); !changed {
		t.Fatal("new feed and episode should be noticed")
	}
	if f, ok := reader.Feed("UC1"); !ok || f.Meta.Name != "Chan" {
		t.Fatalf("%+v %v", f, ok)
	}
	if ep, ok := reader.Episode("UC1", "a"); !ok || ep.Status != podsync.StatusFailed {
		t.Fatalf("%+v %v", ep, ok)
	}
	if reader.Version() == v0 {
		t.Fatal("version must change when Refresh finds changes")
	}
	if changed, _ := reader.Refresh(); changed {
		t.Fatal("a second Refresh with no changes must report none, even inside the settle window")
	}

	// An update to an existing episode is noticed too.
	dirPath, _ := writer.FeedDir("UC1")
	os.WriteFile(filepath.Join(dirPath, "a.mp4"), []byte("x"), 0o644)
	writer.PutEpisode("UC1", podsync.Episode{ID: "a", Title: "A", Status: podsync.StatusDownloaded, File: "a.mp4", Size: 1})
	if changed, _ := reader.Refresh(); !changed {
		t.Fatal("episode update should be noticed")
	}
	if ep, _ := reader.Episode("UC1", "a"); ep.Status != podsync.StatusDownloaded {
		t.Fatalf("%+v", ep)
	}

	// A feed deleted by the other side disappears.
	if err := writer.DeleteFeed("UC1"); err != nil {
		t.Fatal(err)
	}
	if changed, _ := reader.Refresh(); !changed {
		t.Fatal("deleted feed should be noticed")
	}
	if _, ok := reader.Feed("UC1"); ok {
		t.Fatal("feed should be gone from the reader's index")
	}
}

func TestLoadLeavesStagingAloneButCleanTempRemovesIt(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	staged := filepath.Join(l.TempDir(), "UC1", "vid")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatal("Load runs in the web UI too and must not delete the sync service's in-flight downloads")
	}
	if err := l.CleanTemp(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.TempDir()); !os.IsNotExist(err) {
		t.Fatal("CleanTemp should remove the staging dir")
	}
}
