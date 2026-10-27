package podsync

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestDefaults(t *testing.T) {
	d := Default().Defaults
	if d.MaxVideoHeight != 720 || d.AudioQuality != AudioQualityHigh || d.MinDuration.Std() != 61*time.Second || d.PageSize != 10 || d.CleanupKeepLast != 50 {
		t.Fatalf("%+v", d)
	}
}

func TestEffectiveLayersOverridesOverDefaults(t *testing.T) {
	d := Default().Defaults
	f := Feed{ID: "UC1", Format: ptr("audio"), CleanupKeepLast: ptr(3), MinDuration: ptr(Duration(0))}
	got := f.Effective(d)
	if got.MinDuration != 0 || d.MinDuration == 0 || got.Format != "audio" || got.CleanupKeepLast != 3 || got.PageSize != d.PageSize || got.MaxVideoHeight != d.MaxVideoHeight {
		t.Fatalf("%+v", got)
	}
	if !f.HasOverrides() || (Feed{ID: "UC2"}).HasOverrides() || !(Feed{ID: "UC3", TitleExclude: "x"}).HasOverrides() {
		t.Fatal("HasOverrides")
	}
	if !(Feed{ID: "UC4", PlaylistSort: PlaylistSortDesc}).HasOverrides() {
		t.Fatal("HasOverrides should count playlist_sort")
	}
	if !(Feed{ID: "UC5", AudioQuality: ptr(AudioQualityLow)}).HasOverrides() {
		t.Fatal("HasOverrides should count audio_quality")
	}
	if got := (Feed{ID: "UC6", AudioQuality: ptr(AudioQualityLow)}).Effective(d); got.AudioQuality != AudioQualityLow {
		t.Fatalf("audio_quality override: %+v", got)
	}
}

func validConfig() Config {
	c := Default()
	c.Feeds = []Feed{{ID: "UC1", URL: "https://www.youtube.com/channel/UC1/videos"}}
	c.Normalize()
	return c
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"bad include":       func(c *Config) { c.Feeds[0].TitleInclude = "(" },
		"bad exclude":       func(c *Config) { c.Feeds[0].TitleExclude = "(" },
		"bad format":        func(c *Config) { c.Defaults.Format = "gif" },
		"bad audio_quality": func(c *Config) { c.Defaults.AudioQuality = "medium" },
		"dup feed":          func(c *Config) { c.Feeds = append(c.Feeds, c.Feeds[0]) },
		"dup feed url":      func(c *Config) { dup := c.Feeds[0]; dup.ID = "UC-dup"; c.Feeds = append(c.Feeds, dup) },
		"dup feed id+format, different url": func(c *Config) {
			dup := c.Feeds[0]
			dup.URL = "https://www.youtube.com/channel/UC2/videos"
			c.Feeds = append(c.Feeds, dup)
		},
		"missing url":       func(c *Config) { c.Feeds[0].URL = "" },
		"bad url":           func(c *Config) { c.Feeds[0].URL = "https://example.com/not-youtube" },
		"unsafe id":         func(c *Config) { c.Feeds[0].ID = "../x" },
		"bad interval":      func(c *Config) { c.Download.UpdateInterval = Duration(5 * time.Second) },
		"zero page":         func(c *Config) { c.Defaults.PageSize = 0 },
		"neg min dur":       func(c *Config) { c.Defaults.MinDuration = -1 },
		"feed bad kind":     func(c *Config) { c.Feeds[0].Kind = "user" },
		"feed override":     func(c *Config) { c.Feeds[0].PageSize = ptr(9999) },
		"concurrency":       func(c *Config) { c.Download.Concurrency = 99 },
		"bad playlist_sort": func(c *Config) { c.Feeds[0].PlaylistSort = "newest" },
		"playlist_sort on chan": func(c *Config) {
			c.Feeds[0].Kind = FeedChannel
			c.Feeds[0].PlaylistSort = PlaylistSortDesc
		},
	}
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	withPlaylist := validConfig()
	withPlaylist.Feeds[0].Kind = FeedPlaylist
	withPlaylist.Feeds[0].PlaylistSort = PlaylistSortDesc
	if err := withPlaylist.Validate(); err != nil {
		t.Fatalf("playlist_sort=desc on a playlist feed should be valid: %v", err)
	}
	sameURLDifferentFormat := validConfig()
	sameURLDifferentFormat.Feeds = append(sameURLDifferentFormat.Feeds, Feed{
		ID: "UC1-video", URL: sameURLDifferentFormat.Feeds[0].URL, Format: ptr(FormatVideo),
	})
	sameURLDifferentFormat.Normalize()
	if err := sameURLDifferentFormat.Validate(); err != nil {
		t.Fatalf("same url with a different effective format should be valid (audio + video feeds of one channel): %v", err)
	}
	// The same id (not just the same url) may also repeat, once per format:
	// that's the point of indexing on (id, format) rather than id alone.
	sameIDDifferentFormat := validConfig()
	sameIDDifferentFormat.Feeds = append(sameIDDifferentFormat.Feeds, Feed{
		ID: sameIDDifferentFormat.Feeds[0].ID, URL: sameIDDifferentFormat.Feeds[0].URL, Format: ptr(FormatVideo),
	})
	sameIDDifferentFormat.Normalize()
	if err := sameIDDifferentFormat.Validate(); err != nil {
		t.Fatalf("same id with a different effective format should be valid: %v", err)
	}
	if f, ok := sameIDDifferentFormat.Feed(sameIDDifferentFormat.Feeds[0].ID, FormatAudio); !ok || f.Format != nil {
		t.Fatalf("Feed(id, audio) should find the default-format entry: %+v %v", f, ok)
	}
	if f, ok := sameIDDifferentFormat.Feed(sameIDDifferentFormat.Feeds[0].ID, FormatVideo); !ok || f.Format == nil || *f.Format != FormatVideo {
		t.Fatalf("Feed(id, video) should find the overridden entry: %+v %v", f, ok)
	}
	for name, mutate := range cases {
		c := validConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	c := validConfig()
	c.Feeds[0].TitleExclude = "("
	c.Defaults.PageSize = 0
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "title_exclude") || !strings.Contains(err.Error(), "page_size") {
		t.Fatalf("all problems should be listed: %v", err)
	}
}

func TestDurationRoundTripsThroughJSON(t *testing.T) {
	b, err := json.Marshal(Download{UpdateInterval: Duration(30 * time.Minute)})
	if err != nil || !strings.Contains(string(b), `"update_interval":"30m"`) {
		t.Fatalf("%s %v", b, err)
	}
	var d Download
	if err := json.Unmarshal([]byte(`{"update_interval":"2h"}`), &d); err != nil || d.UpdateInterval.Std() != 2*time.Hour {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestFeedOverridesJSONNilMeansInherit(t *testing.T) {
	b, _ := json.Marshal(Feed{ID: "UC1", PageSize: ptr(7)})
	if strings.Contains(string(b), "format") || !strings.Contains(string(b), `"page_size":7`) {
		t.Fatalf("%s", b)
	}
}

func TestErrorKindsMatchWithErrorsIs(t *testing.T) {
	wrapped := Invalid("page_size must be positive")
	if !errors.Is(wrapped, ErrInvalid) || errors.Is(wrapped, ErrNotFound) || wrapped.Error() != "page_size must be positive" {
		t.Fatalf("%v", wrapped)
	}
	if !errors.Is(Busy(), ErrBusy) || !errors.Is(NotFound("x"), ErrNotFound) {
		t.Fatal("kinds")
	}
	if !errors.Is(InvalidErr(errors.New("joined")), ErrInvalid) || InvalidErr(nil) != nil {
		t.Fatal("InvalidErr")
	}
	if got := ErrorFromKind(KindBusy, "m"); !errors.Is(got, ErrBusy) {
		t.Fatal("ErrorFromKind")
	}
	if got := ErrorFromKind("weird", "m"); errors.Is(got, ErrBusy) || got.Error() != "m" {
		t.Fatal("unknown kind becomes a plain error")
	}
}

func TestEpisodeFilter(t *testing.T) {
	d := Default().Defaults
	feed := Feed{ID: "UC1", TitleInclude: "(?i)keep|episode", TitleExclude: "^Sponsored"}
	f, err := feed.Filter(d)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ep   EpisodeInfo
		want bool
	}{
		{EpisodeInfo{Title: "Episode 1"}, true},
		{EpisodeInfo{Title: "Episode 2", DurationSec: 60}, false}, // Shorts are 60s or less
		{EpisodeInfo{Title: "Episode 3", DurationSec: 61}, true},
		{EpisodeInfo{Title: "Episode 4"}, true}, // unknown duration is not held back
		{EpisodeInfo{Title: "Sponsored: Episode"}, false},
		{EpisodeInfo{Title: "Random"}, false},
	}
	for _, c := range cases {
		if got := f.Allows(c.ep); got != c.want {
			t.Errorf("%+v: got %v want %v", c.ep, got, c.want)
		}
	}
	if _, err := (Feed{TitleInclude: "("}).Filter(d); err == nil || !strings.Contains(err.Error(), "title_include") {
		t.Fatalf("bad regex must error: %v", err)
	}
	// A feed's min_duration override wins; the title patterns are the feed's own and never come from defaults.
	nf, _ := (Feed{ID: "UC2", MinDuration: ptr(Duration(0))}).Filter(d)
	if !nf.Allows(EpisodeInfo{Title: "x", DurationSec: 5}) {
		t.Fatal("min_duration = 0 means no minimum")
	}
	plain, _ := (Feed{ID: "UC3"}).Filter(d)
	if !plain.Allows(EpisodeInfo{Title: "Sponsored: anything", DurationSec: 600}) {
		t.Fatal("without a feed pattern every title passes")
	}
}

func TestNormalizePlaylistSort(t *testing.T) {
	c := Default()
	c.Feeds = []Feed{
		{ID: "a", URL: "https://www.youtube.com/playlist?list=PLaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PlaylistSort: PlaylistSortAsc},
		{ID: "b", URL: "https://www.youtube.com/playlist?list=PLbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PlaylistSort: "oldest"},
		{ID: "c", URL: "https://www.youtube.com/playlist?list=PLcccccccccccccccccccccccccccccccc", PlaylistSort: PlaylistSortDesc},
	}
	c.Normalize()
	if got := []string{c.Feeds[0].PlaylistSort, c.Feeds[1].PlaylistSort, c.Feeds[2].PlaylistSort}; got[0] != "" || got[1] != PlaylistSortDesc || got[2] != PlaylistSortDesc {
		t.Fatalf("asc is stored as the empty default and oldest reads as desc: %q", got)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
