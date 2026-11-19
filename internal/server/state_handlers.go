package server

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/recovery"
	"github.com/stperic/zzrouter/pkg/utils"
)

// storeState is GET /system/state: the access store's identity, counters,
// sizes in bytes (null when unmeasured) and the check it passed at startup.
type storeState struct {
	StoreID        string         `json:"store_id"`
	Generation     string         `json:"generation"`
	SchemaVersion  int            `json:"schema_version"`
	Revision       uint64         `json:"revision"`
	Sequence       uint64         `json:"sequence"`
	Status         string         `json:"status"` // ready, or setup_required before the owner claim
	ObservedAt     time.Time      `json:"observed_at"`
	Size           storeSize      `json:"size"`
	Integrity      storeIntegrity `json:"integrity"`
	RetainedCopies []retainedCopy `json:"retained_copies"`
}

// retainedCopy is a copy of access records outside the database: an
// activation's rollback directory, with no path.
type retainedCopy struct {
	Kind string `json:"kind"` // activation_rollback
	recovery.Rollback
}

type storeSize struct {
	DatabaseBytes *int64 `json:"database_bytes"`
	JournalBytes  *int64 `json:"journal_bytes"`
	FreePageBytes *int64 `json:"free_page_bytes"`
	BackupBytes   int64  `json:"backup_bytes"`
}

type storeIntegrity struct {
	CheckedAt time.Time `json:"checked_at"`
	Check     string    `json:"check"`
	Outcome   string    `json:"outcome"` // ok: a failed check stops the node at startup
}

// handleStoreState reports the access store without a query or the writer
// gate, and the retained copies of its records.
func (s *Server) handleStoreState(c *gin.Context) {
	observed := utils.NowUTC()
	st, err := s.stateStore.Status()
	if err != nil {
		RespondToError(c, err)
		return
	}
	rollbacks, err := recovery.Rollbacks(s.configDir)
	if err != nil {
		RespondToError(c, err)
		return
	}
	copies := make([]retainedCopy, 0, len(rollbacks))
	for _, rollback := range rollbacks {
		copies = append(copies, retainedCopy{Kind: "activation_rollback", Rollback: rollback})
	}
	status := "setup_required"
	if st.Initialized {
		status = "ready"
	}
	respondRetrievalSuccess(c, "Access store state", storeState{
		StoreID: st.StoreID, Generation: st.Generation, SchemaVersion: st.SchemaVersion, Revision: st.Revision, Sequence: st.Sequence,
		Status: status, ObservedAt: observed, RetainedCopies: copies,
		Size:      storeSize{DatabaseBytes: st.DatabaseBytes, JournalBytes: st.JournalBytes, FreePageBytes: st.FreePageBytes},
		Integrity: storeIntegrity{CheckedAt: st.Integrity.CheckedAt, Check: st.Integrity.Check, Outcome: "ok"},
	})
}
