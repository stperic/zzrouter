//go:build windows

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestRestartCoveredNeedsRestartActionAndNonCrashFlag(t *testing.T) {
	restart := []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}
	for _, tc := range []struct {
		name     string
		actions  []mgr.RecoveryAction
		nonCrash bool
		covered  bool
		reason   string
	}{
		{name: "restart and flag", actions: restart, nonCrash: true, covered: true},
		// The pre-fix install: actions set, flag off. SCM left the node stopped after an update.
		{name: "restart without flag", actions: restart, reason: "sc.exe failureflag zzrouter 1"},
		{name: "flag without restart", actions: []mgr.RecoveryAction{{Type: mgr.NoAction}}, nonCrash: true, reason: "no restart recovery action"},
		{name: "nothing", reason: "no restart recovery action"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			covered, why := restartCovered(tc.actions, tc.nonCrash)
			assert.Equal(t, tc.covered, covered)
			if tc.covered {
				assert.Empty(t, why)
			} else {
				assert.Contains(t, why, tc.reason)
			}
		})
	}
}
