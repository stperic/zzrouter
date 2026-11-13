package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stperic/zzrouter/pkg/connectivity"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type providerBrowserRoundTripper func(*http.Request) (*http.Response, error)

func (f providerBrowserRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderBrowserProxyStreamsBytesAndStripsAuthority(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Cookie", "Authorization", "X-API-Key", "X-Admin-API-Key", "X-Cluster-API-Key", "Proxy-Authorization", "Forwarded", "X-Forwarded-Host", "X-Real-IP", "X-Zzrouter-Node", "Sec-Fetch-Site"} {
			assert.Empty(t, r.Header.Get(name), name)
		}
		assert.Equal(t, "bytes=1-3", r.Header.Get("Range"))
		assert.Equal(t, "filename=model%2Bname.png&subfolder=a%2Fb", r.URL.RawQuery)
		w.Header().Set("Set-Cookie", "engine-session=must-not-escape")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Range", "bytes 1-3/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0, 128, 255})
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	run := instance.NewInstance("browser", "fixture", "", 1, 0, 1)
	defer run.Cancel()
	manager := connectivity.NewConnectionManager()
	proxy := httptest.NewServer(newProviderBrowserProxy(run, target, manager.GetStreamingClient().Transport, 1024, nil))
	defer proxy.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxy.URL+"/view?filename=model%2Bname.png&subfolder=a%2Fb", nil)
	require.NoError(t, err)
	for _, name := range []string{"Cookie", "Authorization", "X-API-Key", "X-Admin-API-Key", "X-Cluster-API-Key", "Proxy-Authorization", "Forwarded", "X-Forwarded-Host", "X-Real-IP", "X-Zzrouter-Node", "Sec-Fetch-Site"} {
		req.Header.Set(name, "private")
	}
	req.Header.Set("Range", "bytes=1-3")
	resp, err := proxy.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	require.Equal(t, []byte{0, 128, 255}, body)
	require.Equal(t, "image/png", resp.Header.Get("Content-Type"))
	require.Equal(t, "bytes 1-3/5", resp.Header.Get("Content-Range"))
	require.Empty(t, resp.Header.Get("Set-Cookie"))
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
}

func TestProviderBrowserProxyBoundsUploads(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprint(chunked), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) }))
			defer backend.Close()
			target, err := url.Parse(backend.URL)
			require.NoError(t, err)
			run := instance.NewInstance("upload", "fixture", "", 1, 0, 1)
			defer run.Cancel()
			proxy := httptest.NewServer(newProviderBrowserProxy(run, target, nil, 4, nil))
			defer proxy.Close()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxy.URL+"/upload/image", strings.NewReader("oversized"))
			require.NoError(t, err)
			if chunked {
				req.ContentLength = -1
			}
			resp, err := proxy.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
		})
	}
}

func TestProviderBrowserProxyPreservesWebSocketAndClosesWithRun(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Cookie"))
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "http://"+r.Host }}
		conn, err := upgrader.Upgrade(w, r, http.Header{"Set-Cookie": {"engine-session=private"}})
		if err != nil {
			return
		}
		defer conn.Close()
		kind, body, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if err := conn.WriteMessage(kind, body); err != nil {
			return
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	require.NoError(t, err)
	run := instance.NewInstance("socket", "fixture", "", 1, 0, 1)
	defer run.Cancel()
	manager := connectivity.NewConnectionManager()
	finished := make(chan struct{})
	handler := newProviderBrowserProxy(run, target, manager.GetStreamingClient().Transport, 1024, nil)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r); close(finished) }))
	defer proxy.Close()
	conn, resp, err := websocket.DefaultDialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(proxy.URL, "http")+"/ws", http.Header{"Origin": {proxy.URL}, "Authorization": {"Bearer private"}, "Cookie": {"router-session=private"}})
	if resp != nil {
		defer resp.Body.Close()
	}
	require.NoError(t, err)
	defer conn.Close()
	require.Empty(t, resp.Header.Get("Set-Cookie"))
	require.NoError(t, conn.SetReadDeadline(utils.Now().Add(2*time.Second)))
	message := []byte{0, 1, 128, 255}
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, message))
	kind, body, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.BinaryMessage, kind)
	require.Equal(t, message, body)
	run.Cancel()
	_, _, err = conn.ReadMessage()
	require.Error(t, err)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("run cancellation did not drain upgraded proxy")
	}
	require.Zero(t, manager.GetStats().StreamActiveRequests)
}

func TestProviderBrowserStopBeforeDialCannotReachPortSuccessor(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("retired run received a request") }))
	target, err := url.Parse(old.URL)
	require.NoError(t, err)
	defer old.Close()
	run := instance.NewInstance("old-run", "fixture", "", 1, 0, 1)
	defer run.Cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	var descendant context.Context
	blocked := providerBrowserRoundTripper(func(r *http.Request) (*http.Response, error) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		descendant = ctx
		close(entered)
		<-release
		return transport.RoundTrip(r.WithContext(ctx)) //nolint:contextcheck // ctx is an actual descendant of r.Context; the test retains it across the dial gate.
	})
	proxy := httptest.NewServer(newProviderBrowserProxy(run, target, blocked, 1024, nil))
	defer func() { releaseOnce.Do(func() { close(release) }); run.Cancel(); proxy.Close() }()
	response := make(chan int, 1)
	requestError := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxy.URL+"/prompt", nil)
		if err != nil {
			response <- 0
			requestError <- err
			return
		}
		resp, err := proxy.Client().Do(req)
		status := 0
		if resp != nil {
			defer resp.Body.Close()
			status = resp.StatusCode
		}
		response <- status
		requestError <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not reach the dial boundary")
	}
	run.Cancel()
	require.ErrorIs(t, descendant.Err(), context.Canceled, "retirement must synchronously cancel descendant dial contexts")
	old.Close()
	listener, err := net.Listen("tcp", target.Host)
	require.NoError(t, err)
	var successorCalls atomic.Int32
	successor := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { successorCalls.Add(1) }))
	require.NoError(t, successor.Listener.Close())
	successor.Listener = listener
	successor.Start()
	defer successor.Close()
	releaseOnce.Do(func() { close(release) })
	var status int
	select {
	case status = <-response:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled forward did not finish")
	}
	require.NoError(t, <-requestError)
	require.Equal(t, http.StatusGone, status)
	require.Zero(t, successorCalls.Load())
}

func TestProviderBrowserContextPreservesRequestValuesAndLifetime(t *testing.T) {
	type key struct{}
	run, stopRun := context.WithCancel(t.Context())
	request := context.WithValue(t.Context(), key{}, "request-value")
	ctx, cancel := providerRequestContext(run, request)
	defer cancel()
	descendant, stopDescendant := context.WithCancel(ctx)
	defer stopDescendant()
	require.Equal(t, "request-value", descendant.Value(key{}))
	stopRun()
	require.ErrorIs(t, descendant.Err(), context.Canceled)
}
