package modelregistry

import "errors"

// ErrIdentifierMixedSeparators is returned by ParseModelIdentifier when
// the input contains both '::' (version pin) and '@' (hash pin). Callers
// discriminate with errors.Is; wrapped forms include the offending input
// for logs.
var ErrIdentifierMixedSeparators = errors.New("invalid model identifier: cannot contain both '::' and '@' separators")
