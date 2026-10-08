package logs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	utilsclient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// newTestClient spins up an httptest.Server with the given handler and
// returns a *utilsclient.Client pointed at it.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*utilsclient.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := utilsclient.NewClient(pkgConfig.ClientNodeConfig{
		Name:    "test",
		Address: srv.URL,
		APIKey:  "test-key",
	})
	return c, srv
}

// fakeRuns wires a /zzrouter/v1/runs endpoint returning the given
// instances. Tests construct utilsclient.Instance values (the wire
// type) rather than the local Run view, because the server endpoint
// emits Instance JSON — the logs package converts on read.
func fakeRuns(runs []utilsclient.Instance) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zzrouter/v1/runs" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": runs,
		})
	}
}

func TestResolveRuns_EmptyFilterReturnsAll(t *testing.T) {
	runs := []utilsclient.Instance{
		{ID: "r_1", App: "mlx", Status: "running", StartedAt: "2026-04-11T10:00:00Z"},
		{ID: "r_2", App: "vllm", Status: "stopped", StartedAt: "2026-04-11T09:00:00Z"},
	}
	c, _ := newTestClient(t, fakeRuns(runs))

	got, err := ResolveRuns(context.Background(), c, RunFilter{})
	if err != nil {
		t.Fatalf("ResolveRuns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 runs, got %d", len(got))
	}
}

func TestResolveRuns_FiltersByProvider(t *testing.T) {
	runs := []utilsclient.Instance{
		{ID: "r_1", App: "mlx", Status: "running"},
		{ID: "r_2", App: "vllm", Status: "running"},
		{ID: "r_3", App: "MLX", Status: "stopped"}, // case-insensitive match
	}
	c, _ := newTestClient(t, fakeRuns(runs))

	got, err := ResolveRuns(context.Background(), c, RunFilter{Provider: "mlx"})
	if err != nil {
		t.Fatalf("ResolveRuns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 mlx runs, got %d", len(got))
	}
}

func TestResolveRuns_FiltersByRunIDPrefix(t *testing.T) {
	runs := []utilsclient.Instance{
		{ID: "r_8f2a9c", App: "mlx", Status: "running"},
		{ID: "r_8f3b1d", App: "mlx", Status: "running"},
		{ID: "r_91bc22", App: "mlx", Status: "running"},
	}
	c, _ := newTestClient(t, fakeRuns(runs))

	got, err := ResolveRuns(context.Background(), c, RunFilter{RunID: "r_8f"})
	if err != nil {
		t.Fatalf("ResolveRuns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 runs matching r_8f, got %d", len(got))
	}
}

func TestResolveRuns_CombinedFilters(t *testing.T) {
	runs := []utilsclient.Instance{
		{ID: "r_1", App: "mlx", Model: "llama-3.1-8b", Node: "macbook-pro", Status: "running"},
		{ID: "r_2", App: "mlx", Model: "mistral-7b", Node: "macbook-pro", Status: "running"},
		{ID: "r_3", App: "mlx", Model: "llama-3.1-8b", Node: "worker-1", Status: "running"},
	}
	c, _ := newTestClient(t, fakeRuns(runs))

	got, err := ResolveRuns(context.Background(), c, RunFilter{
		Provider: "mlx", Model: "llama-3.1-8b", Node: "macbook-pro",
	})
	if err != nil {
		t.Fatalf("ResolveRuns: %v", err)
	}
	if len(got) != 1 || got[0].ID != "r_1" {
		t.Fatalf("want only r_1, got %+v", got)
	}
}

func TestResolveRuns_NoMatchReturnsSentinel(t *testing.T) {
	c, _ := newTestClient(t, fakeRuns([]utilsclient.Instance{
		{ID: "r_1", App: "mlx", Status: "running"},
	}))

	_, err := ResolveRuns(context.Background(), c, RunFilter{Provider: "vllm"})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

func TestResolveRuns_SortRunningFirstThenByStartedDesc(t *testing.T) {
	runs := []utilsclient.Instance{
		{ID: "r_old_running", App: "mlx", Status: "running", StartedAt: "2026-04-11T08:00:00Z"},
		{ID: "r_new_stopped", App: "mlx", Status: "stopped", StartedAt: "2026-04-11T14:00:00Z"},
		{ID: "r_new_running", App: "mlx", Status: "running", StartedAt: "2026-04-11T12:00:00Z"},
	}
	c, _ := newTestClient(t, fakeRuns(runs))

	got, err := ResolveRuns(context.Background(), c, RunFilter{})
	if err != nil {
		t.Fatalf("ResolveRuns: %v", err)
	}
	want := []string{"r_new_running", "r_old_running", "r_new_stopped"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("position %d: want %s, got %s", i, id, got[i].ID)
		}
	}
}
