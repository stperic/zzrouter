package schema

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheLogDeclarationsValidateCaptures(t *testing.T) {
	for role, groups := range map[string]int{"tokens": 1, "concurrency": 2, "pools": 2, "slots": 3, "sequence_slots": 1} {
		for _, tc := range []struct {
			name, pattern string
			valid         bool
		}{
			{"valid", `(?:prefix)` + strings.Repeat(`(value)`, groups), true},
			{"regex", `(`, false},
			{"too few", strings.Repeat(`(value)`, groups-1), false},
			{"too many", strings.Repeat(`(value)`, groups+1), false},
		} {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				y, err := LoadYAMLSchema([]byte("unknown_future_metadata: ignored\ncache_log:\n  " + role + ": '" + tc.pattern + "'\n"))
				require.NoError(t, err)
				declared, errs := Merge("generic", y)
				if !tc.valid {
					require.NotEmpty(t, errs)
					return
				}
				require.Empty(t, errs)
				assert.Equal(t, groups, declared.CacheLog[role].NumSubexp())
				assert.Equal(t, tc.pattern, declared.CacheLog[role].String())
			})
		}
	}
	_, errs := Merge("generic", &YAMLSchema{CacheLog: map[string]string{"invented": "(value)"}})
	require.NotEmpty(t, errs)
}
