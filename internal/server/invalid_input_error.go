package server

import (
	"errors"
	"fmt"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

// invalidInputError marks a user-input failure that a controller must
// surface as HTTP 400 rather than 500. Services wrap the cause at the
// point they reject it; controllers classify with isInvalidInput.
//
// This replaces substring matching on error text, which rotted in both
// directions: a "budget" clause outlived every producer that ever
// emitted it, and each new rejection needed a matching clause added by
// hand or it fell through to 500.
type invalidInputError struct{ err error }

func (e *invalidInputError) Error() string { return e.err.Error() }
func (e *invalidInputError) Unwrap() error { return e.err }

// invalidInputf builds an invalidInputError with fmt.Errorf semantics,
// %w included.
func invalidInputf(format string, a ...any) error {
	return &invalidInputError{err: fmt.Errorf(format, a...)}
}

// A record the access store refuses as invalid is the caller's input too.
func isInvalidInput(err error) bool {
	var v *invalidInputError
	return errors.As(err, &v) || errors.Is(err, keys.ErrInvalid) || errors.Is(err, teams.ErrInvalid)
}
