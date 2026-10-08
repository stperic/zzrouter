package control

import "errors"

// Authentication sentinel errors. Callers discriminate failure modes with
// errors.Is rather than substring-matching on err.Error(). The authenticator
// wraps ErrKeyExpired and ErrKeySuspended with the offending key id for logs;
// the wrapped error still satisfies errors.Is.
var (
	ErrEmptyKey     = errors.New("empty API key")
	ErrInvalidKey   = errors.New("invalid API key")
	ErrKeyExpired   = errors.New("API key has expired")
	ErrKeySuspended = errors.New("API key is suspended")
)
