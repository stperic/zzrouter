package utils

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Agents are told to branch on `code` rather than parse `detail`. A code
// that is only sometimes present is a field nobody can branch on, so the
// constructor always supplies one.
func TestNewProblemDetailsAlwaysCarriesACode(t *testing.T) {
	tests := []struct {
		title    string
		wantCode string
		wantType string
	}{
		{"Not Found", "not_found", "https://api.zzrouter.com/problems/not-found"},
		{"Conflict", "conflict", "https://api.zzrouter.com/problems/conflict"},
		{"Bad Request", "bad_request", "https://api.zzrouter.com/problems/bad-request"},
		{"Service Unavailable", "service_unavailable", "https://api.zzrouter.com/problems/service-unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			p := NewProblemDetails(http.StatusNotFound, tt.title, "detail", "/x")
			// snake_case matches the closed enums that override it; the type
			// URI stays kebab-case because it is a URI.
			assert.Equal(t, tt.wantCode, p.Code)
			assert.Equal(t, tt.wantType, p.Type)
		})
	}
}

// A caller with something more specific to say still wins.
func TestProblemCodeIsOverridable(t *testing.T) {
	p := NewProblemDetails(http.StatusBadGateway, "Bad Gateway", "detail", "/x")
	p.Code = "node_unreachable"
	assert.Equal(t, "node_unreachable", p.Code)
}
