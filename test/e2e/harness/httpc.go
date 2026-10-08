package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// StaticTier picks the constructor-bound static credential for a
// Client. Virtual keys are per-request via VirtualKey(...) opt; they
// override the static header on that request only.
type StaticTier int

const (
	TierNone StaticTier = iota
	TierAPI
	TierAdmin
	TierCluster
)

// Client is the harness's typed HTTP client for a single Node + tier.
// All verbs return (Response, error) — the library never calls Fatalf.
// Tests obtain *testing.T-fatal behavior via the harness/assert
// sibling package.
type Client struct {
	node *Node
	tier StaticTier
	http *http.Client
}

// NewClient binds a Client to a node + static tier. The HTTP client is
// the node's shared one; override via WithHTTPClient when streaming
// or recording cassettes.
func NewClient(n *Node, tier StaticTier) *Client {
	if n == nil {
		panic("harness: NewClient nil node")
	}
	return &Client{node: n, tier: tier, http: n.HTTPClient()}
}

// WithHTTPClient returns a shallow copy of c using h. Useful for
// cassette-wrapped or no-timeout streaming clients.
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	clone := *c
	clone.http = h
	return &clone
}

// Node returns the underlying node handle so callers can derive their
// own URLs (e.g., SSE consumers that need streaming-tuned transports).
func (c *Client) Node() *Node { return c.node }

// ReqOpt mutates an outgoing request before send. Construction-time
// concerns only; response assertions live on Response.
type ReqOpt func(*request)

// request is the internal builder shape; not exported.
type request struct {
	headers    http.Header
	query      url.Values
	body       []byte
	rawCT      string // Content-Type when body comes from RawBody
	mergePatch bool
	idempKey   string
	virtualKey string // overrides the static-tier header on this request only
}

// Header attaches a single header. Repeats add to the existing value
// rather than replacing — net/http's Header.Add semantics.
func Header(k, v string) ReqOpt {
	return func(r *request) {
		if r.headers == nil {
			r.headers = http.Header{}
		}
		r.headers.Add(k, v)
	}
}

// Query adds a query parameter; repeats append a multi-value.
func Query(k, v string) ReqOpt {
	return func(r *request) {
		if r.query == nil {
			r.query = url.Values{}
		}
		r.query.Add(k, v)
	}
}

// MergePatch sets Content-Type: application/merge-patch+json. Pair
// with PATCH for the parameter-tree resolver per the
// /providers/:name/parameters contract.
func MergePatch() ReqOpt {
	return func(r *request) { r.mergePatch = true }
}

// IdempotencyKey sets the standard Idempotency-Key header for replay
// safety. Combined with the matching cluster-side handler the request
// becomes safe to retry without double-billing.
func IdempotencyKey(s string) ReqOpt {
	return func(r *request) { r.idempKey = s }
}

// RawBody bypasses JSON encoding and sends bytes verbatim with the
// given Content-Type. The pair-required signature avoids the footgun
// where callers forgot to set Content-Type and the server got
// ambiguous bytes.
func RawBody(contentType string, b []byte) ReqOpt {
	return func(r *request) {
		r.body = b
		r.rawCT = contentType
	}
}

// AsVirtualKey overrides the static-tier X-API-Key header on this
// request only. Used to drive virtual-key tests without rebuilding
// the Client. (Was named VirtualKey; renamed when the typed
// VirtualKey response type landed in keys.go.)
func AsVirtualKey(s string) ReqOpt {
	return func(r *request) { r.virtualKey = s }
}

// Response is the decoded HTTP reply. Field methods (JSON, JobID,
// Problem) return errors rather than panicking — the harness library
// never calls t.Fatalf.
//
// Body is the raw response bytes. Callers that need to mutate must
// copy first; the harness shares Response across helpers (assert.MustJSON,
// assert.Problem) and an in-place mutation would corrupt downstream
// asserts.
type Response struct {
	Status    int
	Headers   http.Header
	Body      []byte
	RequestID string // X-Request-ID echo
}

// JSON decodes the body into v.
func (r Response) JSON(v any) error {
	if len(r.Body) == 0 {
		return errors.New("response body empty")
	}
	return json.Unmarshal(r.Body, v)
}

// AcceptedEnvelope is the canonical 202 reply shape. Public-side handlers
// wrap payloads as {success, message, data:{job_id}}; internal-side return
// {job_id} at the top. Accept both.
type AcceptedEnvelope struct {
	JobID string `json:"job_id"`
	Data  *struct {
		JobID string `json:"job_id"`
	} `json:"data,omitempty"`
}

// JobID parses a 202 envelope and returns the job_id. Returns error
// when the status is not 202 or the envelope is malformed.
func (r Response) JobID() (string, error) {
	if r.Status != http.StatusAccepted {
		return "", fmt.Errorf("expected 202, got %d", r.Status)
	}
	var env AcceptedEnvelope
	if err := json.Unmarshal(r.Body, &env); err != nil {
		return "", fmt.Errorf("decode 202 envelope: %w", err)
	}
	if env.JobID != "" {
		return env.JobID, nil
	}
	if env.Data != nil && env.Data.JobID != "" {
		return env.Data.JobID, nil
	}
	return "", errors.New("202 envelope missing job_id")
}

// Problem is the RFC 9457 application/problem+json shape. Matches the
// fields zzrouter actually emits today; extend as new ones land.
type Problem struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title,omitempty"`
	Status   int    `json:"status,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	Code     string `json:"code,omitempty"`
}

// Problem decodes the body as RFC 9457. Returns error when the
// content-type isn't application/problem+json.
func (r Response) Problem() (Problem, error) {
	ct := r.Headers.Get("Content-Type")
	if !strings.Contains(ct, "application/problem+json") {
		return Problem{}, fmt.Errorf("not a problem+json response (Content-Type=%q)", ct)
	}
	var p Problem
	if err := json.Unmarshal(r.Body, &p); err != nil {
		return Problem{}, fmt.Errorf("decode problem: %w", err)
	}
	return p, nil
}

// GET sends a GET request. ReqOpts apply construction-time only.
func (c *Client) GET(ctx context.Context, path string, opts ...ReqOpt) (Response, error) {
	return c.do(ctx, http.MethodGet, path, nil, opts)
}

// POST sends a POST. body is JSON-encoded unless RawBody overrides.
func (c *Client) POST(ctx context.Context, path string, body any, opts ...ReqOpt) (Response, error) {
	return c.do(ctx, http.MethodPost, path, body, opts)
}

// PATCH sends a PATCH. Pair with MergePatch() for merge-patch+json.
func (c *Client) PATCH(ctx context.Context, path string, body any, opts ...ReqOpt) (Response, error) {
	return c.do(ctx, http.MethodPatch, path, body, opts)
}

// PUT sends a PUT.
func (c *Client) PUT(ctx context.Context, path string, body any, opts ...ReqOpt) (Response, error) {
	return c.do(ctx, http.MethodPut, path, body, opts)
}

// DELETE sends a DELETE.
func (c *Client) DELETE(ctx context.Context, path string, opts ...ReqOpt) (Response, error) {
	return c.do(ctx, http.MethodDelete, path, nil, opts)
}

// HEAD sends a HEAD. Per RFC 7231 the response has headers but no body;
// Response.Body will be empty even when Content-Length is non-zero in
// the headers. Used for /api/blobs existence probes.
func (c *Client) HEAD(ctx context.Context, path string, opts ...ReqOpt) (Response, error) {
	return c.do(ctx, http.MethodHead, path, nil, opts)
}

// MultipartFile is one file part in a multipart/form-data POST. MIME is
// optional; defaults to application/octet-stream when empty.
type MultipartFile struct {
	Field    string
	Filename string
	Content  []byte
	MIME     string
}

// POSTMultipart sends a multipart/form-data POST. fields are name=value
// text form fields; files are uploaded with their bytes + filename +
// MIME. Used for /v1/files (purpose+file), /v1/audio/transcriptions
// (model+file), /v1/images/edits (image+mask+prompt), and any other
// upload-bearing OpenAI endpoint. Falls through Client.do so opts and
// auth/Idempotency-Key/etc still apply.
func (c *Client) POSTMultipart(ctx context.Context, path string, fields map[string]string, files []MultipartFile, opts ...ReqOpt) (Response, error) {
	body, ct, err := buildMultipart(fields, files)
	if err != nil {
		return Response{}, err
	}
	opts = append(opts, RawBody(ct, body))
	return c.do(ctx, http.MethodPost, path, nil, opts)
}

// buildMultipart assembles a multipart/form-data body and returns the
// bytes + the Content-Type header (which embeds the boundary). Caller
// passes the Content-Type to RawBody verbatim so the per-request
// boundary lines up with the framing.
func buildMultipart(fields map[string]string, files []MultipartFile) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return nil, "", fmt.Errorf("write field %q: %w", k, err)
		}
	}
	for _, f := range files {
		if f.Field == "" {
			return nil, "", fmt.Errorf("multipart file missing Field name")
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`,
			f.Field, f.Filename))
		ct := f.MIME
		if ct == "" {
			ct = "application/octet-stream"
		}
		h.Set("Content-Type", ct)
		part, err := mw.CreatePart(h)
		if err != nil {
			return nil, "", fmt.Errorf("create part %q: %w", f.Field, err)
		}
		if _, err := part.Write(f.Content); err != nil {
			return nil, "", fmt.Errorf("write part %q: %w", f.Field, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

func (c *Client) do(ctx context.Context, method, p string, body any, opts []ReqOpt) (Response, error) {
	r := &request{}
	for _, o := range opts {
		o(r)
	}

	urlStr, err := c.buildURL(p, r.query)
	if err != nil {
		return Response{}, err
	}
	bodyBytes, autoJSON, err := encodeBody(r, body)
	if err != nil {
		return Response{}, err
	}

	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return Response{}, err
	}

	// User headers FIRST — auto headers below use Set so they win on
	// well-known singletons (Content-Type, Idempotency-Key, X-API-Key)
	// without producing comma-joined duplicates.
	for k, vs := range r.headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	c.applyContentType(req, r, autoJSON)
	if r.idempKey != "" {
		req.Header.Set("Idempotency-Key", r.idempKey)
	}
	c.applyAuth(req, r.virtualKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("%s %s: %w", method, p, err)
	}
	defer resp.Body.Close()

	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read body: %w", err)
	}
	return Response{
		Status:    resp.StatusCode,
		Headers:   resp.Header.Clone(),
		Body:      bs,
		RequestID: resp.Header.Get("X-Request-Id"),
	}, nil
}

// buildURL composes baseURL+path and merges any query parameters.
func (c *Client) buildURL(p string, query url.Values) (string, error) {
	u, err := url.Parse(c.node.baseURL + p)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if query != nil {
		q := u.Query()
		for k, vs := range query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// encodeBody picks the right body bytes given r.body (RawBody opt) or
// the typed body argument. Returns (bytes, autoJSONApplied, error).
// autoJSON=true means the body came from the typed argument and should
// receive the application/json Content-Type unless overridden.
func encodeBody(r *request, body any) ([]byte, bool, error) {
	if r.body != nil {
		return r.body, false, nil
	}
	if body == nil {
		return nil, false, nil
	}
	bb, err := json.Marshal(body)
	if err != nil {
		return nil, false, fmt.Errorf("marshal body: %w", err)
	}
	return bb, true, nil
}

// applyContentType sets Content-Type with Set semantics so a user-
// supplied header doesn't double up. Precedence: MergePatch > RawBody
// CT > auto-JSON. Caller-supplied via Header(...) loses on these keys
// — that's intentional: the verb already declared the wire shape.
func (c *Client) applyContentType(req *http.Request, r *request, autoJSON bool) {
	switch {
	case r.mergePatch:
		req.Header.Set("Content-Type", "application/merge-patch+json")
	case r.rawCT != "":
		req.Header.Set("Content-Type", r.rawCT)
	case autoJSON:
		req.Header.Set("Content-Type", "application/json")
	}
}

// applyAuth installs the static-tier or virtual-key X-API-Key header.
// Used by both Client.do and Jobs.wait so the auth contract has one
// canonical site.
func (c *Client) applyAuth(req *http.Request, virtualKey string) {
	switch {
	case virtualKey != "":
		req.Header.Set("X-API-Key", virtualKey)
	case c.tier == TierAdmin:
		req.Header.Set("X-API-Key", c.node.adminKey)
	case c.tier == TierAPI:
		req.Header.Set("X-API-Key", c.node.apiKey)
	case c.tier == TierCluster:
		req.Header.Set("X-API-Key", c.node.clusterKey)
	}
}
