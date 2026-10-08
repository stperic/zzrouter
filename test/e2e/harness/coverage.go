package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// API coverage recording.
//
// The point of this file is that the coverage ledger is OBSERVED, not
// declared. A hand-maintained matrix of "which endpoints are tested"
// rots the moment someone adds a route, and it rots silently — the
// document keeps claiming coverage that no longer exists. Here the
// only way to appear covered is for a request to actually leave the
// harness and come back.
//
// Every harness request path funnels through Node.HTTPClient(), so
// wrapping that one client's RoundTripper catches the typed Client
// verbs, the SSE/streaming consumers, the jobs poller and the state
// snapshot client alike. Adding a new helper cannot forget to opt in.
//
// Paths are recorded CONCRETE (/keys/7f3a-…, not /keys/:id). Mapping
// them onto route templates is the reporter's job, against the live
// catalog — so the matcher can be improved without re-running a
// multi-hour live suite.

// coverageEnvDir names the directory that receives ledger files. When
// it is unset the recorder still counts in memory but writes nothing,
// which keeps a normal `go test` run free of stray files.
const coverageEnvDir = "ZZROUTER_E2E_COVERAGE_DIR"

// Call is one observed (method, path, status-class) triple with the
// number of times it was seen. Status is kept as a class rather than
// the exact code because the question the report answers is "did any
// test drive this route to a success, and did any drive it to an
// error" — not "which of the 4xx did we see".
type Call struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Class  string `json:"class"` // 2xx|3xx|4xx|5xx|err
	Count  int    `json:"count"`
}

type callKey struct {
	method string
	path   string
	class  string
}

var (
	coverageMu sync.Mutex
	coverage   = map[callKey]int{}
)

// recordCall notes one observed request. Safe for concurrent use; the
// e2e suites run tests in parallel.
func recordCall(method, path, class string) {
	coverageMu.Lock()
	coverage[callKey{method: method, path: path, class: class}]++
	coverageMu.Unlock()
}

// statusClass buckets a status code. A transport error (no response at
// all) is "err" — it still proves the route was reached for, which is
// worth distinguishing from never having been called.
func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "err"
	}
}

// recordingTransport counts every request that passes through it,
// captures one example per (method, path, class), and delegates. It
// never alters what the consumer observes — the wire carries every
// byte, and the tee only retains a capped, redacted copy. A coverage
// probe that changed behaviour would be worse than no probe.
type recordingTransport struct {
	base http.RoundTripper
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	reqBody, reqTruncated := captureRequestBody(req)
	reqContentType := req.Header.Get("Content-Type")

	resp, err := base.RoundTrip(req)
	class := "err"
	if err == nil && resp != nil {
		class = statusClass(resp.StatusCode)
	}
	recordCall(req.Method, req.URL.Path, class)
	if err != nil || resp == nil || resp.Body == nil {
		// A transport error has no response to exemplify.
		return resp, err
	}

	key := callKey{method: req.Method, path: req.URL.Path, class: class}
	ex := Example{
		Method:              req.Method,
		Path:                req.URL.Path,
		Status:              resp.StatusCode,
		RequestContentType:  reqContentType,
		RequestBody:         reqBody,
		RequestTruncated:    reqTruncated,
		ResponseContentType: resp.Header.Get("Content-Type"),
	}
	resp.Body = &exampleBody{
		rc: resp.Body,
		finalize: func(body string, truncated bool) {
			ex.ResponseBody = body
			ex.ResponseTruncated = truncated
			storeExample(key, ex)
		},
	}
	return resp, err
}

// coverageClient is the single shared client every harness request path
// uses. No Timeout: streaming consumers need an open read and callers
// control deadlines via context, which matches the previous behaviour
// of handing back http.DefaultClient.
var coverageClient = &http.Client{Transport: &recordingTransport{base: http.DefaultTransport}}

// plainClient serves nodes marked Unrecorded. Same no-timeout policy
// as coverageClient; the only difference is that nothing it carries
// reaches the ledger.
var plainClient = &http.Client{}

// ObservedCalls returns the calls seen so far, sorted for stable
// output. Copies under the lock so the caller can range freely.
func ObservedCalls() []Call {
	coverageMu.Lock()
	out := make([]Call, 0, len(coverage))
	for k, n := range coverage {
		out = append(out, Call{Method: k.method, Path: k.path, Class: k.class, Count: n})
	}
	coverageMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// Ledger is one suite's observations, as written to disk. Examples
// and ExamplesDropped are additive with omitempty: a reporter built
// before they existed reads the calls and ignores the rest.
type Ledger struct {
	Suite           string    `json:"suite"`
	Calls           []Call    `json:"calls"`
	Examples        []Example `json:"examples,omitempty"`
	ExamplesDropped int       `json:"examples_dropped,omitempty"`
}

// WriteCoverageLedger writes this process's observations into the
// directory named by ZZROUTER_E2E_COVERAGE_DIR, under <suite>.json.
// It is a no-op returning nil when the variable is unset, so suites can
// call it unconditionally from TestMain.
//
// suite names the file; pass the package name. Two processes writing
// the same suite name would clobber each other, which is why the
// reporter merges by file rather than appending to one shared ledger.
func WriteCoverageLedger(suite string) error {
	dir := os.Getenv(coverageEnvDir)
	if dir == "" {
		return nil
	}
	if suite == "" {
		return fmt.Errorf("coverage ledger: suite name is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("coverage ledger: mkdir %s: %w", dir, err)
	}
	exs, dropped := ObservedExamples()
	led := Ledger{Suite: suite, Calls: ObservedCalls(), Examples: exs, ExamplesDropped: dropped}
	b, err := json.MarshalIndent(led, "", "  ")
	if err != nil {
		return fmt.Errorf("coverage ledger: marshal: %w", err)
	}
	// Write-then-rename so a reporter reading the directory concurrently
	// never sees a half-written ledger.
	tmp := filepath.Join(dir, suite+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("coverage ledger: write: %w", err)
	}
	final := filepath.Join(dir, suite+".json")
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("coverage ledger: rename: %w", err)
	}
	return nil
}
