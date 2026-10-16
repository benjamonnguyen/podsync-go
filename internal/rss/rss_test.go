package rss

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"podsync-go/internal/library"
)

func TestBuild(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	eps := []library.Episode{
		{ID: "old", Title: "Old & <Busy>", Status: library.StatusDownloaded, File: "old.m4a", Size: 10, Published: now.AddDate(0, 0, -5), DurationSec: 3725, Thumbnail: "http://t/old.jpg"},
		{ID: "new", Title: "New", Status: library.StatusDownloaded, File: "new one.mp4", Size: 20, Published: now.AddDate(0, 0, -1), Thumbnail: "http://t/x.jpg", ThumbFile: "new.jpg"},
		{ID: "gone", Title: "Cleaned", Status: library.StatusCleaned, Published: now},
		{ID: "bad", Title: "Failed", Status: library.StatusFailed},
	}
	out, err := Build("http://host:8080/", Feed{ID: "UC1", Title: "My Feed", ImageURL: "http://img/a.jpg", Link: "http://host:8080/feed/UC1", Self: "http://host:8080/feeds/UC1.xml"}, eps, now)
	if err != nil {
		t.Fatal(err)
	}
	var doc rssDoc
	if err := xml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not well-formed: %v\n%s", err, out)
	}
	items := doc.Channel.Items
	if len(items) != 2 || items[0].Title != "New" || items[1].Title != "Old & <Busy>" {
		t.Fatalf("items: %+v", items)
	}
	if items[0].Enclosure.URL != "http://host:8080/media/UC1/new%20one.mp4" || items[0].Enclosure.Type != "video/mp4" || items[0].Enclosure.Length != 20 {
		t.Fatalf("enclosure: %+v", items[0].Enclosure)
	}
	if items[1].Enclosure.Type != "audio/mp4" {
		t.Fatalf("item 2: %+v", items[1])
	}
	for _, want := range []string{`xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"`, `<itunes:image href="http://img/a.jpg">`, `<itunes:image href="http://host:8080/media/UC1/new.jpg">`, `<itunes:image href="http://t/old.jpg">`, `<guid isPermaLink="false">UC1:new</guid>`, `<itunes:duration>01:02:05</itunes:duration>`, "&amp; &lt;Busy&gt;"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
