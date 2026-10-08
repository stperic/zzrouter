package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// A verbatim llama-server /v1/messages stream (see the wire package's
// fixture notes): 4 uncached + 55 cache-read input tokens, 20 output.
const llamacppMessagesStream = "../../pkg/dispatch/wire/testdata/anthropic_messages_llamacpp_stream.sse"

func decodeAnthropicError(t *testing.T, body []byte) (errType, message string) {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e), "body: %s", body)
	require.Equal(t, "error", e.Type, "body: %s", body)
	return e.Error.Type, e.Error.Message
}

// The surface's own refusals reach the client in the Anthropic dialect,
// through the real engine and its route group.
func TestMessagesRoute_RefusalsSpeakAnthropic(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			resp := makeRequest(t, s, TestRequest{Method: http.MethodPost, Path: path,
				Body: map[string]any{"max_tokens": 8, "messages": []any{}}})

			assert.Equal(t, http.StatusBadRequest, resp.Code)
			errType, msg := decodeAnthropicError(t, resp.Body)
			assert.Equal(t, "invalid_request_error", errType)
			assert.Contains(t, msg, "model")
		})
	}
}

// Once routed, a Messages stream reaches the client byte for byte on the
// path it was sent to, and meters its input from message_start and its
// output from message_delta.
func TestMessagesThroughInstance_PassesStreamAndMeters(t *testing.T) {
	stream, err := os.ReadFile(llamacppMessagesStream)
	require.NoError(t, err)

	var gotPath string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream)
	}))
	t.Cleanup(engine.Close)

	// The node installs its own inference-log hook; replace it after.
	s := createTestNodeWithDefaults(t)
	var captured llm.InferenceLogData
	llm.SetInferenceLogHook(captureHook(func(d llm.InferenceLogData) { captured = d }))
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })

	body := []byte(`{"model":"org/model","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := routedRequest(httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))
	ctx := context.WithValue(req.Context(), CtxKeyOriginalBody, body)
	ctx = context.WithValue(ctx, CtxKeyModel, "org/model")
	ctx = context.WithValue(ctx, CtxKeyInferenceRecorder, llm.NewInferenceRecorder(ctx, "org/model", ""))
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	s.proxyToInstance(rec, req, instanceFor(t, engine, "org/model", ""), true)

	got, _ := io.ReadAll(rec.Result().Body)
	assert.Equal(t, "/v1/messages", gotPath, "the engine must be asked on the client's path")
	assert.Equal(t, string(stream), string(got), "no [DONE], no rewrite")
	assert.Equal(t, int64(59), captured.TokensIn)
	assert.Equal(t, int64(20), captured.TokensOut)
	assert.Equal(t, int64(55), captured.TokensCached)
}

// serviceBackend is a service provider for the group tests: it records
// what reached it and answers status with reply as contentType (JSON when
// empty), or with a Messages body naming its own model when reply is empty.
type serviceBackend struct {
	wire        []string
	status      int
	reply       string
	contentType string

	mu   sync.Mutex
	hits []recordedRequest
}

func (b *serviceBackend) requests() []recordedRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recordedRequest(nil), b.hits...)
}

// newServiceGroupNode serves each backend as a service provider of the
// same name and routes the alias "claude" over them in the order given.
func newServiceGroupNode(t *testing.T, order []string, backends map[string]*serviceBackend) *Server {
	t.Helper()
	s := createTestNodeWithDefaults(t)
	s.appsConfig = &pkgConfig.AppsConfig{Version: "1.0", Name: "test"}
	enabled := true
	var replicas []modelgroup.Replica
	for i, name := range order {
		b := backends[name]
		reply, contentType := b.reply, b.contentType
		if contentType == "" {
			contentType = "application/json"
		}
		if reply == "" {
			reply = `{"type":"message","model":"` + name + `-model","usage":{"input_tokens":1,"output_tokens":1}}`
		}
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			b.mu.Lock()
			b.hits = append(b.hits, recordedRequest{Method: r.Method, Path: r.URL.RequestURI(), Body: body, Headers: r.Header.Clone()})
			b.mu.Unlock()
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(b.status)
			_, _ = w.Write([]byte(reply))
		}))
		t.Cleanup(engine.Close)
		require.NoError(t, s.appsConfig.AddApp(name, pkgConfig.ServiceConfig{
			Enabled: &enabled, Name: name, Mode: "service", Protocol: pkgConfig.ProtocolOpenAI,
			Runtime:      &pkgConfig.AppRuntimeConfig{Endpoint: engine.URL},
			Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: b.wire},
		}))
		replicas = append(replicas, modelgroup.Replica{Name: name, App: name, Model: name + "-model", Priority: i + 1})
	}
	require.NoError(t, s.model.Groups.Set("claude", modelgroup.ModelGroup{Strategy: modelgroup.StrategyPriority, Replicas: replicas}))
	return s
}

func postAsAdmin(t *testing.T, s *Server, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", TestAdminKey)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	return w
}

// A group alias is checked replica by replica, not against the model
// cache it is not in: a replica that does not speak Messages is never
// sent one, the next takes over when the first fails, and the body goes
// out under the replica's model with nothing of Chat's added to it.
func TestMessagesRoute_GroupFailsOverAcrossMessagesReplicas(t *testing.T) {
	backends := map[string]*serviceBackend{
		"chatonly": {wire: []string{"chat_completions"}, status: http.StatusOK},
		"busy":     {wire: []string{"chat_completions", messagesEndpoint}, status: http.StatusServiceUnavailable},
		"spare":    {wire: []string{messagesEndpoint}, status: http.StatusOK},
	}
	s := newServiceGroupNode(t, []string{"chatonly", "busy", "spare"}, backends)

	w := postAsAdmin(t, s, "/v1/messages?beta=true",
		`{"model":"claude","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	assert.Empty(t, backends["chatonly"].requests(), "a replica without messages must not be tried")
	assert.NotEmpty(t, backends["busy"].requests(), "the first Messages replica is tried first")
	spare := backends["spare"].requests()
	require.Len(t, spare, 1)
	hit := spare[0]
	assert.Equal(t, "/v1/messages?beta=true", hit.Path)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(hit.Body, &sent))
	assert.Equal(t, "spare-model", sent["model"], "the alias must not reach the engine")
	assert.NotContains(t, sent, "stream_options", "an Anthropic body never carries stream_options")
}

// A group none of whose replicas speak Messages is refused before
// anything is sent, on both routes.
func TestMessagesRoute_GroupWithoutMessagesReplicaRefused(t *testing.T) {
	backends := map[string]*serviceBackend{
		"chatonly": {wire: []string{"chat_completions"}, status: http.StatusOK},
	}
	s := newServiceGroupNode(t, []string{"chatonly"}, backends)

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			w := postAsAdmin(t, s, path, `{"model":"claude","max_tokens":8,"messages":[]}`)

			assert.Equal(t, http.StatusBadRequest, w.Code)
			errType, msg := decodeAnthropicError(t, w.Body.Bytes())
			assert.Equal(t, "invalid_request_error", errType)
			assert.Contains(t, msg, messagesEndpoint)
			assert.Empty(t, backends["chatonly"].requests())
		})
	}
}

// Chat through a group still has usage forced on: the chain no longer
// does it, so the Chat surface must before handing the body over.
func TestChatRoute_GroupStillForcesUsage(t *testing.T) {
	backends := map[string]*serviceBackend{
		"chat": {wire: []string{"chat_completions"}, status: http.StatusOK, reply: `{"id":"c","choices":[]}`},
	}
	s := newServiceGroupNode(t, []string{"chat"}, backends)

	w := postAsAdmin(t, s, "/v1/chat/completions",
		`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	hits := backends["chat"].requests()
	require.Len(t, hits, 1)
	assert.Contains(t, string(hits[0].Body), `"include_usage":true`)
}

// Usage forced on for metering is not shown to a group caller who never
// asked for it: the chain is told to drop the terminal usage-only frame.
func TestChatRoute_GroupStripsUnaskedUsageFrame(t *testing.T) {
	const stream = "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	backends := map[string]*serviceBackend{
		"chat": {wire: []string{"chat_completions"}, status: http.StatusOK, reply: stream, contentType: "text/event-stream"},
	}
	s := newServiceGroupNode(t, []string{"chat"}, backends)

	w := postAsAdmin(t, s, "/v1/chat/completions",
		`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	assert.Contains(t, w.Body.String(), `"content":"hi"`)
	assert.NotContains(t, w.Body.String(), `"prompt_tokens"`)
}

// A group-routed Responses call records the replica that answered, not
// the group's first, so a later retrieval goes where the state is.
func TestResponsesRoute_GroupRecordsServingReplica(t *testing.T) {
	const reply = `{"id":"resp_spare","object":"response"}`
	backends := map[string]*serviceBackend{
		"busy":  {wire: []string{"responses"}, status: http.StatusServiceUnavailable, reply: reply},
		"spare": {wire: []string{"responses"}, status: http.StatusOK, reply: reply},
	}
	s := newServiceGroupNode(t, []string{"busy", "spare"}, backends)

	w := postAsAdmin(t, s, "/v1/responses", `{"model":"claude","input":"hi"}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	provider, ok := s.inference.affinity.Lookup("resp_spare")
	require.True(t, ok)
	assert.Equal(t, "spare", provider)
}

// A body over the engine-wide cap is a 413 in the route's dialect, not a
// 400: Claude Code reaches the cap with images and PDFs, and a 400 reads
// as a malformed request rather than one to shrink.
func TestBodyOverSizeCapIs413(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	require.Zero(t, s.config.Node.MaxRequestSize, "the body below is sized for the default cap")
	oversize := `{"model":"m","pad":"` + strings.Repeat("x", defaultMaxRequestSize) + `"}`

	t.Run("anthropic", func(t *testing.T) {
		w := postAsAdmin(t, s, "/v1/messages", oversize)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		errType, _ := decodeAnthropicError(t, w.Body.Bytes())
		assert.Equal(t, "request_too_large", errType)
	})
	t.Run("openai", func(t *testing.T) {
		w := postAsAdmin(t, s, "/v1/chat/completions", oversize)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Contains(t, w.Body.String(), `"code":"request_too_large"`)
	})
}

// count_tokens is routed like inference but is not inference: with no
// recorder of its own, the proxy site used to make one and log a keyless
// 0/0 row, on the coordinator and again on the worker. Messages through
// the same instance still logs, so the hook is known to be live.
func TestCountTokensIsNotLoggedAsInference(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			_, _ = w.Write([]byte(`{"input_tokens":7}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":7,"output_tokens":1}}`))
	}))
	t.Cleanup(engine.Close)

	s := createTestNodeWithDefaults(t)
	inst := instanceFor(t, engine, "org/model", "")
	inst.Endpoint = "chat"
	require.NoError(t, s.providers.appMgr.Instances().Register(inst))

	var mu sync.Mutex
	var logged []llm.InferenceLogData
	llm.SetInferenceLogHook(captureHook(func(d llm.InferenceLogData) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, d)
	}))
	t.Cleanup(func() { llm.SetInferenceLogHook(nil) })
	rows := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(logged)
	}

	// The instance proxy needs a real connection: httputil.ReverseProxy
	// asks the writer for CloseNotify, which a ResponseRecorder lacks.
	node := httptest.NewServer(s.engine)
	t.Cleanup(node.Close)
	post := func(path string) (int, string) {
		req, err := http.NewRequest(http.MethodPost, node.URL+path,
			strings.NewReader(`{"model":"org/model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", TestAdminKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(got)
	}

	code, got := post("/v1/messages/count_tokens")
	require.Equal(t, http.StatusOK, code, "body: %s", got)
	assert.JSONEq(t, `{"input_tokens":7}`, got)
	assert.Zero(t, rows(), "count_tokens must not reach the inference log")

	code, got = post("/v1/messages")
	require.Equal(t, http.StatusOK, code, "body: %s", got)
	assert.Equal(t, 1, rows())
}

// All three proxy sites take their recorder from requestRecorder, so its
// three answers are what keep a marked request out of the log everywhere.
func TestRequestRecorder(t *testing.T) {
	stashed := llm.NewInferenceRecorder(context.Background(), "m", "")
	withStash := context.WithValue(context.Background(), CtxKeyInferenceRecorder, stashed)
	marked := context.WithValue(withStash, CtxKeyNotInference, true)

	assert.Same(t, stashed, requestRecorder(withStash, "m", "p"), "the handler's recorder is reused")
	assert.Nil(t, requestRecorder(marked, "m", "p"), "the marker wins, even over a stashed recorder")
	assert.NotNil(t, requestRecorder(context.Background(), "m", "p"), "an unmarked request is still logged")
}

// A direct (non-group) reply names the node that served it, as chat's
// does: the provider forwards read the node off the request, so dispatch
// has to put it there. A group reply gets it from the chain's commit.
//
// Driven on the worker's cluster engine, the only surface that honours a
// provider header, as the worker's leg of a coordinator hop: the reply
// names this node, not whatever node header arrived.
func TestMessagesRoute_DirectReplyNamesServingNode(t *testing.T) {
	backends := map[string]*serviceBackend{
		"svc": {wire: []string{messagesEndpoint}, status: http.StatusOK},
	}
	s := newServiceGroupNode(t, []string{"svc"}, backends)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"direct-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(constants.HeaderServingProvider, "svc")
	req.Header.Set(constants.HeaderServingNode, "somewhere-else")
	w := httptest.NewRecorder()
	buildWorkerCompatEngine(s).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body)
	require.Len(t, backends["svc"].requests(), 1)
	assert.Equal(t, s.node.Nodename(), w.Header().Get(constants.HeaderServingNode))
}

// Only a coordinator may name the provider or node a request goes to. A
// client that sends the same headers to a node's public surface steers
// nothing: the model resolves as if they were absent.
func TestClientRoutingHintsAreDropped(t *testing.T) {
	backends := map[string]*serviceBackend{
		"svc": {wire: []string{messagesEndpoint}, status: http.StatusOK},
	}
	s := newServiceGroupNode(t, []string{"svc"}, backends)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"direct-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", TestAdminKey)
	req.Header.Set(constants.HeaderServingProvider, "svc")
	req.Header.Set(constants.HeaderServingNode, "somewhere-else")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body)
	assert.Contains(t, w.Body.String(), `"type":"not_found_error"`, "refused as an unknown model, not for some other reason")
	assert.Empty(t, backends["svc"].requests(), "a client header must not pick the provider")
}

// A stream zzRouter rewrites on the way out must not keep the upstream's
// Content-Length. An engine that sends its whole stream in one write gets
// one stamped; usage injection then makes the body longer, and the
// client is promised a length the body never reaches (unexpected EOF),
// which Claude Code retries until it gives up. Driven over a real
// connection, the only place the length is enforced.
func TestRewrittenStreamDropsUpstreamLength(t *testing.T) {
	const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	backends := map[string]*serviceBackend{
		"svc": {wire: []string{messagesEndpoint}, status: http.StatusOK, reply: stream, contentType: "text/event-stream"},
	}
	s := newServiceGroupNode(t, []string{"svc"}, backends)
	s.config.Coordinator.Routing.InjectUsageMetadata = true

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(stream)))
		_, _ = w.Write([]byte(stream))
	}))
	t.Cleanup(engine.Close)
	inst := instanceFor(t, engine, "local-model", "")
	inst.Endpoint = "chat"
	require.NoError(t, s.providers.appMgr.Instances().Register(inst))

	node := httptest.NewServer(s.engine)
	t.Cleanup(node.Close)

	for _, model := range []string{"local-model", "claude"} {
		t.Run(model, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, node.URL+"/v1/messages",
				strings.NewReader(`{"model":"`+model+`","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", TestAdminKey)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", got)
			assert.Contains(t, string(got), "event: message_stop", "the whole stream must arrive")
			assert.Contains(t, string(got), `"zz_provider"`, "usage was injected")
		})
	}
}

// The Responses stream translator replaces the whole body, so a length
// the backend declared for the Chat stream must not reach the client.
func TestResponsesStreamWriterDropsUpstreamLength(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer.Header().Set("Content-Length", "123")

	w := newResponsesStreamWriter(c.Writer, "m")
	w.WriteHeader(http.StatusOK)

	assert.Empty(t, rec.Header().Get("Content-Length"))
}
