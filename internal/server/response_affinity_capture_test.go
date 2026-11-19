package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResponseJSONIDAcrossArbitraryChunks(t *testing.T) {
	large := strings.Repeat(`escaped \" { ] , : `, 5000)
	output, err := json.Marshal(large)
	require.NoError(t, err)
	for _, tc := range []struct{ name, body, id string }{
		{"after-large-output", `{"output":[{"id":"nested-decoy","text":` + string(output) + `}],"id":"resp_real"}`, "resp_real"},
		{"escaped-key-and-id", `{"\u0069\u0064":"resp_\"quoted\\path\u0061","output":` + string(output) + `}`, "resp_\"quoted\\patha"},
		{"nested-only", `{"output":[{"id":"decoy","child":{"id":"another"}}],"object":"response"}`, ""},
		{"long-decoy-key", `{"` + strings.Repeat("i", 2000) + `d":"decoy","id":"resp_real"}`, "resp_real"},
		{"non-string-id", `{"id":{"id":"nested"},"object":"response"}`, ""},
	} {
		for _, chunk := range []int{1, 7, 1024, len(tc.body)} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, chunk), func(t *testing.T) {
				var scan responseJSONIDScanner
				for offset := 0; offset < len(tc.body); offset += chunk {
					end := min(offset+chunk, len(tc.body))
					scan.write([]byte(tc.body[offset:end]), defaultResponseBodyCaptureCap)
				}
				require.Equal(t, tc.id, scan.id)
				require.LessOrEqual(t, cap(scan.token), defaultResponseBodyCaptureCap)
			})
		}
	}
}

func TestResponseIDWriterRecordsBeforeStreamFinishes(t *testing.T) {
	raw := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(raw)
	w := newResponseIDCapturingWriter(c.Writer)
	w.Header().Set("Content-Type", "text/event-stream")
	var recorded string
	w.onID = func(id string) { recorded = id }
	_, err := w.WriteString("event: response.created\ndata: {\"response\":{\"id\":\"resp_live\"}}\n\n")
	require.NoError(t, err)
	require.Equal(t, "resp_live", recorded, "the caller may cancel or retrieve while creation is still streaming")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte(strings.Repeat("x", defaultResponseBodyCaptureCap+1)))
	require.NoError(t, err)
	require.Equal(t, "resp_live", w.ExtractedID())
}
