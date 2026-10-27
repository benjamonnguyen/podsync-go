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
	MaxVideoHeight  int      `toml:"max_video_height" json:"max_video_height"`
	AudioQuality    string   `toml:"audio_quality" json:"audio_quality"` // "high" | "low"; only affects Format == "audio"
	MinDuration     Duration `toml:"min_duration" json:"min_duration"`   // skip videos shorter than this (YouTube Shorts); 0 = no minimum
	CleanupKeepLast int      `toml:"cleanup_keep_last" json:"cleanup_keep_last"`
	PageSize        int      `toml:"page_size" json:"page_size"`
}

// Feed is one configured channel or playlist. Option fields that are nil
// inherit from [defaults]. TitleInclude and TitleExclude (Go regular
// expressions, empty = off) belong to the feed alone; there are no global ones.
type Feed struct {
	ID  string `toml:"id" json:"id"`
	URL string `toml:"url" json:"url"`

	// Kind and Source are derived from URL by Normalize, not configured.
	Kind   string `toml:"-" json:"kind"` // "channel" | "playlist"
	Source string `toml:"-" json:"source,omitempty"`

	Format          *string   `toml:"format,omitempty" json:"format,omitempty"`
	MaxVideoHeight  *int      `toml:"max_video_height,omitempty" json:"max_video_height,omitempty"`
	AudioQuality    *string   `toml:"audio_quality,omitempty" json:"audio_quality,omitempty"`
	MinDuration     *Duration `toml:"min_duration,omitempty" json:"min_duration,omitempty"`
	CleanupKeepLast *int      `toml:"cleanup_keep_last,omitempty" json:"cleanup_keep_last,omitempty"`
	PageSize        *int      `toml:"page_size,omitempty" json:"page_size,omitempty"`

	TitleInclude string `toml:"title_include,omitempty" json:"title_include,omitempty"` // only matching titles are downloaded
	TitleExclude string `toml:"title_exclude,omitempty" json:"title_exclude,omitempty"` // matching titles are skipped

	// PlaylistSort only applies to playlist feeds. Empty (or "asc", which
	// Normalize clears) takes [offset, offset+limit) from the start of the
	// playlist, which suits YouTube's usual newest-first order. "desc" is for
	// a playlist ordered oldest first (a course/series): podsync reverses it
	// before paging, so the newest addition is still what offset 0 returns.
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

	// AudioQualityHigh and AudioQualityLow are the only values of
	// Options.AudioQuality / Feed.AudioQuality.
	AudioQualityHigh = "high"
	AudioQualityLow  = "low"

	// Feed.PlaylistSort values. Asc is the default and is stored as "".
	PlaylistSortAsc  = "asc"
	PlaylistSortDesc = "desc"
	// playlistSortOldest is the old spelling of desc, still read from files.
	playlistSortOldest = "oldest"
)

var (
	idPattern         = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	feedChannelPathRe = regexp.MustCompile(`^/(channel/[^/]+|@[^/]+|c/[^/]+|user/[^/]+)`)
	feedPlaylistIDRe  = regexp.MustCompile(`^(PL|UU|FL|OL|RD|LL)[A-Za-z0-9_-]{10,}$`)
	feedURLHosts      = map[string]bool{
		"youtube.com": true, "www.youtube.com": true, "m.youtube.com": true, "music.youtube.com": true,
	}
)

// ParseFeedURL classifies a configured feed URL as a channel or playlist
// link, the only two kinds a feed can be. It accepts the same channel and
// playlist link shapes youtube/youtube.go's Resolve does, but (deliberately,
// since this is a recurring feed, not a one-off lookup) nothing else: no bare
// IDs, no @handles, no video links.
func ParseFeedURL(raw string) (kind string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || !feedURLHosts[strings.ToLower(u.Hostname())] {
		return "", errors.New("url must be a youtube.com channel or playlist link")
	}
	q := u.Query()
	switch {
	case u.Path == "/playlist" || (u.Path == "/watch" && q.Get("list") != ""):
		if !feedPlaylistIDRe.MatchString(q.Get("list")) {
			return "", errors.New("unrecognized playlist url")
		}
		return FeedPlaylist, nil
	case feedChannelPathRe.MatchString(u.Path):
		return FeedChannel, nil
	}
	return "", errors.New("url must link to a youtube.com channel or playlist")
}

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		Download: Download{
			Concurrency:    2,
			UpdateInterval: Duration(30 * time.Minute),
		},
		Defaults: Options{
			Format:          FormatAudio,
			MaxVideoHeight:  720,
			AudioQuality:    AudioQualityHigh,
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
	if f.MaxVideoHeight != nil {
		o.MaxVideoHeight = *f.MaxVideoHeight
	}
	if f.AudioQuality != nil {
		o.AudioQuality = *f.AudioQuality
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
	return f.Format != nil || f.MaxVideoHeight != nil || f.AudioQuality != nil || f.MinDuration != nil ||
		f.TitleInclude != "" || f.TitleExclude != "" || f.CleanupKeepLast != nil || f.PageSize != nil ||
		f.PlaylistSort != ""
}

// Feed looks a feed up by its (id, format) identity: the same id may be
// configured twice, once per effective format, so id alone is not enough.
func (c Config) Feed(id, format string) (Feed, bool) {
	for _, f := range c.Feeds {
		if f.ID == id && f.Effective(c.Defaults).Format == format {
			return f, true
		}
	}
	return Feed{}, false
}

// FeedsByID returns every feed configured under id (0, 1 or 2 of them: at
// most one per format).
func (c Config) FeedsByID(id string) []Feed {
	var out []Feed
	for _, f := range c.Feeds {
		if f.ID == id {
			out = append(out, f)
		}
	}
	return out
}

// FeedByURL looks a feed up by its configured URL and effective format, used
// by the add flow to detect that a channel or playlist is already in the
// library under a different ID. Matches Validate's duplicate-url check: the
// same URL may be configured twice, once per format, so format is part of
// the lookup too.
func (c Config) FeedByURL(url, format string) (Feed, bool) {
	for _, f := range c.Feeds {
		if f.URL == url && f.Effective(c.Defaults).Format == format {
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
	if o.MaxVideoHeight < 0 {
		errs = append(errs, fmt.Errorf("%s: max_video_height must be >= 0 (0 = no cap)", where))
	}
	if o.AudioQuality != AudioQualityHigh && o.AudioQuality != AudioQualityLow {
		errs = append(errs, fmt.Errorf("%s: audio_quality must be %q or %q", where, AudioQualityHigh, AudioQualityLow))
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
		f.URL = strings.TrimSpace(f.URL)
		f.Source = DefaultSource // only one source today; derived, not configured
		if kind, err := ParseFeedURL(f.URL); err == nil {
			f.Kind = kind
		}
		switch f.PlaylistSort {
		case PlaylistSortAsc:
			f.PlaylistSort = ""
		case playlistSortOldest:
			f.PlaylistSort = PlaylistSortDesc
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
	// A feed's identity is (id, effective format), not id alone: the same id
	// may be configured twice, once per format, to get both an audio and a
	// video feed out of one channel. Keyed the same way as the duplicate-url
	// check below, for the same reason.
	seenKey := map[string]bool{}
	seenURL := map[string]bool{}
	for i, f := range c.Feeds {
		where := fmt.Sprintf("feeds[%d] (%s)", i, f.ID)
		if !idPattern.MatchString(f.ID) {
			errs = append(errs, fmt.Errorf("%s: id must be 1-128 characters of A-Z a-z 0-9 _ -", where))
		}
		key := f.ID + "\x00" + f.Effective(c.Defaults).Format
		if seenKey[key] {
			errs = append(errs, fmt.Errorf("%s: duplicate feed id for the same format (already configured)", where))
		}
		seenKey[key] = true
		if _, err := ParseFeedURL(f.URL); err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", where, err))
		} else {
			if f.Kind != FeedChannel && f.Kind != FeedPlaylist {
				errs = append(errs, fmt.Errorf("%s: kind must be %q or %q", where, FeedChannel, FeedPlaylist))
			}
			urlKey := f.URL + "\x00" + f.Effective(c.Defaults).Format
			if seenURL[urlKey] {
				errs = append(errs, fmt.Errorf("%s: duplicate feed url for the same format (already configured under another id)", where))
			}
			seenURL[urlKey] = true
		}
		if f.PlaylistSort != "" && f.PlaylistSort != PlaylistSortAsc {
			if f.PlaylistSort != PlaylistSortDesc {
				errs = append(errs, fmt.Errorf("%s: playlist_sort must be %q or %q", where, PlaylistSortAsc, PlaylistSortDesc))
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
