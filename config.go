package podsync

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Duration is a time.Duration that round-trips through TOML and JSON as "30m".
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

// String prints "30m" rather than Go's "30m0s".
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Server holds the sync service's addresses. ListenAddr is the public side:
// the podcast feeds and media files podcast apps fetch. APIAddr is the private
// control API used by the web UI. They are separate listeners on purpose, so
// publishing the feeds never exposes the (unauthenticated) control API.
type Server struct {
	ListenAddr string `toml:"listen_addr" json:"listen_addr"`
	APIAddr    string `toml:"api_addr" json:"api_addr"`
	BaseURL    string `toml:"base_url" json:"base_url"` // public address of ListenAddr, as podcast apps and browsers reach it
	DataDir    string `toml:"data_dir" json:"data_dir"`
}

type Download struct {
	YtDlpPath      string   `toml:"yt_dlp_path" json:"yt_dlp_path"`
	Concurrency    int      `toml:"concurrency" json:"concurrency"`
	UpdateInterval Duration `toml:"update_interval" json:"update_interval"`
	SelfUpdate     bool     `toml:"self_update" json:"self_update"`
	CookiesFile    string   `toml:"cookies_file" json:"cookies_file"`
}

// Options are the per-feed settings. [defaults] holds concrete values; each
// feed may override any of them (see Feed).
type Options struct {
	Format           string   `toml:"format" json:"format"` // "video" | "audio"
	MaxResolution    int      `toml:"max_resolution" json:"max_resolution"`
	MinDuration      Duration `toml:"min_duration" json:"min_duration"` // skip videos shorter than this (YouTube Shorts); 0 = no minimum
	TitleInclude     string   `toml:"title_include" json:"title_include"`
	TitleExclude     string   `toml:"title_exclude" json:"title_exclude"`
	CleanupAfterDays int      `toml:"cleanup_after_days" json:"cleanup_after_days"`
	CleanupKeepLast  int      `toml:"cleanup_keep_last" json:"cleanup_keep_last"`
	PageSize         int      `toml:"page_size" json:"page_size"`
}

// Feed is one configured channel or playlist. Option fields that are nil
// inherit from [defaults].
type Feed struct {
	ID     string `toml:"id" json:"id"`
	Source string `toml:"source,omitempty" json:"source,omitempty"`
	Kind   string `toml:"kind" json:"kind"` // "channel" | "playlist"
	Name   string `toml:"name" json:"name"`

	Format           *string   `toml:"format,omitempty" json:"format,omitempty"`
	MaxResolution    *int      `toml:"max_resolution,omitempty" json:"max_resolution,omitempty"`
	MinDuration      *Duration `toml:"min_duration,omitempty" json:"min_duration,omitempty"`
	TitleInclude     *string   `toml:"title_include,omitempty" json:"title_include,omitempty"`
	TitleExclude     *string   `toml:"title_exclude,omitempty" json:"title_exclude,omitempty"`
	CleanupAfterDays *int      `toml:"cleanup_after_days,omitempty" json:"cleanup_after_days,omitempty"`
	CleanupKeepLast  *int      `toml:"cleanup_keep_last,omitempty" json:"cleanup_keep_last,omitempty"`
	PageSize         *int      `toml:"page_size,omitempty" json:"page_size,omitempty"`
}

type Config struct {
	Server   Server   `toml:"server" json:"server"`
	Download Download `toml:"download" json:"download"`
	Defaults Options  `toml:"defaults" json:"defaults"`
	Feeds    []Feed   `toml:"feeds" json:"feeds"`
}

const (
	FormatVideo   = "video"
	FormatAudio   = "audio"
	FeedChannel   = "channel"
	FeedPlaylist  = "playlist"
	DefaultSource = "youtube"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		Server: Server{ListenAddr: ":8080", APIAddr: "127.0.0.1:8081", BaseURL: "http://localhost:8080", DataDir: "./data"},
		Download: Download{
			YtDlpPath:      "yt-dlp",
			Concurrency:    2,
			UpdateInterval: Duration(30 * time.Minute),
		},
		Defaults: Options{
			Format:          FormatVideo,
			MaxResolution:   720,
			MinDuration:     Duration(60 * time.Second),
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
	if f.MinDuration != nil {
		o.MinDuration = *f.MinDuration
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
	return f.Format != nil || f.MaxResolution != nil || f.MinDuration != nil ||
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
	if o.MinDuration < 0 {
		errs = append(errs, fmt.Errorf("%s: min_duration must be >= 0 (0 = no minimum)", where))
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
	c.Server.APIAddr = strings.TrimSpace(c.Server.APIAddr)
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
			f.Kind = FeedChannel
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
	if c.Server.APIAddr == "" {
		errs = append(errs, errors.New("server.api_addr is required"))
	} else if c.Server.APIAddr == c.Server.ListenAddr {
		errs = append(errs, errors.New("server.api_addr must differ from server.listen_addr: the control API must not share the public port"))
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
		if f.Kind != FeedChannel && f.Kind != FeedPlaylist {
			errs = append(errs, fmt.Errorf("%s: kind must be %q or %q", where, FeedChannel, FeedPlaylist))
		}
		if err := f.Effective(c.Defaults).validate(where); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
