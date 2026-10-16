package podsync

import (
	"regexp"
	"time"
)

// EpisodeFilter decides whether an episode should be downloaded for a feed.
type EpisodeFilter struct {
	minDuration time.Duration
	include     *regexp.Regexp
	exclude     *regexp.Regexp
}

// NewEpisodeFilter compiles the feed's effective options. Patterns are validated
// when the config is saved, so an error here means a hand-edited bad file.
func NewEpisodeFilter(o Options) (*EpisodeFilter, error) {
	f := &EpisodeFilter{minDuration: o.MinDuration.Std()}
	var err error
	if o.TitleInclude != "" {
		if f.include, err = regexp.Compile(o.TitleInclude); err != nil {
			return nil, err
		}
	}
	if o.TitleExclude != "" {
		if f.exclude, err = regexp.Compile(o.TitleExclude); err != nil {
			return nil, err
		}
	}
	return f, nil
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
