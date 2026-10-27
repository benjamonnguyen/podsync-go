// Package toml persists the podsync configuration as a TOML file. It
// implements podsync.ConfigStore.
package toml

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"

	btoml "github.com/BurntSushi/toml"

	"podsync"
	"podsync/fs"
)

var _ podsync.ConfigStore = (*Store)(nil)

// Parse decodes TOML over the defaults, rejecting unknown keys (which are
// almost always typos or settings from an older version).
func Parse(data []byte) (podsync.Config, error) {
	cfg := podsync.Default()
	md, err := btoml.Decode(string(data), &cfg)
	if err != nil {
		return podsync.Config{}, fmt.Errorf("parse config: %w", err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return podsync.Config{}, fmt.Errorf("unknown config keys: %s", strings.Join(keys, ", "))
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return podsync.Config{}, fmt.Errorf("invalid config:\n%w", err)
	}
	return cfg, nil
}

const fileHeader = "# podsync configuration.\n# This file is rewritten by the web UI when you change settings or feeds;\n# comments you add by hand will not be preserved.\n\n"

// Marshal encodes the config as TOML with a header comment.
func Marshal(c podsync.Config) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(fileHeader)
	enc := btoml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Store holds the live configuration. The web UI writes the file (Update); the
// sync service reads it (Reload). Both may run at once on one file.
type Store struct {
	path string

	mu   sync.RWMutex
	cfg  podsync.Config
	raw  []byte // file contents the live config was built from
	subs []func(old, new podsync.Config)
}

// Open loads path, creating it with defaults when it does not exist.
func Open(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := podsync.Default()
		cfg.Normalize()
		out, merr := Marshal(cfg)
		if merr != nil {
			return nil, merr
		}
		if werr := fs.WriteFileAtomic(path, out, 0o644); werr != nil {
			return nil, fmt.Errorf("create default config at %s: %w", path, werr)
		}
		return &Store{path: path, cfg: cfg, raw: out}, nil
	}
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Store{path: path, cfg: cfg, raw: data}, nil
}

// NewMemoryStore returns a store that never touches disk (used by tests).
func NewMemoryStore(cfg podsync.Config) *Store { return &Store{cfg: cfg} }

func (s *Store) Path() string { return s.path }

func clone(c podsync.Config) podsync.Config {
	c.Feeds = append([]podsync.Feed(nil), c.Feeds...)
	return c
}

func (s *Store) Get() podsync.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cfg)
}

func (s *Store) OnChange(fn func(old, new podsync.Config)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

func (s *Store) Update(fn func(*podsync.Config) error) error {
	s.mu.Lock()
	base := s.cfg
	if s.path != "" {
		// Build on the file as it is now, not on the last copy we read: a valid
		// hand edit since the last Reload would otherwise be overwritten. A
		// broken one is replaced, as before, so a UI save can repair the file.
		if data, err := os.ReadFile(s.path); err == nil && !bytes.Equal(data, s.raw) {
			if cfg, err := Parse(data); err == nil {
				base = cfg
			}
		}
	}
	next := clone(base)
	if err := fn(&next); err != nil {
		s.mu.Unlock()
		return err
	}
	next.Normalize()
	if err := next.Validate(); err != nil {
		s.mu.Unlock()
		return podsync.InvalidErr(err)
	}
	if s.path != "" {
		out, err := Marshal(next)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if err := fs.WriteFileAtomic(s.path, out, 0o644); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("save config: %w", err)
		}
	}
	old := s.cfg
	s.cfg = next
	if s.path != "" {
		s.raw, _ = Marshal(next)
	}
	subs := append([]func(old, new podsync.Config){}, s.subs...)
	s.mu.Unlock()
	for _, fn := range subs {
		fn(clone(old), clone(next))
	}
	return nil
}

// Reload re-reads the file if its contents changed since the live config was
// built (by someone else; our own Update is already reflected). A file that
// no longer parses or validates is an error and the live config is kept.
func (s *Store) Reload() (bool, error) {
	if s.path == "" {
		return false, nil
	}
	// Read under the lock: a copy read just before a concurrent Update wrote
	// the file is stale, and swapping it in would drop that Update's change.
	s.mu.Lock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		s.mu.Unlock()
		return false, err
	}
	if bytes.Equal(data, s.raw) {
		s.mu.Unlock()
		return false, nil
	}
	cfg, err := Parse(data)
	if err != nil {
		s.mu.Unlock()
		return false, fmt.Errorf("%s: %w", s.path, err)
	}
	old := s.cfg
	s.cfg, s.raw = cfg, data
	changed := !reflect.DeepEqual(old, cfg)
	subs := append([]func(old, new podsync.Config){}, s.subs...)
	s.mu.Unlock()
	if changed {
		for _, fn := range subs {
			fn(clone(old), clone(cfg))
		}
	}
	return changed, nil
}
