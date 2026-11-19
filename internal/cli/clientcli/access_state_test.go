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

// resetServer serves a backup create, a preview of backup "b1" and the reset;
// reset answers each reset request, which carries the proof, the backup and
// an Idempotency-Key.
func resetServer(t *testing.T, reset func(w http.ResponseWriter, key string)) (dir string, paths *[]string) {
	t.Helper()
	paths = new([]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, err := localproof.Read(r.Context(), dir)
		require.NoError(t, err)
		require.Equal(t, []string{file.Proof}, r.Header.Values(localproof.Header))
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		*paths = append(*paths, r.URL.Path)
		switch r.URL.Path {
		case backupsPath:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"data":{"id":"b1"}}`))
		case resetPath + "/preview":
			require.Equal(t, map[string]string{"backup_id": "b1"}, body)
			_, _ = w.Write([]byte(`{"data":{"revision":7,"removes":{"app_keys":3,"teams":2,"people":1,"grants":4},"keeps":{"owner_username":"owner","team_name":"Personal workspace"}}}`))
		case resetPath:
			require.Equal(t, map[string]string{"backup_id": "b1"}, body)
			reset(w, r.Header.Get("Idempotency-Key"))
		}
	}))
	t.Cleanup(server.Close)
	return recoveryFixture(t, server.URL), paths
}

func runReset(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"access", "state", "reset", "--dir", dir}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// TestStateResetBacksUpPreviewsAndResets: without an ID the command takes a
// backup, previews against it, prints what goes and what stays, and resets
// that backup; a response lost on the reused connection is asked again under
// the same Idempotency-Key, and the result and the undo steps are printed.
func TestStateResetBacksUpPreviewsAndResets(t *testing.T) {
	var keys []string
	dir, paths := resetServer(t, func(w http.ResponseWriter, key string) {
		keys = append(keys, key)
		if len(keys) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close() // committed, but the answer is lost
			return
		}
		_, _ = w.Write([]byte(`{"data":{"operation_id":"op-1","new_revision":8}}`))
	})
	out, err := runReset(t, dir, "--yes")
	require.NoError(t, err, out)
	require.Equal(t, []string{backupsPath, resetPath + "/preview", resetPath, resetPath}, *paths)
	require.Len(t, keys, 2)
	require.NotEmpty(t, keys[0])
	require.Equal(t, keys[0], keys[1], "the retry reuses the key")
	for _, line := range []string{
		"Backup b1 holds the access records at revision 7.",
		"removes app keys: 3, teams: 2, people: 1, invitation and sign-in grants: 4,",
		`keeps the owner owner, who must sign in again, and their team "Personal workspace"`,
		"Reset op-1 committed at revision 8.",
		"zzrouter access state export --backup-id b1 --output FILE, stop the service, zzrouter-node recovery restore FILE --to STAGING --dir " + dir + ", then zzrouter-node recovery activate STAGING --dir " + dir,
		"The spend of removed keys and teams does not come back.",
	} {
		require.Contains(t, out, line)
	}
}

// TestStateResetStops: unconfirmed with no terminal, it resets nothing; a
// changed revision stops it with the node's next step.
func TestStateResetStops(t *testing.T) {
	dir, paths := resetServer(t, func(w http.ResponseWriter, _ string) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"state_revision_changed","detail":"The access records changed after this backup was taken, so nothing was reset. Take a new backup and preview the reset with it."}`))
	})
	_, err := runReset(t, dir, "--backup-id", "b1")
	require.ErrorContains(t, err, "not confirmed: nothing was reset")
	require.Equal(t, []string{resetPath + "/preview"}, *paths)

	_, err = runReset(t, dir, "--backup-id", "b1", "--yes")
	require.ErrorContains(t, err, "Take a new backup and preview the reset with it.")
	require.Equal(t, []string{resetPath + "/preview", resetPath + "/preview", resetPath}, *paths, "a refusal is not retried")
}

// TestStateResetPromptEndsOnInterrupt: an interrupt answers the prompt no at
// once, without waiting for the read.
func TestStateResetPromptEndsOnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	answered := make(chan bool)
	go func() { answered <- confirmed(ctx, func() bool { <-blocked; return true }) }()
	cancel()
	select {
	case ok := <-answered:
		require.False(t, ok)
	case <-time.After(10 * time.Second):
		t.Fatal("the prompt waited for its read after the interrupt")
	}
	require.True(t, confirmed(t.Context(), func() bool { return true }), "an answer before an interrupt counts")
}
