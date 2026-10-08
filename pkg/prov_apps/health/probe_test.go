package health

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatternMatcher_StringMatch(t *testing.T) {
	pm := PatternMatcher{Pattern: "Application startup complete"}

	assert.True(t, pm.Match("INFO: Application startup complete"))
	assert.True(t, pm.Match("Application startup complete."))
	assert.False(t, pm.Match("Starting application..."))
	assert.False(t, pm.Match(""))
}

func TestPatternMatcher_RegexMatch(t *testing.T) {
	pm := PatternMatcher{Pattern: `Uvicorn running on \d+\.\d+\.\d+\.\d+:\d+`, IsRegex: true}
	require.NoError(t, pm.Compile())

	assert.True(t, pm.Match("INFO: Uvicorn running on 0.0.0.0:8000"))
	assert.False(t, pm.Match("INFO: Uvicorn starting"))
}

func TestPatternMatcher_EmptyPattern(t *testing.T) {
	pm := PatternMatcher{Pattern: ""}
	assert.False(t, pm.Match("anything"))
}

func TestPatternMatcher_InvalidRegex(t *testing.T) {
	pm := PatternMatcher{Pattern: "[invalid", IsRegex: true}
	err := pm.Compile()
	assert.Error(t, err)
}

func TestLogPatterns_MatchSuccess(t *testing.T) {
	lp := LogPatterns{
		Success: []PatternMatcher{
			{Pattern: "ready to serve"},
			{Pattern: "startup complete"},
		},
	}

	matched, pattern := lp.MatchSuccess("Server ready to serve requests")
	assert.True(t, matched)
	assert.Equal(t, "ready to serve", pattern)

	matched, _ = lp.MatchSuccess("still loading...")
	assert.False(t, matched)
}

func TestLogPatterns_MatchFailure(t *testing.T) {
	lp := LogPatterns{
		Failure: []PatternMatcher{
			{Pattern: "CUDA out of memory"},
			{Pattern: "segmentation fault"},
		},
	}

	matched, pattern := lp.MatchFailure("RuntimeError: CUDA out of memory")
	assert.True(t, matched)
	assert.Equal(t, "CUDA out of memory", pattern)
}

func TestReadinessProbe_Validate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		rp := &ReadinessProbe{
			LogPatterns: LogPatterns{
				Success: []PatternMatcher{{Pattern: "ready"}},
			},
			Timeout: 30 * time.Second,
		}
		assert.NoError(t, rp.Validate())
	})

	t.Run("nil probe", func(t *testing.T) {
		var rp *ReadinessProbe
		assert.Error(t, rp.Validate())
	})

	t.Run("no patterns", func(t *testing.T) {
		rp := &ReadinessProbe{Timeout: 30 * time.Second}
		assert.Error(t, rp.Validate())
	})

	t.Run("zero timeout", func(t *testing.T) {
		rp := &ReadinessProbe{
			LogPatterns: LogPatterns{Success: []PatternMatcher{{Pattern: "ready"}}},
		}
		assert.Error(t, rp.Validate())
	})

	t.Run("invalid regex", func(t *testing.T) {
		rp := &ReadinessProbe{
			LogPatterns: LogPatterns{
				Success: []PatternMatcher{{Pattern: "[invalid", IsRegex: true}},
			},
			Timeout: 30 * time.Second,
		}
		assert.Error(t, rp.Validate())
	})
}

func TestLivenessProbe_Validate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		lp := &LivenessProbe{
			LogPatterns: LogPatterns{
				Failure: []PatternMatcher{{Pattern: "CUDA error"}},
			},
			InitialDelaySeconds: 10,
		}
		assert.NoError(t, lp.Validate())
	})

	t.Run("nil probe", func(t *testing.T) {
		var lp *LivenessProbe
		assert.Error(t, lp.Validate())
	})

	t.Run("no failure patterns", func(t *testing.T) {
		lp := &LivenessProbe{}
		assert.Error(t, lp.Validate())
	})

	t.Run("negative delay", func(t *testing.T) {
		lp := &LivenessProbe{
			LogPatterns:         LogPatterns{Failure: []PatternMatcher{{Pattern: "error"}}},
			InitialDelaySeconds: -1,
		}
		assert.Error(t, lp.Validate())
	})
}
