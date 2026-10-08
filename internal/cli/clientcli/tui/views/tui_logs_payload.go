package views

// Payload pane for the logs view: fetches the request bodies the server
// retained for one entry, renders them, and opens, copies or saves them.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// payloadFileMode keeps a saved payload owner-only: it carries the prompt
// content of a real request.
const payloadFileMode = 0o600

// logsPayloadLoadedMsg carries the entry ID so a slow response for a pane
// the user already left cannot land on the entry they opened next.
type logsPayloadLoadedMsg struct {
	id      string
	payload *pkgClient.InferenceLogPayload
	err     error
}

// logsPayloadActionMsg reports the outcome of a copy or save.
type logsPayloadActionMsg struct {
	status string
	err    error
}

// openPayload switches to the payload pane for the selected entry.
func (m *LogsViewModel) openPayload() tea.Cmd {
	c := m.list.Cursor()
	if c < 0 || c >= len(m.entries) {
		return nil
	}
	entry := m.entries[c]

	m.payloadReturn = m.mode
	m.mode = logsViewPayload
	m.payloadDetail.Reset()
	m.payload = nil
	m.payloadLines = nil
	m.payloadErr = nil
	m.payloadStatus = ""
	m.payloadID = entry.ID
	// Pinned at open so a live-tailing list cannot retitle the pane.
	m.payloadModel = entry.Model

	// The list already knows the server dropped these bodies, so showing
	// the empty state beats a round trip that can only 404.
	if !entry.PayloadAvailable {
		m.payloadLoading = false
		return nil
	}
	m.payloadLoading = true
	return tea.Batch(m.fetchPayloadCmd(entry.ID), shared.SpinnerTickCmd())
}

func (m *LogsViewModel) fetchPayloadCmd(id string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		payload, err := client.GetInferenceLogPayload(id)
		return logsPayloadLoadedMsg{id: id, payload: payload, err: err}
	}
}

// applyPayloadLoaded accepts a fetch result only if it still matches the
// entry the pane is showing.
func (m *LogsViewModel) applyPayloadLoaded(msg logsPayloadLoadedMsg) {
	if msg.id != m.payloadID {
		return
	}
	m.payloadLoading = false
	m.payload, m.payloadErr = msg.payload, msg.err
	// Rendered once here rather than per frame: a payload runs to
	// hundreds of KiB and View() is called on every message.
	m.payloadLines = renderPayloadLines(m.styles, m.termWidth, m.payloadID, msg.payload)
}

func (m *LogsViewModel) handlePayloadKey(msg tea.KeyPressMsg) tea.Cmd {
	if m.payloadDetail.UpdateKey(msg) {
		return nil
	}
	switch {
	case key.Matches(msg, tui.LogsKeys.Open):
		return m.openPayloadFileCmd()
	case key.Matches(msg, tui.LogsKeys.Copy):
		return m.copyPayloadCmd()
	case key.Matches(msg, tui.LogsKeys.Save):
		return m.savePayloadCmd()
	case key.Matches(msg, tui.ListKeys.Back):
		m.closePayload()
	}
	return nil
}

func (m *LogsViewModel) closePayload() {
	m.mode = m.payloadReturn
	m.payloadDetail.Reset()
}

// notLoadedCmd reports why an action did nothing, so no key reads as dead.
func notLoadedCmd() tea.Cmd {
	return func() tea.Msg {
		return logsPayloadActionMsg{status: "No payload to open, copy or save yet"}
	}
}

func (m *LogsViewModel) copyPayloadCmd() tea.Cmd {
	payload := m.payload
	if payload == nil {
		return notLoadedCmd()
	}
	return func() tea.Msg {
		data, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return logsPayloadActionMsg{err: err}
		}
		if err := clipboard.WriteAll(string(data)); err != nil {
			return logsPayloadActionMsg{err: err}
		}
		return logsPayloadActionMsg{status: fmt.Sprintf("Copied %d bytes to clipboard", len(data))}
	}
}

func (m *LogsViewModel) savePayloadCmd() tea.Cmd {
	payload, id := m.payload, m.payloadID
	if payload == nil {
		return notLoadedCmd()
	}
	return func() tea.Msg {
		path, err := writePayloadFile(payload, id)
		if err != nil {
			return logsPayloadActionMsg{err: err}
		}
		return logsPayloadActionMsg{status: "Saved to " + path}
	}
}

// openPayloadFileCmd saves the payload, then hands it to whichever
// application the desktop has registered for JSON.
func (m *LogsViewModel) openPayloadFileCmd() tea.Cmd {
	payload, id, open := m.payload, m.payloadID, m.openFile
	if payload == nil {
		return notLoadedCmd()
	}
	return func() tea.Msg {
		path, err := writePayloadFile(payload, id)
		if err != nil {
			return logsPayloadActionMsg{err: err}
		}
		if err := open(path); err != nil {
			// The file is on disk either way, so say where it landed.
			return logsPayloadActionMsg{
				status: fmt.Sprintf("Saved to %s but could not open it: %v", path, err),
			}
		}
		return logsPayloadActionMsg{status: "Opened " + path}
	}
}

// writePayloadFile writes the payload into the working directory and
// returns its absolute path.
func writePayloadFile(payload *pkgClient.InferenceLogPayload, id string) (string, error) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format payload: %w", err)
	}
	// filepath.Base keeps a server-supplied ID from escaping the working
	// directory.
	path, err := filepath.Abs("zzrouter-payload-" + filepath.Base(id) + ".json")
	if err != nil {
		return "", fmt.Errorf("resolve payload path: %w", err)
	}
	if err := os.WriteFile(path, data, payloadFileMode); err != nil {
		return "", fmt.Errorf("write payload: %w", err)
	}
	return path, nil
}

func (m *LogsViewModel) viewPayload() string {
	width := m.termWidth
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.payloadLoading,
		LoadTick:  m.loadTick,
		Err:       m.payloadErr,
		Empty:     m.payload == nil,
		EmptyText: "No payload retained for this entry",
		BackHint:  tui.Hint(tui.ListKeys.Back, "back"),
	}); done {
		return out
	}

	footer := shared.RenderViewFooter(m.styles, width, tui.JoinHints(
		tui.Hint(tui.ListKeys.Up, "scroll"),
		tui.Hint(tui.LogsKeys.Open, "open"),
		tui.Hint(tui.LogsKeys.Copy, "copy"),
		tui.Hint(tui.LogsKeys.Save, "save"),
		tui.Hint(tui.ListKeys.Back, "back"),
	))
	result, _ := shared.RenderDetailView(m.styles, m.payloadLines, m.payloadDetail.Scroll(),
		m.termHeight, footer, m.payloadStatus, 0)
	return result
}

// renderPayloadLines lays the payload out once, at load time.
func renderPayloadLines(styles ui.Styles, width int, id string, payload *pkgClient.InferenceLogPayload) []string {
	if payload == nil {
		return nil
	}

	d := shared.NewDetail(styles, width)
	d.Field("Entry", id)
	if payload.Oversize {
		d.Field("Oversize", "body exceeded the retention cap and was not kept")
	}

	writeJSONSection(d, "Request", payload.Request)
	writeJSONSection(d, "Upstream Request", payload.Upstream)
	writeJSONSection(d, "Response", payload.Response)

	if len(payload.Elided) > 0 {
		d.Section("Attached Media")
		for _, blob := range payload.Elided {
			label := blob.Media
			if label == "" {
				label = "unknown type"
			}
			d.Field(blob.Path, fmt.Sprintf("%s, %s", label, humanBytes(int64(blob.Bytes))))
		}
	}

	return strings.Split(strings.TrimRight(d.String(), "\n"), "\n")
}

// writeJSONSection renders a body as an indented section, skipping it when
// the server had nothing to return for that slot.
func writeJSONSection(d *shared.DetailBuilder, title string, body json.RawMessage) {
	if len(body) == 0 {
		return
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		pretty.Reset()
		pretty.Write(body)
	}
	d.Section(title)
	for _, line := range strings.Split(pretty.String(), "\n") {
		d.Text(" ", line)
	}
}
