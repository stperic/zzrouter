package harness

import (
	"bytes"
	"io"
	"net/http"
	"sort"
	"sync"

	"github.com/stperic/zzrouter/pkg/security"
)

// Example capture.
//
// The coverage ledger proves a route was driven; an Example shows what
// the wire actually carried, so documentation examples are captured
// rather than authored — an authored example is the first thing to rot.
//
// Bodies are redacted BEFORE they enter the in-memory store, so an
// unredacted secret never exists outside the request's own lifetime.
// The path stays concrete, like the ledger's: mapping onto route
// templates is the reporter's job.
//
// Not captured here: the Python SDK suite and the SSH pairing flow
// build their own HTTP clients and bypass this transport entirely.

const (
	// exampleBodyCap bounds what is RETAINED per body, not what the
	// consumer reads: every management envelope fits, and an example
	// does not need a whole SSE stream to show the framing.
	exampleBodyCap = 8 << 10
	// exampleEntryCap bounds memory across a 90-minute suite. Ring 1's
	// auth sweep alone yields ~1200 (method, path, class) keys.
	exampleEntryCap = 4096
)

// Example is one captured request/response pair, redacted, truncated
// to exampleBodyCap per body.
type Example struct {
	Method              string `json:"method"`
	Path                string `json:"path"`
	Status              int    `json:"status"`
	RequestContentType  string `json:"request_content_type,omitempty"`
	RequestBody         string `json:"request_body,omitempty"`
	RequestTruncated    bool   `json:"request_truncated,omitempty"`
	ResponseContentType string `json:"response_content_type,omitempty"`
	ResponseBody        string `json:"response_body,omitempty"`
	ResponseTruncated   bool   `json:"response_truncated,omitempty"`
}

var (
	// Guarded by coverageMu, like the call ledger.
	examples        = map[callKey]Example{}
	examplesDropped int
)

// storeExample keeps the first example per (method, path, class),
// redacting on the way in. Beyond exampleEntryCap new keys are counted
// as dropped so the reporter can say the examples are incomplete
// instead of presenting a partial set as the whole.
func storeExample(key callKey, ex Example) {
	ex.RequestBody = string(security.RedactJSON([]byte(ex.RequestBody)))
	ex.ResponseBody = string(security.RedactJSON([]byte(ex.ResponseBody)))

	coverageMu.Lock()
	defer coverageMu.Unlock()
	if _, seen := examples[key]; seen {
		return
	}
	if len(examples) >= exampleEntryCap {
		examplesDropped++
		return
	}
	examples[key] = ex
}

// ObservedExamples returns captured examples sorted for stable ledger
// output, plus how many were dropped at the entry cap.
func ObservedExamples() ([]Example, int) {
	coverageMu.Lock()
	out := make([]Example, 0, len(examples))
	for _, ex := range examples {
		out = append(out, ex)
	}
	dropped := examplesDropped
	coverageMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Status < out[j].Status
	})
	return out, dropped
}

// captureRequestBody drains and restores req.Body so the transport can
// retain a copy. The restored body serves the actual send; net/http's
// own retry/redirect replay goes through GetBody, which is untouched.
// Only the retained copy is capped — the wire sees every byte.
func captureRequestBody(req *http.Request) (body string, truncated bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return "", false
	}
	raw, err := io.ReadAll(req.Body)
	closeErr := req.Body.Close()
	if err != nil || closeErr != nil {
		// A body that cannot be drained here would have failed the send
		// anyway; restore what was read and record nothing.
		req.Body = io.NopCloser(bytes.NewReader(raw))
		return "", false
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	if len(raw) > exampleBodyCap {
		return string(raw[:exampleBodyCap]), true
	}
	return string(raw), false
}

// exampleBody tees the response body into a capped buffer without
// changing what the consumer observes.
//
// Locking is deliberately narrow: Read never holds the mutex across
// the underlying Read, and Close closes the underlying body FIRST,
// unblocked. The harness's own SSE path (TailSSE) closes the body from
// a context.AfterFunc goroutine precisely to unblock a stuck Read —
// a wrapper that serialized Read and Close would deadlock there.
// finalize runs once, on EOF or Close, whichever comes first.
type exampleBody struct {
	rc       io.ReadCloser
	finalize func(body string, truncated bool)

	mu        sync.Mutex // guards buf + truncated only
	buf       bytes.Buffer
	truncated bool

	once sync.Once
}

func (b *exampleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		if room := exampleBodyCap - b.buf.Len(); room > 0 {
			if n <= room {
				b.buf.Write(p[:n])
			} else {
				b.buf.Write(p[:room])
				b.truncated = true
			}
		} else {
			b.truncated = true
		}
		b.mu.Unlock()
	}
	if err == io.EOF {
		b.fin()
	}
	return n, err
}

func (b *exampleBody) Close() error {
	err := b.rc.Close()
	b.fin()
	return err
}

func (b *exampleBody) fin() {
	b.once.Do(func() {
		b.mu.Lock()
		body, truncated := b.buf.String(), b.truncated
		b.mu.Unlock()
		b.finalize(body, truncated)
	})
}
