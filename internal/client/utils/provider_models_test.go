package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type providerModelsTransport func(*http.Request) (*http.Response, error)

func (f providerModelsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderModelClientRequests(t *testing.T) {
	workflow := json.RawMessage(`{"nodes":[{"properties":{"models":[{"name":"nested/m.safetensors","url":"org/repo#m.safetensors","directory":"vae"}]}}]}`)
	var calls int
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := &Client{baseURL: "https://coordinator.test", apiKey: "test-key", httpClient: &http.Client{Transport: providerModelsTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "test-key", r.Header.Get("X-API-Key"))
		require.Equal(t, ctx.Done(), r.Context().Done(), "command cancellation reaches the request")
		require.True(t, strings.HasPrefix(r.URL.EscapedPath(), "/zzrouter/v1/providers/studio+test/models"))
		w := httptest.NewRecorder()
		switch calls {
		case 1:
			require.Equal(t, http.MethodGet, r.Method)
			require.Equal(t, "worker+1", r.URL.Query().Get("node"))
			require.Equal(t, "vae", r.URL.Query().Get("category"))
			_, _ = io.WriteString(w, `{"data":{"node":"worker+1","categories":[{"name":"vae","files":[{"name":"nested/m.safetensors","size_bytes":7}]}]}}`)
		case 2:
			require.Equal(t, http.MethodPost, r.Method)
			require.Empty(t, r.URL.RawQuery)
			require.Equal(t, "application/json", r.Header.Get("Content-Type"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, map[string]any{"node": "worker+1", "category": "vae", "source": "org/repo#m.safetensors"}, body, "omitted name lets the server choose the source basename")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"data":{"provider":"studio+test","node":"worker-owner","category":"vae","name":"m.safetensors","source":"org/repo#m.safetensors","revision":"main","status":"downloading","job_id":"job-1"}}`)
		case 3:
			require.Equal(t, http.MethodDelete, r.Method)
			require.Equal(t, "nested/a+b.safetensors", r.URL.Query().Get("name"))
			require.Equal(t, "worker+1", r.URL.Query().Get("node"))
			require.Equal(t, "vae", r.URL.Query().Get("category"))
			_, _ = io.WriteString(w, `{"data":{"deleted":true}}`)
		case 4:
			require.Equal(t, http.MethodPost, r.Method)
			require.True(t, strings.HasSuffix(r.URL.Path, "/models/resolve"))
			var body ProviderModelResolveRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "worker+1", body.Node)
			require.JSONEq(t, string(workflow), string(body.Workflow))
			require.True(t, body.Download)
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"data":{"node":"worker-owner","requirements":[{"status":"downloading","job_id":"job-1"},{"status":"failed","reason":"file_exists"}]}}`)
		default:
			t.Fatalf("unexpected request %d", calls)
		}
		return w.Result(), nil
	})}}
	list, err := c.ListProviderModels(ctx, "studio+test", "worker+1", "vae")
	require.NoError(t, err)
	require.Len(t, list["categories"], 1)
	job, err := c.DownloadProviderModel(ctx, "studio+test", ProviderModelDownloadRequest{Node: "worker+1", Category: "vae", Source: "org/repo#m.safetensors"})
	require.NoError(t, err)
	require.Equal(t, "worker-owner", job.Node)
	require.Equal(t, "job-1", job.JobID)
	deleted, err := c.DeleteProviderModel(ctx, "studio+test", "worker+1", "vae", "nested/a+b.safetensors")
	require.NoError(t, err)
	require.Equal(t, true, deleted["deleted"])
	resolved, err := c.ResolveProviderModels(ctx, "studio+test", ProviderModelResolveRequest{Node: "worker+1", Workflow: workflow, Download: true})
	require.NoError(t, err)
	require.Len(t, resolved["requirements"], 2)
	require.Equal(t, 4, calls)
}

func TestProviderModelClientPreservesProblemAndCancellation(t *testing.T) {
	c := &Client{baseURL: "https://coordinator.test", httpClient: &http.Client{Transport: providerModelsTransport(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"title":"Conflict","status":409,"code":"file_exists","detail":"destination already exists"}`)
		return w.Result(), nil
	})}}
	_, err := c.DeleteProviderModel(t.Context(), "studio", "worker", "vae", "m")
	var problem *APIError
	require.ErrorAs(t, err, &problem)
	require.Equal(t, http.StatusConflict, problem.StatusCode)
	require.Contains(t, problem.Message, "destination already exists")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.ListProviderModels(ctx, "studio", "worker", "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestProviderModelTemplateClientRequests(t *testing.T) {
	calls := 0
	c := &Client{baseURL: "https://coordinator.test", apiKey: "test-key", httpClient: &http.Client{Transport: providerModelsTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "test-key", r.Header.Get("X-API-Key"))
		w := httptest.NewRecorder()
		switch calls {
		case 1:
			require.Equal(t, "/zzrouter/v1/providers/studio/templates", r.URL.Path)
			require.Equal(t, "Turbo", r.URL.Query().Get("search"))
			require.Equal(t, "worker", r.URL.Query().Get("node"))
			for key, value := range map[string]string{"sort": "date", "order": "desc", "since": "2026-10-01", "open_source": "false", "limit": "10"} {
				require.Equal(t, value, r.URL.Query().Get(key))
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"one","date":"2026-11-05"}],"total":1,"has_more":false}`)
		case 2:
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "one", body["template"])
			require.NotContains(t, body, "workflow")
			_, _ = io.WriteString(w, `{"data":{"requirements":[],"template":{"id":"one"}}}`)
		case 3:
			require.Equal(t, http.MethodGet, r.Method)
			require.Equal(t, "/zzrouter/v1/providers/studio/templates/one", r.URL.Path)
			require.Equal(t, "worker", r.URL.Query().Get("node"))
			_, _ = io.WriteString(w, `{"data":{"template":{"id":"one"},"workflow":{"nodes":[{"widgets_values":[9007199254740993,18446744073709551615]}]}}}`)
		default:
			t.Fatal("unexpected request")
		}
		return w.Result(), nil
	})}}
	catalog, err := c.ListProviderTemplates(t.Context(), "studio", url.Values{"node": {"worker"}, "search": {"Turbo"}, "sort": {"date"}, "order": {"desc"}, "since": {"2026-10-01"}, "open_source": {"false"}, "limit": {"10"}})
	require.NoError(t, err)
	require.Equal(t, false, catalog["has_more"])
	require.Equal(t, "2026-11-05", catalog["data"].([]any)[0].(map[string]any)["date"])
	resolved, err := c.ResolveProviderModels(t.Context(), "studio", ProviderModelResolveRequest{Node: "worker", Template: "one", Download: true})
	require.NoError(t, err)
	require.Contains(t, resolved, "template")
	selected, err := c.GetProviderTemplate(t.Context(), "studio", "worker", "one")
	require.NoError(t, err)
	require.Contains(t, string(selected.Workflow), "9007199254740993")
	require.Contains(t, string(selected.Workflow), "18446744073709551615")
	require.Equal(t, 3, calls)
}

func TestSearchQueryClientProviderDefaults(t *testing.T) {
	for _, sort := range []string{"", "date"} {
		c := &Client{baseURL: "https://coordinator.test", httpClient: &http.Client{Transport: providerModelsTransport(func(r *http.Request) (*http.Response, error) {
			q := r.URL.Query()
			require.Equal(t, "openai", q.Get("provider"))
			require.Equal(t, "image & edit", q.Get("q"))
			require.Equal(t, sort, q.Get("sort"))
			_, present := q["sort"]
			require.Equal(t, sort != "", present, "omission preserves the provider's default")
			w := httptest.NewRecorder()
			_, _ = io.WriteString(w, `{"data":{"results":[]}}`)
			return w.Result(), nil
		})}}
		values := url.Values{"type": {"models"}, "q": {"image & edit"}, "provider": {"openai"}, "limit": {"10"}, "order": {"desc"}}
		if sort != "" {
			values.Set("sort", sort)
		}
		_, err := c.SearchModels(values)
		require.NoError(t, err)
	}
}
