// Package status tracks operational issues and live activity so the web UI
// can surface them in a banner instead of burying them in logs.
package status

import (
	"sort"
	"sync"
	"time"
)

type Level string

const (
	Warning Level = "warning"
	Error   Level = "error"
)

// Issue is one currently-active problem. Key identifies the failing
// operation (e.g. "list:UC123"); reporting the same key again updates it.
type Issue struct {
	Key     string
	Level   Level
	Message string
	Since   time.Time
	Last    time.Time
	Count   int
}

// Activity is a point-in-time summary of background work.
type Activity struct {
	Pulling     bool
	Downloading int
	Queued      int
}

type Tracker struct {
	mu        sync.Mutex
	issues    map[string]*Issue
	dismissed map[string]time.Time // key -> Last of the issue instance that was dismissed
	activity  func() Activity
	now       func() time.Time
}

func New() *Tracker {
	return &Tracker{issues: map[string]*Issue{}, dismissed: map[string]time.Time{}, now: time.Now}
}

// SetActivity installs the function used to report live activity.
func (t *Tracker) SetActivity(fn func() Activity) {
	t.mu.Lock()
	t.activity = fn
	t.mu.Unlock()
}

func (t *Tracker) Activity() Activity {
	t.mu.Lock()
	fn := t.activity
	t.mu.Unlock()
	if fn == nil {
		return Activity{}
	}
	return fn()
}

// Report records (or updates) an issue. A previously dismissed issue
// reappears when it is reported again.
func (t *Tracker) Report(key string, level Level, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if is, ok := t.issues[key]; ok {
		is.Level, is.Message, is.Last = level, message, now
		is.Count++
	} else {
		t.issues[key] = &Issue{Key: key, Level: level, Message: message, Since: now, Last: now, Count: 1}
	}
	delete(t.dismissed, key)
}

// Clear removes an issue; call it when the failing operation succeeds.
func (t *Tracker) Clear(key string) {
	t.mu.Lock()
	delete(t.issues, key)
	delete(t.dismissed, key)
	t.mu.Unlock()
}

// ClearPrefix removes all issues whose key starts with prefix.
func (t *Tracker) ClearPrefix(prefix string) {
	t.mu.Lock()
	for k := range t.issues {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(t.issues, k)
			delete(t.dismissed, k)
		}
	}
	t.mu.Unlock()
}

// Dismiss hides an issue until it is reported again.
func (t *Tracker) Dismiss(key string) {
	t.mu.Lock()
	if is, ok := t.issues[key]; ok {
		t.dismissed[key] = is.Last
	}
	t.mu.Unlock()
}

// Active returns the non-dismissed issues, errors first then oldest first.
func (t *Tracker) Active() []Issue {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Issue
	for k, is := range t.issues {
		if d, ok := t.dismissed[k]; ok && !is.Last.After(d) {
			continue
		}
		out = append(out, *is)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level == Error
		}
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].Key < out[j].Key
	})
	return out
}
