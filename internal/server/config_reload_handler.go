package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// registerConfigReloadRoute mounts POST /zzrouter/v1/config/reload on the
// admin-authenticated router group. The handler re-reads providers/ from
// disk, fans listeners, and returns the aggregated ReloadReport so
// operators can see per-listener dispositions (applied / ignored /
// requires_restart / rejected) in one place.
//
// HTTP semantics:
//   - 200 OK on success, including when every listener returned Ignored.
//   - 409 Conflict when any listener flagged RequiresRestart.
//   - 422 Unprocessable Entity when any listener Rejected the change.
//   - 500 Internal Server Error if the disk read fails.
func registerConfigReloadRoute(router *gin.RouterGroup, store *pkgConfig.AppsConfigStore) {
	router.POST("/config/reload", func(c *gin.Context) {
		if store == nil {
			ServiceUnavailable(c, "provider config is not loaded on this node")
			return
		}
		report, err := store.Reload()
		if err != nil {
			InternalNodeError(c, err.Error())
			return
		}
		status := http.StatusOK
		switch {
		case report.HasRejection():
			status = http.StatusUnprocessableEntity
		case report.NeedsRestart():
			status = http.StatusConflict
		}
		entries := make([]gin.H, 0, len(report.Entries))
		for _, e := range report.Entries {
			entries = append(entries, gin.H{
				"listener":    e.Listener,
				"disposition": e.Disposition.String(),
				"message":     e.Message,
			})
		}
		c.JSON(status, gin.H{
			"entries":       entries,
			"needs_restart": report.NeedsRestart(),
			"has_rejection": report.HasRejection(),
		})
	})
}
