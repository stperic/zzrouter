package clientcli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// fakeAssetsAPI records what the commands send and serves canned data.
type fakeAssetsAPI struct {
	data     []byte
	put      []byte
	putName  string
	restart  bool
	runs     *pkgClient.RunsReport
	deleted  string
	failWith error
}

func (f *fakeAssetsAPI) ListProviderAssets(string) ([]pkgClient.ProviderAsset, error) {
	return nil, f.failWith
}

func (f *fakeAssetsAPI) GetProviderAsset(_, _ string) ([]byte, error) {
	return f.data, f.failWith
}

func (f *fakeAssetsAPI) PutProviderAsset(_, name string, data []byte, restart bool) (pkgClient.ProviderAssetWrite, error) {
	f.putName, f.put, f.restart = name, data, restart
	asset := pkgClient.ProviderAsset{Name: name, Size: int64(len(data)), SHA256: "abc"}
	return pkgClient.ProviderAssetWrite{ProviderAsset: asset, Runs: f.runs}, f.failWith
}

func (f *fakeAssetsAPI) DeleteProviderAsset(_, name string) error {
	f.deleted = name
	return f.failWith
}

func newTestCmd(stdin string) (*cobra.Command, *bytes.Buffer) {
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader(stdin))
	return cmd, out
}

func TestProvidersAssetsCommands(t *testing.T) {
	assetsCmd, _, err := NewProvidersCmd().Find([]string{"assets"})
	require.NoError(t, err)
	require.Equal(t, "assets", assetsCmd.Name())

	want := map[string]string{"list": "list <provider>", "get": "get <provider> <name>", "put": "put <provider> <name> <file|->", "rm": "rm <provider> <name>"}
	for name, use := range want {
		sub, _, err := assetsCmd.Find([]string{name})
		require.NoError(t, err, name)
		assert.Equal(t, use, sub.Use)
		require.NotNil(t, sub.RunE, name)
	}

	// get writes a file with --file and leaves -o to the global
	// output-format flag.
	get, _, _ := assetsCmd.Find([]string{"get"})
	assert.NotNil(t, get.Flags().Lookup("file"))
	assert.Nil(t, get.LocalNonPersistentFlags().ShorthandLookup("o"))
}

func TestProvidersCmd_RejectsUnknownSubcommand(t *testing.T) {
	cmd := NewProvidersCmd()
	require.NotNil(t, cmd.Args)
	assert.Error(t, cmd.Args(cmd, []string{"asets"}))
}

func TestRunAssetsGet_ToFile(t *testing.T) {
	api := &fakeAssetsAPI{data: []byte("{{ messages }}")}
	cmd, out := newTestCmd("")
	path := filepath.Join(t.TempDir(), "copy.jinja")

	require.NoError(t, runAssetsGet(cmd, api, "llamacpp", "t.jinja", path))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "{{ messages }}", string(got))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	// os.WriteFile applies the umask, so only the bits it can't clear are pinned.
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm()&0o700, "owner reads and writes, never executes")
	assert.Empty(t, out.String(), "bytes go to the file, not stdout")
}

func TestRunAssetsGet_ToStdout(t *testing.T) {
	api := &fakeAssetsAPI{data: []byte("raw\x00bytes")}
	cmd, out := newTestCmd("")

	require.NoError(t, runAssetsGet(cmd, api, "llamacpp", "t.jinja", ""))
	assert.Equal(t, "raw\x00bytes", out.String(), "stdout carries the bytes unchanged")
}

func TestRunAssetsPut_FromStdin(t *testing.T) {
	api := &fakeAssetsAPI{}
	cmd, out := newTestCmd("from stdin")

	require.NoError(t, runAssetsPut(cmd, api, "llamacpp", "t.jinja", "-", false))
	assert.Equal(t, "t.jinja", api.putName)
	assert.Equal(t, "from stdin", string(api.put))
	assert.False(t, api.restart)
	assert.Contains(t, out.String(), "Stored t.jinja for llamacpp")
}

// The write says which running models it leaves stale, and how to
// restart them; with --restart it names the job doing so.
func TestRunAssetsPut_ReportsRuns(t *testing.T) {
	stale := pkgClient.RunRef{ID: "r1", Node: "worker-1", Model: "qwen", Changed: []string{"parameters.chat-template-file"}}
	api := &fakeAssetsAPI{runs: &pkgClient.RunsReport{Stale: []pkgClient.RunRef{stale}}}
	cmd, out := newTestCmd("x")
	require.NoError(t, runAssetsPut(cmd, api, "llamacpp", "t.jinja", "-", false))
	assert.Contains(t, out.String(), "stale      qwen on worker-1 (r1): parameters.chat-template-file")
	assert.Contains(t, out.String(), "--restart")

	api.runs.RestartJobID = "run_1"
	cmd, out = newTestCmd("x")
	require.NoError(t, runAssetsPut(cmd, api, "llamacpp", "t.jinja", "-", true))
	assert.True(t, api.restart)
	assert.Contains(t, out.String(), "Restarting 1 stale run(s) in job run_1")
}

func TestRunAssetsRm(t *testing.T) {
	api := &fakeAssetsAPI{}
	cmd, out := newTestCmd("")
	require.NoError(t, runAssetsRm(cmd, api, "llamacpp", "t.jinja"))
	assert.Equal(t, "t.jinja", api.deleted)
	assert.Contains(t, out.String(), "Deleted t.jinja from llamacpp")

	api = &fakeAssetsAPI{failWith: errors.New("asset_in_use")}
	cmd, out = newTestCmd("")
	assert.Error(t, runAssetsRm(cmd, api, "llamacpp", "t.jinja"))
	assert.Empty(t, out.String(), "a refused delete reports nothing as deleted")
}

func TestReadAssetSource(t *testing.T) {
	data, err := readAssetSource(strings.NewReader("from stdin"), "-")
	require.NoError(t, err)
	assert.Equal(t, "from stdin", string(data))

	path := filepath.Join(t.TempDir(), "t.jinja")
	require.NoError(t, os.WriteFile(path, []byte("from file"), 0o600))
	data, err = readAssetSource(strings.NewReader("ignored"), path)
	require.NoError(t, err)
	assert.Equal(t, "from file", string(data))

	_, err = readAssetSource(nil, filepath.Join(t.TempDir(), "absent"))
	assert.Error(t, err)
}

func TestShortDigest(t *testing.T) {
	assert.Equal(t, "0123456789ab", shortDigest("0123456789abcdef"))
	assert.Equal(t, "ab", shortDigest("ab"))
}
