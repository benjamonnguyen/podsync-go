// Package status implements podsync.StatusTracker in memory.
package status

import (
	"sort"
	"strings"
	"sync"
	"time"

	"podsync"
)

var _ podsync.StatusTracker = (*Tracker)(nil)

type Tracker struct {
	mu        sync.Mutex
	issues    map[string]*podsync.Issue
	dismissed map[string]time.Time // key -> Last of the issue instance that was dismissed
	activity  func() podsync.Activity
	now       func() time.Time
}

func New() *Tracker {
	return &Tracker{issues: map[string]*podsync.Issue{}, dismissed: map[string]time.Time{}, now: time.Now}
}

func (t *Tracker) SetActivity(fn func() podsync.Activity) {
	t.mu.Lock()
	t.activity = fn
	t.mu.Unlock()
}

func (t *Tracker) Activity() podsync.Activity {
	t.mu.Lock()
	fn := t.activity
	t.mu.Unlock()
	if fn == nil {
		return podsync.Activity{}
	}
	return fn()
}

func (t *Tracker) Report(key string, level podsync.IssueLevel, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if is, ok := t.issues[key]; ok {
		is.Level, is.Message, is.Last = level, message, now
		is.Count++
	} else {
		t.issues[key] = &podsync.Issue{Key: key, Level: level, Message: message, Since: now, Last: now, Count: 1}
	}
	delete(t.dismissed, key)
}

func (t *Tracker) Clear(key string) {
	t.mu.Lock()
	delete(t.issues, key)
	delete(t.dismissed, key)
	t.mu.Unlock()
}

func (t *Tracker) ClearPrefix(prefix string) {
	t.mu.Lock()
	for k := range t.issues {
		if strings.HasPrefix(k, prefix) {
			delete(t.issues, k)
			delete(t.dismissed, k)
		}
	}
	t.mu.Unlock()
}

func (t *Tracker) Dismiss(key string) {
	t.mu.Lock()
	if is, ok := t.issues[key]; ok {
		t.dismissed[key] = is.Last
	}
	t.mu.Unlock()
}

func (t *Tracker) Active() []podsync.Issue {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []podsync.Issue
	for k, is := range t.issues {
		if d, ok := t.dismissed[k]; ok && !is.Last.After(d) {
			continue
		}
		out = append(out, *is)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level == podsync.LevelError
		}
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].Key < out[j].Key
	})
	return out
}
