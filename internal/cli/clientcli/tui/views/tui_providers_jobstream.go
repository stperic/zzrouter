package views

import (
	"encoding/json"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/jobstream"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// Install wizard jobstream helpers. Tiny, self-contained utilities used
// by the SSE-driven Run All path (see tui_providers.go §jobstream.FrameMsg
// and tui_providers_install.go §startInstallJobCmd).

// reflectInstallStepProgress translates the 1-based current step number
// (as emitted by the server's InstallProgress.SetStep) into the wizard's
// per-step status row: steps strictly before curr become done, the step
// at curr becomes running, and later steps stay pending. Preserves
// verified rows (from Verify Step) so we don't overwrite a manually
// verified step with "done".
func (m *ProvidersViewModel) reflectInstallStepProgress(curr int) {
	for i := range m.installStepStatus {
		switch {
		case i+1 < curr:
			if m.installStepStatus[i] != stepDone && m.installStepStatus[i] != stepVerified {
				m.installStepStatus[i] = stepDone
			}
		case i+1 == curr:
			if m.installStepStatus[i] != stepDone && m.installStepStatus[i] != stepVerified {
				m.installStepStatus[i] = stepRunning
			}
		}
	}
}

// stepResultFromFrame re-decodes the StepResult the server stashed on
// the terminal FrameMsg's Meta under the "result" key (see
// InstallCoordinator.ExecuteStepAsync). Returns nil on a missing key,
// decode failure, or an error-terminal frame — callers should treat
// nil as "no server-side StepResult available" and fall back to the
// error path.
func stepResultFromFrame(msg jobstream.FrameMsg) *pkgClient.StepResultResponse {
	if msg.Err != nil || msg.Event.Meta == nil {
		return nil
	}
	raw, ok := msg.Event.Meta["result"]
	if !ok {
		return nil
	}
	// The Meta payload round-trips through JSON, so the value is a
	// map[string]any. Re-marshal + unmarshal into the client-side
	// struct; this tolerates the server adding fields without breaking
	// older clients.
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var result pkgClient.StepResultResponse
	if err := json.Unmarshal(b, &result); err != nil {
		return nil
	}
	return &result
}

// intFromMeta extracts a non-negative int from a jobs.Meta payload.
// Returns 0 when the key is absent, not numeric, or negative. Meta is
// decoded via json.Unmarshal so ints arrive as float64 on the wire;
// this tolerates either encoding.
func intFromMeta(meta map[string]any, key string) int {
	if meta == nil {
		return 0
	}
	switch v := meta[key].(type) {
	case int:
		if v < 0 {
			return 0
		}
		return v
	case int64:
		if v < 0 {
			return 0
		}
		return int(v)
	case float64:
		if v < 0 {
			return 0
		}
		return int(v)
	}
	return 0
}
