package normalizer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeMLXBody_CopiesReasoningIntoContent(t *testing.T) {
	input := []byte(`{"choices":[{"message":{"role":"assistant","reasoning":"Thinking...","content":""}}]}`)
	out := NormalizeMLXBody(input)

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices := obj["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Thinking..." {
		t.Errorf("content not copied: got %v", msg["content"])
	}
	if msg["reasoning"] != "Thinking..." {
		t.Errorf("reasoning should be preserved: got %v", msg["reasoning"])
	}
}

func TestNormalizeMLXBody_PreservesExistingContent(t *testing.T) {
	input := []byte(`{"choices":[{"message":{"reasoning":"r","content":"already here"}}]}`)
	out := NormalizeMLXBody(input)

	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msg := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "already here" {
		t.Errorf("existing content was overwritten: %v", msg["content"])
	}
}

func TestNormalizeMLXBody_NoReasoningIsNoOp(t *testing.T) {
	input := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	out := NormalizeMLXBody(input)
	if string(out) != string(input) {
		t.Errorf("expected no-op; got %s", out)
	}
}

func TestNormalizeMLXStream_CopiesDeltaReasoning(t *testing.T) {
	chunk := []byte(`data: {"choices":[{"delta":{"reasoning":"abc"}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n")
	out := NormalizeMLXStream(chunk)
	if !strings.Contains(string(out), `"content":"abc"`) {
		t.Errorf("content not injected: %s", out)
	}
	if !strings.Contains(string(out), `"reasoning":"abc"`) {
		t.Errorf("reasoning should be preserved: %s", out)
	}
	if !strings.Contains(string(out), "[DONE]") {
		t.Errorf("[DONE] marker lost: %s", out)
	}
}

func TestNormalizeMLXStream_NoReasoningIsNoOp(t *testing.T) {
	chunk := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	out := NormalizeMLXStream(chunk)
	if string(out) != string(chunk) {
		t.Errorf("expected no-op; got %s", out)
	}
}

func TestNormalizeMLXStream_MultipleLinesInChunk(t *testing.T) {
	chunk := []byte(
		`data: {"choices":[{"delta":{"reasoning":"one"}}]}` + "\n" +
			`data: {"choices":[{"delta":{"reasoning":"two"}}]}` + "\n\n",
	)
	out := NormalizeMLXStream(chunk)
	if strings.Count(string(out), `"content":"one"`) != 1 {
		t.Errorf("first line not normalized: %s", out)
	}
	if strings.Count(string(out), `"content":"two"`) != 1 {
		t.Errorf("second line not normalized: %s", out)
	}
}

func TestRegisterMLX(t *testing.T) {
	r := NewRegistry()
	RegisterMLX(r)

	if r.Body("mlx") == nil {
		t.Error("mlx body normalizer not registered")
	}
	if r.Stream("mlx") == nil {
		t.Error("mlx stream normalizer not registered")
	}
}
