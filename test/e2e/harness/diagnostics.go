package harness

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// diagnosticsLogWindow is the wall-clock span the LogFetcher pulls
// around the failure timestamp. ±30s catches both the request that
// blew up and the immediate aftermath (settlement, follow-on alerts)
// without dragging in unrelated noise from a long-running test run.
const diagnosticsLogWindow = 30 * time.Second

// diagnosticsArtifactRoot is the relative path the harness writes
// failure dumps under by default. Callers can override per-run via
// Diagnostics.WithRoot — used by CI to pin under $GITHUB_WORKSPACE.
const diagnosticsArtifactRoot = "test/e2e/_artifacts"

// LogFetcher pulls server logs around a failure timestamp from a
// single Node. Backend-specific: inproc has no logs to pull (server
// runs in the test process); the SSH backend shells out to journalctl.
//
// Implementations must be safe for concurrent use — multiple test
// failures may race the same fetcher.
type LogFetcher interface {
	Name() string
	FetchLogs(ctx context.Context, n *Node, around time.Time, window time.Duration) ([]byte, error)
}

// nopLogFetcher is the default for backends with no remote-log story.
// Returns an empty body + nil error so diagnostics still produces a
// tarball with the rest of the artifacts.
type nopLogFetcher struct{}

func (nopLogFetcher) Name() string { return "none" }
func (nopLogFetcher) FetchLogs(context.Context, *Node, time.Time, time.Duration) ([]byte, error) {
	return nil, nil
}

// Diagnostics collects failure artifacts into per-test directories.
// Wired into harness/assert helpers via DumpOnFailure; tests that want
// to drive it directly call Dump.
//
// Doubles as a Recorder: tests call Record* during execution to
// accumulate request/response/SSE state into an internal Bundle;
// on failure the Bundle is what Dump writes. Single-instance per
// test (not per cluster) — RecordOnFailure attaches one to t.Cleanup.
//
// Concurrent-use contract:
//   - Setup methods (WithRoot, SetLogFetcher) before any Record* call
//   - Record* methods during the test (mutex-guarded)
//   - Dump after the test returns (typically via t.Cleanup)
type Diagnostics struct {
	cluster  *Cluster
	root     string
	runID    string
	fetchers map[string]LogFetcher // node-name → fetcher; default = nop

	mu     sync.Mutex
	bundle Bundle // accumulated via Record*; consumed by DumpAccumulated
}

// NewDiagnostics binds a Diagnostics to a Cluster. Run-id resolves
// from $GITHUB_RUN_ID (CI) or a UTC timestamp + 6-byte random suffix
// (local). Output goes under <root>/<run-id>/<test-name>/ — root
// defaults to test/e2e/_artifacts under the repo cwd.
func NewDiagnostics(c *Cluster) *Diagnostics {
	return &Diagnostics{
		cluster:  c,
		root:     diagnosticsArtifactRoot,
		runID:    resolveRunID(),
		fetchers: map[string]LogFetcher{},
	}
}

// WithRoot overrides the artifact root. Returns the receiver for
// chaining; safe to call before Dump.
func (d *Diagnostics) WithRoot(root string) *Diagnostics {
	d.root = root
	return d
}

// SetLogFetcher installs a per-node LogFetcher. SSH-backed nodes
// register a journalctl fetcher; inproc nodes skip the call (the
// nop fetcher is the implicit default).
func (d *Diagnostics) SetLogFetcher(nodeName string, f LogFetcher) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fetchers[nodeName] = f
}

// fetcherFor returns the LogFetcher for a node, or nopLogFetcher when
// none is registered.
func (d *Diagnostics) fetcherFor(nodeName string) LogFetcher {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.fetchers[nodeName]; ok {
		return f
	}
	return nopLogFetcher{}
}

// RecordRequest pins the most recent request — tests with many
// requests should call this once for the one that produced the
// assertion failure.
func (d *Diagnostics) RecordRequest(req *RequestRecord) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bundle.Request = req
}

// RecordResponse pins the most recent response.
func (d *Diagnostics) RecordResponse(resp *Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bundle.Response = resp
}

// RecordJobSSE accumulates raw SSE per job id. Repeated id overwrites.
func (d *Diagnostics) RecordJobSSE(jobID string, raw []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bundle.JobsSSE == nil {
		d.bundle.JobsSSE = map[string][]byte{}
	}
	d.bundle.JobsSSE[jobID] = raw
}

// RecordStateTar pins a gzipped state snapshot for populated/upgrade
// suites that want the pre-failure state preserved.
func (d *Diagnostics) RecordStateTar(tar []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bundle.StateTar = tar
}

// SetExtra merges into the meta.json Extra map.
func (d *Diagnostics) SetExtra(k string, v any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bundle.Extra == nil {
		d.bundle.Extra = map[string]any{}
	}
	d.bundle.Extra[k] = v
}

// DumpAccumulated snapshots the accumulated bundle and dumps it.
// Maps are deep-copied under the lock so concurrent Record* calls
// during dump don't race the iteration in Dump (Go's runtime fatals
// on concurrent map iteration).
func (d *Diagnostics) DumpAccumulated(ctx context.Context, testName string, failedAt time.Time) (string, error) {
	d.mu.Lock()
	b := d.bundle
	if b.JobsSSE != nil {
		jobs := make(map[string][]byte, len(b.JobsSSE))
		for k, v := range b.JobsSSE {
			jobs[k] = v
		}
		b.JobsSSE = jobs
	}
	if b.Extra != nil {
		extra := make(map[string]any, len(b.Extra))
		for k, v := range b.Extra {
			extra[k] = v
		}
		b.Extra = extra
	}
	d.mu.Unlock()
	b.TestName = testName
	b.FailedAt = failedAt
	return d.Dump(ctx, b)
}

// Bundle is the input to Dump — the per-test context the harness
// captured on failure. Every field is optional; Dump writes only what
// it has.
type Bundle struct {
	TestName string
	FailedAt time.Time
	GitSHA   string

	// Optional — only populated by tests that exercised these surfaces.
	Request  *RequestRecord    // last HTTP request issued before failure
	Response *Response         // matching response
	JobsSSE  map[string][]byte // job_id → raw SSE bytes (Jobs.RawSSE)
	StateTar []byte            // gzipped-tar from State.Snapshot, if available

	// Extra is freeform metadata the test wants pinned in meta.json.
	Extra map[string]any
}

// RequestRecord is the on-the-wire copy of the request that triggered
// the failure. The harness Client doesn't auto-record (overhead);
// tests opt in by stuffing the record into Bundle.Request.
type RequestRecord struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

// CurlLine renders the request as a single-line curl command —
// drop-in reproducer for an operator. Authorization headers are
// preserved as-is; the artifact directory is operator-readable so
// secrets in those headers are already in scope.
func (r *RequestRecord) CurlLine() string {
	var sb strings.Builder
	sb.WriteString("curl -sS -X ")
	sb.WriteString(r.Method)
	for k, vs := range r.Headers {
		for _, v := range vs {
			sb.WriteString(" -H ")
			sb.WriteString(shellQuote(k + ": " + v))
		}
	}
	if len(r.Body) > 0 {
		sb.WriteString(" --data-binary ")
		sb.WriteString(shellQuote(string(r.Body)))
	}
	sb.WriteString(" ")
	sb.WriteString(shellQuote(r.URL))
	return sb.String()
}

// Dump writes the bundle to disk under <root>/<run-id>/<test>/. Logs
// are fetched in parallel from each node + included in the directory.
// Returns the absolute artifact directory + the first error seen
// (subsequent errors are logged via the returned diagnostic notes).
func (d *Diagnostics) Dump(ctx context.Context, b Bundle) (string, error) {
	if b.TestName == "" {
		return "", errors.New("diagnostics: bundle.TestName required")
	}
	if b.FailedAt.IsZero() {
		b.FailedAt = utils.NowUTC()
	}

	dest := filepath.Join(d.root, d.runID, sanitizeName(b.TestName))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("mkdir artifact dir: %w", err)
	}

	notes := []string{}

	if b.Request != nil {
		if err := os.WriteFile(filepath.Join(dest, "request.curl"), []byte(b.Request.CurlLine()+"\n"), 0o644); err != nil {
			notes = append(notes, "request.curl: "+err.Error())
		}
	}
	if b.Response != nil {
		if err := writeResponseJSON(dest, b.Response); err != nil {
			notes = append(notes, "response.json: "+err.Error())
		}
	}
	if len(b.JobsSSE) > 0 {
		manifest := writeJobsSSE(dest, b.JobsSSE, &notes)
		if len(manifest) > 0 {
			// Sort keys so the manifest diffs stably across runs.
			keys := make([]string, 0, len(manifest))
			for k := range manifest {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			ordered := make([][2]string, len(keys))
			for i, k := range keys {
				ordered[i] = [2]string{k, manifest[k]}
			}
			out, _ := json.MarshalIndent(orderedManifest(ordered), "", "  ")
			if err := os.WriteFile(filepath.Join(dest, "jobs", "manifest.json"), out, 0o644); err != nil {
				notes = append(notes, "jobs/manifest.json: "+err.Error())
			}
		}
	}
	if len(b.StateTar) > 0 {
		if err := os.WriteFile(filepath.Join(dest, "state-snapshot.tar.gz"), b.StateTar, 0o600); err != nil {
			notes = append(notes, "state-snapshot.tar.gz: "+err.Error())
		}
	}

	d.collectLogs(ctx, dest, b.FailedAt, &notes)

	backendName := "unknown"
	if d.cluster != nil && d.cluster.backend != nil {
		backendName = d.cluster.backend.Name()
	}
	meta := map[string]any{
		"test":        b.TestName,
		"failed_at":   b.FailedAt.UTC().Format(time.RFC3339Nano),
		"git_sha":     b.GitSHA,
		"run_id":      d.runID,
		"backend":     backendName,
		"log_window":  diagnosticsLogWindow.String(),
		"diagnostics": notes,
	}
	for k, v := range b.Extra {
		meta[k] = v
	}
	metaBytes, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dest, "meta.json"), metaBytes, 0o644); err != nil {
		return dest, fmt.Errorf("meta.json: %w", err)
	}
	return dest, nil
}

// collectLogs fans out FetchLogs across coord + workers in parallel,
// writes each node's blob to server.<role>.<name>.log, records
// per-node fetch errors in notes. Tolerant of half-provisioned
// clusters — if Provision panicked mid-test, the dump still runs.
func (d *Diagnostics) collectLogs(ctx context.Context, dest string, around time.Time, notes *[]string) {
	if d.cluster == nil {
		return
	}
	var nodes []*Node
	if c := d.cluster.Coordinator(); c != nil {
		nodes = append(nodes, c)
	}
	nodes = append(nodes, d.cluster.Workers()...)

	type result struct {
		filename string
		body     []byte
		err      error
	}
	results := make(chan result, len(nodes))
	var wg sync.WaitGroup
	for _, n := range nodes {
		if n == nil {
			continue
		}
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()
			f := d.fetcherFor(n.Name())
			body, err := f.FetchLogs(ctx, n, around, diagnosticsLogWindow)
			results <- result{
				filename: fmt.Sprintf("server.%s.%s.log", string(n.Role()), n.Name()),
				body:     body,
				err:      err,
			}
		}(n)
	}
	go func() { wg.Wait(); close(results) }()

	for r := range results {
		if r.err != nil {
			*notes = append(*notes, r.filename+": fetch: "+r.err.Error())
			continue
		}
		if len(r.body) == 0 {
			continue
		}
		if err := os.WriteFile(filepath.Join(dest, r.filename), r.body, 0o644); err != nil {
			*notes = append(*notes, r.filename+": write: "+err.Error())
		}
	}
}

// orderedManifest serializes a sorted [][filename,jobID] list as a
// JSON object with insertion-ordered keys. Encoding via [][2]string
// produces an array, not an object — wrap into a custom marshaler.
type orderedManifest [][2]string

func (o orderedManifest) MarshalJSON() ([]byte, error) {
	var sb []byte
	sb = append(sb, '{')
	for i, kv := range o {
		if i > 0 {
			sb = append(sb, ',')
		}
		k, _ := json.Marshal(kv[0])
		v, _ := json.Marshal(kv[1])
		sb = append(sb, k...)
		sb = append(sb, ':')
		sb = append(sb, v...)
	}
	sb = append(sb, '}')
	return sb, nil
}

// writeJobsSSE writes one .sse file per job_id under dest/jobs/. When
// two job_ids sanitize to the same filesystem name, the second one
// gets a `-N` suffix. Returns a manifest mapping the on-disk filename
// to the original job_id so triage can recover the unsanitized id.
func writeJobsSSE(dest string, sse map[string][]byte, notes *[]string) map[string]string {
	jobsDir := filepath.Join(dest, "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		*notes = append(*notes, "jobs: mkdir: "+err.Error())
		return nil
	}
	manifest := make(map[string]string, len(sse))
	used := make(map[string]bool, len(sse))
	for jobID, raw := range sse {
		base := sanitizeName(jobID)
		name := base + ".sse"
		for n := 1; used[name]; n++ {
			name = fmt.Sprintf("%s-%d.sse", base, n)
		}
		used[name] = true
		if err := os.WriteFile(filepath.Join(jobsDir, name), raw, 0o644); err != nil {
			*notes = append(*notes, "jobs/"+name+": "+err.Error())
			continue
		}
		manifest[name] = jobID
	}
	return manifest
}

// writeResponseJSON serializes a Response (status + headers + body)
// as a single JSON file. Body is included verbatim when valid JSON,
// otherwise as a base64 string under body_b64 — preserves binary
// payloads (gzipped state, SSE streams) without lossy escaping.
func writeResponseJSON(dest string, r *Response) error {
	doc := map[string]any{
		"status":     r.Status,
		"headers":    r.Headers,
		"request_id": r.RequestID,
	}
	if json.Valid(r.Body) {
		doc["body"] = json.RawMessage(r.Body)
	} else {
		doc["body_b64"] = encodeB64(r.Body)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "response.json"), out, 0o644)
}

// resolveRunID picks $GITHUB_RUN_ID first (so CI artifacts can be
// cross-referenced with the workflow run) else a sortable UTC
// timestamp + 48-bit random suffix for local triage. Cached per
// process so every Diagnostics in one `go test` shares the same
// run dir — sub-tests aren't scattered across N timestamped roots.
func resolveRunID() string {
	runIDOnce.Do(func() {
		if v := os.Getenv("GITHUB_RUN_ID"); v != "" {
			runIDValue = "ci-" + v
			return
		}
		var b [6]byte
		_, _ = rand.Read(b[:])
		runIDValue = utils.NowUTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
	})
	return runIDValue
}

var (
	runIDOnce  sync.Once
	runIDValue string
)

// sanitizeName makes a string safe for use as a filesystem path
// component: keeps ASCII alphanumerics + dash + underscore, replaces
// everything else with '_'. Truncated names get an 8-char hash
// suffix so two long inputs sharing a prefix don't collide. Pure-dot
// outputs (".", "..") are rejected to prevent path traversal.
func sanitizeName(s string) string {
	const maxLen = 120
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
		if sb.Len() >= maxLen {
			break
		}
	}
	out := sb.String()
	if out == "" || out == "." || out == ".." {
		return "_"
	}
	if len(s) > maxLen {
		// Anti-collision suffix when we truncated. Hash the full
		// input — two long names sharing a prefix get distinct dirs.
		sum := sha256.Sum256([]byte(s))
		out = out[:maxLen-9] + "-" + hex.EncodeToString(sum[:4])
	}
	return out
}

// shellQuote single-quotes a string the bourne way: any embedded
// single quote becomes '\” so the curl reproducer is safe to paste
// into bash without re-escaping body content.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// encodeB64 wraps stdlib base64 so a future swap to streaming
// encode (for huge bodies) doesn't ripple through callers.
func encodeB64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// ToTarball packages every file under dir into a gzipped tar. Used by
// CI runners that want to upload a single artifact per failure.
// dir is walked recursively; symlinks are skipped.
func ToTarball(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
