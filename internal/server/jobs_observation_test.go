package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stretchr/testify/require"
)

type snapshotJobRouter struct {
	fakeRouter
	event jobs.Event
	reads atomic.Int64
}

func (r *snapshotJobRouter) Route(context.Context, *routing.Request) (*routing.Response, error) {
	r.reads.Add(1)
	b, err := json.Marshal(r.event)
	return &routing.Response{StatusCode: 200, Body: b}, err
}

func TestJobObservationRequiresAuthenticTerminalEvidence(t *testing.T) {
	event := jobs.Event{JobID: "dl-test", Node: "worker", Epoch: "epoch", Kind: jobs.KindDownload, Phase: jobs.PhaseRunning}
	frame := func(name string, ev jobs.Event) string {
		return fmt.Sprintf("event: %s\ndata: %s\n\n", name, mustJSON(t, ev))
	}
	done := event
	done.Phase = jobs.PhaseDone
	wrong := done
	wrong.JobID = "other-attempt"
	changed := done
	changed.Epoch = "other-epoch"
	dropped := done
	dropped.Dropped = &jobs.DroppedMarker{Since: 1, Current: 2}
	for _, tc := range []struct {
		name, frames string
		snapshot     jobs.Event
		reads        int64
		wantErr      bool
	}{
		{"wrong identity", frame("done", wrong), done, 0, true},
		{"buffered terminal with oversized continuation", "event: done\ndata: " + string(mustJSON(t, done)) + "\ndata: " + strings.Repeat("x", 1024*1024+1) + "\n\n", done, 0, true},
		{"oversized frame", "event: progress\ndata: " + strings.Repeat("x", 1024*1024+1) + "\n\n", done, 0, true},
		{"malformed JSON", "event: progress\ndata: broken\n\n", done, 0, true},
		{"epoch changes", frame("progress", event) + frame("done", changed), done, 0, true},
		{"done name without terminal phase", frame("done", event), done, 0, true},
		{"closed stream reconciles once", "event: stream_closed\ndata: stream closed\n\n", done, 1, false},
		{"dropped marker cannot complete", frame("events_dropped", dropped), event, 1, true},
		{"nonterminal EOF", frame("progress", event), event, 1, true},
		{"terminal event", frame("done", done), event, 0, false},
		{"proxy transport signal", "event: error\ndata: {\"code\":\"node_disconnected\"}\n\n", done, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.frames)
			}))
			defer upstream.Close()
			r := &snapshotJobRouter{event: tc.snapshot}
			a := &jobAccess{local: func(string) bool { return false }, router: r, proxy: &JobsProxy{StreamClient: func() (*http.Client, error) { return upstream.Client(), nil }, ResolveEndpoint: func(string) *mesh.Endpoint { return &mesh.Endpoint{NodeName: "worker", ClusterURL: upstream.URL} }}}
			err := a.Wait(t.Context(), "dl-test", "alias")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.reads, r.reads.Load())
		})
	}
}

func TestJobObservationCancellationClosesBodyWithoutSnapshot(t *testing.T) {
	opened, closed := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		close(opened)
		<-r.Context().Done()
		close(closed)
	}))
	defer upstream.Close()
	r := &snapshotJobRouter{}
	a := &jobAccess{local: func(string) bool { return false }, router: r, proxy: &JobsProxy{StreamClient: func() (*http.Client, error) { return upstream.Client(), nil }, ResolveEndpoint: func(string) *mesh.Endpoint { return &mesh.Endpoint{NodeName: "worker", ClusterURL: upstream.URL} }}}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- a.Wait(ctx, "dl-test", "worker") }()
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("stream did not open")
	}
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream body not closed")
	}
	require.Zero(t, r.reads.Load())
}
