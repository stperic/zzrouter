package clusternode

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// testConfig returns a Config that boots on an OS-assigned port with a
// fresh tempdir tree. Subtests override Mode; other fields are populated
// for every mode so a reviewer can see the full layout.
func testConfig(t *testing.T, mode Mode) Config {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		Mode:           mode,
		Port:           0,
		BindHost:       "127.0.0.1",
		IdentityDir:    filepath.Join(root, "identity"),
		CADir:          filepath.Join(root, "ca"),
		ClusterDir:     filepath.Join(root, "cluster"),
		CoordinatorURL: "https://coord.local:9091",
		NodeName:       "testnode",
		PairingPath:    filepath.Join(root, "pairing.txt"),
	}
	return cfg
}

// startNode boots a Node and schedules its shutdown at test end.
// Returns the bound HTTPS base URL and the node itself for further
// assertions. Unclaimed nodes are dormant on the cluster network
// (no listener), so the returned URL is empty for mode == Unclaimed;
// callers that need a live URL must use a bound mode.
func startNode(t *testing.T, mode Mode) (*Node, string) {
	t.Helper()
	cfg := testConfig(t, mode)
	n, err := New(cfg)
	require.NoError(t, err)

	if mode == Unclaimed || mode == Worker {
		require.NoError(t, n.SetAdminAPIHandler(stubInferenceHandler()))
		require.NoError(t, n.SetWorkerCompatHandler(stubInferenceHandler()))
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = n.Stop(context.Background()) })

	require.NoError(t, n.Start(ctx))

	if mode == Unclaimed {
		// Dormant: no cluster listener. URL is empty — tests that
		// probe HTTP on Unclaimed are invalid.
		return n, ""
	}
	require.NotNil(t, n.Addr(), "listener must be bound after Start")
	return n, "https://" + n.Addr().String()
}

// stubInferenceHandler returns a 501 for every path. Unit tests only
// exercise the listener + auth surface; /internal/* hit is out of scope.
func stubInferenceHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	})
}

func TestModeStringAndMarshaling(t *testing.T) {
	t.Parallel()

	roundTrip := []struct {
		mode Mode
		text string
	}{
		{Disabled, "disabled"},
		{Coordinator, "coordinator"},
		{Unclaimed, "unclaimed"},
		{Worker, "worker"},
	}
	for _, tc := range roundTrip {
		tc := tc
		t.Run(tc.text, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.text, tc.mode.String())
			got, err := tc.mode.MarshalText()
			require.NoError(t, err)
			assert.Equal(t, tc.text, string(got))

			var parsed Mode
			require.NoError(t, parsed.UnmarshalText([]byte(tc.text)))
			assert.Equal(t, tc.mode, parsed)
		})
	}

	t.Run("invalid mode rejected", func(t *testing.T) {
		t.Parallel()
		var m Mode
		err := m.UnmarshalText([]byte("superuser"))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidMode)

		// An out-of-range value won't marshal either.
		_, err = Mode(99).MarshalText()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidMode)
	})
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"disabled needs nothing", Config{Mode: Disabled}, false},
		{"unclaimed needs identity", Config{Mode: Unclaimed}, true},
		{"unclaimed needs node name", Config{Mode: Unclaimed, IdentityDir: "/x", PairingPath: "/p"}, true},
		{"unclaimed needs pairing path", Config{Mode: Unclaimed, IdentityDir: "/x", NodeName: "n"}, true},
		{"unclaimed complete", Config{Mode: Unclaimed, IdentityDir: "/x", NodeName: "n", PairingPath: "/p"}, false},
		{"coordinator needs CA dir", Config{Mode: Coordinator, IdentityDir: "/x", NodeName: "n"}, true},
		{"coordinator needs node name", Config{Mode: Coordinator, IdentityDir: "/x", CADir: "/y"}, true},
		{"coordinator with CA", Config{Mode: Coordinator, IdentityDir: "/x", CADir: "/y", NodeName: "n"}, false},
		{"worker needs cluster dir + url", Config{Mode: Worker, IdentityDir: "/x"}, true},
		{"worker needs url", Config{Mode: Worker, IdentityDir: "/x", ClusterDir: "/y"}, true},
		{"worker needs node name", Config{Mode: Worker, IdentityDir: "/x", ClusterDir: "/y", CoordinatorURL: "https://c:9091"}, true},
		{"worker complete", Config{Mode: Worker, IdentityDir: "/x", ClusterDir: "/y", CoordinatorURL: "https://c:9091", NodeName: "n"}, false},
		{"invalid mode", Config{Mode: Mode(99)}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.validate()
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalidConfig)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// A Disabled node must not open a listener. This is the contract that
// lets single-node dev deployments skip cluster machinery entirely.
func TestDisabledNodeOpensNoListener(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, Disabled)
	n, err := New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, n.Start(ctx))
	defer func() { require.NoError(t, n.Stop(context.Background())) }()

	assert.Nil(t, n.Addr(), "Disabled node must have no bound address")
	assert.Equal(t, Disabled, n.Mode())
	assert.Empty(t, n.Fingerprint(), "Disabled node has no identity")
}

// Unclaimed nodes are dormant on the cluster network: no listener
// runs, /health is unreachable until pairing completes and flips the
// node to Worker. The identity is still live (Fingerprint non-empty)
// so the admin CLI can still read SPKI before starting a pairing
// window.
func TestUnclaimedNodeIsDormant(t *testing.T) {
	t.Parallel()
	n, baseURL := startNode(t, Unclaimed)

	assert.Empty(t, baseURL, "Unclaimed must not bind a cluster listener")
	assert.Nil(t, n.Addr(), "Unclaimed has no bound address")
	assert.Equal(t, Unclaimed, n.Mode())
	assert.NotEmpty(t, n.Fingerprint())
	assert.NotEmpty(t, n.ShortForm())
	assert.True(t, strings.HasPrefix(n.Fingerprint(), "sha256:"))
}

// Unit test for ClusterModeGate in isolation — no listener, no TLS.
// Hits every (mode, allowed-set) cell; Coordinator/Worker paths
// aren't reachable via HTTP in these tests because they require
// client certs.
func TestClusterModeGateUnit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		nodeMode   Mode
		allow      []Mode
		wantStatus int
	}{
		{"coordinator allowed on coordinator node", Coordinator, []Mode{Coordinator}, http.StatusOK},
		{"coordinator denied on worker", Worker, []Mode{Coordinator}, http.StatusNotImplemented},
		{"worker allowed on worker", Worker, []Mode{Worker}, http.StatusOK},
		{"multi-allow matches first", Unclaimed, []Mode{Unclaimed, Worker}, http.StatusOK},
		{"multi-allow matches second", Worker, []Mode{Unclaimed, Worker}, http.StatusOK},
		{"multi-deny", Disabled, []Mode{Unclaimed, Worker}, http.StatusNotImplemented},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			n := &Node{}
			n.mode.Store(int32(tc.nodeMode))

			// gin.CreateTestContext gives us a functional *gin.Context
			// backed by an httptest.ResponseRecorder — no listener,
			// no real HTTP round trip.
			gate := ClusterModeGate(n, tc.allow...)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			gate(c)
			if !c.IsAborted() {
				c.Writer.WriteHeader(http.StatusOK)
			}
			assert.Equal(t, tc.wantStatus, w.Code)
		})
	}
}

func TestStartStopIdempotent(t *testing.T) {
	t.Parallel()
	// Coordinator binds a listener; Unclaimed is dormant. Either
	// would exercise the single-use contract, but Coordinator keeps
	// the assertion space wider.
	cfg := testConfig(t, Coordinator)
	n, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	require.NoError(t, n.Start(context.Background()))

	// Second Start must refuse; second Stop must no-op.
	err = n.Start(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAlreadyStarted)

	require.NoError(t, n.Stop(context.Background()))
	require.NoError(t, n.Stop(context.Background())) // idempotent
}

// Shutdown via ctx cancellation is equivalent to Stop(). Exercised
// on a Coordinator because Unclaimed has no listener to probe.
func TestContextCancellationStops(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, Coordinator)
	n, err := New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, n.Start(ctx))

	servingDone := n.ServingDoneForTest()
	require.NotNil(t, servingDone, "Coordinator must have a serve goroutine")

	cancel()

	// Wait on the serve goroutine exiting — proves the ctx-done watcher
	// fired Shutdown on our behalf, without polling the listener.
	select {
	case <-servingDone:
	case <-time.After(2 * time.Second):
		t.Fatal("serve goroutine did not exit after ctx cancel")
	}

	require.NoError(t, n.Stop(context.Background()))
}

func TestSentinelErrors(t *testing.T) {
	t.Parallel()
	for _, e := range []error{
		ErrInvalidMode, ErrInvalidConfig, ErrNotCoordinator,
		ErrAlreadyStarted, ErrListenerFailed, ErrAdminAPIHandlerMissing,
	} {
		require.Error(t, e)
		assert.True(t, strings.HasPrefix(e.Error(), "clusternode: "), e.Error())
	}
}

// Port collision must surface ErrListenerFailed so callers can branch
// on the sentinel rather than scraping the error string. Uses
// Coordinator because Unclaimed doesn't bind.
func TestStartReturnsErrListenerFailedOnPortCollision(t *testing.T) {
	t.Parallel()

	// Occupy a port, then try to bind a node on the same one.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = occupied.Close() })
	_, portStr, err := net.SplitHostPort(occupied.Addr().String())
	require.NoError(t, err)
	var port int
	_, err = fmt.Sscanf(portStr, "%d", &port)
	require.NoError(t, err)

	cfg := testConfig(t, Coordinator)
	cfg.Port = port
	n, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = n.Stop(context.Background()) })

	err = n.Start(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrListenerFailed)
}
