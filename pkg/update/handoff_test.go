package update

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandoff_SubmitThenClaim(t *testing.T) {
	h := NewHandoff(t.TempDir(), t.TempDir())

	require.NoError(t, h.Submit(&Request{Action: ActionApply, TargetVersion: "2.0.0", RequestedBy: "api"}))

	pending, err := h.Pending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, ActionApply, pending.Action)
	assert.Equal(t, "2.0.0", pending.TargetVersion)
	assert.False(t, pending.RequestedAt.IsZero(), "Submit stamps the request")

	claimed, err := h.Claim()
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, ActionApply, claimed.Action)

	// Claiming must remove the file. systemd's PathExists fires when the
	// file appears and stays quiet while it is still there, so a request
	// left behind means no later request is ever noticed.
	assert.NoFileExists(t, h.RequestPath())

	again, err := h.Claim()
	require.NoError(t, err)
	assert.Nil(t, again)
}

func TestHandoff_ClaimRemovesARequestItCannotRead(t *testing.T) {
	h := NewHandoff(t.TempDir(), t.TempDir())
	require.NoError(t, os.WriteFile(h.RequestPath(), []byte("{not json"), 0640))

	_, err := h.Claim()
	require.Error(t, err)

	// A request that cannot be parsed will never become parseable.
	// Leaving it would wedge the watcher against every future request.
	assert.NoFileExists(t, h.RequestPath())
}

func TestHandoff_RejectsWhatItCannotAct(t *testing.T) {
	h := NewHandoff(t.TempDir(), t.TempDir())

	assert.ErrorIs(t, h.Submit(&Request{Action: "rm -rf"}), ErrUnknownAction)
	assert.Error(t, h.Submit(&Request{Action: ActionApply, TargetVersion: "../../etc/passwd"}))
	assert.NoFileExists(t, h.RequestPath(), "a rejected request must not be left for the updater")
}

func TestHandoff_PendingRejectsAnActionWrittenDirectly(t *testing.T) {
	h := NewHandoff(t.TempDir(), t.TempDir())
	require.NoError(t, os.MkdirAll(h.RequestDir(), 0750))
	// The service user owns this directory and can write whatever it
	// likes; validation on the reading side is what matters.
	require.NoError(t, os.WriteFile(h.RequestPath(), []byte(`{"action":"exec"}`), 0640))

	_, err := h.Pending()
	assert.ErrorIs(t, err, ErrUnknownAction)
}

func TestHandoff_PublishAndReadBack(t *testing.T) {
	h := NewHandoff(t.TempDir(), t.TempDir())

	assert.Nil(t, mustLastRun(t, h), "nothing published yet")

	finished := time.Now().UTC()
	require.NoError(t, h.Publish(&RunStatus{
		Action: ActionApply, State: StateIdle, Success: true,
		FromVersion: "1.0.0", ToVersion: "2.0.0", FinishedAt: &finished,
	}))

	got := mustLastRun(t, h)
	require.NotNil(t, got)
	assert.True(t, got.Success)
	assert.Equal(t, "2.0.0", got.ToVersion)
	assert.True(t, got.Finished())
}

func TestHandoff_StatusIsReadableByTheServiceUser(t *testing.T) {
	h := NewHandoff(t.TempDir(), t.TempDir())
	require.NoError(t, h.Publish(&RunStatus{Action: ActionCheck}))

	// Written by root, read by the unprivileged node. A stricter mode
	// here means the node's own status endpoint cannot say what the
	// updater did.
	info, err := os.Stat(h.StatusPath())
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0044, "status must be world-readable, got %v", info.Mode().Perm())
}

// TestRequestCarriesNoLocation is a guard on the security property the
// whole privilege split rests on.
//
// The request is written by the service user and read by root. If it
// could name a path, a URL or a release feed, anything that compromised
// the node process could choose what root installs -- which is exactly
// the escalation the split exists to prevent. So the fields are pinned:
// adding one fails this test and forces the question to be asked.
func TestRequestCarriesNoLocation(t *testing.T) {
	allowed := map[string]bool{
		"Action":        true, // closed set, validated
		"TargetVersion": true, // parsed as a version, compared against the feed, never a path
		"JobID":         true, // correlation only
		"RequestedAt":   true, // for the log
		"RequestedBy":   true, // for the log
	}

	typ := reflect.TypeOf(Request{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		assert.True(t, allowed[name],
			"new field %q on the privilege-crossing Request: it must not name a path, URL or release feed. "+
				"See the SECURITY note in handoff.go", name)
	}
}

func TestDefaultHandoffSeparatesTheTwoDirections(t *testing.T) {
	h := DefaultHandoff()

	// Fixed rather than resolved: as the service user the data directory
	// is /var/lib/zzrouter, but as root it is /opt/zzrouter, so a
	// resolved path would have the two sides never meet.
	assert.Equal(t, filepath.Join("/var/lib/zzrouter", "update"), h.RequestDir())

	// The security property, pinned. Root publishes the status, so it
	// must not create that file in the service-user-owned request
	// directory: that user can pre-plant the name as a symlink and
	// O_CREATE follows it, landing root's write wherever it points.
	assert.NotEqual(t, filepath.Dir(h.RequestPath()), filepath.Dir(h.StatusPath()),
		"root must not create files in the directory the service user owns")
	assert.Equal(t, filepath.Join("/opt/zzrouter", "update"), filepath.Dir(h.StatusPath()))
}

func mustLastRun(t *testing.T, h *Handoff) *RunStatus {
	t.Helper()
	run, err := h.LastRun()
	require.NoError(t, err)
	return run
}
