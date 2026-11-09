package mesh

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMalformedHealthResourcesRemainUnknown(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"hostname":"A","resources":{"node_name":"A","gpu_memory_free_mb":"invalid","ram_available_mb":8192}}}`))
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	c := &Connector{httpClient: srv.Client(), clusterPort: port}
	conn, err := c.performHealthCheck(t.Context(), srv.URL)
	require.NoError(t, err)
	require.Equal(t, QualityFull, conn.Quality)
	require.Nil(t, conn.Resources, "a malformed resource sample must not become partial numeric evidence")
}
