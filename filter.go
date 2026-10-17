package podsync

import (
	"fmt"
	"regexp"
	"time"
)

// EpisodeFilter decides whether an episode should be downloaded for a feed.
type EpisodeFilter struct {
	minDuration time.Duration
	include     *regexp.Regexp
	exclude     *regexp.Regexp
}

// NewEpisodeFilter compiles the title patterns (empty = off). Patterns are
// validated when the config is saved, so an error here means a hand-edited bad file.
func NewEpisodeFilter(minDuration time.Duration, include, exclude string) (*EpisodeFilter, error) {
	f := &EpisodeFilter{minDuration: minDuration}
	var err error
	if include != "" {
		if f.include, err = regexp.Compile(include); err != nil {
			return nil, fmt.Errorf("title_include is not a valid regular expression: %v", err)
		}
	}
	if exclude != "" {
		if f.exclude, err = regexp.Compile(exclude); err != nil {
			return nil, fmt.Errorf("title_exclude is not a valid regular expression: %v", err)
		}
	}
	return f, nil
}

// Filter builds the feed's episode filter: its effective minimum duration and
// its own title patterns.
func (f Feed) Filter(defaults Options) (*EpisodeFilter, error) {
	return NewEpisodeFilter(f.Effective(defaults).MinDuration.Std(), f.TitleInclude, f.TitleExclude)
}

// Allows applies the minimum duration (which is how YouTube Shorts are
// skipped), then title_include, then title_exclude. An episode whose duration
// is unknown (0) is not held back by the minimum.
func (f *EpisodeFilter) Allows(ep EpisodeInfo) bool {
	if f.minDuration > 0 && ep.DurationSec > 0 && time.Duration(ep.DurationSec)*time.Second < f.minDuration {
		return false
	}
	if f.include != nil && !f.include.MatchString(ep.Title) {
		return false
	}
	if f.exclude != nil && f.exclude.MatchString(ep.Title) {
		return false
	}
	return true
}
