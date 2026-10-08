package anthropic

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatToolChoiceSkipsInvalidFunctionShape(t *testing.T) {
	tools := []map[string]any{{"function": nil}, {"function": "bad"}, {"function": map[string]any{"name": "wanted"}}}
	require.Equal(t, map[string]any{"type": "function", "function": map[string]any{"name": "wanted"}}, chatToolChoice(toolChoice{Type: "tool", Name: "wanted"}, tools))
	require.Nil(t, chatToolChoice(toolChoice{Type: "tool", Name: "missing"}, tools))
}
