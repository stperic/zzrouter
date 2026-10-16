package httperr_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/httperr"
	anthropic "github.com/stperic/zzrouter/pkg/protocol/anthropic"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeclaredEngineFailures(t *testing.T) {
	for _, tc := range []struct {
		provider, body, code string
		status               int
	}{
		{"mlx", `{"error": "Only 'text' content type is supported."}`, httperr.CodeUnsupportedInput, 404},
		{"vllm", `{"error":{"message":"Qwen/Qwen2.5-0.5B-Instruct is not a multimodal model","type":"BadRequestError","code":400}}`, httperr.CodeUnsupportedInput, 400},
		{"llamacpp", `image input is not supported - hint: if this is unexpected, you may need to provide the mmproj`, httperr.CodeUnsupportedInput, 500},
		{"vllm", `OutOfMemoryError: CUDA out of memory. Tried to allocate 24.50 GiB. GPU 0 has a total capacity of 94.97 GiB of which 17.18 GiB is free.`, string(httperr.CodeOutOfRange), 500},
		{"vllm", `RuntimeError: No available memory for the cache blocks`, string(httperr.CodeOutOfRange), 400},
		{"llamacpp", `cudaMalloc failed: out of memory`, string(httperr.CodeOutOfRange), 500},
		{"llamacpp", `failed to allocate CUDA0 buffer`, string(httperr.CodeOutOfRange), 500},
		{"mlx", `RuntimeError: [malloc] Unable to allocate 1024 bytes.`, string(httperr.CodeOutOfRange), 500},
		{"mlx", `RuntimeError: [metal::malloc] Resource limit (100) exceeded.`, string(httperr.CodeOutOfRange), 500},
		{"ollama", `{"error":"model requires more system memory (391.0 GiB) than is available (366.2 GiB)"}`, string(httperr.CodeOutOfRange), 500},
	} {
		t.Run(tc.provider+tc.body, func(t *testing.T) {
			classifier := testClassifier(t, tc.provider)
			for _, status := range []int{tc.status, http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
				resp := &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
				failure := httperr.NormalizeUpstreamResponse(resp, openaiproto.New(), classifier, func() bool { return true })
				assert.Equal(t, tc.code, failure.Code)
				wantStatus := http.StatusBadRequest
				if tc.code == string(httperr.CodeOutOfRange) {
					wantStatus = http.StatusConflict
				}
				assert.Equal(t, wantStatus, failure.Status)
				assert.Empty(t, resp.Header.Get("Retry-After"))
				out, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				relay := &http.Response{StatusCode: resp.StatusCode, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(out)))}
				assert.Equal(t, failure, httperr.NormalizeUpstreamResponse(relay, openaiproto.New(), nil, func() bool { return true }))
			}
		})
	}
}

func TestClassifierRejectsInvalidDeclarations(t *testing.T) {
	for _, pattern := range []httperr.FailurePattern{
		{Pattern: "failure", Class: "unknown"},
		{Pattern: "failure"},
		{Class: httperr.FailureMemory},
		{Pattern: "[", IsRegex: true, Class: httperr.FailureMemory},
	} {
		classifier := httperr.Classifier{pattern}
		assert.Error(t, classifier.Compile())
	}
	for _, provider := range []string{"llamacpp", "vllm", "mlx", "ollama"} {
		classifier := testClassifier(t, provider)
		assert.Nil(t, classifier.Match("image allocation failed: out of memory"))
		assert.Nil(t, classifier.Match("Failed to decode multimodal data"))
		assert.Nil(t, classifier.Match("launching with --enable-auto-tool-choice"))
	}
}

func TestDeclaredFailureRelayDialects(t *testing.T) {
	for _, class := range []httperr.FailureClass{httperr.FailureMemory, httperr.FailureUnsupportedInput, httperr.FailureChatTemplateRejected, httperr.FailureToolCallingNotEnabled} {
		classifier := httperr.Classifier{{Pattern: "engine failure", Class: class}}
		require.NoError(t, classifier.Compile())
		resp := &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"engine failure"}`))}
		got := httperr.NormalizeUpstreamResponse(resp, anthropic.New(nil), &classifier, func() bool { return true })
		out, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		relay := &http.Response{StatusCode: resp.StatusCode, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(out)))}
		assert.Equal(t, got, httperr.NormalizeUpstreamResponse(relay, anthropic.New(nil), nil, func() bool { return true }))
	}
}
