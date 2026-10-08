package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

func TestExpandWireModel(t *testing.T) {
	body := `{"model":"${WIRE_MODEL}","max_tokens":1}`

	t.Run("substitutes the engine's token", func(t *testing.T) {
		out := expandWireModel(body, "/models/org/model")
		var parsed struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &parsed))
		assert.Equal(t, "/models/org/model", parsed.Model)
	})

	t.Run("escapes a path that would break the JSON around it", func(t *testing.T) {
		out := expandWireModel(body, `C:\models\org "quoted"`)
		var parsed struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &parsed), "body must stay valid JSON")
		assert.Equal(t, `C:\models\org "quoted"`, parsed.Model)
	})

	t.Run("bodies without the placeholder are untouched", func(t *testing.T) {
		assert.Equal(t, `{"a":1}`, expandWireModel(`{"a":1}`, "x"))
	})
}

func TestMonitor_Serves(t *testing.T) {
	newInstance := func(t *testing.T, srv *httptest.Server) *instance.Instance {
		t.Helper()
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		port, err := strconv.Atoi(u.Port())
		require.NoError(t, err)
		inst := instance.NewInstance("id", "engine", "org/model", port, 0, 0)
		inst.WireModel = "/models/org/model"
		return inst
	}

	t.Run("no check means the caller's signal stands", func(t *testing.T) {
		m := NewMonitor(instance.NewRegistry(), DefaultMonitorConfig())
		assert.True(t, m.Serves(t.Context(), &instance.Instance{}, nil))
	})

	t.Run("a served request is what marks the engine ready", func(t *testing.T) {
		var gotModel atomic.Value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotModel.Store(body.Model)
			assert.Equal(t, http.MethodPost, r.Method)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		m := NewMonitor(instance.NewRegistry(), DefaultMonitorConfig())
		check := &ServeCheck{Path: "/v1/chat/completions", Body: `{"model":"${WIRE_MODEL}"}`}
		assert.True(t, m.Serves(t.Context(), newInstance(t, srv), check))
		assert.Equal(t, "/models/org/model", gotModel.Load(), "the engine is addressed by its own token")
	})

	t.Run("an engine still loading is not ready", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "model not loaded", http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		m := NewMonitor(instance.NewRegistry(), DefaultMonitorConfig())
		check := &ServeCheck{Path: "/v1/chat/completions", Body: `{"model":"${WIRE_MODEL}"}`}
		assert.False(t, m.Serves(t.Context(), newInstance(t, srv), check))
	})
}
