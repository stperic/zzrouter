package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestExternalOpenAICatalogServesThroughSharedRoutes(t *testing.T) {
	const upstreamID = "strata-model"
	var chats, upstreamDeletes atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer provider-token", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("X-API-Key"), "the caller key must stop at zzRouter")
		if r.Method == http.MethodDelete {
			upstreamDeletes.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"` + upstreamID + `","status":"unloaded"}]}`))
		case "/v1/chat/completions":
			chats.Add(1)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, upstreamID, body["model"])
			if body["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"id\":\"c5\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: {\"id\":\"c5\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
					"data: [DONE]\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"` + upstreamID + `","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		case "/v1/responses":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, upstreamID, body["model"])
			if body["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"object\":\"response\",\"status\":\"completed\"}}\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","model":"` + upstreamID + `","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(engine.Close)
	s := createTestNodeWithDefaults(t)
	s.appsConfig = &config.AppsConfig{}
	require.NoError(t, s.appsConfig.AddApp("strata", config.ServiceConfig{
		Name: "strata", Enabled: new(true), Mode: "external", Protocol: config.ProtocolOpenAI,
		Runtime:      &config.AppRuntimeConfig{Endpoint: engine.URL, API: &config.APIConfig{AuthType: config.AuthTypeBearer, AuthHeader: "Authorization", AuthPrefix: "Bearer ", Token: "provider-token"}},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions", "responses", "messages_compat"}},
	}))
	s.providers.appMgr.ReloadConfig(s.appsConfig)
	s.Invalidate()

	list := makeAuthRequest(t, s, http.MethodGet, "/v1/models", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, list.Code, string(list.Body))
	var catalog OpenAIModelListResponse
	require.NoError(t, json.Unmarshal(list.Body, &catalog))
	id := upstreamID + "@" + s.node.Nodename()
	listed := false
	for _, model := range catalog.Data {
		listed = listed || (model.ID == id && model.ZZRouter != nil && model.ZZRouter.Provider == "strata")
	}
	require.True(t, listed, "the upstream model is published under the shared name@node id")

	for _, model := range []string{upstreamID, id} {
		response := makeAuthRequest(t, s, http.MethodPost, "/v1/chat/completions", TestAdminKey,
			map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
		require.Equal(t, http.StatusOK, response.Code, "%s: %s", model, string(response.Body))
	}
	require.EqualValues(t, 2, chats.Load())
	get := makeAuthRequest(t, s, http.MethodGet, "/v1/models/"+id, TestAdminKey, nil)
	require.Equal(t, http.StatusOK, get.Code, string(get.Body))

	responseAPI := makeAuthRequest(t, s, http.MethodPost, "/v1/responses", TestAdminKey, map[string]any{"model": id, "input": "hi"})
	require.Equal(t, http.StatusOK, responseAPI.Code, string(responseAPI.Body))
	responsesStream := makeAuthRequest(t, s, http.MethodPost, "/v1/responses", TestAdminKey, map[string]any{"model": id, "input": "hi", "stream": true})
	require.Equal(t, http.StatusOK, responsesStream.Code, string(responsesStream.Body))
	require.Contains(t, string(responsesStream.Body), "response.completed")
	messageAPI := makeAuthRequest(t, s, http.MethodPost, "/v1/messages", TestAdminKey,
		map[string]any{"model": id, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	require.Equal(t, http.StatusOK, messageAPI.Code, string(messageAPI.Body))
	messageStream := makeAuthRequest(t, s, http.MethodPost, "/v1/messages", TestAdminKey,
		map[string]any{"model": id, "max_tokens": 16, "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	require.Equal(t, http.StatusOK, messageStream.Code, string(messageStream.Body))
	require.Contains(t, string(messageStream.Body), "event: message_stop")

	show := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/models/show?model="+upstreamID+"&provider=strata", TestAdminKey, nil)
	require.Equal(t, http.StatusNotImplemented, show.Code, string(show.Body))
	require.Contains(t, string(show.Body), "model_detail_unsupported")
	deletion := makeAuthRequest(t, s, http.MethodDelete, "/zzrouter/v1/models", TestAdminKey, map[string]any{"model": upstreamID, "provider": "strata"})
	require.Equal(t, http.StatusConflict, deletion.Code, string(deletion.Body))
	require.Contains(t, string(deletion.Body), "model_not_deletable")
	bulk := makeAuthRequest(t, s, http.MethodDelete, "/zzrouter/v1/models", TestAdminKey, map[string]any{"pattern": upstreamID})
	require.Equal(t, http.StatusOK, bulk.Code, string(bulk.Body))
	require.Contains(t, string(bulk.Body), "external provider owns its model")
	require.Zero(t, upstreamDeletes.Load())
}
