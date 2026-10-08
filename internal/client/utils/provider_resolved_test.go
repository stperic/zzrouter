package client

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A variant write is one merge-patch of one model cell: parameters typed
// by the provider's schema (the validator refuses "131072" for an int),
// request defaults as JSON, deletions as null.
func TestUpdateModelCell(t *testing.T) {
	var got map[string]any
	var gotType, gotQuery string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /zzrouter/v1/providers/llamacpp/schema":
			_, _ = io.WriteString(w, `{"parameters":{"ctx-size":{"type":"int"}},"environment":{}}`)
		case "PATCH /zzrouter/v1/providers/llamacpp/parameters":
			gotType, gotQuery = r.Header.Get("Content-Type"), r.URL.RawQuery
			raw, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(raw, &got))
			_, _ = io.WriteString(w, `{"parameters":{"ctx-size":{"value":"131072","tier":"model","model":"q+agent"}},`+
				`"environment":{},"from":"q","request":{"temperature":{"value":0.7,"tier":"model"}},"runs":{"stale":[]}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))

	res, err := c.UpdateModelCell("llamacpp", "q+agent", ModelCellUpdate{
		From:         "q",
		Parameters:   map[string]string{"ctx-size": "131072"},
		Unset:        []string{"threads"},
		Request:      map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
		UnsetRequest: []string{"temperature"},
	}, true)
	require.NoError(t, err)
	assert.Equal(t, "application/merge-patch+json", gotType)
	assert.Equal(t, "model=q%2Bagent&restart=affected", gotQuery)
	assert.Equal(t, map[string]any{"models": map[string]any{"q+agent": map[string]any{
		"from":       "q",
		"parameters": map[string]any{"ctx-size": float64(131072), "threads": nil},
		"request":    map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}, "temperature": nil},
	}}}, got)
	assert.Equal(t, "q", res.From)
	assert.Equal(t, 0.7, res.Request["temperature"].Value)
	require.NotNil(t, res.Runs)

	require.NoError(t, c.DeleteModelCell("llamacpp", "q+agent"))
	assert.Equal(t, map[string]any{"models": map[string]any{"q+agent": nil}}, got)
}
