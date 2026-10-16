package toml

import (
	"os"
	"path/filepath"
	"podsync"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestParseAppliesDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
[defaults]
page_size = 7

[[feeds]]
id = "UC123"
name = "Chan"
format = "audio"
cleanup_keep_last = 3
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.PageSize != 7 || cfg.Defaults.CleanupKeepLast != 50 || !cfg.Defaults.ExcludeShorts {
		t.Fatalf("defaults not layered correctly: %+v", cfg.Defaults)
	}
	f := cfg.Feeds[0]
	if f.Source != "youtube" || f.Kind != "channel" {
		t.Fatalf("feed defaults: %+v", f)
	}
	eff := f.Effective(cfg.Defaults)
	if eff.Format != "audio" || eff.CleanupKeepLast != 3 || eff.PageSize != 7 || eff.MaxResolution != 1080 {
		t.Fatalf("effective: %+v", eff)
	}
}

func TestParseRejectsUnknownKeysAndBadValues(t *testing.T) {
	cases := map[string]string{
		"unknown key":   "[defaults]\ntitle_filter = \"x\"\n",
		"bad regex":     "[defaults]\ntitle_include = \"(\"\n",
		"bad format":    "[defaults]\nformat = \"gif\"\n",
		"dup feed":      "[[feeds]]\nid=\"a\"\n[[feeds]]\nid=\"a\"\n",
		"unsafe id":     "[[feeds]]\nid=\"../x\"\n",
		"bad interval":  "[download]\nupdate_interval = \"5s\"\n",
		"bad base url":  "[server]\nbase_url = \"localhost\"\n",
		"zero page":     "[defaults]\npage_size = 0\n",
		"feed bad kind": "[[feeds]]\nid=\"a\"\nkind=\"user\"\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRoundTripOmitsNilOverrides(t *testing.T) {
	cfg := podsync.Default()
	cfg.Feeds = []podsync.Feed{
		{ID: "UC1", Name: "One", Kind: "channel", Source: "youtube"},
		{ID: "UC2", Name: "Two", Kind: "playlist", Source: "youtube", Format: ptr("audio"), PageSize: ptr(25), TitleExclude: ptr("(?i)live")},
	}
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Count(text, "page_size") != 2 { // defaults + UC2
		t.Fatalf("expected nil overrides to be omitted:\n%s", text)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatalf("reparse: %v\n%s", err, text)
	}
	if back.Download.UpdateInterval.Std() != 30*time.Minute {
		t.Fatalf("duration round trip: %v", back.Download.UpdateInterval)
	}
	if got := back.Feeds[1].Effective(back.Defaults); got.Format != "audio" || got.PageSize != 25 || got.TitleExclude != "(?i)live" {
		t.Fatalf("overrides lost: %+v", got)
	}
	if back.Feeds[0].HasOverrides() {
		t.Fatal("feed 0 should have no overrides")
	}
}

func TestStoreCreatesUpdatesAndNotifies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config not created: %v", err)
	}
	var notified int
	s.OnChange(func(old, new podsync.Config) {
		notified++
		if len(old.Feeds) != 0 || len(new.Feeds) != 1 {
			t.Errorf("unexpected change payload: %d -> %d", len(old.Feeds), len(new.Feeds))
		}
	})
	err = s.Update(func(c *podsync.Config) error {
		c.Feeds = append(c.Feeds, podsync.Feed{ID: "UC9", Name: "Nine"})
		return nil
	})
	if err != nil || notified != 1 {
		t.Fatalf("update: err=%v notified=%d", err, notified)
	}
	// An invalid update must not change memory or disk.
	before, _ := os.ReadFile(path)
	err = s.Update(func(c *podsync.Config) error { c.Defaults.PageSize = 0; return nil })
	if err == nil {
		t.Fatal("expected validation error")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || s.Get().Defaults.PageSize != 10 {
		t.Fatal("failed update leaked")
	}
	reopened, err := Open(path)
	if err != nil || len(reopened.Get().Feeds) != 1 {
		t.Fatalf("reopen: %v", err)
	}
}
