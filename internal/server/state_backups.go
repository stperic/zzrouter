package server

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/recovery"
)

// registerStateBackups mounts the online backups of the access store: reads for
// a coordinator administrator; a create for one or for the local operator, so
// the local commands can back up with the proof alone; the content for the
// local operator only, since an archive holds credential hashes.
func (s *Server) registerStateBackups(router *gin.RouterGroup) {
	router.GET("/system/state/backups", s.browserCoordinatorOnly, browserResponseHeaders, s.handleListBackups)
	router.GET("/system/state/backups/:id", s.browserCoordinatorOnly, browserResponseHeaders, s.handleGetBackup)
	s.localRoute(http.MethodPost, "/system/state/backups", true, true, s.handleCreateBackup)
	s.localRoute(http.MethodGet, "/system/state/backups/:id/content", false, false, s.handleExportBackup)
}

// handleExportBackup streams a retained backup's archive, whole and from the
// file it was checked on, once its export is recorded; a Range is ignored.
func (s *Server) handleExportBackup(c *gin.Context) {
	export, err := s.backups.Open(auditContext(c), c.Param("id"))
	if err != nil {
		RespondToError(c, err)
		return
	}
	defer func() { _ = export.File.Close() }()
	digest, _ := hex.DecodeString(export.SHA256)
	header := c.Writer.Header()
	header.Set("Content-Type", "application/gzip")
	header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="zzrouter-access-%s-%s.tar.gz"`, export.CreatedAt.UTC().Format("20060102T150405Z"), export.ID[:8]))
	header.Set("Content-Length", strconv.FormatInt(export.SizeBytes, 10))
	header.Set("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest)+":")
	header.Set("Accept-Ranges", "none")
	c.Status(http.StatusOK)
	_, _ = io.CopyN(c.Writer, export.File, export.SizeBytes)
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
// beside the live database, by kind, without paths: its online backups and
// the rollback directories offline activations kept.
func retainedCopies(backups []recovery.OnlineBackup, rollbacks []recovery.Rollback) []retainedCopy {
	copies := make([]retainedCopy, 0, len(backups)+len(rollbacks))
	for _, backup := range backups {
		copies = append(copies, retainedCopy{Kind: "online_backup", ID: backup.ID, CreatedAt: backup.CreatedAt, SizeBytes: backup.SizeBytes})
	}
	for _, rollback := range rollbacks {
		copies = append(copies, retainedCopy{Kind: "activation_rollback", ID: rollback.ID, CreatedAt: rollback.CreatedAt, SizeBytes: rollback.SizeBytes, Pending: rollback.Pending})
	}
	return copies
}
