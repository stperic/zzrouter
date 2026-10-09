package server

import (
	"path"

	"github.com/gin-gonic/gin"
)

// idempotentRoute mounts a route that honours Idempotency-Key; controllers receive it so they cannot mount one any other way.
type idempotentRoute func(router *gin.RouterGroup, method, route string, bodySkip func([]byte) bool, handler gin.HandlerFunc)

// idempotent mounts the shared store's middleware on a route and records the route on the store.
func (s *Server) idempotent(router *gin.RouterGroup, method, route string, bodySkip func([]byte) bool, handler gin.HandlerFunc) {
	router.Handle(method, route, s.idempotency.middlewareWithBodySkip(bodySkip), handler)
	s.idempotency.routes[method+" "+path.Join(router.BasePath(), route)] = struct{}{}
}
