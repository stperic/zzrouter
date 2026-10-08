package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNewEnvelope_WireShape pins the JSON wire format. Any change to
// the envelope's on-the-wire layout is a BREAKING change that will
// silently misclassify errors in every OpenAI SDK — this test
// exists to force such a change to be explicit.
func TestNewEnvelope_WireShape(t *testing.T) {
	env := NewEnvelope(ErrorTypeInvalidRequest, ErrorCodeInvalidRequest, "bad body", "messages")
	got, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"error":{"message":"bad body","type":"invalid_request_error","code":"invalid_request_error","param":"messages"}}`
	if string(got) != want {
		t.Errorf("envelope shape drift\n got=%s\nwant=%s", got, want)
	}
}

// TestNewEnvelope_OmitEmpty verifies `code` and `param` are omitted
// when empty. The OpenAI SDK tolerates absent fields but some
// strict third-party clients reject null-valued code, so the
// omission matters for compatibility.
func TestNewEnvelope_OmitEmpty(t *testing.T) {
	env := NewEnvelope(ErrorTypeServer, "", "internal server error", "")
	got, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(got)
	if strings.Contains(s, `"code"`) {
		t.Errorf("empty code should be omitted: %s", s)
	}
	if strings.Contains(s, `"param"`) {
		t.Errorf("empty param should be omitted: %s", s)
	}
	if !strings.Contains(s, `"type":"server_error"`) {
		t.Errorf("missing type: %s", s)
	}
}
