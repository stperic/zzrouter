package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// CassetteMode controls how cloud HTTP egress is recorded/replayed.
//
//   - CassetteReplay (default in CI): only cassette hits succeed; a miss
//     errors instead of falling through to the live network. Keeps CI
//     deterministic + free of cloud credit burn.
//   - CassetteRecord: forward each request live, persist response to disk.
//     Run locally with real keys when adding a new cloud test case.
//   - CassetteLive: pass-through, no persistence. Ad-hoc debugging only.
type CassetteMode string

const (
	CassetteReplay CassetteMode = "replay"
	CassetteRecord CassetteMode = "record"
	CassetteLive   CassetteMode = "live"
)

// ErrCassetteNotImplemented is reserved for future modes; no current
// path returns it (replay landed in the cassette arc's commit 3).
var ErrCassetteNotImplemented = errors.New("cassette mode not implemented yet")

// ErrCassetteMiss is returned by replay handlers when an incoming
// request doesn't match any recorded interaction. The 599 status code
// surfaces the error visibly in test logs without being mistaken for a
// real upstream 4xx/5xx.
var ErrCassetteMiss = errors.New("cassette miss: request hash not found in recording")

// ErrCassetteUnusedInteractions is returned by Close in replay mode
// when one or more recorded interactions were never matched. Catches
// drift where a test stops making a call but the cassette still has it.
var ErrCassetteUnusedInteractions = errors.New("cassette had unused interactions on close")

// redactedSecretsHeaders is the set of request headers that get
// rewritten to "REDACTED" before persistence. Authorization and
// X-API-Key carry cloud credentials; Cookie is included for completeness.
// Cassettes live alongside test code and may be reviewed in PRs, so the
// list is intentionally conservative.
var redactedRequestHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"X-Api-Key":           true,
	"Cookie":              true,
}

// redactedResponseHeaders strips Set-Cookie from persisted responses;
// real cloud APIs occasionally pin sessions on cookies that would be
// stale by the time replay happens.
var redactedResponseHeaders = map[string]bool{
	"Set-Cookie": true,
}

// CassetteServer is a localhost HTTP stub that the cluster's cloud
// providers dial in place of the real upstream. The site overlay
// rewrites runtime.endpoint at provision time so the spawned coord
// hits this server transparently.
//
// Cloud egress is generic OpenAI-protocol — there is intentionally no
// provider-specific knowledge here. A cassette captures wire bytes,
// nothing more. Header redaction is applied symmetrically on persist.
//
// Lifecycle: NewCassetteServer starts the listener immediately so URL()
// is valid before the cluster spawns. Close() drains the listener and
// (in record mode) flushes pending interactions to Path.
type CassetteServer struct {
	Mode     CassetteMode
	Path     string   // on-disk cassette file (record/replay)
	Upstream *url.URL // real upstream URL (record/live)

	mu           sync.Mutex
	interactions []Interaction // record: appended on response; replay: loaded at construct
	// replay-only:
	hashIndex map[string][]int // body_hash → ordered indices into interactions
	cursor    map[string]int   // body_hash → next unconsumed slot
	consumed  []bool           // interaction-index → matched at least once

	// srv is set once by NewCassetteServer and cleared by Close. Both
	// happen on goroutines that don't share cs.mu (Close after the
	// listener drains; URL anywhere). Plain field would race; atomic
	// pointer keeps load + store concurrency-safe without enlarging
	// the lock's scope.
	srv    atomic.Pointer[httptest.Server]
	client *http.Client // outbound client for record/live forwarding
}

// Interaction is one recorded request/response round-trip. Fields are
// public to keep the on-disk JSON shape obvious from the type.
type Interaction struct {
	Request  RecordedRequest  `json:"request"`
	Response RecordedResponse `json:"response"`
}

// RecordedRequest captures the wire bytes of a request after redaction.
// BodyHash is sha256(method | path | sorted-query | body) and is the
// primary lookup key during replay.
type RecordedRequest struct {
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	Query    string              `json:"query,omitempty"`
	Headers  map[string][]string `json:"headers,omitempty"`
	Body     string              `json:"body,omitempty"`
	BodyHash string              `json:"body_hash"`
}

// RecordedResponse captures the wire bytes of a response after redaction.
// Body is stored as a string; SSE framing (\n\n) is preserved verbatim
// so replay yields a structurally-identical stream.
type RecordedResponse struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body,omitempty"`
}

// NewCassetteServer constructs a cassette stub and starts the listener.
// The caller is responsible for invoking Close on cleanup.
//
//   - Live: WithUpstream(url) required.
//   - Record: WithUpstream(url) + WithPath(file) required. File parent dir
//     is created on demand; existing file is overwritten on Close.
//   - Replay: WithPath(file) required. File is read once at construction;
//     subsequent disk edits are ignored.
func NewCassetteServer(mode CassetteMode, opts ...CassetteOption) (*CassetteServer, error) {
	cs := &CassetteServer{Mode: mode}
	for _, opt := range opts {
		if err := opt(cs); err != nil {
			return nil, err
		}
	}
	switch mode {
	case CassetteLive:
		if cs.Upstream == nil {
			return nil, fmt.Errorf("CassetteLive requires WithUpstream(...)")
		}
		cs.client = &http.Client{}
	case CassetteRecord:
		if cs.Path == "" {
			return nil, fmt.Errorf("CassetteRecord requires WithPath(...)")
		}
		if cs.Upstream == nil {
			return nil, fmt.Errorf("CassetteRecord requires WithUpstream(...)")
		}
		cs.client = &http.Client{}
	case CassetteReplay:
		if cs.Path == "" {
			return nil, fmt.Errorf("CassetteReplay requires WithPath(...)")
		}
		if err := cs.loadCassette(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown cassette mode: %q", mode)
	}
	cs.srv.Store(httptest.NewServer(cs))
	return cs, nil
}

// CassetteOption configures a CassetteServer at construction time.
// Options compose; later options win on collisions.
type CassetteOption func(*CassetteServer) error

// WithUpstream sets the real upstream URL for record/live modes.
// Replay ignores this — cassette interactions are self-contained.
func WithUpstream(rawURL string) CassetteOption {
	return func(cs *CassetteServer) error {
		u, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("WithUpstream: parse %q: %w", rawURL, err)
		}
		cs.Upstream = u
		return nil
	}
}

// WithPath sets the on-disk cassette file path for record/replay modes.
func WithPath(path string) CassetteOption {
	return func(cs *CassetteServer) error {
		cs.Path = path
		return nil
	}
}

// URL returns the local listener URL. Pass this into the site overlay
// so the spawned coord dials the cassette in place of the real upstream.
// Empty until NewCassetteServer succeeds and after Close.
func (cs *CassetteServer) URL() string {
	if cs == nil {
		return ""
	}
	srv := cs.srv.Load()
	if srv == nil {
		return ""
	}
	return srv.URL
}

// Interactions returns a snapshot of recorded interactions. Useful in
// tests to assert N requests were captured without reading disk.
func (cs *CassetteServer) Interactions() []Interaction {
	if cs == nil {
		return nil
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]Interaction, len(cs.interactions))
	copy(out, cs.interactions)
	return out
}

// Close drains the listener and (in record mode) flushes interactions
// to Path. In replay mode, returns ErrCassetteUnusedInteractions if any
// recorded interaction was never matched — drift catcher. First call
// owns the lifecycle; subsequent calls are no-ops (idempotent).
// Test code should wire this through t.Cleanup so panics still flush.
func (cs *CassetteServer) Close() error {
	if cs == nil {
		return nil
	}
	srv := cs.srv.Swap(nil)
	if srv == nil {
		return nil
	}
	srv.Close()
	switch cs.Mode {
	case CassetteRecord:
		return cs.flush()
	case CassetteReplay:
		if unused := cs.UnusedInteractions(); len(unused) > 0 {
			return fmt.Errorf("%w: indices=%v cassette=%s", ErrCassetteUnusedInteractions, unused, cs.Path)
		}
	}
	return nil
}

// flush writes the recorded interactions to Path as a pretty-printed
// JSON array. The parent dir is created on demand; existing files are
// overwritten atomically (write to .tmp + rename).
func (cs *CassetteServer) flush() error {
	cs.mu.Lock()
	snapshot := make([]Interaction, len(cs.interactions))
	copy(snapshot, cs.interactions)
	cs.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(cs.Path), 0o755); err != nil {
		return fmt.Errorf("cassette flush: mkdir: %w", err)
	}
	buf, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("cassette flush: marshal: %w", err)
	}
	tmp := cs.Path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("cassette flush: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, cs.Path); err != nil {
		return fmt.Errorf("cassette flush: rename %s: %w", tmp, err)
	}
	return nil
}

// ServeHTTP routes the request based on Mode.
func (cs *CassetteServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch cs.Mode {
	case CassetteLive:
		cs.serveLive(w, r)
	case CassetteRecord:
		cs.serveRecord(w, r)
	case CassetteReplay:
		cs.serveReplay(w, r)
	default:
		http.Error(w, "cassette mode not implemented", 599)
	}
}

// loadCassette reads Path from disk and builds the replay index. Called
// once at construction in replay mode. A missing or malformed file is a
// hard error — silent fallthrough to "empty cassette, no matches" would
// disguise a misconfigured test as a cassette miss on every request.
func (cs *CassetteServer) loadCassette() error {
	raw, err := os.ReadFile(cs.Path)
	if err != nil {
		return fmt.Errorf("cassette load: read %s: %w", cs.Path, err)
	}
	var ix []Interaction
	if err := json.Unmarshal(raw, &ix); err != nil {
		return fmt.Errorf("cassette load: parse %s: %w", cs.Path, err)
	}
	cs.interactions = ix
	cs.hashIndex = make(map[string][]int, len(ix))
	cs.cursor = make(map[string]int, len(ix))
	cs.consumed = make([]bool, len(ix))
	for i, it := range ix {
		cs.hashIndex[it.Request.BodyHash] = append(cs.hashIndex[it.Request.BodyHash], i)
	}
	return nil
}

// serveReplay matches the incoming request against the loaded cassette
// by body hash and replays the persisted response. Repeat requests with
// the same hash get the recorded responses in original order; once the
// bucket is exhausted, additional hits cycle back to the last entry
// (idempotent polling case).
//
// On miss: 599 with a diagnostic hint listing the request shape and a
// pointer to the cassette path. Test failures should be obvious.
func (cs *CassetteServer) serveReplay(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("cassette replay: read req body: %v", err), http.StatusInternalServerError)
		return
	}
	r.Body.Close()
	hash := hashRequest(r.Method, r.URL.Path, canonicalQuery(r.URL.RawQuery), body)

	cs.mu.Lock()
	indices, ok := cs.hashIndex[hash]
	if !ok || len(indices) == 0 {
		cs.mu.Unlock()
		http.Error(w, fmt.Sprintf(
			"%s\nmethod=%s path=%s query=%s hash=%s\ncassette=%s",
			ErrCassetteMiss.Error(), r.Method, r.URL.Path, canonicalQuery(r.URL.RawQuery), hash, cs.Path,
		), 599)
		return
	}
	pos := cs.cursor[hash]
	if pos >= len(indices) {
		// Bucket exhausted — replay the last entry so polling-style
		// (same request, last response repeated) doesn't trip drift.
		pos = len(indices) - 1
	} else {
		cs.cursor[hash] = pos + 1
	}
	idx := indices[pos]
	cs.consumed[idx] = true
	// Snapshot under the lock — Headers is a map[string][]string
	// reference; if any future code path mutates the loaded cassette,
	// reading after the lock release would race silently.
	src := cs.interactions[idx].Response
	status := src.Status
	respBody := src.Body
	headers := make(map[string][]string, len(src.Headers))
	for k, vs := range src.Headers {
		headers[k] = append([]string(nil), vs...)
	}
	cs.mu.Unlock()

	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(respBody))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// UnusedInteractions returns the indices of recorded interactions that
// were never matched in this replay run. Useful for drift assertions
// in test code; Close also surfaces this via ErrCassetteUnusedInteractions.
func (cs *CassetteServer) UnusedInteractions() []int {
	if cs == nil {
		return nil
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []int
	for i, c := range cs.consumed {
		if !c {
			out = append(out, i)
		}
	}
	return out
}

// serveLive forwards the request to Upstream and copies the response
// back unchanged. No persistence.
func (cs *CassetteServer) serveLive(w http.ResponseWriter, r *http.Request) {
	resp, err := cs.forward(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("cassette live: upstream: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	cs.copyResponse(w, resp)
}

// serveRecord forwards the request to Upstream and streams the response
// back to the caller while tee-ing bytes into an in-memory buffer.
// SSE timing observable to the caller matches Live mode (frames flow
// as upstream emits them); the buffer is what gets persisted on Close.
//
// Body capture uses io.TeeReader → bytes.Buffer wrapped in
// copyAndFlush, so the caller sees streaming behavior unchanged from
// Live mode AND the cassette captures the full byte stream.
func (cs *CassetteServer) serveRecord(w http.ResponseWriter, r *http.Request) {
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("cassette record: read req body: %v", err), http.StatusInternalServerError)
		return
	}
	r.Body.Close()

	// Restore body for the upstream forward — http.Request.Body is
	// single-use, so swap in a new reader.
	r.Body = io.NopCloser(bytes.NewReader(reqBody))
	resp, err := cs.forward(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("cassette record: upstream: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy headers + status, then stream the body through a TeeReader
	// so the caller observes streaming timing and the buffer captures
	// the full response for cassette persistence.
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	var captured bytes.Buffer
	if _, err := copyAndFlush(w, io.TeeReader(resp.Body, &captured)); err != nil {
		// Headers + partial body already sent. Persist what we got — a
		// truncated cassette is better than no cassette for diagnosis.
		// The interaction still appends below.
		_ = err
	}

	interaction := Interaction{
		Request: RecordedRequest{
			Method:   r.Method,
			Path:     r.URL.Path,
			Query:    canonicalQuery(r.URL.RawQuery),
			Headers:  redactHeaders(r.Header, redactedRequestHeaders),
			Body:     string(reqBody),
			BodyHash: hashRequest(r.Method, r.URL.Path, canonicalQuery(r.URL.RawQuery), reqBody),
		},
		Response: RecordedResponse{
			Status:  resp.StatusCode,
			Headers: redactHeaders(resp.Header, redactedResponseHeaders),
			Body:    captured.String(),
		},
	}
	cs.mu.Lock()
	cs.interactions = append(cs.interactions, interaction)
	cs.mu.Unlock()
}

// forward sends r to Upstream with Host + URL rewritten. Caller owns
// the returned body.
func (cs *CassetteServer) forward(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	out.URL.Scheme = cs.Upstream.Scheme
	out.URL.Host = cs.Upstream.Host
	out.Host = cs.Upstream.Host
	out.RequestURI = ""
	return cs.client.Do(out)
}

// copyResponse copies headers, status, and a streaming body from src
// to dst. Used by Live mode (no buffering).
func (cs *CassetteServer) copyResponse(w http.ResponseWriter, src *http.Response) {
	for k, vs := range src.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(src.StatusCode)
	if _, err := copyAndFlush(w, src.Body); err != nil {
		// Headers already sent; can't surface err.
		return
	}
}

// copyAndFlush is io.Copy with a flush after each successful write,
// so streaming responses (SSE) reach the client without buffering.
func copyAndFlush(w http.ResponseWriter, r io.Reader) (int64, error) { //nolint:unparam // Retain the byte-count result to match io.Copy while flushing each write.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}

// hashRequest is the content-key for replay matching. Order-stable on
// query (already canonicalized) and body. Method + path are
// case-sensitive: HTTP methods are upper-case by convention and paths
// are case-significant.
func hashRequest(method, path, query string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(query))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalQuery sorts the query string by key so that semantically-
// equivalent reorderings hash to the same value.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	v, err := url.ParseQuery(raw)
	if err != nil {
		// Bad query — preserve verbatim. Hash will reflect the malformed
		// shape, which is what we want (don't normalize away bugs).
		return raw
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out strings.Builder
	first := true
	for _, k := range keys {
		vals := v[k]
		sort.Strings(vals) // multi-value parameters are also order-stable
		for _, val := range vals {
			if !first {
				out.WriteByte('&')
			}
			out.WriteString(url.QueryEscape(k))
			out.WriteByte('=')
			out.WriteString(url.QueryEscape(val))
			first = false
		}
	}
	return out.String()
}

// redactHeaders returns a copy of h with any header in redacted set
// rewritten to ["REDACTED"]. Header keys are matched case-insensitively
// via http.CanonicalHeaderKey.
func redactHeaders(h http.Header, redacted map[string]bool) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		canon := http.CanonicalHeaderKey(k)
		if redacted[canon] {
			out[canon] = []string{"REDACTED"}
			continue
		}
		out[canon] = append([]string(nil), vs...)
	}
	return out
}
