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
	if cfg.Defaults.PageSize != 7 || cfg.Defaults.CleanupKeepLast != 50 || cfg.Defaults.MinDuration.Std() != 61*time.Second {
		t.Fatalf("defaults not layered correctly: %+v", cfg.Defaults)
	}
	f := cfg.Feeds[0]
	if f.Source != "youtube" || f.Kind != "channel" {
		t.Fatalf("feed defaults: %+v", f)
	}
	eff := f.Effective(cfg.Defaults)
	if eff.Format != "audio" || eff.CleanupKeepLast != 3 || eff.PageSize != 7 || eff.MaxResolution != 720 {
		t.Fatalf("effective: %+v", eff)
	}
}

func TestParseRejectsUnknownKeysAndBadValues(t *testing.T) {
	cases := map[string]string{
		"unknown key":    "[defaults]\ntitle_filter = \"x\"\n",
		"old shorts key": "[defaults]\nexclude_shorts = true\n",
		"neg min dur":    "[defaults]\nmin_duration = \"-5s\"\n",
		"global include": "[defaults]\ntitle_include = \"x\"\n",
		"global exclude": "[defaults]\ntitle_exclude = \"x\"\n",
		"old age key":    "[defaults]\ncleanup_after_days = 30\n",
		"old feed age":   "[[feeds]]\nid=\"a\"\ncleanup_after_days = 30\n",
		"bad regex":      "[[feeds]]\nid=\"a\"\ntitle_include = \"(\"\n",
		"bad format":     "[defaults]\nformat = \"gif\"\n",
		"dup feed":       "[[feeds]]\nid=\"a\"\n[[feeds]]\nid=\"a\"\n",
		"unsafe id":      "[[feeds]]\nid=\"../x\"\n",
		"bad interval":   "[download]\nupdate_interval = \"5s\"\n",
		"bad base url":   "[server]\nbase_url = \"localhost\"\n",
		"zero page":      "[defaults]\npage_size = 0\n",
		"feed bad kind":  "[[feeds]]\nid=\"a\"\nkind=\"user\"\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestMinDurationParsesAndRoundTrips(t *testing.T) {
	cfg, err := Parse([]byte("[defaults]\nmin_duration = \"90s\"\n\n[[feeds]]\nid = \"UC1\"\nmin_duration = \"0s\"\n\n[[feeds]]\nid = \"UC2\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.MinDuration.Std() != 90*time.Second || cfg.Feeds[0].MinDuration == nil || *cfg.Feeds[0].MinDuration != 0 || cfg.Feeds[1].MinDuration != nil {
		t.Fatalf("%+v %+v", cfg.Defaults.MinDuration, cfg.Feeds)
	}
	if got := cfg.Feeds[0].Effective(cfg.Defaults).MinDuration; got != 0 {
		t.Fatal("a feed can turn the minimum off")
	}
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(data)
	if err != nil || back.Feeds[0].MinDuration == nil || *back.Feeds[0].MinDuration != 0 || back.Feeds[1].MinDuration != nil || back.Defaults.MinDuration.Std() != 90*time.Second {
		t.Fatalf("round trip lost the override (an explicit zero must survive): %v\n%s", err, data)
	}
	def, _ := Parse([]byte(""))
	if def.Defaults.MinDuration.Std() != 61*time.Second || def.Defaults.MaxResolution != 720 {
		t.Fatalf("defaults: %+v", def.Defaults)
	}
}

func TestRoundTripOmitsNilOverrides(t *testing.T) {
	cfg := podsync.Default()
	cfg.Feeds = []podsync.Feed{
		{ID: "UC1", Name: "One", Kind: "channel", Source: "youtube"},
		{ID: "UC2", Name: "Two", Kind: "playlist", Source: "youtube", Format: ptr("audio"), PageSize: ptr(25), TitleExclude: "(?i)live"},
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
	if got := back.Feeds[1].Effective(back.Defaults); got.Format != "audio" || got.PageSize != 25 {
		t.Fatalf("overrides lost: %+v", got)
	}
	if back.Feeds[1].TitleExclude != "(?i)live" || back.Feeds[0].TitleExclude != "" || strings.Count(text, "title_exclude") != 1 {
		t.Fatalf("a feed's title pattern must round-trip and appear only on that feed:\n%s", text)
	}
	if strings.Contains(strings.SplitN(text, "[[feeds]]", 2)[0], "title_") {
		t.Fatalf("[defaults] has no title patterns:\n%s", text)
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

func TestReloadPicksUpExternalEditsAndKeepsConfigOnErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	web, err := Open(path) // the web UI: writes
	if err != nil {
		t.Fatal(err)
	}
	sync, err := Open(path) // the sync service: reads
	if err != nil {
		t.Fatal(err)
	}
	var notified []podsync.Config
	sync.OnChange(func(_, n podsync.Config) { notified = append(notified, n) })

	if changed, err := sync.Reload(); err != nil || changed {
		t.Fatalf("untouched file: %v %v", changed, err)
	}
	err = web.Update(func(c *podsync.Config) error {
		c.Feeds = append(c.Feeds, podsync.Feed{ID: "UC1", Name: "One"})
		c.Download.Concurrency = 3
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := web.Reload(); err != nil || changed {
		t.Fatalf("a writer's own Update must not look like an external change: %v %v", changed, err)
	}
	if changed, err := sync.Reload(); err != nil || !changed {
		t.Fatalf("external edit should be picked up: %v %v", changed, err)
	}
	if got := sync.Get(); len(got.Feeds) != 1 || got.Download.Concurrency != 3 || len(notified) != 1 {
		t.Fatalf("%+v notified=%d", got, len(notified))
	}
	if changed, _ := sync.Reload(); changed || len(notified) != 1 {
		t.Fatal("second reload is a no-op")
	}

	// Hand-edited garbage is reported and ignored.
	good, _ := os.ReadFile(path)
	os.WriteFile(path, []byte("[download]\nconcurrency = 99\n"), 0o644)
	if _, err := sync.Reload(); err == nil || !strings.Contains(err.Error(), "concurrency") {
		t.Fatalf("expected validation error, got %v", err)
	}
	if sync.Get().Download.Concurrency != 3 {
		t.Fatal("invalid file must not replace the live config")
	}
	os.WriteFile(path, []byte("not toml ["), 0o644)
	if _, err := sync.Reload(); err == nil {
		t.Fatal("expected parse error")
	}
	// Restoring the file recovers without a notification (content equals what is live).
	os.WriteFile(path, good, 0o644)
	if changed, err := sync.Reload(); err != nil || changed {
		t.Fatalf("restored: %v %v", changed, err)
	}

	if changed, err := NewMemoryStore(podsync.Default()).Reload(); changed || err != nil {
		t.Fatal("memory stores never reload")
	}
}

// The documented example must stay loadable as the config format evolves.
func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile("../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("config.example.toml no longer parses: %v", err)
	}
	if len(cfg.Feeds) == 0 {
		t.Fatalf("%+v", cfg)
	}
	dev, err := os.ReadFile("../dev/config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(dev); err != nil {
		t.Fatalf("dev/config.toml no longer parses: %v", err)
	}
}

