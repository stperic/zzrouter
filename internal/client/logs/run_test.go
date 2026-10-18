package logs

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	utilsclient "github.com/stperic/zzrouter/internal/client/utils"
)

func TestGetRun_ExactMatch(t *testing.T) {
	c, _ := newTestClient(t, fakeRuns([]utilsclient.Instance{
		{ID: "r_1", App: "mlx"},
		{ID: "r_2", App: "vllm"},
	}))

	got, err := GetRun(context.Background(), c, "r_1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != "r_1" {
		t.Fatalf("want r_1, got %s", got.ID)
	}
}

func TestGetRun_PrefixUnique(t *testing.T) {
	c, _ := newTestClient(t, fakeRuns([]utilsclient.Instance{
		{ID: "r_8f2a9c", App: "mlx"},
		{ID: "r_91bc22", App: "mlx"},
	}))

	got, err := GetRun(context.Background(), c, "r_8f")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != "r_8f2a9c" {
		t.Fatalf("want r_8f2a9c, got %s", got.ID)
	}
}

func TestGetRun_PrefixAmbiguous(t *testing.T) {
	c, _ := newTestClient(t, fakeRuns([]utilsclient.Instance{
		{ID: "r_8f2a9c", App: "mlx"},
		{ID: "r_8f3b1d", App: "mlx"},
	}))

	_, err := GetRun(context.Background(), c, "r_8f")
	var ambig *ErrAmbiguousPrefix
	if !errors.As(err, &ambig) {
		t.Fatalf("want ErrAmbiguousPrefix, got %v", err)
	}
	if len(ambig.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(ambig.Candidates))
	}
}

func TestGetRun_NotFound(t *testing.T) {
	c, _ := newTestClient(t, fakeRuns([]utilsclient.Instance{
		{ID: "r_1", App: "mlx"},
	}))

	_, err := GetRun(context.Background(), c, "nosuch")
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("want ErrRunNotFound, got %v", err)
	}
}

func TestGetRun_ExactMatchBeatsPrefix(t *testing.T) {
	// r_8 is an exact match; r_8f2a9c has the same prefix.
	// Exact match must win.
	c, _ := newTestClient(t, fakeRuns([]utilsclient.Instance{
		{ID: "r_8f2a9c", App: "mlx"},
		{ID: "r_8", App: "vllm"},
	}))

	got, err := GetRun(context.Background(), c, "r_8")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != "r_8" {
		t.Fatalf("want exact match r_8, got %s", got.ID)
	}
}

func TestRunLookupsExcludeRecordOnly(t *testing.T) {
	c, _ := newTestClient(t, fakeRuns([]utilsclient.Instance{
		{ID: "pin-id", Model: "pinned", RecordOnly: true, Status: "stopped"},
		{ID: "run-id", Model: "live", Status: "running"},
	}))
	for _, prefix := range []string{"pin-id", "pin"} {
		_, err := GetRun(t.Context(), c, prefix)
		require.ErrorIs(t, err, ErrRunNotFound)
		require.ErrorContains(t, err, "pin with no run has no logs")
		_, err = ResolveRuns(t.Context(), c, RunFilter{RunID: prefix})
		require.ErrorIs(t, err, ErrNoMatch)
	}
	_, err := ResolveRuns(t.Context(), c, RunFilter{Model: "pinned"})
	require.ErrorIs(t, err, ErrNoMatch)
	all, err := ResolveRuns(t.Context(), c, RunFilter{})
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, "run-id", all[0].ID)
}
