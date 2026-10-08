// Package assert is the *testing.T-fatal shim layer over the
// error-returning harness core. The harness library itself never
// imports testing — these helpers exist so tests don't repeat the
// `if err != nil { t.Fatal(err) }` boilerplate.
package assert

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/test/e2e/harness"

	"github.com/stperic/zzrouter/pkg/utils"
)

// MustOK fails the test if err is non-nil.
func MustOK(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// MustProvision provisions cluster and registers Teardown on cleanup.
// Returns the cluster so the call site reads as one expression.
func MustProvision(t testing.TB, cfg *harness.Config) *harness.Cluster {
	t.Helper()
	cluster, err := harness.New(cfg)
	MustOK(t, err)
	MustOK(t, cluster.Provision(context.Background()))
	t.Cleanup(func() {
		// Teardown errors are logged but not fatal — the test already passed
		// or failed by this point and we don't want to mask the original cause.
		if err := cluster.Teardown(context.Background()); err != nil {
			t.Logf("teardown: %v", err)
		}
	})
	return cluster
}

// Status asserts the response status is exactly one of want.
func Status(t testing.TB, r harness.Response, want ...int) {
	t.Helper()
	for _, w := range want {
		if r.Status == w {
			return
		}
	}
	t.Fatalf("status %d not in %v\nbody: %s", r.Status, want, r.Body)
}

// MustJSON decodes r.Body into a fresh T and returns it. Fails the
// test on decode error or empty body.
func MustJSON[T any](t testing.TB, r harness.Response) T {
	t.Helper()
	var v T
	if len(r.Body) == 0 {
		t.Fatal("response body empty")
	}
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("decode JSON into %T: %v\nbody: %s", v, err, r.Body)
	}
	return v
}

// JobID asserts r is a 202 with a valid job_id and returns it.
func JobID(t testing.TB, r harness.Response) string {
	t.Helper()
	id, err := r.JobID()
	if err != nil {
		t.Fatalf("expected 202+job_id: %v\nstatus=%d body=%s", err, r.Status, r.Body)
	}
	return id
}

// Problem asserts r carries an RFC 9457 problem+json envelope and
// returns it. Optionally also asserts the closed-enum code field.
func Problem(t testing.TB, r harness.Response, wantCode ...string) harness.Problem {
	t.Helper()
	p, err := r.Problem()
	if err != nil {
		t.Fatalf("expected problem+json: %v\nstatus=%d ct=%q body=%s",
			err, r.Status, r.Headers.Get("Content-Type"), r.Body)
	}
	if len(wantCode) > 0 && p.Code != wantCode[0] {
		t.Fatalf("problem code: got %q want %q\ndetail=%q", p.Code, wantCode[0], p.Detail)
	}
	return p
}

// JobOK runs Jobs.Wait against jobID under ctx and fails the test
// unless the outcome is phase=done. Pass ctx so test deadlines + the
// Jobs internal timeout both apply.
func JobOK(t testing.TB, ctx context.Context, j *harness.Jobs, jobID string) harness.JobOutcome {
	t.Helper()
	out, err := j.Wait(ctx, jobID)
	MustOK(t, err)
	if !out.Succeeded() {
		t.Fatalf("job %s: status=%q error=%q", jobID, out.Status, out.Error)
	}
	return out
}

// Headers asserts every named header is non-empty in r. Useful for the
// Ring 1 envelope check (X-Request-ID, X-API-Version on /zzrouter/v1).
func Headers(t testing.TB, r harness.Response, names ...string) {
	t.Helper()
	for _, name := range names {
		if r.Headers.Get(name) == "" {
			t.Errorf("missing header %q", name)
		}
	}
}

// ContentType asserts the Content-Type header has the given prefix.
func ContentType(t testing.TB, r harness.Response, prefix string) {
	t.Helper()
	got := r.Headers.Get("Content-Type")
	if !strings.HasPrefix(got, prefix) {
		t.Errorf("Content-Type=%q want prefix %q", got, prefix)
	}
}

// dumpTimeout caps the auto-dump on test failure. Generous because
// log fetch over SSH may take ~30s per node and we'd rather wait
// than truncate forensic data.
const dumpTimeout = 60 * time.Second

// Diagnostics binds a *harness.Diagnostics to the test and registers
// a t.Cleanup that auto-dumps when the test failed. Tests call the
// returned *Diagnostics's Record* methods during execution to pin
// the request/response/SSE state that produced the failure.
//
// Returns nil if cluster is nil — caller's choice whether to noop
// the Record* calls or skip them entirely.
func Diagnostics(t testing.TB, cluster *harness.Cluster) *harness.Diagnostics {
	t.Helper()
	if cluster == nil {
		return nil
	}
	d := harness.NewDiagnostics(cluster)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), dumpTimeout)
		defer cancel()
		dest, err := d.DumpAccumulated(ctx, t.Name(), utils.NowUTC())
		if err != nil {
			t.Logf("diagnostics dump: %v", err)
			return
		}
		t.Logf("diagnostics: %s", dest)
	})
	return d
}
