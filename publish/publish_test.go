package publish_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"podsync"
	"podsync/fs"
	"podsync/publish"
	"podsync/toml"
)

func setup(t *testing.T) (*httptest.Server, *fs.Library) {
	t.Helper()
	c := podsync.Default()
	c.Feeds = []podsync.Feed{{ID: "UCone", Kind: "channel", Source: "youtube"}}
	c.Normalize()
	lib := fs.New(t.TempDir())
	if err := lib.Load(); err != nil {
		t.Fatal(err)
	}
	lib.EnsureFeed("youtube", "UCone", "audio")
	lib.UpdateMeta("UCone", "audio", func(m *podsync.FeedMeta) {
		m.Name, m.Description, m.ImageURL = "Channel One", "About it", "https://img/a.jpg"
	})
	dir, _ := lib.FeedDir("UCone", "audio")
	os.WriteFile(filepath.Join(dir, "ep1.mp4"), []byte("0123456789"), 0o644)
	os.WriteFile(filepath.Join(dir, "ep1.jpg"), []byte("JPEGDATA"), 0o644)
	os.WriteFile(filepath.Join(dir, "ep2.jpg"), []byte("JPEG2"), 0o644)
	pub := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	lib.PutEpisode("UCone", "audio", podsync.Episode{ID: "ep1", Title: "One", Status: podsync.StatusDownloaded, File: "ep1.mp4", ThumbFile: "ep1.jpg", Size: 10, Published: pub, DurationSec: 75})
	lib.PutEpisode("UCone", "audio", podsync.Episode{ID: "ep2", Title: "Two (cleaned)", Status: podsync.StatusCleaned, ThumbFile: "ep2.jpg", Published: pub.Add(-time.Hour)})
	lib.PutEpisode("UCone", "audio", podsync.Episode{ID: "ep3", Title: "Three (failed)", Status: podsync.StatusFailed})
	srv := httptest.NewServer(publish.NewServer(toml.NewMemoryStore(c), lib, "http://pod.example:8080").Handler())
	t.Cleanup(srv.Close)
	return srv, lib
}

func get(t *testing.T, url string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestRSSListsOnlyDownloadedEpisodes(t *testing.T) {
	srv, _ := setup(t)
	resp, body := get(t, srv.URL+"/feeds/audio/UCone.xml")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/rss+xml") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	for _, want := range []string{
		"<title>Channel One</title>", "<description>About it</description>", `<itunes:image href="https://img/a.jpg">`,
		`<enclosure url="http://pod.example:8080/media/UCone/audio/ep1.mp4" length="10" type="video/mp4">`,
		`<itunes:image href="http://pod.example:8080/media/UCone/audio/ep1.jpg">`, "<itunes:duration>00:01:15</itunes:duration>",
		`href="http://pod.example:8080/feeds/audio/UCone.xml"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "cleaned") || strings.Contains(body, "failed") {
		t.Fatal("only downloaded episodes belong in the feed")
	}
}

func TestRSSFormatInURLMustMatchTheFeedsEffectiveFormat(t *testing.T) {
	srv, _ := setup(t)
	if resp, _ := get(t, srv.URL+"/feeds/video/UCone.xml"); resp.StatusCode != 404 {
		t.Fatalf("UCone is audio; /feeds/video/UCone.xml should 404, got %d", resp.StatusCode)
	}
}

func TestMediaThumbnailsAndRanges(t *testing.T) {
	srv, _ := setup(t)
	resp, body := get(t, srv.URL+"/media/UCone/audio/ep1.mp4")
	if resp.StatusCode != 200 || body != "0123456789" || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("%d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, body := get(t, srv.URL+"/media/UCone/audio/ep1.mp4", "Range", "bytes=2-4"); resp.StatusCode != 206 || body != "234" {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}
	if resp, body := get(t, srv.URL+"/media/UCone/audio/ep1.jpg"); resp.StatusCode != 200 || body != "JPEGDATA" || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	// A cleaned episode keeps its thumbnail.
	if resp, _ := get(t, srv.URL+"/media/UCone/audio/ep2.jpg"); resp.StatusCode != 200 {
		t.Fatalf("cleaned episode's thumbnail: %d", resp.StatusCode)
	}
	// Right id, wrong format: the media belongs to the audio feed, not video.
	if resp, _ := get(t, srv.URL+"/media/UCone/video/ep1.mp4"); resp.StatusCode != 404 {
		t.Fatalf("wrong format should 404, got %d", resp.StatusCode)
	}
}

func TestOPML(t *testing.T) {
	c := podsync.Default()
	c.Feeds = []podsync.Feed{
		{ID: "UCone", Kind: "channel", Source: "youtube"},
		{ID: "UCtwo", Kind: "channel", Source: "youtube", OPMLExclude: true},
	}
	c.Normalize()
	lib := fs.New(t.TempDir())
	if err := lib.Load(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(publish.NewServer(toml.NewMemoryStore(c), lib, "http://pod.example:8080").Handler())
	t.Cleanup(srv.Close)

	resp, body := get(t, srv.URL+"/opml.xml")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/x-opml") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(body, `xmlUrl="http://pod.example:8080/feeds/audio/UCone.xml"`) {
		t.Errorf("missing included feed in\n%s", body)
	}
	if strings.Contains(body, "UCtwo") {
		t.Errorf("excluded feed should not appear in\n%s", body)
	}
}

func TestNothingElseIsPublished(t *testing.T) {
	srv, _ := setup(t)
	for _, bad := range []string{
		"/media/UCone/audio/_meta.json", "/media/UCone/audio/ep1.json", "/media/UCone/audio/..%2F..%2Fetc%2Fpasswd", "/media/UCnope/audio/ep1.mp4",
		"/media/UCone/audio/ep2.mp4", "/media/UCone/audio/unknown.jpg", "/feeds/audio/UCnope.xml", "/feeds/audio/UCone", "/feeds/audio/UCone.txt", "/feeds/UCone.xml",
		"/api/v1/status", "/api/v1/apply", "/feed/UCone", "/",
	} {
		if resp, _ := get(t, srv.URL+bad); resp.StatusCode != 404 {
			t.Errorf("%s -> %d, want 404", bad, resp.StatusCode)
		}
	}
	if resp, _ := get(t, srv.URL+"/healthz"); resp.StatusCode != 200 {
		t.Fatal("healthz")
	}
	// Feeds that are only on disk (not configured) are not published.
	_, lib := setup(t)
	lib.EnsureFeed("youtube", "UCorphan", "audio")
	srv2 := httptest.NewServer(publish.NewServer(toml.NewMemoryStore(podsync.Default()), lib, "http://pod.example:8080").Handler())
	defer srv2.Close()
	if resp, _ := get(t, srv2.URL+"/feeds/audio/UCorphan.xml"); resp.StatusCode != 404 {
		t.Fatal("orphaned folders must not be published")
	}
}
