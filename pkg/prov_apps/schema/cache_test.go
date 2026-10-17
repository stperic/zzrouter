package schema

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				y, err := LoadYAMLSchema([]byte("unknown_future_metadata: ignored\ncache_log:\n  " + role + ": '" + tc.pattern + "'\n"))
				require.NoError(t, err)
				y.Parameters = map[string]YAMLParam{"long-tail": {Type: "int"}}
				declared, errs := Merge("generic", y)
				require.Empty(t, errs)
				assert.Equal(t, ParamInt, declared.Parameters["long-tail"].Kind)
				if !tc.valid {
					assert.Nil(t, declared.CacheLog)
					require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("\n")))
					var record map[string]any
					require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
					assert.Equal(t, "WARN", record["level"])
					assert.Equal(t, "generic", record["provider"])
					assert.NotEmpty(t, record["error"])
					return
				}
				assert.Empty(t, logs.String())
				assert.Equal(t, groups, declared.CacheLog[role].NumSubexp())
				assert.Equal(t, tc.pattern, declared.CacheLog[role].String())
			})
		}
	}
	declared, errs := Merge("generic", &YAMLSchema{CacheLog: map[string]string{"tokens": "(value)", "invented": "(value)"}})
	require.Empty(t, errs)
	assert.Nil(t, declared.CacheLog)
}
