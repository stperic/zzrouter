package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/audit"
)

// stateAuditSubject is the canonical Subject used on state snapshot/
// restore audit events. The endpoint is whole-cluster-scoped — there's
// no narrower object to name — so a fixed sentinel keeps grep-ability
// trivial for compliance tooling.
const stateAuditSubject = "cluster-state"

func (s *Server) auditState(c *gin.Context, action audit.Action, meta map[string]any) {
	if s.auditSink == nil {
		return
	}
	reqID := ""
	if v, ok := c.Get("request_id"); ok {
		if id, ok := v.(string); ok {
			reqID = id
		}
	}
	audit.Log(c.Request.Context(), s.auditSink, audit.Event{
		Action:    action,
		Actor:     auditActor(c),
		Subject:   stateAuditSubject,
		RequestID: reqID,
		Metadata:  meta,
	})
}

// State snapshot/restore — wired for the e2e harness "populated cluster"
// and "upgrade in place" suites. Admin-key gated on the public surface
// (the public-engine /internal/* mount is reserved for in-process
// dispatch — see internalRequestOnlyMiddleware). Covers keys.yaml,
// teams.yaml, model_groups.yaml, spend.json.
//
// SECURITY POSTURE: snapshot exposes raw on-disk file contents to any
// admin-key holder, including Argon2id hashes from keys.yaml that the
// public /keys controller redacts. Documented as a known posture
// change pending a dedicated maintenance-key gate; do NOT widen the
// scope of these endpoints (e.g., to TierAPI) without revisiting.
//
// Atomicity bounds:
//   - Stores are flushed before tar reads from disk. Concurrent admin
//     mutations (POST /keys etc.) during the read window are NOT fenced;
//     the harness must not drive concurrent admin writes against a node
//     while a snapshot is in flight.
//   - In-flight quota reservations stash for ≤30s post-response
//     (AccessControl.RecordSpendByKey, reaper sweep every 5s). A
//     snapshot taken during active stashes captures spend.json before
//     those reservations settle; the populated-cluster suite must
//     quiesce inference before snapshot.
//   - spend.json is written by the SpendTracker persist loop, not by
//     the snapshot handler. The tar reflects the loop's most recent
//     flush (≤persist interval old).

// snapshotFiles names the entries the snapshot/restore endpoints care
// about. Files that don't exist on snapshot are silently skipped; files
// not present in a restore tar are left untouched on disk.
var snapshotFiles = []string{
	"keys.yaml",
	"teams.yaml",
	"model_groups.yaml",
	"spend.json",
}

// maxRestoreEntryBytes caps each tar entry to bound memory exposure
// against an admin-key holder posting a gzip-bomb. 16 MiB is ~1000×
// a typical keys.yaml; spend.json is the largest in practice and stays
// well under this on any realistic cluster.
const maxRestoreEntryBytes = 16 << 20

// handleStateSnapshot returns a gzipped tar of the cluster's persistent
// state files. Triggers store-level flushes first so the tar reflects
// in-memory state, not just whatever last hit disk.
func (s *Server) handleStateSnapshot(c *gin.Context) {
	dir, err := s.stateConfigDir()
	if err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}

	if s.keyStore != nil && len(s.keyStore.List()) > 0 {
		if err := s.keyStore.Save(); err != nil {
			InternalNodeError(c, "keys flush: "+err.Error())
			return
		}
	}
	if s.teamStore != nil && len(s.teamStore.List()) > 0 {
		if err := s.teamStore.Save(); err != nil {
			InternalNodeError(c, "teams flush: "+err.Error())
			return
		}
	}
	if s.model != nil && s.model.Groups != nil && s.model.Groups.Path() != "" {
		if err := s.model.Groups.Save(); err != nil {
			InternalNodeError(c, "groups flush: "+err.Error())
			return
		}
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range snapshotFiles {
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			InternalNodeError(c, fmt.Sprintf("read %s: %v", name, err))
			return
		}
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			InternalNodeError(c, "tar header: "+err.Error())
			return
		}
		if _, err := tw.Write(data); err != nil {
			InternalNodeError(c, "tar write: "+err.Error())
			return
		}
	}
	if err := tw.Close(); err != nil {
		InternalNodeError(c, "tar close: "+err.Error())
		return
	}
	if err := gz.Close(); err != nil {
		InternalNodeError(c, "gzip close: "+err.Error())
		return
	}

	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", "attachment; filename=zzrouter-state.tar.gz")
	c.Data(http.StatusOK, "application/gzip", buf.Bytes())

	s.auditState(c, audit.ActionStateSnapshot, map[string]any{
		"bytes": buf.Len(),
	})
}

// handleStateRestore accepts a gzipped tar from handleStateSnapshot,
// writes the contained files into the config dir, then reloads the
// in-memory stores so the running server sees the restored state
// without a process restart.
//
// Atomicity: every entry is fully decoded + size-capped + validated in
// memory BEFORE any file is written. Writes go to sibling .tmp files in
// snapshotFiles order, then rename in the same order — partial-restore
// on a mid-loop crash leaves a deterministic prefix of files renamed,
// not a random subset, which makes recovery (re-POST same tar) idempotent.
// Reload failure after a successful rename leaves disk consistent but
// in-memory state partially new; the caller is expected to restart.
func (s *Server) handleStateRestore(c *gin.Context) {
	dir, err := s.stateConfigDir()
	if err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}

	gz, err := gzip.NewReader(c.Request.Body)
	if err != nil {
		BadRequest(c, "gzip decode: "+err.Error())
		return
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	allowed := make(map[string]bool, len(snapshotFiles))
	for _, name := range snapshotFiles {
		allowed[name] = true
	}

	staged := make(map[string][]byte, len(snapshotFiles))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			BadRequest(c, "tar read: "+err.Error())
			return
		}
		// tar uses '/' as separator regardless of OS; use path.Base so
		// Windows can't reinterpret backslashes as non-separators.
		if hdr.Name != path.Base(hdr.Name) || !allowed[hdr.Name] {
			BadRequest(c, "rejected entry: "+hdr.Name)
			return
		}
		// Bounded read: a single entry larger than the cap is rejected
		// before any allocation grows beyond the cap. Reading cap+1 lets
		// us distinguish "exactly at cap" (legal) from "over cap" (reject).
		data, err := io.ReadAll(io.LimitReader(tr, maxRestoreEntryBytes+1))
		if err != nil {
			BadRequest(c, "tar entry read: "+err.Error())
			return
		}
		if int64(len(data)) > maxRestoreEntryBytes {
			RespondWithProblem(c, http.StatusRequestEntityTooLarge, "Request Entity Too Large", "entry "+hdr.Name+" exceeds size cap")
			return
		}
		staged[hdr.Name] = data
	}

	// Stage all files as .tmp siblings, then rename in snapshotFiles
	// order. A crash mid-rename leaves a deterministic prefix of files
	// updated and the rest unchanged + their .tmp lingering — recovery
	// is "re-POST the same tar" or "rm *.tmp; restart". Iteration over
	// the ordered slice (not the staged map) makes that prefix
	// reproducible across runs.
	tmpPaths := make(map[string]string, len(staged))
	for _, name := range snapshotFiles {
		data, ok := staged[name]
		if !ok {
			continue
		}
		tmp := filepath.Join(dir, name+".tmp")
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			for _, p := range tmpPaths {
				_ = os.Remove(p)
			}
			InternalNodeError(c, "stage "+name+": "+err.Error())
			return
		}
		tmpPaths[name] = tmp
	}

	written := make(map[string]bool, len(staged))
	writtenNames := make([]string, 0, len(staged))
	for _, name := range snapshotFiles {
		tmp, ok := tmpPaths[name]
		if !ok {
			continue
		}
		final := filepath.Join(dir, name)
		if err := os.Rename(tmp, final); err != nil {
			InternalNodeError(c, "commit "+name+": "+err.Error())
			return
		}
		written[name] = true
		writtenNames = append(writtenNames, name)
	}

	// Rehydrate in-memory caches only for files we actually wrote.
	// Snapshot intentionally skips files that don't exist (line 90-92);
	// reload symmetry means we don't reload caches whose backing file
	// the tar didn't carry — the on-disk file may be absent and Load
	// will (correctly) return ErrNotExist.
	if s.keyStore != nil && written["keys.yaml"] {
		if err := s.keyStore.Reload(); err != nil {
			InternalNodeError(c, "keys reload: "+err.Error())
			return
		}
	}
	if s.teamStore != nil && written["teams.yaml"] {
		if err := s.teamStore.Reload(); err != nil {
			InternalNodeError(c, "teams reload: "+err.Error())
			return
		}
	}
	if s.model != nil && s.model.Groups != nil && written["model_groups.yaml"] {
		if groupsPath := s.model.Groups.Path(); groupsPath != "" {
			if err := s.model.Groups.LoadFromFile(groupsPath); err != nil {
				InternalNodeError(c, "groups reload: "+err.Error())
				return
			}
		}
	}

	sort.Strings(writtenNames) // stable order for tests + diff-friendly logs
	c.JSON(http.StatusOK, gin.H{"restored": writtenNames})

	s.auditState(c, audit.ActionStateRestore, map[string]any{
		"files": writtenNames,
	})
}

func (s *Server) stateConfigDir() (string, error) {
	if s.auditConfigDir != "" {
		return s.auditConfigDir, nil
	}
	return "", fmt.Errorf("config dir not set on server")
}
