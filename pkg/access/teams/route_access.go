package teams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// RouteAccess restricts requests to exact named routes. Direct mode retains model access.
// The zero value preserves the unrestricted policy of existing teams.
type RouteAccess struct {
	Mode   string   `json:"mode"`
	Routes []string `json:"routes,omitempty"`
}

// IsZero matches omission in PATCH and preserves allocated-empty client slices.
func (p RouteAccess) IsZero() bool { return p.Mode == "" && len(p.Routes) == 0 }

// Restricted reports whether a policy narrows route access.
func (p RouteAccess) Restricted() bool { return p.Mode != "" && p.Mode != "direct" }

// Allows compares exact route keys, refusing unknown targets when restricted.
func (p RouteAccess) Allows(route string) bool {
	return !p.Restricted() || p.Mode == "selected" && route != "" && slices.Contains(p.Routes, route)
}

// Clone keeps policy snapshots independent of request and stored slices.
func (p RouteAccess) Clone() RouteAccess {
	p.Routes = slices.Clone(p.Routes)
	return p
}

// Validate rejects ambiguous policies rather than widening access.
func (p RouteAccess) Validate() error {
	switch p.Mode {
	case "", "direct", "none":
		if len(p.Routes) != 0 {
			return fmt.Errorf("route_access.routes requires selected mode")
		}
	case "selected":
		if len(p.Routes) == 0 {
			return fmt.Errorf("select at least one route, or use none")
		}
		seen := make(map[string]bool, len(p.Routes))
		for _, key := range p.Routes {
			if key == "" || strings.TrimSpace(key) != key || seen[key] {
				return fmt.Errorf("route_access.routes must contain unique nonempty route keys")
			}
			seen[key] = true
		}
	default:
		return fmt.Errorf("route_access.mode must be direct, selected or none")
	}
	return nil
}

// UnmarshalJSON distinguishes an omitted legacy policy from malformed input.
func (p *RouteAccess) UnmarshalJSON(data []byte) error {
	type plain RouteAccess
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.Mode == "" {
		return fmt.Errorf("route_access.mode is required")
	}
	*p = RouteAccess(value)
	return p.Validate()
}

// MarshalJSON makes the legacy default explicit in API responses.
func (p RouteAccess) MarshalJSON() ([]byte, error) {
	type plain RouteAccess
	if p.Mode == "" {
		p.Mode = "direct"
	}
	return json.Marshal(plain(p))
}
