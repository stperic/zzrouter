package wire

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripHopByHop(t *testing.T) {
	cases := []struct {
		name     string
		in       http.Header
		mustKeep []string
		mustGone []string
	}{
		{
			name: "RFC 7230 §6.1 base set",
			in: http.Header{
				"Connection":          {"close"},
				"Keep-Alive":          {"timeout=5"},
				"Proxy-Authenticate":  {"Basic"},
				"Proxy-Authorization": {"Basic xyz"},
				"Te":                  {"trailers"},
				"Trailer":             {"X-Foo"},
				"Transfer-Encoding":   {"chunked"},
				"Upgrade":             {"h2c"},
				"Content-Length":      {"123"},
				"Content-Encoding":    {"gzip"},
				"Content-Type":        {"application/json"},
				"X-Custom":            {"keep-me"},
			},
			mustKeep: []string{"Content-Type", "X-Custom"},
			mustGone: []string{
				"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
				"Te", "Trailer", "Transfer-Encoding", "Upgrade",
				"Content-Length", "Content-Encoding",
			},
		},
		{
			name: "Connection header lists additional hop-by-hop names",
			in: http.Header{
				"Connection":   {"X-Hop, X-Also-Hop"},
				"X-Hop":        {"upstream"},
				"X-Also-Hop":   {"upstream"},
				"X-Real-Stays": {"keep-me"},
			},
			mustKeep: []string{"X-Real-Stays"},
			mustGone: []string{"Connection", "X-Hop", "X-Also-Hop"},
		},
		{
			name: "empty header — no-op",
			in:   http.Header{},
		},
		{
			name: "only end-to-end headers — no-op",
			in: http.Header{
				"Content-Type":  {"application/json"},
				"Cache-Control": {"no-cache"},
			},
			mustKeep: []string{"Content-Type", "Cache-Control"},
		},
		{
			name: "Connection: close (the empty-name token edge)",
			in: http.Header{
				"Connection":   {"close"},
				"Content-Type": {"application/json"},
			},
			mustKeep: []string{"Content-Type"},
			mustGone: []string{"Connection"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			StripHopByHop(tc.in)
			for _, name := range tc.mustKeep {
				assert.NotEmpty(t, tc.in.Get(name),
					"%s should have been preserved", name)
			}
			for _, name := range tc.mustGone {
				assert.Empty(t, tc.in.Get(name),
					"%s should have been stripped", name)
			}
		})
	}
}
