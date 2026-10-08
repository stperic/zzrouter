package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// handleSpendEventsStream serves GET /zzrouter/v1/spend/events as a
// server-sent-events stream of quota.BreachEvent rows. Mirrors the
// /jobs/:id/stream contract (events_dropped + stream_closed) so an
// agent's SSE handling stays uniform across both surfaces.
func (s *Server) handleSpendEventsStream(c *gin.Context) {
	if s.spendEvents == nil {
		ServiceUnavailable(c, "spend events disabled")
		c.Abort()
		return
	}

	events, dropped, unsub := s.spendEvents.Subscribe()
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
			c.SSEvent("breach", string(data))

			// Drop marker shape mirrors pkg/jobs.DroppedMarker so an agent's
			// SSE consumer can share decode logic across /spend/events and
			// /jobs/:id/stream.
			if cur := dropped.Load(); cur > lastDropped {
				marker, _ := json.Marshal(map[string]int64{"since": lastDropped, "current": cur})
				lastDropped = cur
				c.SSEvent("events_dropped", string(marker))
			}
			c.Writer.Flush()
		}
	}
}
