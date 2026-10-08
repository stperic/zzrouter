package server

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mountModelGroupsWithEventBus(t *testing.T) (*gin.Engine, *route_events.Bus) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	store := modelgroup.NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(idemTestYAML)))
	store.SetPath(filepath.Join(t.TempDir(), "model_groups.yaml"))

	bus := route_events.NewBus()
	ctrl := NewModelGroupsController(store, nil, nil, nil, nil, nil, bus, nil, "/model-groups/")

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(CtxKeyAccessContext), &AccessContext{Key: &KeyPrincipal{ID: "alice"}})
		c.Next()
	})
	api := r.Group("/zzrouter/v1")
	ctrl.RegisterPublicRoutes(api)
	return r, bus
}

func TestRouteEventsStream_Filter_EventTypeGlob(t *testing.T) {
	t.Run("breaker_*", func(t *testing.T) {
		f, err := buildRouteEventFilter("breaker_*", "")
		require.NoError(t, err)
		assert.True(t, f(route_events.Event{Type: route_events.EventBreakerOpened}))
		assert.True(t, f(route_events.Event{Type: route_events.EventBreakerClosed}))
		assert.False(t, f(route_events.Event{Type: route_events.EventCooldownStarted}))
	})
	t.Run("exact match", func(t *testing.T) {
		f, err := buildRouteEventFilter("route_created", "")
		require.NoError(t, err)
		assert.True(t, f(route_events.Event{Type: route_events.EventRouteCreated}))
		assert.False(t, f(route_events.Event{Type: route_events.EventRouteUpdated}))
	})
	t.Run("invalid glob → error", func(t *testing.T) {
		_, err := buildRouteEventFilter("[unclosed", "")
		assert.Error(t, err)
	})
}

func TestRouteEventsStream_Filter_RouteExact(t *testing.T) {
	f, err := buildRouteEventFilter("", "fast-chat")
	require.NoError(t, err)
	assert.True(t, f(route_events.Event{Type: route_events.EventRouteCreated, Route: "fast-chat"}))
	assert.False(t, f(route_events.Event{Type: route_events.EventRouteCreated, Route: "other"}))
}

func TestRouteEventsStream_Filter_Both(t *testing.T) {
	f, err := buildRouteEventFilter("breaker_*", "fast-chat")
	require.NoError(t, err)
	assert.True(t, f(route_events.Event{Type: route_events.EventBreakerOpened, Route: "fast-chat"}))
	assert.False(t, f(route_events.Event{Type: route_events.EventBreakerOpened, Route: "other"}))
	assert.False(t, f(route_events.Event{Type: route_events.EventCooldownStarted, Route: "fast-chat"}))
}

func TestRouteEventsStream_Filter_BothEmptyIsNil(t *testing.T) {
	f, err := buildRouteEventFilter("", "")
	require.NoError(t, err)
	assert.Nil(t, f, "no filter params → nil filter so the bus skips the per-event check")
}

func TestRouteEventsStream_ServiceUnavailableWhenBusNil(t *testing.T) {
	store := modelgroup.NewGroupStore()
	require.NoError(t, store.LoadFromBytes([]byte(idemTestYAML)))
	store.SetPath(filepath.Join(t.TempDir(), "model_groups.yaml"))
	gin.SetMode(gin.TestMode)
	ctrl := NewModelGroupsController(store, nil, nil, nil, nil, nil, nil, nil, "/model-groups/")
	r := gin.New()
	api := r.Group("/zzrouter/v1")
	ctrl.RegisterPublicRoutes(api)

	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/events", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestRouteEventsStream_DeliversPublishedEvent(t *testing.T) {
	r, bus := mountModelGroupsWithEventBus(t)

	// Spawn the stream request and arrange a cancellation so the handler
	// exits cleanly once we've seen the event.
	pr, pw := newPipeReaderWriter()
	defer pr.Close()
	req := httptest.NewRequest("GET", "/zzrouter/v1/model-groups/events", nil)
	w := &flushRecorder{ResponseWriter: pw}
	go func() {
		defer pw.Close()
		r.ServeHTTP(w, req)
	}()

	// Publish in a goroutine because the handler must already be
	// subscribed before the publish fans out.
	time.Sleep(20 * time.Millisecond)
	bus.Publish(route_events.Event{
		Type:  route_events.EventRouteCreated,
		Route: "fast-chat",
	})

	scanner := bufio.NewScanner(pr)
	deadline := time.After(500 * time.Millisecond)
	var sawEvent, sawData bool
	for !(sawEvent && sawData) {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for SSE event; lines so far: %v", scannerLines(scanner))
		default:
		}
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") && strings.Contains(line, "route_created") {
			sawEvent = true
		}
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `"route":"fast-chat"`) {
			sawData = true
		}
	}
	assert.True(t, sawEvent, "must see `event: route_created` line")
	assert.True(t, sawData, "must see `data: {... route:fast-chat ...}` line")
}

// scannerLines drains a scanner into a slice for diagnostic output.
// Only used when a test fails — never on the happy path.
func scannerLines(s *bufio.Scanner) []string {
	var out []string
	for s.Scan() {
		out = append(out, s.Text())
	}
	return out
}

// newPipeReaderWriter returns an io.PipeReader paired with a writer the
// SSE handler can write to in real time. httptest.ResponseRecorder
// buffers all writes until ServeHTTP returns, which is fine for normal
// handlers but defeats the per-chunk timing the stream tests need.
func newPipeReaderWriter() (*io.PipeReader, io.WriteCloser) {
	return io.Pipe()
}

// flushRecorder is a gin/http.ResponseWriter-compatible wrapper around
// an io.Writer. The handler's c.Writer.Flush() becomes a no-op on this
// shape (writes are inherently un-buffered through the pipe), which is
// the desired semantic for streaming tests.
type flushRecorder struct {
	ResponseWriter io.Writer
	mu             sync.Mutex
	status         int
	headers        http.Header
}

func (f *flushRecorder) Header() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.headers == nil {
		f.headers = http.Header{}
	}
	return f.headers
}

func (f *flushRecorder) Write(p []byte) (int, error) { return f.ResponseWriter.Write(p) }
func (f *flushRecorder) WriteHeader(code int)        { f.status = code }
func (f *flushRecorder) Flush()                      {}
func (f *flushRecorder) Status() int                 { return f.status }
func (f *flushRecorder) Size() int                   { return 0 }
func (f *flushRecorder) Written() bool               { return f.status != 0 }
func (f *flushRecorder) WriteHeaderNow()             {}
func (f *flushRecorder) Pusher() http.Pusher         { return nil }
func (f *flushRecorder) CloseNotify() <-chan bool    { return make(chan bool) }
func (f *flushRecorder) Hijack() (interface{}, interface{}, error) {
	return nil, nil, nil
}
func (f *flushRecorder) WriteString(s string) (int, error) {
	return f.Write([]byte(s))
}
