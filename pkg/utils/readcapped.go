package utils

import (
	"errors"
	"fmt"
	"io"
)

// ErrResponseTooLarge is returned by ReadCapped when the reader yields
// more than the caller's max. Callers typically propagate this as a
// 502 at the dispatch layer — the upstream node exceeded a safety
// bound and the response cannot be trusted as complete.
var ErrResponseTooLarge = errors.New("response exceeds size limit")

// ReadCapped reads up to max bytes from r. Returns ErrResponseTooLarge
// (wrapped with the observed length) if r contains more data than max,
// rather than silently truncating like io.ReadAll(io.LimitReader(r,
// max)) does.
//
// The check uses a (max+1) limited read: if the result has max+1
// bytes, we know at least max+1 bytes were available and the response
// is oversized. This is the same pattern sync_client.go uses for
// bounded file downloads.
//
// Zero or negative max is a programming error — returns an error
// immediately rather than succeeding with an empty slice (which would
// mask the mistake).
func ReadCapped(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		return nil, fmt.Errorf("utils.ReadCapped: max must be > 0, got %d", max)
	}
	buf, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > max {
		return nil, fmt.Errorf("%w: read > %d bytes", ErrResponseTooLarge, max)
	}
	return buf, nil
}
