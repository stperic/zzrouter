package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestClusterPairRequestContract pins the JSON shape of
// clusterPairRequest. A mirror test lives at
// internal/cli/servercli/tui/pair/pair_test.go:TestContract_PairRequestJSONShape
// which pins the TUI-side copy of this struct. Both tests must
// encode the same expected JSON keys — a drift in one surfaces as
// a failure in the other (the TUI test fails because its tags no
// longer match this handler; this test fails because the handler
// changed and the TUI-side copy is now out of sync).
//
// The TUI package cannot reflect on this unexported type directly,
// so the contract is pinned by mirrored golden-JSON expectations
// rather than by reflect.Type equality.
func TestClusterPairRequestContract(t *testing.T) {
	typ := reflect.TypeOf(clusterPairRequest{})
	want := map[string]string{
		"Regenerate":               "regenerate",
		"Cancel":                   "cancel",
		"CoordinatorURL":           "coordinator_url,omitempty",
		"CoordinatorCAFingerprint": "coordinator_ca_fingerprint,omitempty",
	}
	for fname, tag := range want {
		f, ok := typ.FieldByName(fname)
		if !ok {
			t.Errorf("clusterPairRequest missing field %s", fname)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("clusterPairRequest.%s json tag = %q, want %q", fname, got, tag)
		}
	}
	out, err := json.Marshal(clusterPairRequest{
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

// TestClusterPairResponseContract mirrors the request contract on the
// response side. See TestClusterPairRequestContract for the drift
// pairing rationale.
func TestClusterPairResponseContract(t *testing.T) {
	typ := reflect.TypeOf(clusterPairResponse{})
	want := map[string]string{
		"Status":   "status",
		"Code":     "code,omitempty",
		"Deadline": "deadline,omitempty",
	}
	for fname, tag := range want {
		f, ok := typ.FieldByName(fname)
		if !ok {
			t.Errorf("clusterPairResponse missing field %s", fname)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("clusterPairResponse.%s json tag = %q, want %q", fname, got, tag)
		}
	}
}
