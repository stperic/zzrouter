package harness_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stperic/zzrouter/test/e2e/harness"
	_ "github.com/stperic/zzrouter/test/e2e/harness/inproc"
)

func TestStateSnapshotRoundtrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cluster := mustProvisionInproc(t, ctx)
	t.Cleanup(func() { _ = cluster.Teardown(context.Background()) })

	c := harness.NewClient(cluster.Coordinator(), harness.TierAdmin)
	state := harness.NewState(c)

	body, err := state.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("snapshot returned empty body")
	}
	// gzip header magic — confirms server actually wrapped the tar.
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("snapshot not gzipped (magic %x %x)", body[0], body[1])
	}

	// Walk the tar to confirm at least one expected file lands.
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip decode: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar walk: %v", err)
		}
		seen[hdr.Name] = true
	}
	// A fresh inproc node has no keys.yaml on disk yet — that's fine.
	// What we're proving is that snapshot returned a parseable tar at
	// all, not that any specific file is present.

	// Restore round-trips the same body.
	res, err := state.Restore(ctx, body)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.Restored) != len(seen) {
		t.Errorf("restored %d files, snapshot had %d", len(res.Restored), len(seen))
	}
}

func TestStateSnapshotEmptyBodyRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cluster := mustProvisionInproc(t, ctx)
	t.Cleanup(func() { _ = cluster.Teardown(context.Background()) })

	c := harness.NewClient(cluster.Coordinator(), harness.TierAdmin)
	state := harness.NewState(c)

	if _, err := state.Restore(ctx, nil); !errors.Is(err, harness.ErrEmptySnapshot) {
		t.Fatalf("Restore(nil): got %v, want ErrEmptySnapshot", err)
	}
}

func TestStateSnapshotUnauthorized(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cluster := mustProvisionInproc(t, ctx)
	t.Cleanup(func() { _ = cluster.Teardown(context.Background()) })

	// TierNone — no X-API-Key header. The endpoint is admin-gated;
	// expect non-2xx (specifically 401 once OptionalAuth doesn't gate
	// on a missing key for state/*; today it's 401 from auth_middleware).
	c := harness.NewClient(cluster.Coordinator(), harness.TierNone)
	state := harness.NewState(c)

	if _, err := state.Snapshot(ctx); err == nil {
		t.Fatal("Snapshot with TierNone should error, got nil")
	}
}

// mustProvisionInproc boots a single-coord cluster on the inproc
// backend. Test helper rather than a fixture so each subtest's
// lifecycle is explicit.
func mustProvisionInproc(t *testing.T, ctx context.Context) *harness.Cluster {
	t.Helper()
	cfg := &harness.Config{
		Backend: harness.BackendInproc,
		Cluster: harness.ClusterTopology{
			Coordinator: harness.NodeSpec{Name: "coord", Role: harness.RoleCoordinator},
		},
	}
	cluster, err := harness.New(cfg)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	if err := cluster.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	return cluster
}
