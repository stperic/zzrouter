package wire

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetDefaultFields(t *testing.T) {
	body := []byte(`{"model":"m","temperature":0.2,"messages":[]}`)
	out := SetDefaultFields(body, map[string]any{
		"temperature":          0.9,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	})
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 0.2, got["temperature"], "a field the client sent wins")
	assert.Equal(t, map[string]any{"enable_thinking": false}, got["chat_template_kwargs"])
	assert.Equal(t, "m", got["model"])

	// An explicit null is something the client sent.
	out = SetDefaultFields([]byte(`{"temperature":null}`), map[string]any{"temperature": 0.9})
	assert.JSONEq(t, `{"temperature":null}`, string(out))

	// A field sent in another case is sent: a case-insensitive engine
	// would read the two as one.
	out = SetDefaultFields([]byte(`{"Temperature":0.2}`), map[string]any{"temperature": 0.9})
	assert.JSONEq(t, `{"Temperature":0.2}`, string(out))

	assert.Equal(t, body, SetDefaultFields(body, nil))
	assert.Equal(t, []byte(`[1]`), SetDefaultFields([]byte(`[1]`), map[string]any{"a": 1}), "not an object")
	assert.Equal(t, []byte(`not json`), SetDefaultFields([]byte(`not json`), map[string]any{"a": 1}))
}
