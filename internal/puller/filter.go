package puller

import (
	"regexp"

	"podsync-go/internal/config"
	"podsync-go/internal/source"
)

// Filter decides whether an episode should be downloaded for a feed.
type Filter struct {
	excludeShorts bool
	include       *regexp.Regexp
	exclude       *regexp.Regexp
}

// NewFilter compiles the feed's effective options. Patterns are validated
// when the config is saved, so an error here means a hand-edited bad file.
func NewFilter(o config.Options) (*Filter, error) {
	f := &Filter{excludeShorts: o.ExcludeShorts}
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

// Allows applies Shorts exclusion, then title_include, then title_exclude.
func (f *Filter) Allows(ep source.EpisodeInfo) bool {
	if f.excludeShorts && ep.IsShort {
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
