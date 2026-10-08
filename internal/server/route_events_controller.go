package server

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
)

// handleRouteEventsStream serves
// GET /zzrouter/v1/model-groups/events as a server-sent-events stream
// of routes lifecycle events. Mirrors /spend/events on framing
// (events_dropped + stream_closed + ping keepalive) so an agent's SSE
// handling stays uniform across both surfaces.
//
// Filters:
//
//	?event_type=<glob>  — path.Match against EventType (e.g. "breaker_*")
//	?route=<exact>      — exact match on the event's route name
//
// Both filters are server-side: a slow subscriber doesn't even buffer
// events it discarded, so a /events poller observing one route in a
// fleet of 100 routes stays cheap.
//
// Scope: coord-only. Worker-local breaker/cooldown transitions don't
// fan into this bus — cross-node bus is a future arc, documented in
// the package godoc on pkg/observability/route_events.
func (ctrl *ModelGroupsController) handleRouteEventsStream(c *gin.Context) {
	if ctrl.routeEvents == nil {
		ServiceUnavailable(c, "route events bus not configured on this node")
		c.Abort()
		return
	}

	filter, perr := buildRouteEventFilter(c.Query("event_type"), c.Query("route"))
	if perr != nil {
		BadRequest(c, perr.Error())
		return
	}

	events, dropped, unsub := ctrl.routeEvents.Subscribe(filter)
	defer unsub()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.Flush()

	streamEndEmitted := false
	defer func() {
		if !streamEndEmitted {
			c.SSEvent("stream_closed", "stream closed")
			c.Writer.Flush()
		}
	}()

	keepAlive := time.NewTicker(sseKeepAliveInterval)
	defer keepAlive.Stop()

	var lastDropped int64

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-keepAlive.C:
			if _, err := c.Writer.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			c.Writer.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				c.SSEvent("error", "encode failed: "+err.Error())
				c.Writer.Flush()
				continue
			}
			// Event name on the wire is the EventType itself — agents
			// can use the SSE `event:` line as their dispatch key
			// without parsing the JSON body.
			c.SSEvent(string(ev.Type), string(data))

			if cur := dropped.Load(); cur > lastDropped {
				marker, _ := json.Marshal(map[string]int64{"since": lastDropped, "current": cur})
				lastDropped = cur
				c.SSEvent("events_dropped", string(marker))
			}
			c.Writer.Flush()
		}
	}
}

// buildRouteEventFilter compiles the ?event_type and ?route query
// params into a single closure run under the bus lock. Both empty
// returns nil so the bus skips the filter check entirely.
//
// Glob syntax for ?event_type is path.Match — supports `*`, `?`, and
// character classes. Concrete examples agents care about:
//
//	event_type=breaker_*            → opened + closed
//	event_type=cooldown_*           → started + ended
//	event_type=auto_route_*         → generated + revoked
//	event_type=route_created        → exact match
//
// Invalid globs fail fast with a 400 rather than silently matching
// nothing — an agent that mistypes a pattern shouldn't wait forever
// for events that will never arrive.
func buildRouteEventFilter(eventType, route string) (func(route_events.Event) bool, error) {
	eventType = strings.TrimSpace(eventType)
	route = strings.TrimSpace(route)
	if eventType == "" && route == "" {
		return nil, nil
	}
	if eventType != "" {
		// path.Match returns ErrBadPattern on syntax issues; probe
		// once with a stand-in so an invalid pattern fails at
		// subscribe time, not on every published event.
		if _, err := path.Match(eventType, "probe"); err != nil {
			return nil, err
		}
	}
	return func(ev route_events.Event) bool {
		if route != "" && ev.Route != route {
			return false
		}
		if eventType != "" {
			ok, err := path.Match(eventType, string(ev.Type))
			if err != nil || !ok {
				return false
			}
		}
		return true
	}, nil
}
