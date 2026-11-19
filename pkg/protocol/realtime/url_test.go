package realtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildUpstreamURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://example.com/provider/openai/realtime?model=a%2Fb", "wss://example.com/provider/openai/realtime?model=a%2Fb"},
		{"http://127.0.0.1:9000/v1/realtime", "ws://127.0.0.1:9000/v1/realtime"},
		{"wss://example.com/v1/realtime", "wss://example.com/v1/realtime"},
	} {
		got, err := BuildUpstreamURL(tc.in)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
	_, err := BuildUpstreamURL("ftp://example.com/realtime")
	assert.Error(t, err)
}
