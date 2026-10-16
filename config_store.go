package podsync

// ConfigStore holds the live configuration. In production the web UI is its
// only writer (Update); the sync service only reads it (Reload).
type ConfigStore interface {
	// Get returns a snapshot. Callers must not mutate the option overrides of
	// feeds in place; replace them instead.
	Get() Config
	// Update applies fn to a copy, validates it, persists it, then swaps it in.
	// On any error the live configuration and the stored copy are untouched.
	Update(fn func(*Config) error) error
	// Reload re-reads the stored configuration if it changed on disk (the sync
	// service and the web UI share the config file). On a parse or validation
	// error the live configuration is kept and the error returned.
	Reload() (changed bool, err error)
	// OnChange registers a callback run after every change (Update or Reload).
	OnChange(fn func(old, new Config))
	// Path is where the configuration is stored ("" for in-memory stores).
	Path() string
}
