package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Example capture must never change what the consumer observes, and
// must never store an unredacted secret. Both claims are load-bearing:
// the first keeps the probe honest, the second is the only thing
// standing between a live admin key and a committed markdown file.

func exampleTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/secret":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"key":"zzr_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ab","id":"k1"}`))
		case "/big":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(bytes.Repeat([]byte("x"), exampleBodyCap+100))
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			for i := 0; i < 3; i++ {
				_, _ = w.Write([]byte("data: {\"n\":1}\n\n"))
				f.Flush()
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
}

func getViaRecorder(t *testing.T, url string) string {
	t.Helper()
	resp, err := coverageClient.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(b)
}

func capturedExamples(t *testing.T) []Example {
	t.Helper()
	exs, _ := ObservedExamples()
	return exs
}

func TestExampleCapture_ConsumerSeesIdenticalBytes(t *testing.T) {
	resetCoverage(t)
	srv := exampleTestServer(t)
	defer srv.Close()

	got := getViaRecorder(t, srv.URL+"/plain")
	if got != `{"ok":true}` {
		t.Fatalf("consumer body altered: %q", got)
	}
	exs := capturedExamples(t)
	if len(exs) != 1 {
		t.Fatalf("want 1 example, got %d", len(exs))
	}
	ex := exs[0]
	if ex.Status != 200 || ex.Method != http.MethodGet || ex.Path != "/plain" {
		t.Errorf("example identity wrong: %+v", ex)
	}
	if ex.ResponseContentType != "application/json" {
		t.Errorf("content type = %q", ex.ResponseContentType)
	}
	if ex.ResponseBody != `{"ok":true}` {
		t.Errorf("captured body = %q", ex.ResponseBody)
	}
}

func TestExampleCapture_RedactsBeforeStorage(t *testing.T) {
	resetCoverage(t)
	srv := exampleTestServer(t)
	defer srv.Close()

	got := getViaRecorder(t, srv.URL+"/secret")
	if !strings.Contains(got, "zzr_") {
		t.Fatal("consumer must receive the raw body untouched")
	}
	exs := capturedExamples(t)
	if len(exs) != 1 {
		t.Fatalf("want 1 example, got %d", len(exs))
	}
	if strings.Contains(exs[0].ResponseBody, "zzr_") {
		t.Fatalf("raw key stored in example: %s", exs[0].ResponseBody)
	}
	if !strings.Contains(exs[0].ResponseBody, `"k1"`) {
		t.Errorf("benign field lost: %s", exs[0].ResponseBody)
	}
}

func TestExampleCapture_RequestBodyRedactedAndWireIntact(t *testing.T) {
	resetCoverage(t)
	var serverSaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverSaw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	payload := `{"type":"ollama-connect","name":"nas","token":"hf_notARealTokenButSecret00"}`
	resp, err := coverageClient.Post(srv.URL+"/providers/instances", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(serverSaw) != payload {
		t.Fatalf("wire body altered: %q", serverSaw)
	}
	exs := capturedExamples(t)
	if len(exs) != 1 {
		t.Fatalf("want 1 example, got %d", len(exs))
	}
	ex := exs[0]
	if strings.Contains(ex.RequestBody, "hf_") {
		t.Fatalf("token stored in example: %s", ex.RequestBody)
	}
	if !strings.Contains(ex.RequestBody, `"nas"`) {
		t.Errorf("benign request field lost: %s", ex.RequestBody)
	}
	if ex.Status != http.StatusCreated {
		t.Errorf("status = %d", ex.Status)
	}
}

func TestExampleCapture_TruncatesAtCapConsumerUnaffected(t *testing.T) {
	resetCoverage(t)
	srv := exampleTestServer(t)
	defer srv.Close()

	got := getViaRecorder(t, srv.URL+"/big")
	if len(got) != exampleBodyCap+100 {
		t.Fatalf("consumer got %d bytes, want %d", len(got), exampleBodyCap+100)
	}
	exs := capturedExamples(t)
	if len(exs) != 1 {
		t.Fatalf("want 1 example, got %d", len(exs))
	}
	if !exs[0].ResponseTruncated {
		t.Error("truncated flag not set")
	}
	if len(exs[0].ResponseBody) != exampleBodyCap {
		t.Errorf("stored %d bytes, cap is %d", len(exs[0].ResponseBody), exampleBodyCap)
	}
}

// The harness SSE consumer closes the body from a context.AfterFunc
// goroutine to unblock a stuck Read (sse.go). The wrapper must not
// deadlock under that pattern, and the partial body read so far must
// still become the example.
func TestExampleCapture_SSECloseFromAnotherGoroutine(t *testing.T) {
	resetCoverage(t)
	srv := exampleTestServer(t)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/sse", nil)
	resp, err := coverageClient.Do(req)
	if err != nil {
		t.Fatalf("get sse: %v", err)
	}
	stop := context.AfterFunc(ctx, func() { resp.Body.Close() })
	defer stop()

	r := bufio.NewReader(resp.Body)
	first, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(first, "data: ") {
		t.Fatalf("sse frame: %q err=%v", first, err)
	}
	cancel() // closes the body from the AfterFunc goroutine

	done := make(chan struct{})
	go func() {
		for {
			if _, err := r.ReadString('\n'); err != nil {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("read never unblocked after cross-goroutine Close: the wrapper deadlocks")
	}

	// finalize runs on the AfterFunc goroutine's Close, so the example
	// lands asynchronously; wait for it rather than racing it. Without
	// this the stray finalize would also land in the NEXT test's map.
	deadline := time.Now().Add(5 * time.Second)
	for {
		exs := capturedExamples(t)
		if len(exs) == 1 {
			if !strings.Contains(exs[0].ResponseBody, "data: ") {
				t.Errorf("partial SSE body not captured: %q", exs[0].ResponseBody)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("example never finalized; got %d", len(exs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExampleCapture_FirstWinsPerKey(t *testing.T) {
	resetCoverage(t)
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		_, _ = w.Write([]byte(`{"n":` + string(rune('0'+n)) + `}`))
	}))
	defer srv.Close()

	_ = getViaRecorder(t, srv.URL+"/same")
	_ = getViaRecorder(t, srv.URL+"/same")
	exs := capturedExamples(t)
	if len(exs) != 1 {
		t.Fatalf("want 1 example, got %d", len(exs))
	}
	if exs[0].ResponseBody != `{"n":1}` {
		t.Errorf("first example lost to a later one: %s", exs[0].ResponseBody)
	}
}

func TestExampleCapture_EntryCapCountsDrops(t *testing.T) {
	resetCoverage(t)
	coverageMu.Lock()
	for i := 0; i < exampleEntryCap; i++ {
		examples[callKey{method: "GET", path: "/fill", class: string(rune(i))}] = Example{}
	}
	coverageMu.Unlock()

	srv := exampleTestServer(t)
	defer srv.Close()
	_ = getViaRecorder(t, srv.URL+"/plain")

	exs, dropped := ObservedExamples()
	if len(exs) != exampleEntryCap {
		t.Fatalf("cap not enforced: %d entries", len(exs))
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
}

func TestExampleCapture_TransportErrorStoresNothing(t *testing.T) {
	resetCoverage(t)
	// Connection refused: a closed server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	//nolint:bodyclose // a transport error returns a nil response; there is no body
	if _, err := coverageClient.Get(url + "/gone"); err == nil {
		t.Fatal("expected a transport error")
	}
	if exs := capturedExamples(t); len(exs) != 0 {
		t.Fatalf("transport error produced %d example(s)", len(exs))
	}
}

func TestLedgerRoundTripCarriesExamples(t *testing.T) {
	resetCoverage(t)
	srv := exampleTestServer(t)
	defer srv.Close()
	_ = getViaRecorder(t, srv.URL+"/plain")

	dir := t.TempDir()
	t.Setenv(coverageEnvDir, dir)
	if err := WriteCoverageLedger("example_suite"); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "example_suite.json"))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var led Ledger
	if err := json.Unmarshal(raw, &led); err != nil {
		t.Fatalf("parse ledger: %v", err)
	}
	if len(led.Examples) != 1 || led.Examples[0].ResponseBody != `{"ok":true}` {
		t.Fatalf("examples did not round-trip: %+v", led.Examples)
	}
}

// Close must RETURN promptly while a Read is stuck on a silent
// connection. On HTTP/1 closing the body does not unblock the read
// (context cancellation does, via the transport); what Close must
// never do is queue behind the stuck Read - TailSSE calls Close from
// an AfterFunc goroutine, and a wrapper serializing Read and Close on
// one mutex would leak that goroutine forever. The naive same-mutex
// wrapper fails exactly here.
func TestExampleCapture_CloseReturnsWhileReadIsStuck(t *testing.T) {
	resetCoverage(t)
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"n\":1}\n\n"))
		w.(http.Flusher).Flush()
		<-hold // hold the connection open; the client's next Read blocks
	}))
	held := true
	defer srv.Close()
	defer func() { // LIFO: release the handler BEFORE srv.Close waits on it
		if held {
			close(hold)
		}
	}()

	resp, err := coverageClient.Get(srv.URL + "/stuck") //nolint:bodyclose // closed below via the goroutine whose latency IS the assertion
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	firstFrame := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		r := bufio.NewReader(resp.Body)
		if _, err := r.ReadString('\n'); err == nil {
			close(firstFrame)
		}
		for {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
		}
	}()

	select {
	case <-firstFrame:
	case <-time.After(5 * time.Second):
		t.Fatal("never received the first frame")
	}

	closed := make(chan struct{})
	go func() {
		resp.Body.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked behind a stuck Read: the wrapper serializes them")
	}

	close(hold) // release the handler; the reader unblocks on connection end
	held = false
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader never finished after the server released the connection")
	}
}
