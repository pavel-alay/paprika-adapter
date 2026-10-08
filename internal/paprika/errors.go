package paprika

import (
	"errors"
	"fmt"
)

// Kind classifies a failure so callers can map it to a stable tool error code.
type Kind string

const (
	KindAuth        Kind = "auth_failed"          // credentials rejected or token refused after re-login
	KindUnavailable Kind = "upstream_unavailable" // network, timeout, 429, 5xx
	KindNotFound    Kind = "not_found"
	KindProtocol    Kind = "protocol_changed" // response did not have the shape this client was built against
	KindRejected    Kind = "rejected"         // server understood the request and refused it
	KindInvalid     Kind = "invalid_input"    // refused locally, nothing was sent
)

// Error is the only error type the client returns. It never carries request
// headers, bodies, tokens or credentials.
type Error struct {
	Kind    Kind
	Op      string // e.g. "GET /api/v2/sync/meals/"
	Status  int    // HTTP status, 0 if the request never completed
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("paprika %s: %s (%s, HTTP %d)", e.Kind, e.Message, e.Op, e.Status)
	}
	return fmt.Sprintf("paprika %s: %s (%s)", e.Kind, e.Message, e.Op)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether repeating the same call may succeed without operator action.
func (e *Error) Retryable() bool { return e.Kind == KindUnavailable }

// KindOf returns the Kind of err, or "" if err is not a client error.
func KindOf(err error) Kind {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return ""
}
