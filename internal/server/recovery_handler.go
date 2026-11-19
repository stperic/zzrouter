package server

import (
	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/recovery"
)

func (s *Server) handleRecoveryInventory(c *gin.Context) {
	inventory, err := recovery.Read(c.Request.Context(), s.configDir)
	if err != nil {
		ServiceUnavailable(c, "Recovery inventory is unavailable; inspect the installed node locally")
		return
	}
	respondRetrievalSuccess(c, "Installation recovery scope", inventory)
}
