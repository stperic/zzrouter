//go:build darwin

package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// launchctlListOutput is what `launchctl list <label>` prints. Passing a
// label switches launchctl from the PID/Status/Label table to this
// plist-style dict — parsing it as the table reported every job as not
// running, which in turn told the updater nothing was supervising it.
const launchctlListOutput = `{
	"LimitLoadToSessionType" = "Aqua";
	"Label" = "com.zzrouter.node";
	"OnDemand" = false;
	"LastExitStatus" = 9;
	"PID" = 64367;
	"Program" = "/usr/local/bin/zzrouter-node";
};`

func TestLaunchctlDictInt(t *testing.T) {
	pid, ok := launchctlDictInt(launchctlListOutput, "PID")
	assert.True(t, ok)
	assert.Equal(t, 64367, pid)

	code, ok := launchctlDictInt(launchctlListOutput, "LastExitStatus")
	assert.True(t, ok)
	assert.Equal(t, 9, code)

	_, ok = launchctlDictInt(launchctlListOutput, "Missing")
	assert.False(t, ok)

	// A quoted string value is not an int, and must not read as zero.
	_, ok = launchctlDictInt(launchctlListOutput, "Label")
	assert.False(t, ok)
}

func TestLaunchctlDictInt_NotLoaded(t *testing.T) {
	_, ok := launchctlDictInt("", "PID")
	assert.False(t, ok)
}
