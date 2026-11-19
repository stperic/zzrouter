package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/recovery"
)

// registerStateBackups mounts the online backups of the access store: reads for
// a coordinator administrator; a create for one or for the local operator, so
// the local commands can back up with the proof alone.
func (s *Server) registerStateBackups(router *gin.RouterGroup) {
	router.GET("/system/state/backups", s.browserCoordinatorOnly, browserResponseHeaders, s.handleListBackups)
	router.GET("/system/state/backups/:id", s.browserCoordinatorOnly, browserResponseHeaders, s.handleGetBackup)
	s.localRoute(http.MethodPost, "/system/state/backups", true, true, s.handleCreateBackup)
}

func (s *Server) handleListBackups(c *gin.Context) {
	backups := s.backups.List()
	respondList(c, backups, len(backups), false)
}

func (s *Server) handleGetBackup(c *gin.Context) {
	backup, err := s.backups.Get(c.Param("id"))
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondRetrievalSuccess(c, "Online backup", backup)
}

// handleCreateBackup takes an empty JSON object, so a retry hashes the same.
func (s *Server) handleCreateBackup(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<10))
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, body) != nil || compact.String() != "{}" {
		BadRequest(c, "The body must be an empty JSON object")
		return
	}
	backup, err := s.backups.Create(auditContext(c))
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondCreatedWithMessage(c, "Online backup created", backup)
}

// retainedCopies lists the copies of access records the coordinator keeps
// beside the live database, by kind, without paths.
func retainedCopies(backups []recovery.OnlineBackup) []retainedCopy {
	copies := make([]retainedCopy, 0, len(backups))
	for _, backup := range backups {
		copies = append(copies, retainedCopy{Kind: "online_backup", ID: backup.ID, CreatedAt: backup.CreatedAt, SizeBytes: backup.SizeBytes})
	}
	return copies
}
