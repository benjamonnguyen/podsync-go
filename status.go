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

// StatusTracker collects operational issues so they can be shown to the user
// instead of being buried in logs.
type StatusTracker interface {
	// Report records or updates an issue (same key = same issue).
	Report(key string, level IssueLevel, message string)
	Clear(key string)
	ClearPrefix(prefix string)
	// Active returns the current issues, errors first then oldest first.
	Active() []Issue
	// SetActivity installs the function reporting live background work.
	SetActivity(fn func() Activity)
	Activity() Activity
}
