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
	if d.MaxResolution != 720 || d.MinDuration.Std() != 60*time.Second || d.PageSize != 10 || d.CleanupKeepLast != 50 {
		t.Fatalf("%+v", d)
	}
}

func TestEffectiveLayersOverridesOverDefaults(t *testing.T) {
	d := Default().Defaults
	f := Feed{ID: "UC1", Format: ptr("audio"), CleanupKeepLast: ptr(3), TitleExclude: ptr("x"), MinDuration: ptr(Duration(0))}
	got := f.Effective(d)
	if got.MinDuration != 0 || d.MinDuration == 0 || got.Format != "audio" || got.CleanupKeepLast != 3 || got.TitleExclude != "x" || got.PageSize != d.PageSize || got.MaxResolution != d.MaxResolution {
		t.Fatalf("%+v", got)
	}
	if !f.HasOverrides() || (Feed{ID: "UC2"}).HasOverrides() {
		t.Fatal("HasOverrides")
	}
}

func validConfig() Config {
	c := Default()
	c.Feeds = []Feed{{ID: "UC1", Name: "One", Kind: FeedChannel, Source: DefaultSource}}
	c.Normalize()
	return c
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"bad regex":     func(c *Config) { c.Defaults.TitleInclude = "(" },
		"bad format":    func(c *Config) { c.Defaults.Format = "gif" },
		"dup feed":      func(c *Config) { c.Feeds = append(c.Feeds, c.Feeds[0]) },
		"unsafe id":     func(c *Config) { c.Feeds[0].ID = "../x" },
		"bad interval":  func(c *Config) { c.Download.UpdateInterval = Duration(5 * time.Second) },
		"bad base url":  func(c *Config) { c.Server.BaseURL = "localhost" },
		"zero page":     func(c *Config) { c.Defaults.PageSize = 0 },
		"neg min dur":   func(c *Config) { c.Defaults.MinDuration = -1 },
		"feed bad kind": func(c *Config) { c.Feeds[0].Kind = "user" },
		"feed override": func(c *Config) { c.Feeds[0].PageSize = ptr(9999) },
		"concurrency":   func(c *Config) { c.Download.Concurrency = 99 },
		"no api addr":   func(c *Config) { c.Server.APIAddr = "" },
		"shared port":   func(c *Config) { c.Server.APIAddr = c.Server.ListenAddr },
	}
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
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
	c.Defaults.TitleExclude = "("
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
	o := Default().Defaults
	o.TitleInclude, o.TitleExclude = "(?i)keep|episode", "^Sponsored"
	f, err := NewEpisodeFilter(o)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ep   EpisodeInfo
		want bool
	}{
		{EpisodeInfo{Title: "Episode 1"}, true},
		{EpisodeInfo{Title: "Episode 2", DurationSec: 59}, false}, // under the default 60s minimum
		{EpisodeInfo{Title: "Episode 3", DurationSec: 60}, true},
		{EpisodeInfo{Title: "Episode 4"}, true}, // unknown duration is not held back
		{EpisodeInfo{Title: "Sponsored: Episode"}, false},
		{EpisodeInfo{Title: "Random"}, false},
	}
	for _, c := range cases {
		if got := f.Allows(c.ep); got != c.want {
			t.Errorf("%+v: got %v want %v", c.ep, got, c.want)
		}
	}
	if _, err := NewEpisodeFilter(Options{TitleInclude: "("}); err == nil {
		t.Fatal("bad regex must error")
	}
	none := Default().Defaults
	none.MinDuration = 0
	nf, _ := NewEpisodeFilter(none)
	if !nf.Allows(EpisodeInfo{Title: "x", DurationSec: 5}) {
		t.Fatal("min_duration = 0 means no minimum")
	}
}
