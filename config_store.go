package podsync

// ConfigStore owns the live configuration and is the only writer of it.
type ConfigStore interface {
	// Get returns a snapshot. Callers must not mutate the option overrides of
	// feeds in place; replace them instead.
	Get() Config
	// Update applies fn to a copy, validates it, persists it, then swaps it in.
	// On any error the live configuration and the stored copy are untouched.
	Update(fn func(*Config) error) error
	// OnChange registers a callback run after every successful Update.
	OnChange(fn func(old, new Config))
	// Path is where the configuration is stored ("" for in-memory stores).
	Path() string
}
