package pair

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/pkg/discovery/network"
)

// fixedClock returns a deterministic clock anchored at t.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// drainCmd runs a tea.Cmd synchronously and returns the resulting Msg
// (or nil). Used to simulate the tea runtime in unit tests without
// spinning up a tea.Program.
func drainCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// TestInit_FlagsSkipDiscovery pins that supplying both flags jumps
// straight to the confirm state without attempting mDNS.
func TestInit_FlagsSkipDiscovery(t *testing.T) {
	m := New(Config{
		CoordURL:      "https://coord:9091",
		CAFingerprint: "sha256:abc",
		LocalPort:     9090,
		Discover: func(context.Context) (*network.NodeEntry, error) {
			t.Fatal("discover must not be called when flags are set")
			return nil, nil
		},
	})
	cmd := m.Init()
	if cmd != nil {
		t.Fatalf("expected no Init cmd when flags supplied, got %T", cmd)
	}
	if m.state != stateConfirmCoordinator {
		t.Fatalf("state = %v, want stateConfirmCoordinator", m.state)
	}
	if m.coordURL != "https://coord:9091" || m.caFingerprint != "sha256:abc" {
		t.Fatalf("coord fields not set from flags")
	}
}

// TestInit_NoMDNSSkipsDiscovery pins that NoMDNS + no flags jumps to
// manual-entry without calling mDNS. The focus cmd is non-nil (it
// kicks the textinput's cursor blink) but no discovery runs.
func TestInit_NoMDNSSkipsDiscovery(t *testing.T) {
	m := New(Config{
		NoMDNS: true,
		Discover: func(context.Context) (*network.NodeEntry, error) {
			t.Fatal("discover must not be called when NoMDNS is set")
			return nil, nil
		},
	})
	_ = m.Init()
	if m.state != stateManualEntry {
		t.Fatalf("state = %v, want stateManualEntry", m.state)
	}
	if !m.manualReady {
		t.Fatal("manual entry inputs should be initialized")
	}
}

// TestManualEntry_SubmitAdvancesToConfirm pins that submitting
// populated URL + fingerprint routes through confirmation (not
// directly to pair) so the operator still verifies before sending.
func TestManualEntry_SubmitAdvancesToConfirm(t *testing.T) {
	m := New(Config{NoMDNS: true})
	m.Init()
	m.manualURL.SetValue("https://coord:9091")
	m.manualFP.SetValue("sha256:abc123")

	newModel, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	got := newModel.(*Model)
	if got.state != stateConfirmCoordinator {
		t.Fatalf("state = %v, want stateConfirmCoordinator", got.state)
	}
	if got.coordURL != "https://coord:9091" {
		t.Errorf("coordURL = %q", got.coordURL)
	}
	if got.caFingerprint != "sha256:abc123" {
		t.Errorf("caFingerprint = %q", got.caFingerprint)
	}
}

// TestManualEntry_EmptyFieldsShowError pins that submitting with an
// empty URL keeps the form visible with an error message.
func TestManualEntry_EmptyFieldsShowError(t *testing.T) {
	m := New(Config{NoMDNS: true})
	m.Init()

	newModel, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	got := newModel.(*Model)
	if got.state != stateManualEntry {
		t.Fatalf("state = %v, want stateManualEntry (stayed on form)", got.state)
	}
	if got.manualErrMsg == "" {
		t.Error("manualErrMsg should be set")
	}
}

// TestDiscovered_TransitionsToConfirm pins that a usable NodeEntry
// populates the model + advances to confirm.
func TestDiscovered_TransitionsToConfirm(t *testing.T) {
	m := New(Config{})
	m.state = stateDiscovering
	entry := &network.NodeEntry{
		Name:          "coord-1",
		Node:          "coord.local",
		ClusterPort:   9091,
		IsCoordinator: true,
	}
	newModel, _ := m.Update(discoveredMsg{entry: entry})
	got := newModel.(*Model)
	if got.state != stateConfirmCoordinator {
		t.Fatalf("state = %v, want stateConfirmCoordinator", got.state)
	}
	if got.coordURL != "https://coord.local:9091" {
		t.Fatalf("coordURL = %q", got.coordURL)
	}
	if got.coordName != "coord-1" {
		t.Fatalf("coordName = %q", got.coordName)
	}
}

// TestDiscovered_EmptyFallsToManual pins no-coord-found → manual.
func TestDiscovered_EmptyFallsToManual(t *testing.T) {
	m := New(Config{})
	m.state = stateDiscovering
	newModel, _ := m.Update(discoveredMsg{entry: nil})
	if newModel.(*Model).state != stateManualEntry {
		t.Fatalf("empty discovery must route to stateManualEntry")
	}
}

// TestConfirm_YAdvancesToRequesting pins the confirmation gate.
func TestConfirm_YAdvancesToRequesting(t *testing.T) {
	m := New(Config{
		PairPost: func(ctx context.Context, req PairRequest) (PairResponse, error) {
			if req.CoordinatorURL == "" || req.CoordinatorCAFingerprint == "" {
				t.Fatal("confirm must forward coord fields")
			}
			return PairResponse{Status: "new", Code: "ABCDEFGHIJKLMNOP", Deadline: time.Now().Add(15 * time.Minute)}, nil
		},
	})
	m.state = stateConfirmCoordinator
	m.coordURL = "https://coord:9091"
	m.caFingerprint = "sha256:abc"

	newModel, cmd := m.Update(tea.KeyPressMsg(tea.Key{Text: "y"}))
	got := newModel.(*Model)
	if got.state != stateRequesting {
		t.Fatalf("state = %v, want stateRequesting", got.state)
	}
	if cmd == nil {
		t.Fatal("expected pair POST cmd")
	}
}

// TestPairResp_NewEntersPairing pins that status="new" opens the
// pairing window + starts polling.
func TestPairResp_NewEntersPairing(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	m := New(Config{Clock: fixedClock(now)})
	m.state = stateRequesting
	m.reqGen = 1

	resp := PairResponse{Status: "new", Code: "CODE1234CODE5678", Deadline: now.Add(15 * time.Minute)}
	newModel, cmd := m.Update(pairRespMsg{gen: 1, resp: resp})
	got := newModel.(*Model)
	if got.state != statePairing {
		t.Fatalf("state = %v, want statePairing", got.state)
	}
	if !got.seenActive {
		t.Fatal("seenActive must be true after new/existing")
	}
	if got.code != "CODE1234CODE5678" {
		t.Fatalf("code = %q", got.code)
	}
	if cmd == nil {
		t.Fatal("expected tick cmd after entering pairing state")
	}
}

// TestPairResp_StaleGenDropped pins that a late response from a
// prior generation (e.g. a poll in flight when the operator pressed
// `r` to regenerate) does not clobber the fresh window.
func TestPairResp_StaleGenDropped(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	m := New(Config{Clock: fixedClock(now)})
	m.state = statePairing
	m.reqGen = 2
	m.code = "FRESH"
	m.deadline = now.Add(10 * time.Minute)

	// Stale response from gen=1 tries to set code to STALE.
	stale := PairResponse{Status: "existing", Code: "STALE", Deadline: now.Add(1 * time.Minute)}
	newModel, _ := m.Update(pairRespMsg{gen: 1, resp: stale})
	got := newModel.(*Model)
	if got.code != "FRESH" {
		t.Errorf("stale response overwrote code: got %q, want FRESH", got.code)
	}
	if got.state != statePairing {
		t.Errorf("stale response altered state: %v", got.state)
	}
}

// TestPairResp_NoActiveWithWorkerModeIsSuccess pins success detection
// on mode: the coord consumed the window and the node's lifecycle
// flipped to worker.
func TestPairResp_NoActiveWithWorkerModeIsSuccess(t *testing.T) {
	m := New(Config{})
	m.state = statePairing
	m.seenActive = true
	m.code = "C"
	m.coordURL = "url"
	m.caFingerprint = "fp"

	newModel, _ := m.Update(pairRespMsg{resp: PairResponse{Status: "no_active_window", Mode: "worker"}})
	got := newModel.(*Model)
	if got.state != stateSuccess {
		t.Fatalf("state = %v, want stateSuccess", got.state)
	}
	if !got.result.Success || got.result.Code != "C" {
		t.Fatalf("result = %+v", got.result)
	}
}

// TestPairResp_NoActiveWithUnclaimedModeAfterSeenActiveIsExpired pins
// the expiry path: we saw the window exist but it's now gone without a
// mode flip — TTL elapsed (or coord cancelled), not success.
func TestPairResp_NoActiveWithUnclaimedModeAfterSeenActiveIsExpired(t *testing.T) {
	m := New(Config{})
	m.state = statePairing
	m.seenActive = true
	newModel, _ := m.Update(pairRespMsg{resp: PairResponse{Status: "no_active_window", Mode: "unclaimed"}})
	if newModel.(*Model).state != stateExpired {
		t.Fatalf("state = %v, want stateExpired", newModel.(*Model).state)
	}
}

// TestPairResp_NoActiveBeforeSeenActiveIsFailure pins the inverse:
// no_active_window without prior existing → failure.
func TestPairResp_NoActiveBeforeSeenActiveIsFailure(t *testing.T) {
	m := New(Config{})
	m.state = stateRequesting
	newModel, _ := m.Update(pairRespMsg{resp: PairResponse{Status: "no_active_window"}})
	if newModel.(*Model).state != stateFailed {
		t.Fatal("no_active_window without seenActive must fail")
	}
}

// TestFlashMsg_MutationStaysOnUpdateGoroutine pins that flash text
// flows through a tea.Msg rather than being written from the cmd
// goroutine. This is the data-race-free contract the cold review
// required.
func TestFlashMsg_MutationStaysOnUpdateGoroutine(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	m := New(Config{Clock: fixedClock(now)})
	newModel, _ := m.Update(flashMsg{text: "hello"})
	got := newModel.(*Model)
	if got.flash != "hello" {
		t.Fatalf("flash = %q, want 'hello'", got.flash)
	}
	if !got.flashUntil.After(now) {
		t.Fatalf("flashUntil not advanced past clock: %v vs %v", got.flashUntil, now)
	}
}

// TestTick_ExpiresWhenPastDeadline pins TTL countdown derived from
// deadline - Clock().
func TestTick_ExpiresWhenPastDeadline(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	m := New(Config{Clock: fixedClock(now)})
	m.state = statePairing
	m.deadline = now.Add(-1 * time.Second) // already past
	newModel, _ := m.Update(tickMsg(now))
	if newModel.(*Model).state != stateExpired {
		t.Fatal("tick past deadline must expire")
	}
}

// TestCopyCode_FlashesSuccess pins the clipboard path + flash state.
// The cmd must return flashMsg (consumed by Update), not mutate
// m.flash directly — keeps model state single-threaded.
func TestCopyCode_FlashesSuccess(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	var copied string
	m := New(Config{
		Clock:     fixedClock(now),
		Clipboard: func(s string) error { copied = s; return nil },
	})
	m.state = statePairing
	m.code = "CODE"

	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Text: "c"}))
	msg := drainCmd(cmd)
	if copied != "CODE" {
		t.Fatalf("clipboard got %q, want %q", copied, "CODE")
	}
	f, ok := msg.(flashMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want flashMsg", msg)
	}
	if !strings.Contains(f.text, "copied") {
		t.Fatalf("flash text = %q, want 'copied' substring", f.text)
	}
	// Apply the msg and verify Update wrote m.flash on the main goroutine.
	_, _ = m.Update(f)
	if !strings.Contains(m.flash, "copied") {
		t.Fatalf("post-Update flash = %q", m.flash)
	}
}

// TestCopyCode_ClipboardErrorFlashesError pins the SSH-no-clipboard path.
func TestCopyCode_ClipboardErrorFlashesError(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	m := New(Config{
		Clock:     fixedClock(now),
		Clipboard: func(string) error { return errors.New("no display") },
	})
	m.state = statePairing
	m.code = "CODE"

	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Text: "c"}))
	msg := drainCmd(cmd)
	f, ok := msg.(flashMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want flashMsg", msg)
	}
	if !strings.Contains(f.text, "unavailable") {
		t.Fatalf("flash text = %q, want 'unavailable' substring", f.text)
	}
}

// TestContract_PairRequestJSONShape pins the wire format of
// PairRequest. The server-side type is internal/server.clusterPairRequest
// (see internal/server/cluster_pair_handler.go). A mirror test at
// internal/server/cluster_pair_handler_contract_test.go pins the
// same JSON shape on the server side — both tests must encode the
// same expected keys. If the server struct changes, this TUI-side
// test fails; if this TUI struct changes, the server-side test
// fails. That catches drift from either direction without forcing
// one package to import the other.
func TestContract_PairRequestJSONShape(t *testing.T) {
	typ := reflect.TypeOf(PairRequest{})
	want := map[string]string{
		"Regenerate":               "regenerate",
		"Cancel":                   "cancel",
		"CoordinatorURL":           "coordinator_url,omitempty",
		"CoordinatorCAFingerprint": "coordinator_ca_fingerprint,omitempty",
	}
	for fname, tag := range want {
		f, ok := typ.FieldByName(fname)
		if !ok {
			t.Errorf("PairRequest missing field %s", fname)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("PairRequest.%s json tag = %q, want %q", fname, got, tag)
		}
	}
	// Also verify a round-trip produces the expected JSON keys.
	out, err := json.Marshal(PairRequest{
		Regenerate: true, CoordinatorURL: "u", CoordinatorCAFingerprint: "f",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	for _, k := range []string{`"regenerate":true`, `"coordinator_url":"u"`, `"coordinator_ca_fingerprint":"f"`} {
		if !strings.Contains(s, k) {
			t.Errorf("marshal output missing %q; got %s", k, s)
		}
	}
}

// TestContract_PairResponseJSONShape mirrors the request contract on
// the response side.
func TestContract_PairResponseJSONShape(t *testing.T) {
	typ := reflect.TypeOf(PairResponse{})
	want := map[string]string{
		"Status":   "status",
		"Code":     "code,omitempty",
		"Deadline": "deadline,omitempty",
	}
	for fname, tag := range want {
		f, ok := typ.FieldByName(fname)
		if !ok {
			t.Errorf("PairResponse missing field %s", fname)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("PairResponse.%s json tag = %q, want %q", fname, got, tag)
		}
	}
}
