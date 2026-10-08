package server

import (
	"context"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit coverage for the DownloadTracker → jobs translator. End-to-end
// download wiring is covered by the existing deployments integration
// tests plus the pkg/jobs suite; this test pins the status→event
// mapping so a future refactor can't silently lose terminal signals.

func newTestJobsRegistry(t *testing.T) *jobs.Registry {
	t.Helper()
	r, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(r.Stop)
	return r
}

func TestPublishJobEvent_NilHandleIsNoOp(t *testing.T) {
	e := &DeploymentsExecutor{jobKeys: map[string]string{}}
	// Must not panic.
	e.publishJobEvent(nil, "key", "downloading", "file.gguf", 50, 100, 200)
}

func TestPublishJobEvent_ProgressEmitsRunningEvent(t *testing.T) {
	r := newTestJobsRegistry(t)
	e := &DeploymentsExecutor{jobs: r, jobKeys: map[string]string{}}
	h, err := r.Start(context.Background(), jobs.KindDownload, "", nil)
	require.NoError(t, err)

	sub, err := r.Subscribe(h.ID(), jobs.SubscribeOptions{})
	require.NoError(t, err)

	e.publishJobEvent(h, "k", "downloading", "file.gguf", 42, 1000, 2400)
	h.Done() // close the stream

	var events []jobs.Event
	for ev := range sub.Events() {
		events = append(events, ev)
	}
	// Subscribe replays the ring, so we see pending + running + done.
	require.NotEmpty(t, events)
	var running *jobs.Event
	for i := range events {
		if events[i].Phase == jobs.PhaseRunning && events[i].Step == "file.gguf" {
			running = &events[i]
			break
		}
	}
	require.NotNil(t, running, "expected a running event with step=file.gguf; got %+v", events)
	assert.Equal(t, 42, running.Percent)
	require.NotNil(t, running.Bytes)
	assert.Equal(t, int64(1000), running.Bytes.Done)
	assert.Equal(t, int64(2400), running.Bytes.Total)
}

func TestPublishJobEvent_TerminalStatusesCarryReason(t *testing.T) {
	cases := []struct {
		status     string
		phase      jobs.Phase
		errMessage string
		wantErr    string
	}{
		{string(constants.StatusCompleted), jobs.PhaseDone, "", ""},
		{string(constants.StatusFailed), jobs.PhaseFailed, "", "download failed"},
		{string(constants.StatusFailed), jobs.PhaseFailed, "checksum mismatch", "download failed: checksum mismatch"},
		{string(constants.StatusCancelled), jobs.PhaseFailed, "", "download cancelled"},
		{string(constants.StatusCancelled), jobs.PhaseFailed, "user aborted", "download cancelled: user aborted"},
	}
	for _, tc := range cases {
		t.Run(tc.status+"/"+tc.errMessage, func(t *testing.T) {
			r := newTestJobsRegistry(t)
			e := &DeploymentsExecutor{jobs: r, jobKeys: map[string]string{"k": ""}}
			h, err := r.Start(context.Background(), jobs.KindDownload, "", nil)
			require.NoError(t, err)

			sub, err := r.Subscribe(h.ID(), jobs.SubscribeOptions{})
			require.NoError(t, err)

			e.publishJobEvent(h, "k", tc.status, tc.errMessage, 100, 0, 0)

			deadline := time.After(500 * time.Millisecond)
			var last jobs.Event
			sawTerminal := false
		loop:
			for {
				select {
				case ev, ok := <-sub.Events():
					if !ok {
						break loop
					}
					last = ev
					if ev.Phase.IsTerminal() {
						sawTerminal = true
					}
				case <-deadline:
					break loop
				}
			}
			require.True(t, sawTerminal, "expected terminal event for status=%s", tc.status)
			assert.Equal(t, tc.phase, last.Phase)
			if tc.wantErr != "" {
				assert.Equal(t, tc.wantErr, last.Err, "upstream error detail must be preserved")
			}
		})
	}
}

func TestOpenDownloadJob_NilRegistryReturnsNil(t *testing.T) {
	e := &DeploymentsExecutor{jobKeys: map[string]string{}}
	h := e.openDownloadJob(nil, "key", "m", constants.RepoOllama)
	assert.Nil(t, h, "nil jobs registry must yield nil handle (caller nil-guards)")
}

func TestOpenDownloadJob_RegistryStartsJob(t *testing.T) {
	r := newTestJobsRegistry(t)
	e := &DeploymentsExecutor{jobs: r, jobKeys: map[string]string{}}

	h := e.openDownloadJob(nil, "some-key", "llama3", constants.RepoOllama)
	require.NotNil(t, h)
	assert.NotEmpty(t, h.ID())
	assert.NotEmpty(t, h.Epoch())

	ev, err := r.Get(h.ID())
	require.NoError(t, err)
	assert.Equal(t, "llama3", ev.Meta["model"])
	assert.Equal(t, constants.RepoOllama, ev.Meta["registry"])
	assert.Equal(t, "some-key", ev.Meta["key"])

	// Dedup index must carry the downloadKey → jobID mapping.
	assert.Equal(t, h.ID(), e.activeJobIDForKey("some-key"))
}

func TestJobKeyIndex_ClearedOnTerminal(t *testing.T) {
	r := newTestJobsRegistry(t)
	e := &DeploymentsExecutor{jobs: r, jobKeys: map[string]string{}}

	h := e.openDownloadJob(nil, "k1", "llama3", constants.RepoOllama)
	require.NotNil(t, h)
	require.Equal(t, h.ID(), e.activeJobIDForKey("k1"))

	e.publishJobEvent(h, "k1", string(constants.StatusCompleted), "", 100, 0, 0)
	assert.Equal(t, "", e.activeJobIDForKey("k1"), "terminal status must clear the index")
}
