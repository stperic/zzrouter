package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Replies are synthetic Ollama fixtures; routing and mutual TLS use real servers.
type ollamaImportDaemon struct {
	mu          sync.Mutex
	blob        []byte
	model       string
	calls       int
	status      int
	reply       string
	length      int64
	contentType string
	auth        string
	create      map[string]json.RawMessage
	started     chan struct{}
	canceled    chan struct{}
}

func (d *ollamaImportDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/tags":
		d.mu.Lock()
		model := d.model
		d.mu.Unlock()
		if model == "" {
			_, _ = io.WriteString(w, `{"models":[]}`)
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"name": model, "model": model, "details": map[string]string{"format": "gguf"}}}})
		}
		return
	case "/api/show", "/api/ps", "/api/version":
		_, _ = io.WriteString(w, `{}`)
		return
	}
	d.mu.Lock()
	d.calls++
	d.length, d.contentType, d.auth = r.ContentLength, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
	status, reply, started, canceled := d.status, d.reply, d.started, d.canceled
	d.mu.Unlock()
	if started != nil {
		one := make([]byte, 1)
		_, _ = io.ReadFull(r.Body, one)
		close(started)
		if canceled != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			close(canceled)
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
		return
	}
	switch r.URL.Path {
	case "/api/blobs/sha256:fixture":
		w.Header().Set("X-Blob-Owner", "worker")
		if r.Method == http.MethodHead {
			d.mu.Lock()
			exists := d.blob != nil
			d.mu.Unlock()
			if !exists {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.blob = body
		d.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case "/api/create", "/api/pull":
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.mu.Lock()
		d.create = body
		_ = json.Unmarshal(body["model"], &d.model)
		d.mu.Unlock()
		_, _ = io.WriteString(w, `{"status":"success"}`)
	default:
		http.NotFound(w, r)
	}
}

func configureOllamaImportNode(t *testing.T, s *Server, endpoint string) {
	t.Helper()
	provider := config.NewOllamaConnectProvider("ollama", endpoint, "")
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", config.ServiceConfig{Enabled: provider.Enabled, Mode: "external", Protocol: provider.Protocol, Runtime: &config.AppRuntimeConfig{Endpoint: endpoint, API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "worker-daemon-fixture"}}, Capabilities: provider.Capabilities}))
	s.configStore = config.NewMemoryAppsConfigStore(cfg)
	s.providers.appMgr.ReloadConfig(cfg)
	s.model.Registry.ReloadConfig(cfg)
	s.model.Cache.Invalidate()
}

func ollamaImportWorker(t *testing.T, worker *Server) (string, *http.Client) {
	t.Helper()
	root := t.TempDir()
	ca, err := clusterid.LoadOrCreateCA(filepath.Join(root, "ca"))
	require.NoError(t, err)
	sign := func(name string, role clusterid.Role) tls.Certificate {
		identity, err := clusterid.LoadOrCreateIdentity(filepath.Join(root, name))
		require.NoError(t, err)
		csr, err := identity.CSR(name, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
		require.NoError(t, err)
		cert, err := ca.Sign(csr, role)
		require.NoError(t, err)
		require.NoError(t, identity.InstallSignedCert(cert))
		return tls.Certificate{Certificate: [][]byte{identity.Certificate().Raw}, PrivateKey: identity.Signer()}
	}
	workerCert, coordCert := sign("worker", clusterid.RoleWorker), sign("coordinator", clusterid.RoleCoordinator)
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())
	endpoint := httptest.NewUnstartedServer(buildWorkerCompatEngine(worker))
	endpoint.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{workerCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || !slices.Contains(state.PeerCertificates[0].Subject.OrganizationalUnit, clusterid.RoleCoordinator.OU()) {
			return errors.New("coordinator certificate required")
		}
		return nil
	}}
	endpoint.StartTLS()
	t.Cleanup(endpoint.Close)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{coordCert}}}
	t.Cleanup(transport.CloseIdleConnections)
	return endpoint.URL, &http.Client{Transport: transport}
}

func ollamaImportRig(t *testing.T) (*Server, *Server, *ollamaImportDaemon, *ollamaImportDaemon, *atomic.Int32) {
	t.Helper()
	local, remote := &ollamaImportDaemon{}, &ollamaImportDaemon{}
	localDaemon, workerDaemon := httptest.NewServer(local), httptest.NewServer(remote)
	t.Cleanup(localDaemon.Close)
	t.Cleanup(workerDaemon.Close)
	coord := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, UserKey: TestUserKey, NodeName: "coordinator"})
	configureOllamaImportNode(t, coord, localDaemon.URL)
	worker := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker, NodeName: "worker"})
	configureOllamaImportNode(t, worker, workerDaemon.URL)
	endpoint, client := ollamaImportWorker(t, worker)
	service := coord.services.Ollama.(*OllamaServiceImpl)
	service.hasRouter = true
	service.getClusterNodeMTLSURL = func(node string) (string, error) {
		if node != "worker" {
			return "", fmt.Errorf("unknown node %q", node)
		}
		return endpoint, nil
	}
	service.getMTLSClient = func() *http.Client { return client }
	published := &atomic.Int32{}
	service.publishCatalog = func() error {
		published.Add(1)
		return coord.publishCatalog()
	}
	return coord, worker, local, remote, published
}

func ollamaImportRequest(t *testing.T, node *Server, method, path, key, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("X-API-Key", key)
	req.Header.Set("X-Node", target)
	req.Header.Set("Content-Type", "application/json")
	if strings.Contains(path, "/blobs/") {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	w := httptest.NewRecorder()
	node.engine.ServeHTTP(w, req)
	return w
}

func TestIntegrationOllamaImportWorkerOwnership(t *testing.T) {
	coord, worker, local, remote, published := ollamaImportRig(t)
	path := "/api/blobs/sha256:fixture"
	response := ollamaImportRequest(t, coord, http.MethodHead, path, TestAdminKey, "worker", nil)
	require.Equal(t, http.StatusNotFound, response.Code)
	require.Equal(t, "worker", response.Header().Get("X-Blob-Owner"))
	blob := bytes.Repeat([]byte("GGUF-fixture"), 8192)
	response = ollamaImportRequest(t, coord, http.MethodPost, path, TestAdminKey, "worker", bytes.NewReader(blob))
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	remote.mu.Lock()
	require.Equal(t, blob, remote.blob)
	require.EqualValues(t, len(blob), remote.length)
	require.Equal(t, "application/octet-stream", remote.contentType)
	require.Equal(t, "Bearer worker-daemon-fixture", remote.auth)
	remote.mu.Unlock()
	response = ollamaImportRequest(t, coord, http.MethodHead, path, TestAdminKey, "worker", nil)
	require.Equal(t, http.StatusOK, response.Code)
	body := `{"model":"brand-new:latest@worker","files":{"fixture.gguf":"sha256:fixture"},"parameters":{"temperature":0.2},"stream":false}`
	response = ollamaImportRequest(t, coord, http.MethodPost, "/api/create", TestAdminKey, "", strings.NewReader(body))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.JSONEq(t, `{"status":"success"}`, response.Body.String())
	remote.mu.Lock()
	require.Equal(t, "brand-new:latest", remote.model)
	require.JSONEq(t, `{"fixture.gguf":"sha256:fixture"}`, string(remote.create["files"]))
	require.JSONEq(t, `{"temperature":0.2}`, string(remote.create["parameters"]))
	remote.mu.Unlock()
	require.True(t, worker.model.Cache.HasModel("brand-new:latest"), "worker publishes the new manifest before reporting success")
	require.EqualValues(t, 1, published.Load())
	response = ollamaImportRequest(t, coord, http.MethodPost, "/api/create", TestAdminKey, "worker", strings.NewReader(`{"name":"alias-only@worker","stream":false}`))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	remote.mu.Lock()
	require.Equal(t, "alias-only", remote.model)
	require.JSONEq(t, `"alias-only"`, string(remote.create["name"]))
	remote.mu.Unlock()
	response = ollamaImportRequest(t, coord, http.MethodPost, "/api/pull", TestAdminKey, "worker", strings.NewReader(`{"model":"pulled-new","stream":false}`))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.True(t, worker.model.Cache.HasModel("pulled-new"))
	require.EqualValues(t, 3, published.Load())
	local.mu.Lock()
	require.Zero(t, local.calls, "worker import must never touch the coordinator daemon")
	local.mu.Unlock()
	response = ollamaImportRequest(t, worker, http.MethodPost, "/api/create", TestAdminKey, "", strings.NewReader(body))
	require.Equal(t, http.StatusNotFound, response.Code, "worker public admin engine never exposes mutations")
}

func TestIntegrationOllamaImportRejectsTargetsAndUnauthorizedCallers(t *testing.T) {
	coord, _, local, remote, _ := ollamaImportRig(t)
	for _, operation := range []struct{ method, path, body string }{
		{http.MethodHead, "/api/blobs/sha256:fixture", ""},
		{http.MethodPost, "/api/blobs/sha256:fixture", "binary fixture"},
		{http.MethodPost, "/api/create", `{"model":"new"}`},
		{http.MethodPost, "/api/pull", `{"model":"new"}`},
	} {
		t.Run(operation.method+operation.path, func(t *testing.T) {
			for _, tc := range []struct {
				target, key, suffix string
				status              int
			}{
				{"missing", TestAdminKey, "", http.StatusBadGateway},
				{"worker", TestAdminKey, "?node=worker", http.StatusBadRequest},
				{"worker", "", "", http.StatusUnauthorized},
				{"worker", TestUserKey, "", http.StatusForbidden},
			} {
				if operation.method == http.MethodHead && tc.key != TestAdminKey {
					continue
				}
				response := ollamaImportRequest(t, coord, operation.method, operation.path+tc.suffix, tc.key, tc.target, strings.NewReader(operation.body))
				require.Equal(t, tc.status, response.Code, response.Body.String())
			}
		})
	}
	for _, body := range []string{`{"model":"new@worker"}`, `{"model":"new@"}`, `{"source":"new"}`, `{"model":"hf/org/new@worker"}`} {
		response := ollamaImportRequest(t, coord, http.MethodPost, "/api/create", TestAdminKey, "coordinator", strings.NewReader(body))
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	}
	for _, path := range []string{"/api/create", "/api/pull"} {
		for _, model := range []string{"llamacpp/new@worker", "llamacpp/ollama/new@worker", "new#Q4_K_M@worker", "new#@worker"} {
			response := ollamaImportRequest(t, coord, http.MethodPost, path, TestAdminKey, "worker", strings.NewReader(fmt.Sprintf(`{"model":%q}`, model)))
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		}
	}
	local.mu.Lock()
	require.Zero(t, local.calls)
	local.mu.Unlock()
	remote.mu.Lock()
	require.Zero(t, remote.calls)
	remote.mu.Unlock()
}

func TestIntegrationOllamaImportPreservesWorkerFailures(t *testing.T) {
	coord, _, local, remote, published := ollamaImportRig(t)
	remote.mu.Lock()
	remote.status, remote.reply = http.StatusConflict, `{"error":"fixture digest is unavailable"}`
	remote.mu.Unlock()
	for _, path := range []string{"/api/blobs/sha256:fixture", "/api/create", "/api/pull"} {
		response := ollamaImportRequest(t, coord, http.MethodPost, path, TestAdminKey, "worker", strings.NewReader(`{"model":"new"}`))
		require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
		require.Equal(t, remote.reply, response.Body.String())
	}
	require.Zero(t, published.Load(), "failed mutations must not publish success")
	local.mu.Lock()
	require.Zero(t, local.calls)
	local.mu.Unlock()
	service := coord.services.Ollama.(*OllamaServiceImpl)
	transport := service.getMTLSClient().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.Certificates = nil
	t.Cleanup(transport.CloseIdleConnections)
	service.getMTLSClient = func() *http.Client { return &http.Client{Transport: transport} }
	response := ollamaImportRequest(t, coord, http.MethodPost, "/api/create", TestAdminKey, "worker", strings.NewReader(`{"model":"new"}`))
	require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	remote.mu.Lock()
	require.Equal(t, 3, remote.calls, "a client without the cluster certificate cannot reach the worker engine")
	remote.mu.Unlock()
}

func TestIntegrationOllamaBlobWorkerStreamingCancellation(t *testing.T) {
	coord, _, local, remote, _ := ollamaImportRig(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	remote.mu.Lock()
	remote.started, remote.canceled = started, canceled
	remote.mu.Unlock()
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/blobs/sha256:fixture", reader).WithContext(ctx)
	req.Header.Set("X-API-Key", TestAdminKey)
	req.Header.Set("X-Node", "worker")
	req.Header.Set("Content-Type", "application/octet-stream")
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); coord.engine.ServeHTTP(w, req); response <- w }()
	_, err := writer.Write([]byte("x"))
	require.NoError(t, err)
	select {
	case <-started:
	case <-t.Context().Done():
		t.Fatal("blob did not reach worker while the upload remained open")
	}
	cancel()
	_ = writer.Close()
	select {
	case <-response:
	case <-t.Context().Done():
		t.Fatal("coordinator did not cancel forwarding")
	}
	select {
	case <-canceled:
	case <-t.Context().Done():
		t.Fatal("worker daemon did not receive cancellation")
	}
	local.mu.Lock()
	assert.Zero(t, local.calls)
	local.mu.Unlock()
}
