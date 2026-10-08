package prov_apps

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The installers prefix each step with "[n/m]", so an upgrade can report a
// real percentage without the coordinator having to know the plan up front.
func TestUpgradeStepPercent(t *testing.T) {
	tests := []struct {
		msg  string
		want int
	}{
		{"[1/6] Preflight checks", 16},
		{"[3/6] Download archive", 50},
		{"[6/6] Mark installation as managed by zzRouter", 100},
		{"[1/1] Single step", 100},

		// No counter: 0 means "unknown", and the emit path still carries the
		// step text, so an installer wording its steps differently still
		// streams something useful.
		{"Downloading", 0},
		{"", 0},

		// Malformed counters must not produce a nonsense percentage.
		{"[0/0] broken", 0},
		{"[abc/6] broken", 0},
		{"[3/0] broken", 0},
		{"3/6 no brackets", 0},

		// A counter past its total is clamped rather than exceeding 100.
		{"[9/6] overshoot", 100},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			assert.Equal(t, tt.want, upgradeStepPercent(tt.msg))
		})
	}
}
