package process

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartTokenDocs(t *testing.T) {
	for _, tc := range []struct {
		path string
		want []string
	}{
		{path: "../../../docs/run_core.md", want: []string{"`start_token`", "field 22", "Darwin", "GetProcessTimes", "compares the token exactly"}},
		{path: "../../../CHANGELOG.md", want: []string{"per-platform start token", "raw Linux start ticks"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			data, err := os.ReadFile(tc.path)
			require.NoError(t, err)
			for _, want := range tc.want {
				assert.True(t, strings.Contains(string(data), want), "missing identity description: %s", want)
			}
			assert.False(t, strings.Contains(string(data), "`start_time`"), "old identity field name")
			assert.False(t, strings.Contains(string(data), "milliseconds since the Unix epoch"), "token must have no epoch unit contract")
		})
	}
}
