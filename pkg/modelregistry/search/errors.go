package search

import "errors"

// Validation sentinels for SearchParams and PaginationParams. Callers
// branch on these via errors.Is; the raise-site wraps carry per-call
// context (offending value, platform, etc.) for logs without changing
// identity.
var (
	ErrInvalidProvider = errors.New("invalid provider")
	ErrLimitNegative   = errors.New("limit cannot be negative")
	ErrLimitExceedsMax = errors.New("limit cannot exceed maximum")
	ErrOffsetNegative  = errors.New("offset cannot be negative")
)
