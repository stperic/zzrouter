package prov_apps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stretchr/testify/require"
)

type workflowTemplateTransport func(*http.Request) (*http.Response, error)

func (f workflowTemplateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func templateTestClient(t *testing.T, handler http.Handler) *http.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &http.Client{Transport: workflowTemplateTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "raw.githubusercontent.com", r.URL.Host)
		require.True(t, strings.HasPrefix(r.URL.Path, "/Comfy-Org/workflow_templates/main/templates/"))
		copy := r.Clone(r.Context())
		copy.URL.Scheme = "http"
		copy.URL.Host = strings.TrimPrefix(server.URL, "http://")
		return http.DefaultTransport.RoundTrip(copy)
	})}
}

func TestWorkflowTemplateCatalogSelection(t *testing.T) {
	const catalog = `[{"templates":[{"name":"z","title":"Shared","openSource":true},{"name":"a","title":"Shared","openSource":true},{"name":"edit","title":"Image Edit","minComfyUIVersion":"0.39.0"},{"name":"api","title":"API","openSource":false}]}]`
	var paths []string
	client := templateTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "index.json") {
			_, _ = io.WriteString(w, catalog)
			return
		}
		require.True(t, strings.HasSuffix(r.URL.Path, "edit.json"))
		_, _ = io.WriteString(w, `{"nodes":[]}`)
	}))
	source := NewWorkflowTemplateCatalog(client)
	require.Nil(t, client.CheckRedirect, "the reader must not mutate its caller's client")
	list, err := source.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"a", "api", "edit", "z"}, []string{list[0].ID, list[1].ID, list[2].ID, list[3].ID})
	for _, selector := range []string{"edit", "  image EDIT  "} {
		entry, raw, err := source.Resolve(t.Context(), selector)
		require.NoError(t, err)
		require.Equal(t, "edit", entry.ID)
		require.Len(t, entry.SHA256, 64)
		require.Equal(t, "0.39.0", entry.MinComfyUIVersion)
		require.JSONEq(t, `{"nodes":[]}`, string(raw))
	}
	for selector, code := range map[string]httperr.RouteErrorCode{"Shared": httperr.CodeTemplateAmbiguous, "../outside": httperr.CodeTemplateNotFound, "API": httperr.CodeTemplateUnsupported} {
		before := len(paths)
		_, _, err := source.Resolve(t.Context(), selector)
		var wire httperr.Error
		require.ErrorAs(t, err, &wire)
		require.Equal(t, string(code), wire.Code)
		require.Len(t, paths, before+1, "refused selectors never fetch a workflow")
		if code == httperr.CodeTemplateAmbiguous {
			require.Len(t, wire.Extra["templates"], 2)
		}
	}
}

func TestWorkflowTemplateCatalogRefusesInvalidSources(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
	}{
		{"malformed", "[", 200}, {"empty", "[]", 200},
		{"unsafe-id", `[{"templates":[{"name":"../outside","title":"bad"}]}]`, 200},
		{"conflict", `[{"templates":[{"name":"a","title":"one"},{"name":"a","title":"two"}]}]`, 200},
		{"oversized", strings.Repeat(" ", MaxWorkflowBytes+1), 200},
		{"not-found", "", 404}, {"redirect", "", 302},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			source := NewWorkflowTemplateCatalog(templateTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "https://example.test/escape")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})))
			_, err := source.List(t.Context())
			var wire httperr.Error
			require.ErrorAs(t, err, &wire)
			require.Equal(t, http.StatusBadGateway, wire.Status)
			require.Equal(t, 1, calls)
		})
	}
}

func TestWorkflowTemplateCatalogDuplicateAndCancellation(t *testing.T) {
	source := NewWorkflowTemplateCatalog(templateTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"templates": []any{map[string]any{"name": "one", "title": "One", "openSource": true}, map[string]any{"name": "one", "title": "One", "openSource": true}}}})
	})))
	entries, err := source.List(t.Context())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = source.List(ctx)
	var wire httperr.Error
	require.ErrorAs(t, err, &wire)
	require.Equal(t, string(httperr.CodeUpstreamCanceled), wire.Code)
}

func TestWorkflowTemplateRejectsInvalidWorkflowDocument(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{`, strings.Repeat(" ", MaxWorkflowBytes+1)} {
		t.Run(raw[:min(len(raw), 8)], func(t *testing.T) {
			source := NewWorkflowTemplateCatalog(templateTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "index.json") {
					_, _ = io.WriteString(w, `[{"templates":[{"name":"one","title":"One"}]}]`)
					return
				}
				_, _ = io.WriteString(w, raw)
			})))
			_, _, err := source.Resolve(t.Context(), "one")
			var wire httperr.Error
			require.ErrorAs(t, err, &wire)
			require.Equal(t, string(httperr.CodeUpstreamShape), wire.Code)
		})
	}
}

type templateFailedBody struct{ err error }

func (r templateFailedBody) Read([]byte) (int, error) { return 0, r.err }

func TestWorkflowTemplateSourceBodyFailureCodes(t *testing.T) {
	for _, tc := range []struct {
		cause  error
		code   httperr.RouteErrorCode
		status int
	}{{context.Canceled, httperr.CodeUpstreamCanceled, 502}, {context.DeadlineExceeded, httperr.CodeUpstreamTimeout, 504}, {io.ErrUnexpectedEOF, httperr.CodeTemplateSourceUnavailable, 502}} {
		t.Run(string(tc.code), func(t *testing.T) {
			source := NewWorkflowTemplateCatalog(&http.Client{Transport: workflowTemplateTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(templateFailedBody{err: tc.cause})}, nil
			})})
			_, err := source.List(t.Context())
			var wire httperr.Error
			require.ErrorAs(t, err, &wire)
			require.Equal(t, string(tc.code), wire.Code)
			require.Equal(t, tc.status, wire.Status)
		})
	}
}
