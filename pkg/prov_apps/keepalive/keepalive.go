// Package keepalive parses the keep_alive field from inference request bodies
// (Ollama /api/* or OpenAI /v1/*) and carries it as a per-request override
// that influences zzRouter's instance lifecycle — not forwarded to the
// upstream provider.
//
// Scope and invariants:
//
//   - Overrides are localhost-only. Requests routed to a remote cluster node
//     do not carry their overrides across the RPC boundary today.
//   - Concurrent requests with conflicting overrides: last-one-wins at the
//     apply site (timer reset uses the most recent value).
//   - A nil *Override means "no override" — callers should proceed with
//     configured defaults.
package keepalive

import (
	"encoding/json"
	"fmt"
	"time"
)

// Override carries a per-request keep_alive override parsed from the
// inference request body. A nil *Override means "no override".
type Override struct {
	// Duration, when non-nil, overrides the instance's configured keep-alive
	// duration for this request. Semantics match Ollama's keep_alive field:
	//   - positive duration: reset idle timer to this value
	//   - zero: unload as soon as the idle reaper notices (not strictly
	//     "after this request" — bounded by reaper cadence)
	//   - negative: keep the instance loaded indefinitely
	Duration *time.Duration
}

// Parse extracts a keep_alive override from a JSON request body. It always
// returns a non-nil *Override. Parse is JSON-tolerant: malformed bodies or
// unknown shapes return a zero-valued Override rather than an error, because
// the main handler will reject invalid JSON later with a more appropriate
// error.
func Parse(body []byte) *Override {
	o := &Override{}
	if len(body) == 0 {
		return o
	}
	var raw struct {
		KeepAlive *keepAliveValue `json:"keep_alive"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return o
	}
	if raw.KeepAlive != nil {
		d := raw.KeepAlive.duration
		o.Duration = &d
	}
	return o
}

// keepAliveValue custom-unmarshals the keep_alive wire field, which may be
// either a duration string ("10m", "1h") or an integer number of seconds
// (300, 0, -1) per Ollama's API. It is not exported: callers see
// *time.Duration via Override.Duration.
type keepAliveValue struct {
	duration time.Duration
}

func (k *keepAliveValue) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty keep_alive")
	}
	// String form: "10m", "1h", "300ms"
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("keep_alive string: %w", err)
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("keep_alive %q: %w", s, err)
		}
		k.duration = d
		return nil
	}
	// Numeric form: integer seconds (may be negative for "indefinite"
	// per Ollama; 0 means "unload promptly").
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("keep_alive number: %w", err)
	}
	if n < 0 {
		// Any negative value collapses to a sentinel negative duration.
		// Instance code treats Duration < 0 as "indefinite".
		k.duration = -1 * time.Second
		return nil
	}
	k.duration = time.Duration(n) * time.Second
	return nil
}
