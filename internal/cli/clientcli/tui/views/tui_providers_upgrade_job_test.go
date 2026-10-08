package views

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/jobstream"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

func TestUpgradeProgressLine(t *testing.T) {
	tests := []struct {
		name    string
		event   pkgClient.JobEvent
		elapsed time.Duration
		want    string
	}{
		{
			// The shape the install coordinator actually emits.
			name:  "step and percent",
			event: pkgClient.JobEvent{Step: "[3/6] Download archive", Percent: 45},
			want:  "Upgrading ollama… 45% [3/6] Download archive",
		},
		{
			name:  "step only",
			event: pkgClient.JobEvent{Step: "[6/6] Mark installation as managed by zzRouter"},
			want:  "Upgrading ollama… [6/6] Mark installation as managed by zzRouter",
		},
		{
			// Some frames carry the step on Meta instead of the field.
			name:  "step from meta",
			event: pkgClient.JobEvent{Meta: map[string]any{"step": "[1/6] Preflight"}},
			want:  "Upgrading ollama… [1/6] Preflight",
		},
		{
			name:  "percent only",
			event: pkgClient.JobEvent{Percent: 10},
			want:  "Upgrading ollama… 10%",
		},
		{
			// A bare frame still says something is happening rather than
			// leaving the previous line stale.
			name:  "no detail at all",
			event: pkgClient.JobEvent{},
			want:  "Upgrading ollama…",
		},
		{
			name:  "falls back to phase",
			event: pkgClient.JobEvent{Phase: "running"},
			want:  "Upgrading ollama… running",
		},
		{
			// The complaint this exists for: vLLM's "Install vllm" step
			// covers a multi-GB pip resolve and emits nothing for minutes.
			// A frozen percent reads as hung; a running clock reads as slow.
			name:    "a long-silent step shows how long it has been silent",
			event:   pkgClient.JobEvent{Step: "[5/9] Install vllm", Percent: 55},
			elapsed: 2*time.Minute + 18*time.Second,
			want:    "Upgrading ollama… 55% [5/9] Install vllm (2m18s elapsed)",
		},
		{
			// Steps that come and go quickly would only be made noisier.
			name:    "a step that has just started carries no clock",
			event:   pkgClient.JobEvent{Step: "[2/9] Download", Percent: 22},
			elapsed: 3 * time.Second,
			want:    "Upgrading ollama… 22% [2/9] Download",
		},
		{
			// Before the first frame there is no step to name, and that is
			// exactly when a caller most needs to know it is not wedged.
			name:    "no frame yet still reports the wait",
			event:   pkgClient.JobEvent{},
			elapsed: 45 * time.Second,
			want:    "Upgrading ollama… (45s elapsed)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, upgradeProgressLine("ollama", tt.event, tt.elapsed))
		})
	}
}

// A frame arriving with no upgrade in flight must be left for another
// handler rather than swallowed.
func TestHandleUpgradeFrameIgnoresForeignFrames(t *testing.T) {
	m := &ProvidersViewModel{}
	_, owned := m.handleUpgradeFrame(jobstream.FrameMsg{JobID: "inst_other"})
	assert.False(t, owned, "no upgrade in flight means the frame is not ours")
}

// The whole point of subscribing is that the status line keeps moving. A
// frame handler that returns no command consumes exactly one frame and then
// waits forever — the line freezes on the first step, and the terminal frame
// never arrives, so a failed upgrade reports nothing at all.
func TestHandleUpgradeFrameKeepsPumping(t *testing.T) {
	const jobID = "inst_pump"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		require.True(t, ok)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`{"job_id":"` + jobID + `","phase":"running","percent":25,"step":"[2/8] Download"}`,
			`{"job_id":"` + jobID + `","phase":"running","percent":50,"step":"[4/8] Extract"}`,
		} {
			_, _ = fmt.Fprintf(w, "event: progress\ndata: %s\n\n", frame)
			f.Flush()
		}
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n",
			`{"job_id":"`+jobID+`","phase":"failed","err":"download returned status 404"}`)
		f.Flush()
	}))
	defer srv.Close()

	client := pkgClient.NewClient(pkgConfig.ClientNodeConfig{
		Name: "test", Address: srv.URL, APIKey: "test-key",
	})
	row, next := jobstream.Subscribe(context.Background(), client, jobID, "")
	defer row.Cancel()

	m := &ProvidersViewModel{upgrade: newUpgradeJob(row, "ollama")}

	var statuses []string
	var terminal tea.Msg
	// Bounded: a stalled stream must fail the assertions below, not hang.
	for i := 0; i < 8 && next != nil; i++ {
		msg := next()
		frame, ok := msg.(jobstream.FrameMsg)
		require.True(t, ok, "expected a FrameMsg, got %T", msg)

		cmd, owned := m.handleUpgradeFrame(frame)
		require.True(t, owned, "the in-flight upgrade owns its own frames")
		if frame.Done {
			require.NotNil(t, cmd, "the terminal frame must report the outcome")
			terminal = cmd()
			break
		}
		statuses = append(statuses, m.actionStatus)
		next = cmd
	}

	// Every progress frame reached the status line, not just the first.
	assert.Equal(t, []string{
		"Upgrading ollama… 25% [2/8] Download",
		"Upgrading ollama… 50% [4/8] Extract",
	}, statuses)

	// And the failure surfaced rather than being swallowed.
	action, ok := terminal.(providersActionMsg)
	require.True(t, ok, "expected providersActionMsg, got %T", terminal)
	assert.Equal(t, "upgrade", action.action)
	require.Error(t, action.err)
	assert.Contains(t, action.err.Error(), "404")
}

// The tick chain is what redraws the status line. An upgrade leaves the list
// on screen, so neither of the old re-arm conditions holds and the chain used
// to end on its first tick — which is why a step that ran for over two
// minutes showed a percentage that never moved.
func TestSpinnerTickKeepsRunningWhileAnUpgradeIsInFlight(t *testing.T) {
	m := &ProvidersViewModel{upgrade: newUpgradeJob(&jobstream.Row{JobID: "inst_tick"}, "ollama")}
	_, cmd := m.Update(shared.SpinnerTickMsg{})
	require.NotNil(t, cmd, "an in-flight upgrade must keep the redraw chain alive")

	// And with nothing in flight it still stops, so an idle view is not
	// redrawing twelve times a second forever.
	idle := &ProvidersViewModel{}
	_, cmd = idle.Update(shared.SpinnerTickMsg{})
	assert.Nil(t, cmd)
}

// Each tick re-renders the last frame with a longer clock, so silence is
// visible without the job having to say anything.
func TestTickAdvancesTheElapsedClockOnASilentStep(t *testing.T) {
	job := newUpgradeJob(&jobstream.Row{JobID: "inst_silent"}, "vllm")
	job.frame = pkgClient.JobEvent{Step: "[5/9] Install vllm", Percent: 55}
	job.frameAt = utils.Now().Add(-90 * time.Second)
	m := &ProvidersViewModel{upgrade: job}
	m.Update(shared.SpinnerTickMsg{})
	assert.Contains(t, m.actionStatus, "[5/9] Install vllm")
	assert.Contains(t, m.actionStatus, "1m3", "the clock reflects time since the last frame")
}

// Announcing success while the table still holds the pre-upgrade row is how
// "it said succeeded but showed the old version" happens. The line stays
// provisional until the refetch that proves it lands.
func TestSuccessStaysProvisionalUntilTheTableCatchesUp(t *testing.T) {
	m := &ProvidersViewModel{providers: []shared.ProviderInfo{{Name: "vllm", Version: "0.19.0"}}}

	m.Update(providersActionMsg{action: "upgrade", name: "vllm"})
	assert.Equal(t, "✓ upgrade vllm succeeded, refreshing…", m.actionLine())

	m.Update(providersLoadedMsg{providers: []shared.ProviderInfo{{Name: "vllm", Version: "0.27.1"}}})
	assert.Equal(t, "✓ upgrade vllm succeeded", m.actionLine())
	assert.Equal(t, "0.27.1", m.providers[0].Version)
}

// A failure is already final — there is no later fetch that could turn it
// into a success, so it must not be overwritten by one.
func TestAFailedActionIsNotRewrittenByTheRefetch(t *testing.T) {
	m := &ProvidersViewModel{providers: []shared.ProviderInfo{{Name: "vllm"}}}
	m.Update(providersActionMsg{action: "upgrade", name: "vllm", err: assert.AnError})
	failed := m.actionLine()
	require.Contains(t, failed, "failed")
	assert.NotContains(t, failed, "refreshing", "a failure is not waiting on the table")

	m.Update(providersLoadedMsg{providers: []shared.ProviderInfo{{Name: "vllm"}}})
	assert.Equal(t, failed, m.actionLine())
}
