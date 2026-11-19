package clientcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/access/localproof"
	"github.com/stretchr/testify/require"
)

var exportArchive = bytes.Repeat([]byte("archive bytes "), 1000)

// exportServer serves a backup create and the content of backup "b1" with the
// proof in its header; content writes the body and its headers.
func exportServer(t *testing.T, content func(w http.ResponseWriter, r *http.Request)) (dir string, requests *atomic.Int32) {
	t.Helper()
	requests = new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		file, err := localproof.Read(r.Context(), dir)
		require.NoError(t, err)
		require.Equal(t, []string{file.Proof}, r.Header.Values(localproof.Header))
		switch r.Method + " " + r.URL.Path {
		case "POST " + backupsPath:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"id": "b1"}})
		case "GET " + backupsPath + "/b1/content":
			content(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "state_backup_not_found", "detail": "No retained online backup has this ID."})
		}
	}))
	t.Cleanup(server.Close)
	return recoveryFixture(t, server.URL), requests
}

// serveArchive answers with the archive and digest, cut to body.
func serveArchive(body []byte, digest [32]byte) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(exportArchive)))
		w.Header().Set("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
		_, _ = w.Write(body)
	}
}

func requireNoPartial(t *testing.T, output string) {
	t.Helper()
	require.NoFileExists(t, output)
	require.NoFileExists(t, output+".partial")
}

// TestStateExportWritesACheckedCopy: without an ID the command backs up first,
// then writes the archive, owner-only, and names the offline check; with an
// ID it only copies.
func TestStateExportWritesACheckedCopy(t *testing.T) {
	dir, requests := exportServer(t, serveArchive(exportArchive, sha256.Sum256(exportArchive)))
	for _, args := range [][]string{{}, {"--backup-id", "b1"}} {
		before := requests.Load()
		output := filepath.Join(t.TempDir(), "copy.tar.gz")
		var out bytes.Buffer
		cmd := newRootCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"access", "state", "export", "--dir", dir, "--output", output}, args...))
		require.NoError(t, cmd.Execute(), out.String())
		require.Equal(t, exportArchive, readFile(t, output))
		require.NoFileExists(t, output+".partial")
		if runtime.GOOS != "windows" {
			info, err := os.Stat(output)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		}
		require.Contains(t, out.String(), "zzrouter-node recovery check "+output)
		require.EqualValues(t, 2-len(args)/2, requests.Load()-before, "a backup first only without an ID")
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// TestStateExportLeavesNothingWhenItFails: a digest or length that does not
// match, a refusal and an interrupt each fail the export and leave neither
// the copy nor its partial.
func TestStateExportLeavesNothingWhenItFails(t *testing.T) {
	var cancel context.CancelFunc // the running case's
	var output string
	for name, tc := range map[string]struct {
		content func(http.ResponseWriter, *http.Request)
		id      string
		err     string
	}{
		"a wrong digest": {content: serveArchive(exportArchive, sha256.Sum256([]byte("other"))), id: "b1", err: "does not match"},
		"a short body":   {content: serveArchive(exportArchive[:100], sha256.Sum256(exportArchive)), id: "b1", err: "unexpected EOF"},
		"a refusal":      {id: "gone", err: "Without --backup-id, the command takes a new backup"},
		"an interruption": {id: "b1", err: "context canceled", content: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(exportArchive)))
			_, _ = w.Write(exportArchive[:100])
			w.(http.Flusher).Flush()
			require.Eventually(t, func() bool { _, err := os.Stat(output + ".partial"); return err == nil }, 10*time.Second, time.Millisecond, "the copy has begun")
			cancel()
			<-r.Context().Done()
		}},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := exportServer(t, tc.content)
			output = filepath.Join(t.TempDir(), "copy.tar.gz")
			var ctx context.Context
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			err := exportBackup(ctx, dir, tc.id, output)
			require.ErrorContains(t, err, tc.err)
			requireNoPartial(t, output)
		})
	}
}

// TestStateExportRefusesItsOutput: an existing output and one inside the
// installation are refused before the node is asked anything.
func TestStateExportRefusesItsOutput(t *testing.T) {
	dir, requests := exportServer(t, serveArchive(exportArchive, sha256.Sum256(exportArchive)))
	existing := filepath.Join(t.TempDir(), "copy.tar.gz")
	require.NoError(t, os.WriteFile(existing, []byte("keep"), 0o600))
	require.ErrorContains(t, exportBackup(t.Context(), dir, "b1", existing), "exists")
	require.Equal(t, []byte("keep"), readFile(t, existing))
	require.ErrorContains(t, exportBackup(t.Context(), dir, "b1", filepath.Join(dir, "copy.tar.gz")), "inside the installation")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	require.ErrorContains(t, exportBackup(t.Context(), dir, "b1", filepath.Join(dir, "sub", "copy.tar.gz")), "inside the installation")
	outside := t.TempDir()
	if os.Symlink(filepath.Join(dir, "sub"), filepath.Join(outside, "link")) == nil { // link/.. is the installation
		require.ErrorContains(t, exportBackup(t.Context(), dir, "b1", outside+"/link/../dotdot.tar.gz"), "inside the installation")
	}
	if upper := strings.ToUpper(dir); upper != dir {
		if _, err := os.Stat(upper); err == nil { // a case-insensitive disk
			require.ErrorContains(t, exportBackup(t.Context(), dir, "b1", filepath.Join(upper, "copy.tar.gz")), "inside the installation")
		}
	}
	require.Zero(t, requests.Load())
}
