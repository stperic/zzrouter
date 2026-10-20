package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperatorSchemaSyncReachesWorkerAndGates16(t *testing.T) {
	source := newFanOutTestStore(t)
	require.NoError(t, source.ApplyParameterPatch("ollama", func(sc *config.ServiceConfig) error {
		sc.Schema = map[string]config.ParameterDeclaration{"future_flag": {Type: new("int"), Min: new(1.0)}}
		sc.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{"future-flag": "2"}}
		return nil
	}))
	cfg := DefaultTestNodeConfig()
	cfg.ClusterMode = config.ClusterModeWorker
	worker := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: cfg, providers: []string{"ollama"}})
	router := gin.New()
	var pushes atomic.Int32
	router.POST("/zzrouter/v1/internal/sync/providers/:name", func(c *gin.Context) { pushes.Add(1); worker.HandleProviderSync(c) })
	srv := httptest.NewTLSServer(router)
	defer srv.Close()
	var protocol atomic.Int32
	protocol.Store(16)
	probe := &syncProbeClient{probe: func(context.Context, string, string, *mesh.QueryParams) (*mesh.Response, error) {
		data, err := json.Marshal(version.VersionInfo{ClusterProtocol: int(protocol.Load())})
		return &mesh.Response{StatusCode: 200, Body: data}, err
	}}
	f := newProviderSyncFanOut(source, func() bool { return true }, func() []*mesh.Endpoint {
		return []*mesh.Endpoint{{URL: srv.URL, ClusterURL: srv.URL, NodeName: "worker"}}
	}, func() *http.Client { return srv.Client() }, func() mesh.ClusterClient { return probe })
	f.fire(source.Config())
	assert.Equal(t, SyncPartial, awaitSyncTest(t, f).State)
	assert.Zero(t, pushes.Load())
	protocol.Store(int32(version.ClusterProtocolVersion))
	f.fire(source.Config())
	assert.Equal(t, SyncComplete, awaitSyncTest(t, f).State)
	assert.EqualValues(t, 1, pushes.Load())
	received, ok := worker.configStore.Config().LookupApp("ollama")
	require.True(t, ok)
	assert.Equal(t, "int", *received.Schema["future_flag"].Type)
	assert.Equal(t, "2", received.Defaults.Parameters["future-flag"])
	shape, err := providerSchema(worker.configStore)("ollama")
	require.NoError(t, err)
	assert.True(t, shape.Parameters["future-flag"].Operator)
}
