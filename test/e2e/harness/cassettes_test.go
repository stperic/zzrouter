package harness_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/test/e2e/harness"
)

func sleep(ns int)    { time.Sleep(time.Duration(ns)) }
func nowNanos() int64 { return time.Now().UnixNano() }

func TestNewCassetteServer_LiveForwardsToUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Probe", "upstream")
		_, _ = io.WriteString(w, "pong:"+r.URL.Path)
	}))
	defer upstream.Close()

	cs, err := harness.NewCassetteServer(harness.CassetteLive, harness.WithUpstream(upstream.URL))
	if err != nil {
		t.Fatalf("NewCassetteServer: %v", err)
	}
	defer cs.Close()

	resp, err := http.Get(cs.URL() + "/v1/models")
	if err != nil {
		t.Fatalf("GET cassette: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "pong:/v1/models" {
		t.Errorf("body=%q, want pong:/v1/models", got)
	}
	if got := resp.Header.Get("X-Probe"); got != "upstream" {
		t.Errorf("X-Probe=%q, want upstream (header copy broken)", got)
	}
}

func TestNewCassetteServer_LiveRequiresUpstream(t *testing.T) {
	_, err := harness.NewCassetteServer(harness.CassetteLive)
	if err == nil {
		t.Fatal("expected error when CassetteLive constructed without WithUpstream")
	}
	if !strings.Contains(err.Error(), "WithUpstream") {
		t.Errorf("error %q should mention WithUpstream", err.Error())
	}
}

func TestNewCassetteServer_ReplayMissingFile(t *testing.T) {
	_, err := harness.NewCassetteServer(harness.CassetteReplay, harness.WithPath("/tmp/does-not-exist-cassette.json"))
	if err == nil || !strings.Contains(err.Error(), "cassette load") {
		t.Errorf("want cassette load error, got %v", err)
	}
}

func TestNewCassetteServer_ReplayMalformedFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write bad cassette: %v", err)
	}
	_, err := harness.NewCassetteServer(harness.CassetteReplay, harness.WithPath(bad))
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("want parse error, got %v", err)
	}
}

// TestCassetteReplay_HitFromRecording records one interaction, then
// replays the same cassette and asserts the response is served from
// disk without touching the upstream.
func TestCassetteReplay_HitFromRecording(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "hit.json")

	// Record
	rec, err := harness.NewCassetteServer(harness.CassetteRecord,
		harness.WithUpstream(upstream.URL),
		harness.WithPath(path))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	body := strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(rec.URL()+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("record POST: %v", err)
	}
	resp.Body.Close()
	if err := rec.Close(); err != nil {
		t.Fatalf("record Close: %v", err)
	}

	// Tear down upstream so a replay miss would fail loud.
	upstream.Close()

	// Replay
	rep, err := harness.NewCassetteServer(harness.CassetteReplay, harness.WithPath(path))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	body = strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	resp, err = http.Post(rep.URL()+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("replay POST: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(got), `"choices"`) {
		t.Errorf("replay body=%q, want recorded payload", got)
	}
	if resp.StatusCode != 200 {
		t.Errorf("replay status=%d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type=%q, want application/json (header replay broken)", resp.Header.Get("Content-Type"))
	}
	if err := rep.Close(); err != nil {
		t.Errorf("replay Close: %v (want nil — all interactions consumed)", err)
	}
}

// TestCassetteReplay_MissReturns599 asserts a request whose hash isn't
// in the cassette gets the 599 miss response with diagnostic hint.
func TestCassetteReplay_MissReturns599(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, []byte(`[]`), 0o644); err != nil {
		t.Fatalf("seed empty cassette: %v", err)
	}
	rep, err := harness.NewCassetteServer(harness.CassetteReplay, harness.WithPath(path))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer rep.Close()
	resp, err := http.Get(rep.URL() + "/missing")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 599 {
		t.Errorf("status=%d, want 599", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "cassette miss") {
		t.Errorf("miss body=%q, want diagnostic", body)
	}
}

// TestCassetteReplay_CloseFlagsUnusedInteractions records two
// interactions, replays only one, and asserts Close returns
// ErrCassetteUnusedInteractions.
func TestCassetteReplay_CloseFlagsUnusedInteractions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("path:" + r.URL.Path))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "drift.json")

	rec, err := harness.NewCassetteServer(harness.CassetteRecord,
		harness.WithUpstream(upstream.URL),
		harness.WithPath(path))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	for _, p := range []string{"/a", "/b"} {
		resp, err := http.Get(rec.URL() + p)
		if err != nil {
			t.Fatalf("record GET %s: %v", p, err)
		}
		resp.Body.Close()
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("record Close: %v", err)
	}
	upstream.Close()

	rep, err := harness.NewCassetteServer(harness.CassetteReplay, harness.WithPath(path))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	resp, err := http.Get(rep.URL() + "/a")
	if err != nil {
		t.Fatalf("replay GET: %v", err)
	}
	resp.Body.Close()
	// /b never replayed.
	closeErr := rep.Close()
	if !errors.Is(closeErr, harness.ErrCassetteUnusedInteractions) {
		t.Errorf("Close: want ErrCassetteUnusedInteractions, got %v", closeErr)
	}
}

func TestNewCassetteServer_RecordRequirements(t *testing.T) {
	if _, err := harness.NewCassetteServer(harness.CassetteRecord); err == nil || !strings.Contains(err.Error(), "WithPath") {
		t.Errorf("missing path: want WithPath error, got %v", err)
	}
	if _, err := harness.NewCassetteServer(harness.CassetteRecord, harness.WithPath("x.json")); err == nil || !strings.Contains(err.Error(), "WithUpstream") {
		t.Errorf("missing upstream: want WithUpstream error, got %v", err)
	}
}

func TestNewCassetteServer_ReplayRequiresPath(t *testing.T) {
	_, err := harness.NewCassetteServer(harness.CassetteReplay)
	if err == nil || !strings.Contains(err.Error(), "WithPath") {
		t.Errorf("want WithPath error, got %v", err)
	}
}

// TestCassetteRecord_RoundTripPersists exercises the full record path:
// upstream call → in-memory capture → on-disk flush via Close. Asserts
// the cassette contains exactly one interaction with method/path/body
// preserved and Authorization header redacted.
func TestCassetteRecord_RoundTripPersists(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the request body in the response so the round-trip
		// asserts both directions captured the bytes.
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"echo":"` + string(body) + `"}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cassette := filepath.Join(dir, "openrouter", "round_trip.json")

	cs, err := harness.NewCassetteServer(harness.CassetteRecord,
		harness.WithUpstream(upstream.URL),
		harness.WithPath(cassette))
	if err != nil {
		t.Fatalf("NewCassetteServer: %v", err)
	}
	defer cs.Close()

	req, _ := http.NewRequest("POST", cs.URL()+"/v1/chat/completions?stream=true",
		strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer secret-key-do-not-leak")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := string(body); got != `{"echo":"{"model":"x"}"}` {
		t.Errorf("response body=%q, want echo of request", got)
	}

	// Client EOF can precede the handler's append; Close joins all handlers.
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(cs.Interactions()); got != 1 {
		t.Fatalf("Interactions()=%d, want 1", got)
	}

	// On-disk shape.
	raw, err := os.ReadFile(cassette)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", cassette, err)
	}
	var ix []harness.Interaction
	if err := json.Unmarshal(raw, &ix); err != nil {
		t.Fatalf("Unmarshal cassette: %v", err)
	}
	if len(ix) != 1 {
		t.Fatalf("cassette has %d interactions, want 1", len(ix))
	}
	got := ix[0]
	if got.Request.Method != "POST" {
		t.Errorf("Method=%q, want POST", got.Request.Method)
	}
	if got.Request.Path != "/v1/chat/completions" {
		t.Errorf("Path=%q, want /v1/chat/completions", got.Request.Path)
	}
	if got.Request.Query != "stream=true" {
		t.Errorf("Query=%q, want stream=true", got.Request.Query)
	}
	if got.Request.Body != `{"model":"x"}` {
		t.Errorf("Request.Body=%q", got.Request.Body)
	}
	if got.Request.BodyHash == "" {
		t.Error("Request.BodyHash is empty")
	}
	if auth := got.Request.Headers["Authorization"]; len(auth) != 1 || auth[0] != "REDACTED" {
		t.Errorf("Authorization header=%v, want [REDACTED]", auth)
	}
	if got.Response.Status != 200 {
		t.Errorf("Response.Status=%d", got.Response.Status)
	}
	if !strings.Contains(got.Response.Body, `"echo"`) {
		t.Errorf("Response.Body missing echo: %q", got.Response.Body)
	}
	if strings.Contains(string(raw), "secret-key-do-not-leak") {
		t.Error("on-disk cassette contains the live API key — redaction broken")
	}
}

// TestCassetteRecord_QueryCanonicalization pins query-param sorting so
// hashes are stable across reorderings.
func TestCassetteRecord_QueryCanonicalization(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cs, err := harness.NewCassetteServer(harness.CassetteRecord,
		harness.WithUpstream(upstream.URL),
		harness.WithPath(filepath.Join(dir, "q.json")))
	if err != nil {
		t.Fatalf("NewCassetteServer: %v", err)
	}
	defer cs.Close()

	for _, q := range []string{"b=2&a=1", "a=1&b=2"} {
		resp, err := http.Get(cs.URL() + "/x?" + q)
		if err != nil {
			t.Fatalf("GET ?%s: %v", q, err)
		}
		resp.Body.Close()
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ix := cs.Interactions()
	if len(ix) != 2 {
		t.Fatalf("got %d interactions, want 2", len(ix))
	}
	if ix[0].Request.BodyHash != ix[1].Request.BodyHash {
		t.Errorf("BodyHash differs across query orderings: %s vs %s",
			ix[0].Request.BodyHash, ix[1].Request.BodyHash)
	}
	if ix[0].Request.Query != "a=1&b=2" {
		t.Errorf("Query=%q, want canonical a=1&b=2", ix[0].Request.Query)
	}
}

func TestNewCassetteServer_UnknownMode(t *testing.T) {
	_, err := harness.NewCassetteServer(harness.CassetteMode("invalid"))
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("want unknown-mode error, got %v", err)
	}
}

func TestCassetteServer_CloseIdempotent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cs, err := harness.NewCassetteServer(harness.CassetteLive, harness.WithUpstream(upstream.URL))
	if err != nil {
		t.Fatalf("NewCassetteServer: %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("Close (1): %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Errorf("Close (2): want nil (idempotent), got %v", err)
	}
}

// TestCassetteRecord_StreamsToCallerWhileCapturing pins the C1 fix:
// record mode must let SSE-style chunks reach the caller as they arrive,
// not buffer the whole response. Upstream emits chunks 50ms apart;
// we assert the caller observes the first chunk well before the last.
func TestCassetteRecord_StreamsToCallerWhileCapturing(t *testing.T) {
	const chunkCount = 4
	const interval = 50 * 1000 * 1000 // 50ms in nanoseconds, expressed as int (avoid time import duplication)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunkCount; i++ {
			_, _ = w.Write([]byte("data: chunk-" + string(rune('A'+i)) + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			sleep(interval)
		}
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cs, err := harness.NewCassetteServer(harness.CassetteRecord,
		harness.WithUpstream(upstream.URL),
		harness.WithPath(filepath.Join(dir, "stream.json")))
	if err != nil {
		t.Fatalf("NewCassetteServer: %v", err)
	}

	start := nowNanos()
	resp, err := http.Get(cs.URL() + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	firstChunkLatencyNs := nowNanos() - start

	// First chunk must arrive well before the full upstream timeline
	// (which is chunkCount × interval ≈ 200ms). Allow generous slack
	// for slow CI; 75% of the total stream window is the threshold.
	totalStreamNs := int64(chunkCount * interval)
	if firstChunkLatencyNs > totalStreamNs*3/4 {
		t.Errorf("first chunk arrived in %dms; upstream stream window ~%dms — record mode is buffering",
			firstChunkLatencyNs/1_000_000, totalStreamNs/1_000_000)
	}
	if !strings.HasPrefix(string(buf[:n]), "data: chunk-A") {
		t.Errorf("first chunk = %q, want SSE frame", string(buf[:n]))
	}

	// Drain the rest so the cassette captures every chunk.
	rest, _ := io.ReadAll(resp.Body)
	full := string(buf[:n]) + string(rest)
	if !strings.Contains(full, "chunk-D") {
		t.Errorf("final chunk missing from caller stream: %q", full)
	}

	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Cassette body should match the full upstream stream.
	if got := cs.Interactions(); len(got) != 1 {
		t.Fatalf("got %d interactions, want 1", len(got))
	}
	if !strings.Contains(cs.Interactions()[0].Response.Body, "chunk-D") {
		t.Errorf("captured body missing final chunk: %q", cs.Interactions()[0].Response.Body)
	}
}

// TestCassetteRecord_ProxyAuthorizationRedacted pins C4: cassettes
// must scrub Proxy-Authorization in addition to Authorization.
func TestCassetteRecord_ProxyAuthorizationRedacted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.json")
	cs, err := harness.NewCassetteServer(harness.CassetteRecord,
		harness.WithUpstream(upstream.URL),
		harness.WithPath(path))
	if err != nil {
		t.Fatalf("NewCassetteServer: %v", err)
	}

	req, _ := http.NewRequest("GET", cs.URL()+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer api-secret")
	req.Header.Set("Proxy-Authorization", "Basic proxy-secret")
	req.Header.Set("X-Api-Key", "key-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, _ := os.ReadFile(path)
	for _, secret := range []string{"api-secret", "proxy-secret", "key-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("cassette contains %q — redaction broken", secret)
		}
	}
}

// TestCassetteServer_ConcurrentURLAndClose exercises the C2 fix.
// Race detector catches a torn read of cs.srv if the atomic isn't
// honored. Test passes only when -race is clean.
func TestCassetteServer_ConcurrentURLAndClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cs, err := harness.NewCassetteServer(harness.CassetteLive, harness.WithUpstream(upstream.URL))
	if err != nil {
		t.Fatalf("NewCassetteServer: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_ = cs.URL()
		}
		close(done)
	}()
	_ = cs.Close()
	<-done
}

func TestCassetteServer_NilSafe(t *testing.T) {
	var cs *harness.CassetteServer
	if got := cs.URL(); got != "" {
		t.Errorf("nil.URL() = %q, want empty", got)
	}
	if err := cs.Close(); err != nil {
		t.Errorf("nil.Close() = %v, want nil", err)
	}
}
