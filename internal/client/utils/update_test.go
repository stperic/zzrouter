package client

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bodies captured from a running coordinator on 2026-09-10, not written
// from the handler: a pass-through type checked against a body we
// invented tests that we can read our own writing.
const (
	liveUpdateStatusBody = `{"success":true,"message":"Update status retrieved","data":{` +
		`"state":"failed","current_version":{"major":0,"minor":1,"patch":1,"build_meta":"09091713"},` +
		`"next_check_time":"2026-09-10T13:44:43.886213-04:00",` +
		`"error":"failed to fetch releases: repository stperic/zzrouter not found or not public (HTTP 404)",` +
		`"channel":"stable","update_available":false}}`

	liveUpdateHistoryBody = `{"entries":[` +
		`{"id":"1787778604452594000","timestamp":"2026-08-26T17:10:04.452595-04:00",` +
		`"from_version":{"major":1,"minor":0,"patch":0},"to_version":{"major":1,"minor":1,"patch":0},` +
		`"success":false,"error":"installation failed","duration":5000000000,"automatic":true}],` +
		`"last_updated":"2026-08-26T17:10:04.452595-04:00"}`
)

func TestGetUpdateStatus_ReadsTheBodyTheNodeSends(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/zzrouter/v1/update/status", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveUpdateStatusBody))
	}))

	status, err := c.GetUpdateStatus()
	require.NoError(t, err)
	assert.Equal(t, "failed", status.State)
	assert.Equal(t, "stable", status.Channel)
	assert.False(t, status.UpdateAvailable)
	assert.Equal(t, "0.1.1+09091713", status.CurrentVersion.String(),
		"the version arrives in parts and has to be put back together for display")
	assert.Contains(t, status.Error, "not found or not public")
	require.NotNil(t, status.NextCheckTime)
}

// History is the odd route out: the handler writes the object itself
// instead of the success envelope its four siblings use. Decoding it
// like the others yields an empty history and no error at all.
func TestGetUpdateHistory_IsNotEnveloped(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/zzrouter/v1/update/history", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveUpdateHistoryBody))
	}))

	history, err := c.GetUpdateHistory()
	require.NoError(t, err)
	require.Len(t, history.Entries, 1)
	entry := history.Entries[0]
	assert.False(t, entry.Success)
	assert.Equal(t, "installation failed", entry.Error)
	assert.Equal(t, "1.0.0", entry.FromVersion.String())
	assert.Equal(t, "1.1.0", entry.ToVersion.String())
	assert.True(t, entry.Automatic)
}

// Which field comes back says who is doing the work, and the delegated
// answer carries no job id — there is nothing to stream, because the
// work runs in a process this node does not own.
func TestApplyUpdate_TellsAStreamFromAHandoff(t *testing.T) {
	t.Run("this node installs it", func(t *testing.T) {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"success":true,"message":"Update apply dispatched.","data":{"job_id":"upd_123"}}`))
		}))
		res, err := c.ApplyUpdate()
		require.NoError(t, err)
		assert.Equal(t, "upd_123", res.JobID)
		assert.False(t, res.Delegated())
	})

	t.Run("a privileged updater installs it", func(t *testing.T) {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"success":true,"message":"Update handed to the privileged updater.",` +
				`"data":{"delegated_to":"/var/lib/zzrouter/update/request.json","action":"apply"}}`))
		}))
		res, err := c.ApplyUpdate()
		require.NoError(t, err)
		assert.True(t, res.Delegated())
		assert.Empty(t, res.JobID, "a caller that assumes a stream here waits forever")
		assert.Equal(t, "apply", res.Action)
	})
}

// The statuses these routes answer with are outcomes, not faults, and a
// caller that told them apart by matching the message would break the
// first time one was reworded.
func TestUpdatePredicates_ReadTheStatusNotTheProse(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		call    func(c *Client) error
		matches func(error) bool
		others  []func(error) bool
	}{
		{
			name:    "auto-update switched off",
			status:  http.StatusServiceUnavailable,
			body:    `{"title":"Service Unavailable","detail":"auto-update system is not enabled; set update.enabled: true in node.yaml"}`,
			call:    func(c *Client) error { _, err := c.GetUpdateStatus(); return err },
			matches: UpdateDisabled,
			others:  []func(error) bool{UpdateNothingToApply, UpdateNoBackups},
		},
		{
			name:    "nothing to apply",
			status:  http.StatusConflict,
			body:    `{"title":"Conflict","detail":"no update is available; run POST /zzrouter/v1/update/check first"}`,
			call:    func(c *Client) error { _, err := c.ApplyUpdate(); return err },
			matches: UpdateNothingToApply,
			others:  []func(error) bool{UpdateDisabled, UpdateNoBackups},
		},
		{
			name:    "nothing to roll back to",
			status:  http.StatusNotFound,
			body:    `{"title":"Not Found","detail":"no backups available"}`,
			call:    func(c *Client) error { _, err := c.RollbackUpdate(); return err },
			matches: UpdateNoBackups,
			others:  []func(error) bool{UpdateDisabled, UpdateNothingToApply},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))

			err := tc.call(c)
			require.Error(t, err)
			assert.True(t, tc.matches(err), "the status was not readable from the error: %v", err)
			for _, other := range tc.others {
				assert.False(t, other(err), "one outcome matched another's predicate: %v", err)
			}
			// The detail still reaches a person: the UI prints it.
			assert.Contains(t, err.Error(), "update")
		})
	}
}
