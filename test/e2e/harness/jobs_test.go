package harness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// jobsFixture mounts /zzrouter/v1/jobs/:id and /zzrouter/v1/jobs/:id/stream.
// streamBody is the SSE payload using the SAME event names the server
// emits (event: progress | done | events_dropped | error). The
// `done` event carries phase=done|failed in the JSON data.
type jobsFixture struct {
	streamBody   string
	streamStatus int
	getStatus    int
	getBody      string // raw body — caller wraps in SuccessResponse envelope shape
}

func (f jobsFixture) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/zzrouter/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			if f.streamStatus != 0 && f.streamStatus != 200 {
				w.WriteHeader(f.streamStatus)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(f.streamBody))
			return
		}
		if f.getStatus != 0 {
			w.WriteHeader(f.getStatus)
		}
		if f.getBody != "" {
			_, _ = w.Write([]byte(f.getBody))
		}
	})
	return mux
}

func newJobsHelper(t *testing.T, f jobsFixture) (*Jobs, *httptest.Server) {
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	n, err := NewNode("c", srv.URL, RoleCoordinator, nil,
		NodeKeys{Admin: "ADMIN", Cluster: "CLUSTER"})
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(n, TierAdmin)
	return NewJobs(c, 5*time.Second), srv
}

// successEnvelope wraps the bare event in the SuccessResponse shape
// the real server uses (response_helpers.go::respondSuccess).
func successEnvelope(eventJSON string) string {
	return `{"success":true,"message":"Job retrieved","data":` + eventJSON + `}`
}

func TestJobs_Wait_Done(t *testing.T) {
	// Real-server emission: event=done with phase=done in payload.
	body := "event: progress\ndata: {\"phase\":\"running\",\"seq\":1}\n\n" +
		"event: done\ndata: {\"phase\":\"done\",\"seq\":2}\n\n"
	j, _ := newJobsHelper(t, jobsFixture{streamBody: body})
	out, err := j.Wait(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !out.Succeeded() || out.Status != "done" {
		t.Errorf("status: %q (succeeded=%v)", out.Status, out.Succeeded())
	}
	if len(out.Events) != 2 {
		t.Errorf("events: %d", len(out.Events))
	}
	if out.Duration <= 0 {
		t.Errorf("duration not measured: %v", out.Duration)
	}
}

func TestJobs_Wait_Failed(t *testing.T) {
	// Real-server emission: failed jobs ALSO use event=done.
	body := "event: done\ndata: {\"phase\":\"failed\",\"err\":\"boom\"}\n\n"
	j, _ := newJobsHelper(t, jobsFixture{streamBody: body})
	out, err := j.Wait(context.Background(), "job-2")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if out.Succeeded() {
		t.Error("phase=failed must not be Succeeded")
	}
	if out.Status != "failed" {
		t.Errorf("status: %q", out.Status)
	}
	if out.Error != "boom" {
		t.Errorf("error: %q", out.Error)
	}
}

func TestJobs_Wait_StreamErrorEvent(t *testing.T) {
	body := "event: error\ndata: {\"code\":\"node_disconnected\"}\n\n"
	j, _ := newJobsHelper(t, jobsFixture{streamBody: body})
	_, err := j.Wait(context.Background(), "job-err")
	if !errors.Is(err, ErrJobStreamError) {
		t.Fatalf("expected ErrJobStreamError, got %v", err)
	}
}

func TestJobs_Wait_EventsDroppedCounted(t *testing.T) {
	body := "event: events_dropped\ndata: {\"dropped\":{\"since\":0,\"current\":5}}\n\n" +
		"event: done\ndata: {\"phase\":\"done\"}\n\n"
	j, _ := newJobsHelper(t, jobsFixture{streamBody: body})
	out, err := j.Wait(context.Background(), "job-dropped")
	if err != nil {
		t.Fatal(err)
	}
	if out.DroppedEvents != 1 {
		t.Errorf("DroppedEvents: %d", out.DroppedEvents)
	}
	if !out.Succeeded() {
		t.Errorf("status: %q", out.Status)
	}
}

func TestJobs_Wait_Evicted(t *testing.T) {
	j, _ := newJobsHelper(t, jobsFixture{streamStatus: 404})
	_, err := j.Wait(context.Background(), "ghost")
	if !errors.Is(err, ErrJobUnknownOrEvicted) {
		t.Fatalf("expected ErrJobUnknownOrEvicted, got %v", err)
	}
}

func TestJobs_Wait_FallbackOnStreamClosed(t *testing.T) {
	// Stream closes after one progress frame + stream_closed; fallback GET
	// must decode the SuccessResponse envelope, not a bare event.
	body := "event: progress\ndata: {\"phase\":\"running\"}\n\n" +
		"event: stream_closed\ndata: {}\n\n"
	j, _ := newJobsHelper(t, jobsFixture{
		streamBody: body,
		getStatus:  200,
		getBody:    successEnvelope(`{"phase":"done","seq":2}`),
	})
	out, err := j.Wait(context.Background(), "job-3")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !out.Succeeded() {
		t.Errorf("fallback status: %q", out.Status)
	}
}

func TestJobs_Wait_FallbackEvicts(t *testing.T) {
	body := "event: progress\ndata: {\"phase\":\"running\"}\n\n" +
		"event: stream_closed\ndata: {}\n\n"
	j, _ := newJobsHelper(t, jobsFixture{
		streamBody: body,
		getStatus:  404,
	})
	_, err := j.Wait(context.Background(), "job-4")
	if !errors.Is(err, ErrJobUnknownOrEvicted) {
		t.Fatalf("expected ErrJobUnknownOrEvicted from fallback, got %v", err)
	}
}

func TestJobs_Run_Convenience(t *testing.T) {
	body := "event: done\ndata: {\"phase\":\"done\"}\n\n"
	j, _ := newJobsHelper(t, jobsFixture{streamBody: body})
	out, err := j.Run(context.Background(), func() (string, error) { return "j-r", nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Succeeded() {
		t.Errorf("status: %q", out.Status)
	}
}

func TestJobs_Wait_EmptyID(t *testing.T) {
	j, _ := newJobsHelper(t, jobsFixture{})
	if _, err := j.Wait(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty job id")
	}
}
