package server

import (
	"cmp"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stperic/zzrouter/pkg/utils"
)

var (
	errStateRevisionChanged = errors.New("the access records changed after the backup was taken")
	errBackupUnprotected    = errors.New("the backup is past its protected_until")
)

// resetRequest names the backup a reset preview or a reset relies on.
type resetRequest struct {
	BackupID string `json:"backup_id" binding:"required"`
}

type resetPreview struct {
	StoreID    string               `json:"store_id"`
	Generation string               `json:"generation"`
	Revision   uint64               `json:"revision"`
	BackupID   string               `json:"backup_id"`
	Removes    sqlstore.ResetCounts `json:"removes"`
	Keeps      *resetKeeps          `json:"keeps"` // null: no owner, so nothing stays and owner setup stays open
}

type resetKeeps struct {
	OwnerID       string `json:"owner_id"`
	OwnerUsername string `json:"owner_username"`
	TeamID        string `json:"team_id"`
	TeamName      string `json:"team_name"`
}

// backsReset judges whether backup, which Backups.Get returned with err, can
// undo a reset of the live store: retained, inside its protected window so
// retention cannot remove it first, and holding exactly the live records. The
// backup is taken before the preview, so at commit this also means nothing
// changed since the preview.
func backsReset(backup recovery.OnlineBackup, err error, live sqlstore.Meta) error {
	switch {
	case err != nil:
		return err
	case !utils.NowUTC().Before(backup.ProtectedUntil):
		return errBackupUnprotected
	case backup.StoreID != live.StoreID || backup.Generation != live.Generation || backup.Revision != live.Revision:
		return errStateRevisionChanged
	}
	return nil
}

// handleResetPreview names what a reset removes and keeps, for a backup that
// can back it.
func (s *Server) handleResetPreview(c *gin.Context) {
	var req resetRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	backup, err := s.backups.Get(req.BackupID)
	preview, previewErr := s.stateStore.PreviewReset()
	if err = cmp.Or(previewErr, backsReset(backup, err, preview.Meta)); err != nil {
		RespondToError(c, err)
		return
	}
	answer := resetPreview{StoreID: preview.StoreID, Generation: preview.Generation, Revision: preview.Revision, BackupID: backup.ID, Removes: preview.Removes}
	if preview.Owner != nil {
		answer.Keeps = &resetKeeps{OwnerID: preview.Owner.ID, OwnerUsername: preview.Owner.Username, TeamID: preview.Team.ID, TeamName: preview.Team.Name}
	}
	respondSuccess(c, "Reset preview", answer)
}

// handleReset resets the access records as the preview said. The backup is
// read outside the writer gate and judged inside it, so the store never calls
// into recovery.
func (s *Server) handleReset(c *gin.Context) {
	var req resetRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	backup, backupErr := s.backups.Get(req.BackupID)
	result, err := s.stateStore.Reset(auditContext(c), req.BackupID, func(live sqlstore.Meta) error { return backsReset(backup, backupErr, live) })
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondSuccess(c, "Access state reset", result)
}
