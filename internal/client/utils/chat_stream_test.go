package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChatCompletionsStream_EndsOnlyWhenTheReplySaysSo(t *testing.T) {
	const delta = `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"
	for name, tc := range map[string]struct {
		body string
		want error
	}{
		"done":                     {delta + "data: [DONE]\n\n", nil},
		"finish reason, no [DONE]": {`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n", nil},
		"cut":                      {delta, errStreamCut},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.body))
			}))
			var got string
			err := c.ChatCompletionsStream(context.Background(), ChatRequest{Model: "m"}, func(chunk StreamChunk) error {
				got += chunk.Content
				return nil
			})
			assert.ErrorIs(t, err, tc.want)
			assert.Equal(t, "hi", got)
		})
	}
}
