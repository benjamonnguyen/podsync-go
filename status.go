package podsync

import "time"

// IssueLevel is the severity of a status issue.
type IssueLevel string

const (
	LevelWarning IssueLevel = "warning"
	LevelError   IssueLevel = "error"
)

// Issue is one currently active problem. Key identifies the failing operation
// (e.g. "list:UC123"); reporting the same key again updates it.
type Issue struct {
	Key     string     `json:"key"`
	Level   IssueLevel `json:"level"`
	Message string     `json:"message"`
	Since   time.Time  `json:"since"`
	Last    time.Time  `json:"last"`
	Count   int        `json:"count"`
}

// Activity is a point-in-time summary of background work.
type Activity struct {
	Pulling     bool `json:"pulling"`
	Downloading int  `json:"downloading"`
	Queued      int  `json:"queued"`
}

// Status is what the UI shows in its banner.
type Status struct {
	Activity
	Issues []Issue `json:"issues"`
	// Version changes whenever the library changes; clients refresh when it differs from the one they saw.
	Version  uint64            `json:"version"`
	Versions map[string]string `json:"versions,omitempty"` // tool versions, e.g. yt-dlp
}

// StatusTracker collects operational issues so they can be shown to the user
// instead of being buried in logs.
type StatusTracker interface {
	// Report records or updates an issue; a dismissed issue reappears when reported again.
	Report(key string, level IssueLevel, message string)
	Clear(key string)
	ClearPrefix(prefix string)
	// Dismiss hides an issue until it is reported again.
	Dismiss(key string)
	// Active returns non-dismissed issues, errors first then oldest first.
	Active() []Issue
	// SetActivity installs the function reporting live background work.
	SetActivity(fn func() Activity)
	Activity() Activity
}
