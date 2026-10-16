package views

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/jobstream"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"

	tea "charm.land/bubbletea/v2"
)

// TestDeploys_FrameMsgRoutingByJobID is the critical N>1 invariant for
// the deploys view: a FrameMsg whose JobID matches an owned row must
// update that row only; mismatched JobIDs must be dropped silently.
// Wizard-style consumers got away with a singleton pointer; the deploy
// panel needs map-keyed routing and this test pins it down.
func TestDeploys_FrameMsgRoutingByJobID(t *testing.T) {
	v := NewDeploysViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	// Hand-park two rows on the model (skipping Subscribe — we're
	// testing the routing logic, not network IO).
	v.rows["A"] = &deployRow{JobID: "A", Model: "modelA", Node: "n1", stream: &jobstream.Row{JobID: "A"}}
	v.rows["B"] = &deployRow{JobID: "B", Model: "modelB", Node: "n2", stream: &jobstream.Row{JobID: "B"}}
	v.order = []string{"A", "B"}

	// Frame for A: updates A only.
	updated, _ := v.Update(jobstream.FrameMsg{
		JobID: "A",
		Event: pkgClient.JobEvent{Phase: "running", Percent: 42},
	})
	v = updated.(*DeploysViewModel)
	assert.Equal(t, 42, v.rows["A"].stream.Latest.Percent)
	assert.Equal(t, 0, v.rows["B"].stream.Latest.Percent, "B must not be touched by A's frame")

	// Frame for unknown JobID: dropped silently.
	updated, _ = v.Update(jobstream.FrameMsg{
		JobID: "ghost",
		Event: pkgClient.JobEvent{Percent: 99},
	})
	v = updated.(*DeploysViewModel)
	assert.Equal(t, 42, v.rows["A"].stream.Latest.Percent)
	assert.Equal(t, 0, v.rows["B"].stream.Latest.Percent)

	// Terminal for B: marks Done, no further Next cmd.
	updated, cmd := v.Update(jobstream.FrameMsg{
		JobID: "B",
		Event: pkgClient.JobEvent{Phase: "done"},
		Done:  true,
	})
	v = updated.(*DeploysViewModel)
	assert.True(t, v.rows["B"].stream.Done)
	assert.Equal(t, "done", v.rows["B"].stream.Phase)
	assert.Nil(t, cmd, "terminal frame returns no follow-up cmd")
}

// TestDeploys_OrderSorted_DonePushedDown verifies the visual grouping
// invariant: terminated rows render below active ones.
func TestDeploys_OrderSorted_DonePushedDown(t *testing.T) {
	v := NewDeploysViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	v.rows["A"] = &deployRow{JobID: "A", stream: &jobstream.Row{JobID: "A", Done: true}}
	v.rows["B"] = &deployRow{JobID: "B", stream: &jobstream.Row{JobID: "B"}}
	v.rows["C"] = &deployRow{JobID: "C", stream: &jobstream.Row{JobID: "C", Done: true}}
	v.rows["D"] = &deployRow{JobID: "D", stream: &jobstream.Row{JobID: "D"}}
	v.order = []string{"A", "B", "C", "D"}

	got := v.orderSorted()
	// Active (B, D) before done (A, C); within each group, original order.
	assert.Equal(t, []string{"B", "D", "A", "C"}, got)
}

// TestDeploys_CancelTearsDownAllRows confirms the view's Cancel()
// cascades to every row.stream — the contract Root.TeardownAll relies
// on for clean program exit.
func TestDeploys_CancelTearsDownAllRows(t *testing.T) {
	// Start two real subscriptions against an httptest server so each
	// row has a producer goroutine + ctx that Cancel must tear down.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: progress\ndata: {\"job_id\":\"j\",\"phase\":\"running\"}\n\n")
		f.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	client := pkgClient.NewClient(pkgConfig.ClientNodeConfig{Address: srv.URL, APIKey: "k"})
	v := NewDeploysViewModel(client, ui.NewStyles(ui.CatppuccinMocha()))
	rowA, _ := jobstream.Subscribe(context.Background(), client, "A", "")
	rowB, _ := jobstream.Subscribe(context.Background(), client, "B", "")
	v.rows["A"] = &deployRow{JobID: "A", stream: rowA}
	v.rows["B"] = &deployRow{JobID: "B", stream: rowB}
	v.order = []string{"A", "B"}

	// Cancel must not panic and must mark rows for teardown. The
	// producer goroutines exit on ctx.Done; the drain goroutine in
	// Row.Cancel handles the close.
	require.NotPanics(t, func() { v.Cancel() })
	require.NotPanics(t, func() { v.Cancel() }, "second Cancel must be a no-op")
}

// Compile-time guard: DeploysViewModel implements tea.Model + Canceller
// via the package interfaces.
var _ tea.Model = (*DeploysViewModel)(nil)

func TestTUICancelUsesJobID(t *testing.T) {
	for _, view := range []string{"jobs", "models", "missing job"} {
		t.Run(view, func(t *testing.T) {
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.RequestURI())
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodDelete || r.URL.Path != "/zzrouter/v1/jobs/dl_selected" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = fmt.Fprint(w, `{"accepted":true,"job_id":"dl_selected"}`)
			}))
			t.Cleanup(srv.Close)
			client := pkgClient.NewClient(pkgConfig.ClientNodeConfig{Address: srv.URL, APIKey: "k"})
			styles := ui.NewStyles(ui.CatppuccinMocha())
			if view == "jobs" {
				v := NewDeploysViewModel(client, styles)
				v.rows["dl_done"] = &deployRow{JobID: "dl_done", stream: &jobstream.Row{Done: true}}
				v.rows["dl_selected"] = &deployRow{JobID: "dl_selected", Kind: "download", stream: &jobstream.Row{}}
				v.rows["dl_other"] = &deployRow{JobID: "dl_other", Kind: "download", stream: &jobstream.Row{}}
				v.order = []string{"dl_done", "dl_other", "dl_selected"}
				v.cursor = 1
				require.Equal(t, "dl_selected", v.orderSorted()[v.cursor])
				require.Equal(t, "dl_other", v.order[v.cursor])
				cmd := v.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
				require.NotNil(t, cmd)
				msg, ok := cmd().(deploysCancelMsg)
				require.True(t, ok)
				require.NoError(t, msg.err)
			} else {
				v := NewModelsViewModel(client, styles)
				jobID := "dl_selected"
				if view == "missing job" {
					jobID = ""
				}
				v.rows = []modelRow{{deploy: &shared.DeploymentInfo{JobID: jobID}}}
				v.syncListItems()
				v.confirmKind = confirmDeleteDownload
				cmd := v.executeConfirmedAction()
				require.NotNil(t, cmd)
				msg, ok := cmd().(modelsActionMsg)
				require.True(t, ok)
				if view == "missing job" {
					require.EqualError(t, msg.err, "this download is no longer cancellable")
					require.Empty(t, requests)
					return
				}
				require.NoError(t, msg.err)
			}
			require.Equal(t, []string{"DELETE /zzrouter/v1/jobs/dl_selected"}, requests)
		})
	}
}

func TestModelsCancelTerminalJob(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{name: "accepted", status: http.StatusAccepted, want: "cancel completed"},
		{name: "terminal or evicted", status: http.StatusNotFound, want: "Download already finished"},
		{name: "unreachable", status: http.StatusServiceUnavailable, want: "Error: CancelJob: status 503:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.RequestURI())
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			client := pkgClient.NewClient(pkgConfig.ClientNodeConfig{Address: srv.URL, APIKey: "k"})
			v := NewModelsViewModel(client, ui.NewStyles(ui.CatppuccinMocha()))
			v.rows = []modelRow{{deploy: &shared.DeploymentInfo{JobID: "dl_finished"}}}
			v.syncListItems()
			v.confirmKind = confirmDeleteDownload
			cmd := v.executeConfirmedAction()
			require.NotNil(t, cmd)
			_, _ = v.Update(cmd())
			require.Contains(t, v.statusMsg, tc.want)
			require.Equal(t, []string{"DELETE /zzrouter/v1/jobs/dl_finished"}, requests)
		})
	}
}
