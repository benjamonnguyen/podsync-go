// Package podsync holds the domain types and interfaces shared by every part
// of the system: feeds, episodes, the Source interface that feed providers
// (YouTube) implement, the Library that stores them, the Syncer service
// contract that the REST API and web UI are built around, and so on.
//
// It has no dependencies beyond the standard library. Concrete implementations
// live in subpackages grouped by what they depend on (youtube, fs, toml,
// rest, webui, ...) and are wired together by the main packages in cmd/.
package podsync

import (
	"errors"
	"fmt"
)

// ErrorKind classifies errors that cross package and process boundaries.
type ErrorKind string

const (
	KindNotFound ErrorKind = "not_found"
	KindInvalid  ErrorKind = "invalid"
	KindBusy     ErrorKind = "busy"
)

// Error is an application error with a kind. Match with errors.Is against
// ErrNotFound, ErrInvalid or ErrBusy.
type Error struct {
	Kind ErrorKind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Is reports whether target is an *Error of the same kind.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind
}

var (
	ErrNotFound = &Error{KindNotFound, "not found"}
	ErrInvalid  = &Error{KindInvalid, "invalid"}
	ErrBusy     = &Error{KindBusy, "an update is already running"}
)

func NotFound(format string, args ...any) error {
	return &Error{KindNotFound, fmt.Sprintf(format, args...)}
}

func Invalid(format string, args ...any) error {
	return &Error{KindInvalid, fmt.Sprintf(format, args...)}
}

// InvalidErr marks an existing error (e.g. joined validation errors) as invalid input.
func InvalidErr(err error) error {
	var e *Error
	if err == nil || errors.As(err, &e) {
		return err
	}
	return &Error{KindInvalid, err.Error()}
}

func Busy() error { return &Error{KindBusy, ErrBusy.Msg} }

// ErrorFromKind rebuilds an application error received over the wire.
func ErrorFromKind(kind ErrorKind, msg string) error {
	switch kind {
	case KindNotFound, KindInvalid, KindBusy:
		return &Error{kind, msg}
	}
	return errors.New(msg)
}
