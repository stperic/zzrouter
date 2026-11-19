package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/localproof"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

const localGatePath = "/zzrouter/v1/test/local-operator"

func localGateRequest(method, path, proof, body string) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:50123"
	r.Header.Set(localproof.Header, proof)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func serveLocalGate(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	return w
}

func readLocalProof(t *testing.T, s *Server) *localproof.File {
	t.Helper()
	file, err := localproof.Read(t.Context(), s.configDir)
	require.NoError(t, err)
	return file
}

// A coordinator without browser sign-in still has a proof, and its gate admits only
// an operator on this host: each refusal gives the same answer.
func TestLocalOperatorGate(t *testing.T) {
	disabled := false
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeDisabled, OAuthEnabled: &disabled})
	require.Nil(t, s.access.oauth)
	require.Nil(t, s.accessStore.People())
	file := readLocalProof(t, s)
	require.Empty(t, file.ConsoleURL)
	endpoint, err := url.Parse(file.Endpoint)
	require.NoError(t, err)
	require.True(t, localproof.Address(endpoint.Hostname()), file.Endpoint)
	calls := 0
	s.localRoute(http.MethodPost, strings.TrimPrefix(localGatePath, "/zzrouter/v1"), false, nil, func(c *gin.Context) {
		calls++
		require.Empty(t, c.Request.Header.Values(localproof.Header), "the gate removes the proof")
		require.Equal(t, audit.ActorLocalOperator, PrincipalFromContext(c))
		require.Equal(t, audit.ActorLocalOperator, auditActor(c))
		_, _, recorded := idempotencyOf(c)
		require.False(t, recorded, "no Idempotency-Key, nothing recorded")
		c.JSON(http.StatusOK, gin.H{"admitted": true})
	})

	admitted := serveLocalGate(s, localGateRequest(http.MethodPost, localGatePath, file.Proof, "{}"))
	require.Equal(t, http.StatusOK, admitted.Code, admitted.Body.String())
	require.Equal(t, 1, calls)

	type refusal struct{ Type, Title, Detail, Code string }
	var want *refusal
	for name, mutate := range map[string]func(*http.Request){
		"no header":         func(r *http.Request) { r.Header.Del(localproof.Header) },
		"proof in the URL":  func(r *http.Request) { r.Header.Del(localproof.Header); r.URL.RawQuery = "proof=" + file.Proof },
		"wrong proof":       func(r *http.Request) { r.Header.Set(localproof.Header, strings.Repeat("x", 43)) },
		"two proofs":        func(r *http.Request) { r.Header.Add(localproof.Header, file.Proof) },
		"origin":            func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:9090") },
		"cookie":            func(r *http.Request) { r.Header.Set("Cookie", "unrelated=1") },
		"csrf token":        func(r *http.Request) { r.Header.Set("X-CSRF-Token", "token") },
		"forwarded":         func(r *http.Request) { r.Header.Set("Forwarded", "for=203.0.113.9") },
		"x-forwarded-for":   func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.9") },
		"x-forwarded-host":  func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "router.example") },
		"x-forwarded-proto": func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") },
		"x-real-ip":         func(r *http.Request) { r.Header.Set("X-Real-IP", "203.0.113.9") },
		"via":               func(r *http.Request) { r.Header.Set("Via", "1.1 proxy.example") },
		"sec-fetch-site":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") },
		"sec-fetch-mode":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "navigate") },
		"lower-case header": func(r *http.Request) { r.Header["x-forwarded-for"] = []string{"203.0.113.9"} },
		"remote peer":       func(r *http.Request) { r.RemoteAddr = "203.0.113.20:50123" },
		"unparsable peer":   func(r *http.Request) { r.RemoteAddr = "127.0.0.1" },
	} {
		t.Run(name, func(t *testing.T) {
			r := localGateRequest(http.MethodPost, localGatePath, file.Proof, "{}")
			mutate(r)
			w := serveLocalGate(s, r)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			var got refusal
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			if want == nil {
				want = &got
			}
			require.Equal(t, *want, got, "every refusal answers alike")
			require.NotContains(t, w.Body.String(), file.Proof)
			require.Empty(t, r.Header.Values(localproof.Header), "a refused request loses the proof too")
		})
	}
	require.Equal(t, "forbidden", want.Code)
	require.Equal(t, 1, calls, "no refused request reached the handler")
}

// The gate runs before replay, so replay keys by the local operator and an unproven
// caller never reads a recorded reply.
func TestLocalOperatorPrecedesIdempotency(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	file := readLocalProof(t, s)
	const body = `{"choice":"one"}`
	calls := 0
	s.localRoute(http.MethodPost, strings.TrimPrefix(localGatePath, "/zzrouter/v1"), true, nil, func(c *gin.Context) {
		calls++
		key, hash, ok := idempotencyOf(c)
		require.True(t, ok)
		require.Equal(t, "replay-1", key)
		require.Equal(t, sha256.Sum256([]byte(body)), hash)
		c.JSON(http.StatusOK, gin.H{"secret_reply": calls})
	})
	send := func(proof string) *httptest.ResponseRecorder {
		r := localGateRequest(http.MethodPost, localGatePath, proof, body)
		r.Header.Set(idempotencyHeader, "replay-1")
		return serveLocalGate(s, r)
	}
	first := send(file.Proof)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	wrong := send(strings.Repeat("x", 43))
	require.Equal(t, http.StatusForbidden, wrong.Code)
	require.NotContains(t, wrong.Body.String(), "secret_reply")
	replay := send(file.Proof)
	require.Equal(t, http.StatusOK, replay.Code)
	require.JSONEq(t, first.Body.String(), replay.Body.String())
	require.Equal(t, 1, calls, "the proven retry replayed the recorded reply")
}

func TestLocalOperatorRouteOnWorkerIsMisdirected(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, ClusterMode: config.ClusterModeWorker})
	require.Nil(t, s.localProof)
	w := serveLocalGate(s, localGateRequest(http.MethodPost, "/zzrouter/v1/access/recovery/local", strings.Repeat("x", 43), "{}"))
	require.Equal(t, http.StatusMisdirectedRequest, w.Code, w.Body.String())
}

// Every route localRoute mounts is local in the catalog, absent from discovery and refused
// by zz_api before dispatch, whatever its path.
func TestLocalOperatorRoutesStayOutOfAgentSurfaces(t *testing.T) {
	s, _ := oauthTestNode(t, config.ClusterModeDisabled)
	calls := 0
	probe := func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{})
	}
	s.localRoute(http.MethodPost, "/system/state/reset", true, nil, probe)
	s.localRoute(http.MethodGet, "/system/state/backups/:id/content", false, nil, probe)
	require.Len(t, s.localRoutes, 3, "owner recovery and two probes")
	catalog := makeAuthRequest(t, s, http.MethodGet, "/zzrouter/v1/server/routes", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, catalog.Code)
	var routes struct{ Routes []RouteSpec }
	require.NoError(t, json.Unmarshal(catalog.Body, &routes))
	tiers := map[string]string{}
	for _, route := range routes.Routes {
		tiers[route.Method+" "+route.Path] = route.Auth
	}
	discovered := map[string]bool{}
	for _, entries := range collectAPIDiscoveryGroups(s) {
		for _, entry := range entries {
			discovered[entry] = true
		}
	}
	file := readLocalProof(t, s)
	for route := range s.localRoutes {
		method, template, _ := strings.Cut(route, " ")
		require.Equal(t, authLocal, tiers[route], route)
		require.False(t, discovered[method+" "+uriTemplatePath(template)], route)
		routePath := strings.ReplaceAll(template, ":id", "backup-1")
		_, err := s.eligibleAPIRoute(method, routePath)
		require.Error(t, err, route)
		tool, arguments := "zz_api", map[string]any{"method": method, "path": routePath, "body": map[string]any{}}
		if method == http.MethodGet {
			tool, arguments = "zz_api_get", map[string]any{"path": routePath}
		}
		r := mcpWireRequest(t, "tools/call", "2026-07-28", map[string]any{"name": tool, "arguments": arguments})
		r.RemoteAddr = "127.0.0.1:50124"
		r.Header.Set(localproof.Header, file.Proof)
		result := mcpWireResponse(t, s, r, http.StatusOK)["result"].(map[string]any)
		require.Equal(t, true, result["isError"], result)
		require.Equal(t, err.Error(), result["structuredContent"].(map[string]any)["detail"], "refused by the catalog, before dispatch")
	}
	require.Zero(t, calls)
}

// An MCP tool call from this host never carries the caller's proof into the engine.
func TestLocalOperatorProofStaysOutOfMCPDispatch(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	file := readLocalProof(t, s)
	var seen []string
	calls := 0
	s.engine.POST("/zzrouter/v1/test/headers", func(c *gin.Context) {
		calls++
		seen = c.Request.Header.Values(localproof.Header)
		c.JSON(http.StatusOK, gin.H{})
	})
	r := mcpWireRequest(t, "tools/call", "2026-07-28", map[string]any{"name": "zz_api", "arguments": map[string]any{"method": "POST", "path": "/zzrouter/v1/test/headers", "body": map[string]any{}}})
	r.RemoteAddr = "127.0.0.1:50125"
	r.Header.Set(localproof.Header, file.Proof)
	result := mcpWireResponse(t, s, r, http.StatusOK)["result"].(map[string]any)
	require.NotEqual(t, true, result["isError"], result)
	require.Equal(t, 1, calls)
	require.Empty(t, seen)
}

// Only localRoute names the gate, so no route sits behind it without being recorded.
func TestLocalOperatorMountedOnlyByLocalRoute(t *testing.T) {
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	uses := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			ast.Inspect(decl, func(n ast.Node) bool {
				if selector, ok := n.(*ast.SelectorExpr); ok && selector.Sel.Name == "localOperator" {
					uses++
					fn, _ := decl.(*ast.FuncDecl)
					require.True(t, fn != nil && fn.Name.Name == "localRoute", "%s names the gate outside localRoute", name)
				}
				return true
			})
		}
	}
	require.Equal(t, 1, uses)
}

// Neither the access log nor the server log ever holds the proof.
func TestLocalOperatorProofNeverLogged(t *testing.T) {
	logs := captureServerLogs(t)
	s, endpoint := oauthTestNode(t, config.ClusterModeDisabled)
	file := readLocalProof(t, s)
	for _, proof := range []string{file.Proof, strings.Repeat("y", 43)} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.URL+"/zzrouter/v1/access/recovery/local", strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set(localproof.Header, proof)
		response, err := endpoint.Client().Do(req)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}
	require.Contains(t, logs.String(), "/zzrouter/v1/access/recovery/local", "the access log saw both requests")
	require.NotContains(t, logs.String(), file.Proof)
	require.NotContains(t, logs.String(), strings.Repeat("y", 43))
}

// captureServerLogs collects the access log and the server log of nodes built after it.
func captureServerLogs(t *testing.T) *lockedLog {
	t.Helper()
	logs := &lockedLog{}
	previousWriter, previousLogger := gin.DefaultWriter, slog.Default()
	gin.DefaultWriter = logs
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { gin.DefaultWriter = previousWriter; slog.SetDefault(previousLogger) })
	return logs
}

// lockedLog is written by request goroutines while the test reads it.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
