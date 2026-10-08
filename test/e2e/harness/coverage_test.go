package harness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// resetCoverage clears the process-global ledger so tests don't observe
// each other's traffic. The recorder is deliberately global (one shared
// client for the whole suite), so tests that assert on it must isolate.
func resetCoverage(t *testing.T) {
	t.Helper()
	coverageMu.Lock()
	coverage = map[callKey]int{}
	examples = map[callKey]Example{}
	examplesDropped = 0
	coverageMu.Unlock()
}

func TestRecordingTransportRecordsMethodPathAndClass(t *testing.T) {
	resetCoverage(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &http.Client{Transport: &recordingTransport{base: http.DefaultTransport}}
	for _, p := range []string{"/ok", "/ok", "/missing"} {
		resp, err := c.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		resp.Body.Close()
	}

	got := map[string]Call{}
	for _, c := range ObservedCalls() {
		got[c.Method+" "+c.Path+" "+c.Class] = c
	}
	if c, ok := got["GET /ok 2xx"]; !ok || c.Count != 2 {
		t.Errorf("GET /ok 2xx = %+v, want count 2", c)
	}
	if _, ok := got["GET /missing 4xx"]; !ok {
		t.Error("404 must be recorded as a 4xx, not dropped")
	}
}

// The query string must not leak into the ledger: /keys?limit=1 and
// /keys?limit=2 are the same route, and splitting them would inflate
// the path list with duplicates the matcher then has to re-merge.
func TestRecordingTransportIgnoresQueryString(t *testing.T) {
	resetCoverage(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &http.Client{Transport: &recordingTransport{base: http.DefaultTransport}}
	for _, q := range []string{"?limit=1", "?limit=2"} {
		resp, err := c.Get(srv.URL + "/keys" + q)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
	}

	calls := ObservedCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d distinct calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].Path != "/keys" || calls[0].Count != 2 {
		t.Errorf("got %+v, want /keys with count 2", calls[0])
	}
}

// A transport error still proves the route was aimed at, which is worth
// distinguishing from never having been called.
func TestRecordingTransportRecordsTransportError(t *testing.T) {
	resetCoverage(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c := &http.Client{Transport: &recordingTransport{base: http.DefaultTransport}}
	resp, err := c.Get(url + "/dead")
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected a transport error")
	}
	calls := ObservedCalls()
	if len(calls) != 1 || calls[0].Class != "err" {
		t.Fatalf("got %+v, want one call classed err", calls)
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{
		200: "2xx", 204: "2xx", 301: "3xx",
		400: "4xx", 404: "4xx", 500: "5xx", 503: "5xx", 0: "err",
	}
	for code, want := range cases {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestWriteCoverageLedgerNoopWhenUnset(t *testing.T) {
	resetCoverage(t)
	t.Setenv(coverageEnvDir, "")
	if err := WriteCoverageLedger("suite"); err != nil {
		t.Fatalf("expected a silent no-op, got %v", err)
	}
}

func TestWriteCoverageLedgerRoundTrips(t *testing.T) {
	resetCoverage(t)
	dir := t.TempDir()
	t.Setenv(coverageEnvDir, dir)
	recordCall("GET", "/zzrouter/v1/nodes", "2xx")
	recordCall("GET", "/zzrouter/v1/nodes", "2xx")
	recordCall("DELETE", "/zzrouter/v1/keys/abc", "4xx")

	if err := WriteCoverageLedger("ring2_admin"); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ring2_admin.json"))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var led Ledger
	if err := json.Unmarshal(b, &led); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if led.Suite != "ring2_admin" {
		t.Errorf("suite = %q", led.Suite)
	}
	if len(led.Calls) != 2 {
		t.Fatalf("calls = %+v, want 2 distinct", led.Calls)
	}
	// Sorted by path: /keys/abc before /nodes.
	if led.Calls[0].Path != "/zzrouter/v1/keys/abc" {
		t.Errorf("calls not sorted: %+v", led.Calls)
	}
	if led.Calls[1].Count != 2 {
		t.Errorf("repeat call not aggregated: %+v", led.Calls[1])
	}
	// No .tmp file may survive a successful write.
	if _, err := os.Stat(filepath.Join(dir, "ring2_admin.json.tmp")); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

func TestWriteCoverageLedgerRejectsEmptySuite(t *testing.T) {
	resetCoverage(t)
	t.Setenv(coverageEnvDir, t.TempDir())
	if err := WriteCoverageLedger(""); err == nil {
		t.Error("expected an error for an empty suite name")
	}
}

// Node.HTTPClient is the one choke point the whole ledger depends on.
// If it ever hands back an unwrapped client again, coverage silently
// reads zero, so pin it.
func TestNodeHTTPClientIsRecording(t *testing.T) {
	n := &Node{name: "coord", baseURL: "http://127.0.0.1:1"}
	if _, ok := n.HTTPClient().Transport.(*recordingTransport); !ok {
		t.Fatal("Node.HTTPClient must return the coverage-recording client")
	}
}

// The other half of the choke point: a node marked Unrecorded must
// stay out of the ledger entirely. The ledger's worth comes from being
// evidence about the system under test, and a meta-test's stub answers
// for a route it does not implement — one such stub reached
// docs/api_examples.md as the published shape of GET /zzrouter/v1/keys.
func TestUnrecordedNodeStaysOutOfTheLedger(t *testing.T) {
	resetCoverage(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	n, err := NewNode("fake", srv.URL, RoleCoordinator, nil, NodeKeys{Admin: "k"}, Unrecorded())
	if err != nil {
		t.Fatal(err)
	}
	if _, recording := n.HTTPClient().Transport.(*recordingTransport); recording {
		t.Fatal("an Unrecorded node must not hand back the recording client")
	}
	resp, err := n.HTTPClient().Get(srv.URL + "/zzrouter/v1/keys")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if calls := ObservedCalls(); len(calls) != 0 {
		t.Errorf("ledger recorded %d call(s) from a test double: %+v", len(calls), calls)
	}
	if got, _ := ObservedExamples(); len(got) != 0 {
		t.Errorf("captured %d example(s) from a test double: %+v", len(got), got)
	}
}
