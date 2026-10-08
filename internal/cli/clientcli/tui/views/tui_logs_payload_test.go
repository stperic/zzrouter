package views

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
)

func newLogsViewWithEntry(t *testing.T, entry pkgClient.InferenceLogEntry) *LogsViewModel {
	t.Helper()
	v := NewLogsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	v.entries = []pkgClient.InferenceLogEntry{entry}
	v.syncLogItems()
	v.loading = false
	// Drive the real size path so the list computes its visible range.
	updated, _ := v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return updated.(*LogsViewModel)
}

func keyPress(k string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: rune(k[0]), Text: k}
}

// The pane must be reachable by keystroke from both the list and the
// detail pane, and Esc must back out of it.
func TestLogsPayload_KeyRouting(t *testing.T) {
	for _, from := range []logsViewMode{logsViewList, logsViewDetail} {
		v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
		v.mode = from

		updated, cmd := v.Update(keyPress("p"))
		v = updated.(*LogsViewModel)
		require.Equal(t, logsViewPayload, v.mode)
		require.NotNil(t, cmd)

		updated, _ = v.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		v = updated.(*LogsViewModel)
		assert.Equal(t, from, v.mode)
	}
}

func TestLogsPayload_OpenSwitchesModeAndTargetsCursorEntry(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})

	cmd := v.openPayload()

	assert.Equal(t, logsViewPayload, v.mode)
	assert.Equal(t, "entry-1", v.payloadID)
	assert.True(t, v.payloadLoading)
	assert.NotNil(t, cmd, "opening must kick off the fetch")
	assert.Equal(t, []string{"Logs", "llama3", "Payload"}, v.Breadcrumb())
}

func TestLogsPayload_BackReturnsToOriginatingPane(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
	v.mode = logsViewDetail

	v.openPayload()
	require.Equal(t, logsViewPayload, v.mode)
	v.closePayload()

	assert.Equal(t, logsViewDetail, v.mode, "back must return to the pane that opened it, not the list")
}

func TestLogsPayload_LoadedMessageClearsLoading(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
	v.openPayload()

	updated, _ := v.Update(logsPayloadLoadedMsg{id: "entry-1", payload: &pkgClient.InferenceLogPayload{
		Request: []byte(`{"model":"llama3"}`),
	}})
	v = updated.(*LogsViewModel)

	assert.False(t, v.payloadLoading)
	require.NotNil(t, v.payload)
	assert.Contains(t, v.viewPayload(), "Request")
}

func TestLogsPayload_RendersElidedMediaNotBinary(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llava", PayloadAvailable: true})
	v.openPayload()
	v.applyPayloadLoaded(logsPayloadLoadedMsg{id: "entry-1", payload: &pkgClient.InferenceLogPayload{
		Request: []byte(`{"model":"llava","images":["<elided 204800 bytes>"]}`),
		Elided: []pkgClient.ElidedBlob{
			{Path: "messages[0].images[0]", Media: "image/png", Bytes: 204800},
		},
	}})

	out := v.viewPayload()

	assert.Contains(t, out, "Attached Media")
	assert.Contains(t, out, "image/png")
	assert.Contains(t, out, "200.0 KiB")
}

func TestLogsPayload_ActionStatusIsRendered(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
	v.openPayload()
	v.applyPayloadLoaded(logsPayloadLoadedMsg{id: "entry-1",
		payload: &pkgClient.InferenceLogPayload{Request: []byte(`{"model":"llama3"}`)}})

	updated, _ := v.Update(logsPayloadActionMsg{err: assertError{}})
	v = updated.(*LogsViewModel)
	assert.Contains(t, v.payloadStatus, "Failed:")
	assert.Contains(t, v.viewPayload(), "clipboard unavailable",
		"a status pinned below a long payload must still reach the screen")

	updated, _ = v.Update(logsPayloadActionMsg{status: "Copied 42 bytes to clipboard"})
	v = updated.(*LogsViewModel)
	assert.Contains(t, v.viewPayload(), "Copied 42 bytes")
}

// A response for a pane the user already left must not overwrite the one
// they are looking at now.
func TestLogsPayload_StaleResponseIsDropped(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-2", Model: "llama3", PayloadAvailable: true})
	v.openPayload()
	require.Equal(t, "entry-2", v.payloadID)

	v.applyPayloadLoaded(logsPayloadLoadedMsg{id: "entry-1",
		payload: &pkgClient.InferenceLogPayload{Request: []byte(`{"model":"stale"}`)}})

	assert.Nil(t, v.payload, "a response for a different entry must be discarded")
	assert.True(t, v.payloadLoading, "and must not clear the pending state")
}

// The spinner keeps ticking while the payload is in flight.
func TestLogsPayload_SpinnerRunsDuringFetch(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
	v.openPayload()
	require.True(t, v.payloadLoading)

	_, cmd := v.Update(shared.SpinnerTickMsg{})

	assert.NotNil(t, cmd, "the tick must re-arm while the payload loads")
}

// An entry the list already reports as unretained must not cost a request.
func TestLogsPayload_SkipsFetchWhenUnavailable(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3"})

	cmd := v.openPayload()

	assert.Nil(t, cmd, "no round trip when the server already said it is gone")
	assert.False(t, v.payloadLoading)
	assert.Contains(t, v.viewPayload(), "No payload retained")
}

// Opening the payload pane must not throw away where the detail pane was.
func TestLogsPayload_PreservesDetailScroll(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
	v.mode = logsViewDetail
	for i := 0; i < 3; i++ {
		v.detail.UpdateKey(tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	before := v.detail.Scroll()
	require.Positive(t, before)

	v.openPayload()
	v.closePayload()

	assert.Equal(t, before, v.detail.Scroll())
}

// Open is the headline action: it must write the file AND hand that exact
// path to the desktop handler.
func TestLogsPayload_OpenSavesThenLaunchesHandler(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var opened string

	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
	v.openFile = func(path string) error { opened = path; return nil }
	v.openPayload()
	v.applyPayloadLoaded(logsPayloadLoadedMsg{id: "entry-1",
		payload: &pkgClient.InferenceLogPayload{Request: []byte(`{"model":"llama3"}`)}})

	msg := v.openPayloadFileCmd()().(logsPayloadActionMsg)

	require.NoError(t, msg.err)
	assert.Contains(t, msg.status, "Opened ")

	want := filepath.Join(dir, "zzrouter-payload-entry-1.json")
	assert.Equal(t, want, opened, "the handler must get the file just written")

	data, err := os.ReadFile(want)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"model": "llama3"`)
}

// A missing xdg-open must not lose the payload the user asked for.
func TestLogsPayload_OpenFailureStillReportsTheSavedPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
	v.openFile = func(string) error { return assertError{} }
	v.openPayload()
	v.applyPayloadLoaded(logsPayloadLoadedMsg{id: "entry-1",
		payload: &pkgClient.InferenceLogPayload{Request: []byte(`{"model":"llama3"}`)}})

	msg := v.openPayloadFileCmd()().(logsPayloadActionMsg)

	require.NoError(t, msg.err)
	assert.Contains(t, msg.status, "zzrouter-payload-entry-1.json")
	assert.Contains(t, msg.status, "could not open it")

	_, err := os.Stat(filepath.Join(dir, "zzrouter-payload-entry-1.json"))
	assert.NoError(t, err, "the file must survive a failed launch")
}

// O and Enter both open, since opening is what the pane is usually for.
func TestLogsPayload_OpenKeyBindings(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{
		{Code: 'o', Text: "o"},
		{Code: 'O', Text: "O"},
		{Code: tea.KeyEnter},
	} {
		dir := t.TempDir()
		t.Chdir(dir)

		var opened string

		v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
		v.openFile = func(path string) error { opened = path; return nil }
		v.openPayload()
		v.applyPayloadLoaded(logsPayloadLoadedMsg{id: "entry-1",
			payload: &pkgClient.InferenceLogPayload{Request: []byte(`{}`)}})

		cmd := v.handlePayloadKey(k)
		require.NotNil(t, cmd, "key %v must be bound to open", k)
		cmd()

		assert.NotEmpty(t, opened)

		// Negative control: an unbound key must not launch anything.
		opened = ""
		assert.Nil(t, v.handlePayloadKey(tea.KeyPressMsg{Code: 'z', Text: "z"}))
		assert.Empty(t, opened)
	}
}

func TestLogsPayload_CopyWithoutPayloadReportsWhy(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
	v.openPayload()

	msg := v.copyPayloadCmd()().(logsPayloadActionMsg)

	assert.Contains(t, msg.status, "No payload")
}

type assertError struct{}

func (assertError) Error() string { return "clipboard unavailable" }

func TestLogsPayload_SaveWritesOwnerOnlyFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3"})
	v.payloadID = "entry-1"
	v.payload = &pkgClient.InferenceLogPayload{Request: []byte(`{"model":"llama3"}`)}

	msg := v.savePayloadCmd()().(logsPayloadActionMsg)
	require.NoError(t, msg.err)

	path := filepath.Join(dir, "zzrouter-payload-entry-1.json")
	assert.Contains(t, msg.status, "zzrouter-payload-entry-1.json")

	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(payloadFileMode), info.Mode().Perm(), "payloads carry prompt content")
	} else {
		// Windows mode bits do not express owner-only access; this test does not inspect ACLs.
		assert.True(t, info.Mode().IsRegular())
	}

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"model": "llama3"`)
}

// A server-supplied ID must not steer the save outside the working directory.
func TestLogsPayload_SaveIgnoresPathTraversalInID(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "x", Model: "m"})
	v.payloadID = "../../escaped"
	v.payload = &pkgClient.InferenceLogPayload{Request: []byte(`{}`)}

	msg := v.savePayloadCmd()().(logsPayloadActionMsg)
	require.NoError(t, msg.err)

	assert.False(t, strings.Contains(msg.status, ".."), "status: %s", msg.status)
	_, err := os.Stat(filepath.Join(dir, "zzrouter-payload-escaped.json"))
	assert.NoError(t, err, "the file must land in the working directory")
}

func TestLogsPayload_ListMarksAvailability(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "a", Model: "llama3", PayloadAvailable: true})
	v.entries = append(v.entries, pkgClient.InferenceLogEntry{ID: "b", Model: "llama3"})
	v.syncLogItems()

	out := v.viewList()

	assert.Contains(t, out, "PAYLOAD")
	assert.Contains(t, out, "yes")
}

// The fetch must reach the payload endpoint for the selected entry.
func TestLogsPayload_FetchHitsPayloadEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"request":{"model":"llama3"}}}`))
	}))
	t.Cleanup(srv.Close)

	client := pkgClient.NewClient(pkgConfig.ClientNodeConfig{Name: "test", Address: srv.URL, APIKey: "k"})
	v := NewLogsViewModel(client, ui.NewStyles(ui.CatppuccinMocha()))

	msg := v.fetchPayloadCmd("entry-1")().(logsPayloadLoadedMsg)

	require.NoError(t, msg.err)
	assert.Equal(t, "/zzrouter/v1/inference-logs/entry-1/payload", gotPath)
	assert.JSONEq(t, `{"model":"llama3"}`, string(msg.payload.Request))
}

// The reply must be visible on the entry that produced it.
func TestLogsDetail_ShowsTheResponse(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{
		ID: "entry-1", Model: "llama3",
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: "Hi"}},
		Response: "Hey Eric!",
	})
	v.mode = logsViewDetail

	out := v.viewDetail()

	assert.Contains(t, out, "Response")
	assert.Contains(t, out, "Hey Eric!")
}

func TestLogsPayload_RendersResponseBody(t *testing.T) {
	v := newLogsViewWithEntry(t, pkgClient.InferenceLogEntry{ID: "entry-1", Model: "llama3", PayloadAvailable: true})
	v.openPayload()
	v.applyPayloadLoaded(logsPayloadLoadedMsg{id: "entry-1", payload: &pkgClient.InferenceLogPayload{
		Request:  []byte(`{"model":"llama3"}`),
		Response: []byte(`{"choices":[{"message":{"content":"Hey Eric!"}}]}`),
	}})

	out := v.viewPayload()

	assert.Contains(t, out, "Response")
	assert.Contains(t, out, "Hey Eric!")
}

// An empty logs view must still say how to get logs — offering only
// "back" leaves refresh undiscoverable.
func TestLogsList_EmptyStateOffersRefresh(t *testing.T) {
	v := NewLogsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	v.loading = false
	updated, _ := v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v = updated.(*LogsViewModel)

	out := v.viewList()

	assert.Contains(t, out, "No inference logs found")
	assert.Contains(t, out, "refresh")
	assert.Contains(t, out, "follow")
	assert.Contains(t, out, "back")
}

// A failed fetch is exactly when a retry hint is worth showing.
func TestLogsList_ErrorStateOffersRetry(t *testing.T) {
	v := NewLogsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	v.loading = false
	v.err = assertError{}
	updated, _ := v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v = updated.(*LogsViewModel)

	out := v.viewList()

	assert.Contains(t, out, "retry")
}

// The key must work in the empty state, not just be advertised there.
func TestLogsList_RefreshWorksWhenEmpty(t *testing.T) {
	v := NewLogsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	v.loading = false

	updated, cmd := v.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	v = updated.(*LogsViewModel)

	require.NotNil(t, cmd, "R must issue a fetch with no entries loaded")
	assert.True(t, v.loading)
}
