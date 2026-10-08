package client

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderAssetMethods(t *testing.T) {
	var gotBody []byte
	var gotType, gotQuery string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "test-key", r.Header.Get("X-API-Key"))
		switch r.Method + " " + r.URL.Path {
		case "GET /zzrouter/v1/providers/eng/assets":
			_, _ = io.WriteString(w, `{"assets":[{"name":"t.jinja","size":2,"sha256":"ab","shipped":true,"referenced_by":["defaults.parameters.k"]}]}`)
		case "GET /zzrouter/v1/providers/eng/assets/t.jinja":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{0x00, 0xff})
		case "PUT /zzrouter/v1/providers/eng/assets/t.jinja":
			gotBody, _ = io.ReadAll(r.Body)
			gotType, gotQuery = r.Header.Get("Content-Type"), r.URL.RawQuery
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"t.jinja","size":2,"sha256":"cd","shipped":false,"referenced_by":[]}`)
		case "DELETE /zzrouter/v1/providers/eng/assets/t.jinja":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"type":"about:blank","title":"Conflict","status":409,"code":"asset_in_use",`+
				`"detail":"defaults.parameters.k names asset \"t.jinja\"",`+
				`"errors":[{"key":"defaults.parameters.k","code":"asset_in_use","message":"defaults.parameters.k names asset \"t.jinja\""}]}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))

	list, err := c.ListProviderAssets("eng")
	require.NoError(t, err)
	assert.Equal(t, []ProviderAsset{{Name: "t.jinja", Size: 2, SHA256: "ab", Shipped: true, ReferencedBy: []string{"defaults.parameters.k"}}}, list)

	data, err := c.GetProviderAsset("eng", "t.jinja")
	require.NoError(t, err)
	assert.Equal(t, []byte{0x00, 0xff}, data, "bytes come back as sent, not JSON-decoded")

	asset, err := c.PutProviderAsset("eng", "t.jinja", []byte{0x01, 0x02}, false)
	require.NoError(t, err)
	assert.Empty(t, gotQuery, "nothing restarts unless asked")
	_, err = c.PutProviderAsset("eng", "t.jinja", []byte{0x01, 0x02}, true)
	require.NoError(t, err)
	assert.Equal(t, "restart=affected", gotQuery)
	assert.Equal(t, []byte{0x01, 0x02}, gotBody, "the file goes up raw, not JSON-encoded")
	assert.Equal(t, "application/octet-stream", gotType)
	assert.Equal(t, "cd", asset.SHA256)

	err = c.DeleteProviderAsset("eng", "t.jinja")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.StatusCode)
	assert.Contains(t, apiErr.Message, "names asset")
}

func TestGetProviderAsset_NotFound(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"title":"Not Found","status":404,"detail":"asset not found"}`)
	}))
	_, err := c.GetProviderAsset("eng", "absent")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}
