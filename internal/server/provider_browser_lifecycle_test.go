package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func browserBindAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

func browserLifecycleServer(t *testing.T, bind string) *Server {
	t.Helper()
	manager, err := role.NewManager(role.RoleCoordinator)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	s := &Server{role: manager, node: &NodeIdentity{name: "local"}, config: &config.NodeConfig{Node: config.ServeConfig{Port: 9090}, Coordinator: config.CoordinatorConfig{ProviderBrowser: config.ProviderBrowserConfig{Origin: "https://comfy.example.com", Listen: bind}}}, shutdownCtx: ctx, httpClient: http.DefaultClient, listenerReady: make(chan struct{}), servingErrors: make(chan error, 1)}
	s.providerBrowser.authority = &browserAuthorityFixture{}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, s.stopProviderBrowser(ctx))
	})
	return s
}

func TestProviderBrowserListenerLifecycle(t *testing.T) {
	bind := browserBindAddress(t)
	s := browserLifecycleServer(t, bind)
	require.NoError(t, s.startProviderBrowser(s.shutdownCtx))
	first := s.providerBrowser.gateway
	require.NoError(t, s.startProviderBrowser(s.shutdownCtx))
	require.Same(t, first, s.providerBrowser.gateway)
	require.Equal(t, "https://comfy.example.com/runs/local/run-a/", s.providerBrowserURL("local", "run-a"))
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+bind+"/runs/local/run-a/", nil)
	require.NoError(t, err)
	req.Host = "comfy.example.com"
	req.Header.Set("Accept", "text/html")
	response, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	require.NoError(t, s.stopProviderBrowser(t.Context()))
	require.Empty(t, s.providerBrowserURL("local", "run-a"))
	require.ErrorIs(t, first.ctx.Err(), context.Canceled)
	listener, err := net.Listen("tcp", bind)
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	require.NoError(t, s.startProviderBrowser(s.shutdownCtx))
	require.NotSame(t, first, s.providerBrowser.gateway)
}

func TestProviderBrowserBindFailureClosesAdministrativeListener(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	s := browserLifecycleServer(t, occupied.Addr().String())
	adminBind := browserBindAddress(t)
	s.httpServer = &http.Server{Addr: adminBind, ReadHeaderTimeout: time.Second}
	require.Error(t, s.startServing())
	require.Nil(t, s.providerBrowser.gateway)
	select {
	case <-s.listenerReady:
		t.Fatal("failed combined bind was announced as ready")
	default:
	}
	listener, err := net.Listen("tcp", adminBind)
	require.NoError(t, err, "administrative listener leaked on provider bind failure")
	require.NoError(t, listener.Close())
}

func TestProviderBrowserFailedPromotionRemainsUnready(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	cfg := DefaultTestNodeConfig()
	cfg.ProviderBrowser = config.ProviderBrowserConfig{Origin: "https://comfy.example.com", Listen: occupied.Addr().String()}
	s := createTestNode(t, cfg)
	s.providerBrowser.authority = &browserAuthorityFixture{}
	close(s.listenerReady)
	require.NoError(t, s.role.Set(t.Context(), role.RoleCoordinator, "occupied browser bind"))
	require.False(t, s.role.Transitioning())
	require.Equal(t, "provider_browser_unavailable", s.readinessUnavailable())
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "provider_browser_unavailable")
	require.NoError(t, occupied.Close())
	require.NoError(t, s.role.Set(t.Context(), role.RoleDisabled, "demote"))
	require.NoError(t, s.role.Set(t.Context(), role.RoleCoordinator, "promote after bind released"))
	require.Empty(t, s.readinessUnavailable())
	require.NoError(t, s.role.Set(t.Context(), role.RoleDisabled, "demote and close browser"))
	require.Nil(t, s.providerBrowser.gateway)
}
