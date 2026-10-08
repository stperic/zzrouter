package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	payloadTestClientBody   = `{"model":"fast-chat","messages":[{"role":"user","content":"hi"}]}`
	payloadTestUpstreamBody = `{"model":"llama3:8b","messages":[{"role":"user","content":"hi"}]}`
)

func mountInferenceLogs(t *testing.T, maxPayloads int) (*gin.Engine, *inferencelog.Store, *InferenceLogBridge) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	store := inferencelog.NewStore(10, maxPayloads)
	t.Cleanup(store.Stop)
	bridge := NewInferenceLogBridge(store, "test-node", true, nil)

	r := gin.New()
	api := r.Group("/zzrouter/v1")
	NewInferenceLogController(NewInferenceLogService(store), nil).RegisterPublicRoutes(api)
	return r, store, bridge
}

// completeInference drives one inference through the bridge and returns
// the ID of the entry it produced.
func completeInference(t *testing.T, store *inferencelog.Store, bridge *InferenceLogBridge, client, upstream string) string {
	t.Helper()
	return completeInferenceWithReply(t, store, bridge, client, upstream, "", nil)
}

func completeInferenceWithReply(t *testing.T, store *inferencelog.Store, bridge *InferenceLogBridge,
	client, upstream, replyText string, replyBody []byte,
) string {
	t.Helper()
	bridge.OnInferenceComplete(llm.InferenceLogData{
		Model:        "fast-chat",
		App:          "ollama",
		Status:       inferencelog.StatusSuccess,
		RequestBody:  []byte(client),
		UpstreamBody: []byte(upstream),
		ResponseText: replyText,
		ResponseBody: replyBody,
	})
	entries := store.Query(inferencelog.QueryFilter{Limit: 1})
	require.Len(t, entries, 1)
	return entries[0].ID
}

func getPayload(t *testing.T, r *gin.Engine, id string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/zzrouter/v1/inference-logs/"+id+"/payload", nil))
	return w
}

func TestInferenceLogPayload_ServesBothBodies(t *testing.T) {
	r, store, bridge := mountInferenceLogs(t, 5)
	id := completeInference(t, store, bridge, payloadTestClientBody, payloadTestUpstreamBody)

	w := getPayload(t, r, id)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data inferencelog.Payload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.JSONEq(t, payloadTestClientBody, string(resp.Data.Request))
	assert.JSONEq(t, payloadTestUpstreamBody, string(resp.Data.Upstream),
		"the model rewrite between client and provider must be visible")
}

func TestInferenceLogPayload_ListFlagsAvailability(t *testing.T) {
	r, store, bridge := mountInferenceLogs(t, 5)
	completeInference(t, store, bridge, payloadTestClientBody, payloadTestUpstreamBody)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/zzrouter/v1/inference-logs", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data []inferencelog.LogEntry `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 1)
	assert.True(t, resp.Data[0].PayloadAvailable)
}

func TestInferenceLogPayload_404sSeparatelyForAgedOutAndUnknown(t *testing.T) {
	r, store, bridge := mountInferenceLogs(t, 1)
	aged := completeInference(t, store, bridge, payloadTestClientBody, payloadTestUpstreamBody)
	completeInference(t, store, bridge, payloadTestClientBody, payloadTestUpstreamBody)

	w := getPayload(t, r, aged)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "no longer retained")

	w = getPayload(t, r, "does-not-exist")
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "entry not found")
}

func TestInferenceLogPayload_RetentionDisabled(t *testing.T) {
	r, store, bridge := mountInferenceLogs(t, 0)
	id := completeInference(t, store, bridge, payloadTestClientBody, payloadTestUpstreamBody)

	require.Equal(t, http.StatusNotFound, getPayload(t, r, id).Code)
}

// Payloads carry prompt content, so the existing capture_prompts=false
// posture must suppress them too.
func TestInferenceLogPayload_NotCapturedWhenPromptsDisabled(t *testing.T) {
	store := inferencelog.NewStore(10, 5)
	t.Cleanup(store.Stop)
	bridge := NewInferenceLogBridge(store, "test-node", false, nil)

	id := completeInference(t, store, bridge, payloadTestClientBody, payloadTestUpstreamBody)

	_, ok := store.Payload(id)
	assert.False(t, ok)
}

// An attached image must reach the payload as a described placeholder,
// never as megabytes of base64.
func TestInferenceLogPayload_ElidesAttachedMedia(t *testing.T) {
	blob := strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 20000)
	body := `{"model":"llava","messages":[{"role":"user","content":"what is this?",` +
		`"images":["` + blob + `"]}]}`

	r, store, bridge := mountInferenceLogs(t, 5)
	id := completeInference(t, store, bridge, body, body)

	w := getPayload(t, r, id)
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), blob)

	var resp struct {
		Data inferencelog.Payload `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, string(resp.Data.Request), "what is this?")
	require.Len(t, resp.Data.Elided, 1)
	assert.Equal(t, "messages[0].images[0]", resp.Data.Elided[0].Path)
	assert.Equal(t, len(blob), resp.Data.Elided[0].Bytes)
}

// The firehose mirrors entries, not bodies — a payload on the SSE path
// would put a full prompt on every event.
func TestInferenceLogPayload_NotMarshaledIntoLogEntry(t *testing.T) {
	_, store, bridge := mountInferenceLogs(t, 5)
	completeInference(t, store, bridge, payloadTestClientBody, payloadTestUpstreamBody)

	entries := store.Query(inferencelog.QueryFilter{Limit: 1})
	require.Len(t, entries, 1)
	raw, err := json.Marshal(entries[0])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"upstream_request"`)
}

// The reply must land on the entry itself. Without it an operator has to
// read the next request's history to find out what the model said.
func TestInferenceLog_EntryCarriesTheReply(t *testing.T) {
	_, store, bridge := mountInferenceLogs(t, 5)
	reply := `{"choices":[{"message":{"content":"Hey Eric!"}}]}`

	id := completeInferenceWithReply(t, store, bridge,
		payloadTestClientBody, payloadTestUpstreamBody, "Hey Eric!", []byte(reply))

	entry, ok := store.Get(id)
	require.True(t, ok)
	assert.Equal(t, "Hey Eric!", entry.Response)

	payload, ok := store.Payload(id)
	require.True(t, ok)
	assert.JSONEq(t, reply, string(payload.Response))
}

func TestInferenceLog_ReplySuppressedWhenPromptsDisabled(t *testing.T) {
	store := inferencelog.NewStore(10, 5)
	t.Cleanup(store.Stop)
	bridge := NewInferenceLogBridge(store, "test-node", false, nil)

	id := completeInferenceWithReply(t, store, bridge,
		payloadTestClientBody, payloadTestUpstreamBody, "Hey Eric!",
		[]byte(`{"choices":[{"message":{"content":"Hey Eric!"}}]}`))

	entry, ok := store.Get(id)
	require.True(t, ok)
	assert.Empty(t, entry.Response, "replies carry the same content capture_prompts gates")
}

// A streamed reply has no single body, but its text must still be there.
func TestInferenceLog_StreamedReplyHasTextButNoBody(t *testing.T) {
	_, store, bridge := mountInferenceLogs(t, 5)

	id := completeInferenceWithReply(t, store, bridge,
		payloadTestClientBody, payloadTestUpstreamBody, "streamed reply", nil)

	entry, ok := store.Get(id)
	require.True(t, ok)
	assert.Equal(t, "streamed reply", entry.Response)

	payload, ok := store.Payload(id)
	require.True(t, ok)
	assert.Empty(t, payload.Response)
}
