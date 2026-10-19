package podsync

import (
	"errors"
	"fmt"
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

// Download holds the feed-independent download settings a user can change in
// the web UI. Everything that a deployment sets once and never edits again
// (the listen address, the public base URL, the data directory, the yt-dlp
// cookies file, the web UI toggle and its login) is a flag/environment
// variable instead, not part of this file: see cmd/podsync's -help.
type Download struct {
	Concurrency    int      `toml:"concurrency" json:"concurrency"`
	UpdateInterval Duration `toml:"update_interval" json:"update_interval"`
	SelfUpdate     bool     `toml:"self_update" json:"self_update"`
}

// Options are the per-feed settings that have a default. [defaults] holds
// concrete values; each feed may override any of them (see Feed). Title
// filters are not here: they only make sense per feed.
type Options struct {
	Format          string   `toml:"format" json:"format"` // "video" | "audio"
	MaxResolution   int      `toml:"max_resolution" json:"max_resolution"`
	MinDuration     Duration `toml:"min_duration" json:"min_duration"` // skip videos shorter than this (YouTube Shorts); 0 = no minimum
	CleanupKeepLast int      `toml:"cleanup_keep_last" json:"cleanup_keep_last"`
	PageSize        int      `toml:"page_size" json:"page_size"`
}

// Feed is one configured channel or playlist. Option fields that are nil
// inherit from [defaults]. TitleInclude and TitleExclude (Go regular
// expressions, empty = off) belong to the feed alone; there are no global ones.
type Feed struct {
	ID     string `toml:"id" json:"id"`
	Source string `toml:"source,omitempty" json:"source,omitempty"`
	Kind   string `toml:"kind" json:"kind"` // "channel" | "playlist"
	Name   string `toml:"name" json:"name"`

	Format          *string   `toml:"format,omitempty" json:"format,omitempty"`
	MaxResolution   *int      `toml:"max_resolution,omitempty" json:"max_resolution,omitempty"`
	MinDuration     *Duration `toml:"min_duration,omitempty" json:"min_duration,omitempty"`
	CleanupKeepLast *int      `toml:"cleanup_keep_last,omitempty" json:"cleanup_keep_last,omitempty"`
	PageSize        *int      `toml:"page_size,omitempty" json:"page_size,omitempty"`

	TitleInclude string `toml:"title_include,omitempty" json:"title_include,omitempty"` // only matching titles are downloaded
	TitleExclude string `toml:"title_exclude,omitempty" json:"title_exclude,omitempty"` // matching titles are skipped

	// PlaylistSort only applies to playlist feeds. Empty assumes YouTube's own
	// playlist order is newest first, so [offset, offset+limit) is taken from
	// the start. Set to "oldest" for a playlist ordered oldest first (a
	// course/series): podsync then reverses it before paging, so the newest
	// addition is still what offset 0 returns.
	PlaylistSort string `toml:"playlist_sort,omitempty" json:"playlist_sort,omitempty"`

	// OPMLExclude leaves the feed out of the OPML export at /opml.xml. Zero
	// value (false) includes it, so existing feeds and newly added ones are
	// in the export unless a user opts them out.
	OPMLExclude bool `toml:"opml_exclude,omitempty" json:"opml_exclude,omitempty"`
}

// Config is everything the web UI reads back and rewrites: feed data and the
// per-feed-overridable defaults. It holds nothing about how the service is
// deployed (ports, the public URL, the data directory, yt-dlp or its
// cookies file); those are flags/environment variables on each binary.
type Config struct {
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

	// PlaylistSortOldest is the only non-empty value of Feed.PlaylistSort.
	PlaylistSortOldest = "oldest"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		Download: Download{
			Concurrency:    2,
			UpdateInterval: Duration(30 * time.Minute),
		},
		Defaults: Options{
			Format:          FormatAudio,
			MaxResolution:   720,
			MinDuration:     Duration(61 * time.Second), // Shorts are 60s or less
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
		f.TitleInclude != "" || f.TitleExclude != "" || f.CleanupKeepLast != nil || f.PageSize != nil ||
		f.PlaylistSort != ""
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
	if o.CleanupKeepLast < 0 {
		errs = append(errs, fmt.Errorf("%s: cleanup_keep_last must be >= 0 (0 = keep everything)", where))
	}
	if o.PageSize < 1 || o.PageSize > 500 {
		errs = append(errs, fmt.Errorf("%s: page_size must be between 1 and 500", where))
	}
	return errors.Join(errs...)
}

// Normalize fills derived defaults and trims whitespace. It runs before Validate.
func (c *Config) Normalize() {
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
		if f.PlaylistSort != "" {
			if f.PlaylistSort != PlaylistSortOldest {
				errs = append(errs, fmt.Errorf("%s: playlist_sort must be empty or %q", where, PlaylistSortOldest))
			} else if f.Kind != FeedPlaylist {
				errs = append(errs, fmt.Errorf("%s: playlist_sort only applies to playlist feeds", where))
			}
		}
		if err := f.Effective(c.Defaults).validate(where); err != nil {
			errs = append(errs, err)
		}
		if _, err := f.Filter(c.Defaults); err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", where, err))
		}
	}
	return errors.Join(errs...)
}
