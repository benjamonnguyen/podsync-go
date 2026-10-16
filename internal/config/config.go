// Package config loads, validates and saves the TOML configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"podsync-go/internal/fsutil"
)

// Duration is a time.Duration that round-trips through TOML as "30m".
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

type Server struct {
	ListenAddr string `toml:"listen_addr"`
	BaseURL    string `toml:"base_url"`
	DataDir    string `toml:"data_dir"`
}

type Download struct {
	YtDlpPath      string   `toml:"yt_dlp_path"`
	Concurrency    int      `toml:"concurrency"`
	UpdateInterval Duration `toml:"update_interval"`
	SelfUpdate     bool     `toml:"self_update"`
	CookiesFile    string   `toml:"cookies_file"`
}

// Options are the per-feed settings. [defaults] holds concrete values; each
// feed may override any of them (see Feed).
type Options struct {
	Format           string `toml:"format"` // "video" | "audio"
	MaxResolution    int    `toml:"max_resolution"`
	ExcludeShorts    bool   `toml:"exclude_shorts"`
	TitleInclude     string `toml:"title_include"`
	TitleExclude     string `toml:"title_exclude"`
	CleanupAfterDays int    `toml:"cleanup_after_days"`
	CleanupKeepLast  int    `toml:"cleanup_keep_last"`
	PageSize         int    `toml:"page_size"`
}

// Feed is one configured channel or playlist. Option fields that are nil
// inherit from [defaults].
type Feed struct {
	ID     string `toml:"id"`
	Source string `toml:"source,omitempty"`
	Kind   string `toml:"kind"` // "channel" | "playlist"
	Name   string `toml:"name"`

	Format           *string `toml:"format,omitempty"`
	MaxResolution    *int    `toml:"max_resolution,omitempty"`
	ExcludeShorts    *bool   `toml:"exclude_shorts,omitempty"`
	TitleInclude     *string `toml:"title_include,omitempty"`
	TitleExclude     *string `toml:"title_exclude,omitempty"`
	CleanupAfterDays *int    `toml:"cleanup_after_days,omitempty"`
	CleanupKeepLast  *int    `toml:"cleanup_keep_last,omitempty"`
	PageSize         *int    `toml:"page_size,omitempty"`
}

type Config struct {
	Server   Server   `toml:"server"`
	Download Download `toml:"download"`
	Defaults Options  `toml:"defaults"`
	Feeds    []Feed   `toml:"feeds"`
}

const (
	FormatVideo   = "video"
	FormatAudio   = "audio"
	KindChannel   = "channel"
	KindPlaylist  = "playlist"
	DefaultSource = "youtube"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		Server: Server{ListenAddr: ":8080", BaseURL: "http://localhost:8080", DataDir: "./data"},
		Download: Download{
			YtDlpPath:      "yt-dlp",
			Concurrency:    2,
			UpdateInterval: Duration(30 * time.Minute),
		},
		Defaults: Options{
			Format:          FormatVideo,
			MaxResolution:   1080,
			ExcludeShorts:   true,
			CleanupKeepLast: 50,
			PageSize:        10,
		},
	}
}

// Effective merges a feed's overrides over the defaults.
func (f Feed) Effective(d Options) Options {
	o := d
	if f.Format != nil {
		o.Format = *f.Format
	}
	if f.MaxResolution != nil {
		o.MaxResolution = *f.MaxResolution
	}
	if f.ExcludeShorts != nil {
		o.ExcludeShorts = *f.ExcludeShorts
	}
	if f.TitleInclude != nil {
		o.TitleInclude = *f.TitleInclude
	}
	if f.TitleExclude != nil {
		o.TitleExclude = *f.TitleExclude
	}
	if f.CleanupAfterDays != nil {
		o.CleanupAfterDays = *f.CleanupAfterDays
	}
	if f.CleanupKeepLast != nil {
		o.CleanupKeepLast = *f.CleanupKeepLast
	}
	if f.PageSize != nil {
		o.PageSize = *f.PageSize
	}
	return o
}

// HasOverrides reports whether the feed overrides any default.
func (f Feed) HasOverrides() bool {
	return f.Format != nil || f.MaxResolution != nil || f.ExcludeShorts != nil ||
		f.TitleInclude != nil || f.TitleExclude != nil || f.CleanupAfterDays != nil ||
		f.CleanupKeepLast != nil || f.PageSize != nil
}

// Feed looks a feed up by ID.
func (c Config) Feed(id string) (Feed, bool) {
	for _, f := range c.Feeds {
		if f.ID == id {
			return f, true
		}
	}
	return Feed{}, false
}

func (o Options) validate(where string) error {
	var errs []error
	if o.Format != FormatVideo && o.Format != FormatAudio {
		errs = append(errs, fmt.Errorf("%s: format must be %q or %q", where, FormatVideo, FormatAudio))
	}
	if o.MaxResolution < 0 {
		errs = append(errs, fmt.Errorf("%s: max_resolution must be >= 0 (0 = no cap)", where))
	}
	if o.CleanupAfterDays < 0 || o.CleanupKeepLast < 0 {
		errs = append(errs, fmt.Errorf("%s: cleanup values must be >= 0 (0 = disabled)", where))
	}
	if o.PageSize < 1 || o.PageSize > 500 {
		errs = append(errs, fmt.Errorf("%s: page_size must be between 1 and 500", where))
	}
	for name, pat := range map[string]string{"title_include": o.TitleInclude, "title_exclude": o.TitleExclude} {
		if pat == "" {
			continue
		}
		if _, err := regexp.Compile(pat); err != nil {
			errs = append(errs, fmt.Errorf("%s: %s is not a valid regular expression: %v", where, name, err))
		}
	}
	return errors.Join(errs...)
}

// Normalize fills derived defaults and trims whitespace. It runs before Validate.
func (c *Config) Normalize() {
	c.Server.ListenAddr = strings.TrimSpace(c.Server.ListenAddr)
	c.Server.BaseURL = strings.TrimRight(strings.TrimSpace(c.Server.BaseURL), "/")
	c.Server.DataDir = strings.TrimSpace(c.Server.DataDir)
	c.Download.YtDlpPath = strings.TrimSpace(c.Download.YtDlpPath)
	c.Download.CookiesFile = strings.TrimSpace(c.Download.CookiesFile)
	for i := range c.Feeds {
		f := &c.Feeds[i]
		f.ID = strings.TrimSpace(f.ID)
		if f.Source == "" {
			f.Source = DefaultSource
		}
		if f.Kind == "" {
			f.Kind = KindChannel
		}
		if f.Name == "" {
			f.Name = f.ID
		}
	}
}

// Validate checks the whole configuration and returns every problem found.
func (c Config) Validate() error {
	var errs []error
	if c.Server.ListenAddr == "" {
		errs = append(errs, errors.New("server.listen_addr is required"))
	}
	if u, err := url.Parse(c.Server.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("server.base_url must be an absolute http(s) URL, e.g. http://192.168.1.10:8080"))
	}
	if c.Server.DataDir == "" {
		errs = append(errs, errors.New("server.data_dir is required"))
	}
	if c.Download.YtDlpPath == "" {
		errs = append(errs, errors.New("download.yt_dlp_path is required"))
	}
	if c.Download.Concurrency < 1 || c.Download.Concurrency > 16 {
		errs = append(errs, errors.New("download.concurrency must be between 1 and 16"))
	}
	if c.Download.UpdateInterval.Std() < time.Minute {
		errs = append(errs, errors.New("download.update_interval must be at least 1m"))
	}
	if err := c.Defaults.validate("defaults"); err != nil {
		errs = append(errs, err)
	}
	seen := map[string]bool{}
	for i, f := range c.Feeds {
		where := fmt.Sprintf("feeds[%d] (%s)", i, f.ID)
		switch {
		case !idPattern.MatchString(f.ID):
			errs = append(errs, fmt.Errorf("%s: id must be 1-128 characters of A-Z a-z 0-9 _ -", where))
		case seen[f.ID]:
			errs = append(errs, fmt.Errorf("%s: duplicate feed id", where))
		}
		seen[f.ID] = true
		if !idPattern.MatchString(f.Source) {
			errs = append(errs, fmt.Errorf("%s: invalid source %q", where, f.Source))
		}
		if f.Kind != KindChannel && f.Kind != KindPlaylist {
			errs = append(errs, fmt.Errorf("%s: kind must be %q or %q", where, KindChannel, KindPlaylist))
		}
		if err := f.Effective(c.Defaults).validate(where); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Parse decodes TOML over the defaults, rejecting unknown keys (which are
// almost always typos or settings from an older version).
func Parse(data []byte) (Config, error) {
	cfg := Default()
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("unknown config keys: %s", strings.Join(keys, ", "))
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config:\n%w", err)
	}
	return cfg, nil
}

const fileHeader = "# podsync-go configuration.\n# This file is rewritten by the web UI when you change settings or feeds;\n# comments you add by hand will not be preserved.\n\n"

// Marshal encodes the config as TOML with a header comment.
func Marshal(c Config) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(fileHeader)
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Store owns the live configuration and is the only writer of the file.
type Store struct {
	path string

	mu   sync.RWMutex
	cfg  Config
	subs []func(old, new Config)
}

// Open loads path, creating it with defaults when it does not exist.
func Open(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := Default()
		cfg.Normalize()
		out, merr := Marshal(cfg)
		if merr != nil {
			return nil, merr
		}
		if werr := fsutil.WriteFileAtomic(path, out, 0o644); werr != nil {
			return nil, fmt.Errorf("create default config at %s: %w", path, werr)
		}
		return &Store{path: path, cfg: cfg}, nil
	}
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Store{path: path, cfg: cfg}, nil
}

// NewMemoryStore returns a store that never touches disk (tests).
func NewMemoryStore(cfg Config) *Store { return &Store{cfg: cfg} }

func (s *Store) Path() string { return s.path }

func clone(c Config) Config {
	c.Feeds = append([]Feed(nil), c.Feeds...)
	return c
}

// Get returns a snapshot. Callers must not mutate the pointed-to option
// overrides of feeds; replace them instead.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cfg)
}

// OnChange registers a callback run after every successful Update.
func (s *Store) OnChange(fn func(old, new Config)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

// Update applies fn to a copy, validates it, writes the file, then swaps it
// in. On any error the live configuration and the file are left untouched.
func (s *Store) Update(fn func(*Config) error) error {
	s.mu.Lock()
	next := clone(s.cfg)
	if err := fn(&next); err != nil {
		s.mu.Unlock()
		return err
	}
	next.Normalize()
	if err := next.Validate(); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.path != "" {
		out, err := Marshal(next)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if err := fsutil.WriteFileAtomic(s.path, out, 0o644); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("save config: %w", err)
		}
	}
	old := s.cfg
	s.cfg = next
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, fn := range subs {
		fn(clone(old), clone(next))
	}
	return nil
}
