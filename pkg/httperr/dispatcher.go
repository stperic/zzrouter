package httperr

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// responderKey keys the attached Responder on the request context.
// Unexported so callers go through AttachResponder and FromContext.
type responderKey struct{}

// PathDispatcher selects a Responder based on the request path prefix.
//
// It is used by cross-cutting handlers (gin NoRoute, NoMethod, panic
// recovery) that run outside of any route group and therefore cannot
// read the active Responder from the gin context. Group-attached
// middleware and handlers should prefer FromContext — the dispatcher is
// the fallback for code paths that have no group in scope.
//
// Prefix matching is longest-match-wins: a rule registered for
// "/zzrouter/v1/" takes precedence over a rule registered for "/" when
// both match the request path. The zero value is not usable; construct
// a dispatcher with NewPathDispatcher.
//
// PathDispatcher is immutable after construction in the steady state.
// Register is intended to be called only during server bootstrap. Once
// the server is serving requests, For is the only safe method to call.
type PathDispatcher struct {
	rules    []pathRule
	fallback Responder
}

// pathRule is a single prefix → responder binding, kept unexported so
// the dispatcher owns invariants about the rule set (sort order, no
// duplicates, stable iteration).
type pathRule struct {
	prefix    string
	responder Responder
}

// NewPathDispatcher constructs a PathDispatcher with the given fallback
// responder. The fallback is used whenever For is called with a path
// that does not match any registered prefix — including when the
// dispatcher has no rules at all. Passing a nil fallback is a
// programming error; callers should supply a Problem Details responder
// or the dialect that best matches the server's management surface.
func NewPathDispatcher(fallback Responder) *PathDispatcher {
	if fallback == nil {
		panic("httperr: NewPathDispatcher requires a non-nil fallback Responder")
	}
	return &PathDispatcher{fallback: fallback}
}

// Register binds the given path prefix to the given Responder.
//
// Prefixes SHOULD include a trailing slash so "/v1" does not match an
// unrelated path like "/v1-beta"; the dispatcher does not enforce this
// but longest-match ordering relies on the prefix reflecting the true
// group boundary. Registering the same prefix twice replaces the
// earlier rule.
func (d *PathDispatcher) Register(prefix string, r Responder) {
	if r == nil {
		panic("httperr: PathDispatcher.Register requires a non-nil Responder")
	}
	// Replace any existing rule with the same prefix so Register is
	// idempotent across bootstrap retries.
	for i := range d.rules {
		if d.rules[i].prefix == prefix {
			d.rules[i].responder = r
			return
		}
	}
	d.rules = append(d.rules, pathRule{prefix: prefix, responder: r})
	sort.SliceStable(d.rules, func(i, j int) bool {
		return len(d.rules[i].prefix) > len(d.rules[j].prefix)
	})
}

// For returns the Responder bound to the longest prefix of `path`, or
// the fallback Responder when no rule matches. Never returns nil.
func (d *PathDispatcher) For(path string) Responder {
	for _, r := range d.rules {
		if strings.HasPrefix(path, r.prefix) {
			return r.responder
		}
	}
	return d.fallback
}

// Fallback returns the dispatcher's fallback Responder. Useful for
// bootstrap code that needs to emit an error before any routing has
// happened (e.g. server-startup failures surfaced over HTTP).
func (d *PathDispatcher) Fallback() Responder {
	return d.fallback
}

// FromContext returns the Responder attached to `c` by
// AttachResponder, or nil if no responder has been attached. Group
// handlers and group middleware should prefer this over a direct
// PathDispatcher lookup because the context value reflects the route
// group that matched, not a path-prefix guess.
func FromContext(c *gin.Context) Responder {
	if c == nil {
		return nil
	}
	return FromRequest(c.Request)
}

// FromRequest is FromContext for code that holds only the request, as
// the proxy and wire layers do. The responder rides on the request
// context, so it survives into every request derived from it.
func FromRequest(req *http.Request) Responder {
	if req == nil {
		return nil
	}
	r, _ := req.Context().Value(responderKey{}).(Responder)
	return r
}

// FromRequestOr is FromRequest with a fallback for a request no route
// group has claimed.
func FromRequestOr(req *http.Request, fallback Responder) Responder {
	if r := FromRequest(req); r != nil {
		return r
	}
	return fallback
}

func withResponder(req *http.Request, r Responder) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), responderKey{}, r))
}

// FromContextOr returns FromContext(c) when a responder has been
// attached, or `fallback` otherwise. It is the canonical way for
// shared middleware to emit an error: the middleware passes its own
// default (typically the management-surface responder) and the call
// naturally upgrades to a per-group dialect when one is in scope.
func FromContextOr(c *gin.Context, fallback Responder) Responder {
	if r := FromContext(c); r != nil {
		return r
	}
	return fallback
}
