package server

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stperic/zzrouter/pkg/utils"
)

// resetConfirmationTTL bounds a preview's token; a backup's protected_until can only shorten it.
const resetConfirmationTTL = 5 * time.Minute

// confirmationDomain keeps a confirmation's MAC from meaning anything else under the key.
const confirmationDomain = "zzrouter-reset-v1\x00"

var (
	errStateRevisionChanged = errors.New("the access records changed after the backup was taken")
	errResetConfirmation    = errors.New("the reset confirmation is not this process's, has expired or names another backup")
)

type resetPreviewRequest struct {
	BackupID string `json:"backup_id" binding:"required"`
}

type resetRequest struct {
	BackupID          string `json:"backup_id" binding:"required"`
	ConfirmationToken string `json:"confirmation_token" binding:"required"`
}

// resetState is the access records' state a reset is confirmed against.
type resetState struct {
	StoreID    string `json:"s"`
	Generation string `json:"g"`
	Revision   uint64 `json:"r"`
}

func stateOf(m sqlstore.Meta) resetState { return resetState{m.StoreID, m.Generation, m.Revision} }

// confirmation is what a preview's token binds: the state it saw, the backup
// of that state and the token's expiry in Unix seconds.
type confirmation struct {
	resetState
	BackupID string `json:"b"`
	Expires  int64  `json:"e"`
}

type resetPreview struct {
	StoreID           string               `json:"store_id"`
	Generation        string               `json:"generation"`
	Revision          uint64               `json:"revision"`
	BackupID          string               `json:"backup_id"`
	Removes           sqlstore.ResetCounts `json:"removes"`
	Keeps             *resetKeeps          `json:"keeps"` // null: no owner, so nothing stays and owner setup stays open
	ConfirmationToken string               `json:"confirmation_token"`
	ExpiresAt         time.Time            `json:"expires_at"`
}

type resetKeeps struct {
	OwnerID       string `json:"owner_id"`
	OwnerUsername string `json:"owner_username"`
	TeamID        string `json:"team_id"`
	TeamName      string `json:"team_name"`
}

// handleResetPreview names what a reset removes and keeps, and confirms it
// with a token for a protected backup of exactly the state previewed.
func (s *Server) handleResetPreview(c *gin.Context) {
	var req resetPreviewRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	now := utils.NowUTC()
	backup, err := s.backups.Get(req.BackupID)
	if err == nil && !now.Before(backup.ProtectedUntil) {
		err = recovery.ErrBackupNotFound
	}
	preview, previewErr := s.stateStore.PreviewReset()
	if err = cmp.Or(err, previewErr); err == nil && stateOf(sqlstore.Meta(*backup.StoreMeta)) != stateOf(preview.Meta) {
		err = errStateRevisionChanged
	}
	if err != nil {
		RespondToError(c, err)
		return
	}
	expires := now.Add(resetConfirmationTTL)
	if backup.ProtectedUntil.Before(expires) {
		expires = backup.ProtectedUntil
	}
	answer := resetPreview{StoreID: preview.StoreID, Generation: preview.Generation, Revision: preview.Revision, BackupID: backup.ID,
		Removes: preview.Removes, ExpiresAt: time.Unix(expires.Unix(), 0).UTC()}
	if preview.Owner != nil {
		answer.Keeps = &resetKeeps{OwnerID: preview.Owner.ID, OwnerUsername: preview.Owner.Username, TeamID: preview.Team.ID, TeamName: preview.Team.Name}
	}
	answer.ConfirmationToken = s.signConfirmation(confirmation{stateOf(preview.Meta), backup.ID, expires.Unix()})
	respondSuccess(c, "Reset preview", answer)
}

// handleReset resets the access records as its preview said, once its token
// confirms that preview of this state, and answers with the stored receipt; a
// receipt stored for the same key and body answers before any check.
func (s *Server) handleReset(c *gin.Context) {
	key, bodyHash, ok := idempotencyOf(c)
	if !ok {
		RespondToError(c, httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeRequired),
			Message: "A reset requires an Idempotency-Key header, so a retry is answered from its receipt instead of resetting again"})
		return
	}
	var req resetRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	_, backupErr := s.backups.Get(req.BackupID) // judged under the writer gate, after the receipt
	receipt, err := s.stateStore.Reset(auditContext(c), sqlstore.ResetRequest{Principal: PrincipalFromContext(c), IdempotencyKey: key,
		BodySHA256: hex.EncodeToString(bodyHash[:]), BackupID: req.BackupID}, func(live sqlstore.Meta) error {
		confirmed, ok := s.readConfirmation(req.ConfirmationToken)
		switch {
		case backupErr != nil:
			return backupErr
		case !ok || confirmed.BackupID != req.BackupID || !utils.NowUTC().Before(time.Unix(confirmed.Expires, 0)):
			return errResetConfirmation
		case confirmed.resetState != stateOf(live):
			return errStateRevisionChanged
		}
		return nil
	})
	if err != nil {
		RespondToError(c, err)
		return
	}
	respondSuccess(c, "Access state reset", receipt)
}

// signConfirmation is c as a token: its JSON and an HMAC-SHA256 of it under
// this process's key.
func (s *Server) signConfirmation(c confirmation) string {
	payload, _ := json.Marshal(c) // strings and integers always encode
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(s.confirmationMAC(payload))
}

// readConfirmation returns what a token this process signed binds.
func (s *Server) readConfirmation(token string) (confirmation, bool) {
	var c confirmation
	encoded, sum, _ := strings.Cut(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	mac, macErr := base64.RawURLEncoding.DecodeString(sum)
	ok := err == nil && macErr == nil && hmac.Equal(mac, s.confirmationMAC(payload)) && json.Unmarshal(payload, &c) == nil
	return c, ok
}

func (s *Server) confirmationMAC(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.resetKey)
	mac.Write([]byte(confirmationDomain))
	mac.Write(payload)
	return mac.Sum(nil)
}
