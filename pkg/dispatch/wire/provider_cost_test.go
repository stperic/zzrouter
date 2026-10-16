package wire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/observability/llm"
)

func TestProviderCostPresence(t *testing.T) {
	for _, tc := range []struct {
		name, field, extension string
		cost                   float64
		present                bool
	}{
		{"positive", `,"cost":0.000001989`, "", 0.000001989, true},
		{"BYOK", `,"cost":0,"cost_details":{"upstream_inference_cost":0.0021}`, "", 0.0021, true},
		{"account plus upstream", `,"cost":0.001,"cost_details":{"upstream_inference_cost":0.0021}`, "", 0.0031, true},
		{"null upstream", `,"cost":0,"cost_details":{"upstream_inference_cost":null}`, "", 0, true},
		{"invalid details", `,"cost":0,"cost_details":"invalid"`, "", 0, true},
		{"invalid upstream", `,"cost":0,"cost_details":{"upstream_inference_cost":"0.0021"}`, "", 0, true},
		{"negative upstream", `,"cost":0,"cost_details":{"upstream_inference_cost":-0.0021}`, "", 0, true},
		{"zero", `,"cost":0`, `,"x_gateway":{"cost":9}`, 0, true},
		{"missing", "", "", 0, false},
		{"null", `,"cost":null`, "", 0, false},
		{"string", `,"cost":"1"`, "", 0, false},
		{"object", `,"cost":{}`, "", 0, false},
		{"boolean", `,"cost":false`, "", 0, false},
		{"overflow", `,"cost":1e999`, "", 0, false},
		{"negative credit", `,"cost":-0.01`, "", 0, false},
		{"legacy", "", `,"x_gateway":{"cost":0.01}`, 0.01, true},
		{"legacy zero", "", `,"x_gateway":{"cost":0}`, 0, false},
		{"legacy negative", "", `,"x_gateway":{"cost":-1}`, 0, false},
		{"legacy invalid", "", `,"x_gateway":{"cost":"1"}`, 0, false},
		{"null uses legacy", `,"cost":null`, `,"x_gateway":{"cost":0.03}`, 0.03, true},
		{"invalid uses legacy", `,"cost":[]`, `,"x_gateway":{"cost":0.03}`, 0.03, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := ParseUsageExtended([]byte(`{"usage":{"prompt_tokens":19,"completion_tokens":3` + tc.field + `}` + tc.extension + `}`))
			assert.EqualValues(t, 19, u.In)
			assert.EqualValues(t, 3, u.Out)
			assert.Equal(t, tc.cost, u.Cost)
			assert.Equal(t, tc.present, u.CostSource == CostSourceProvider)
		})
	}
	t.Run("cost-only reaches recorder", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprint(stream), func(t *testing.T) {
				rec := llm.NewInferenceRecorder(t.Context(), "m", "p")
				body := `{"choices":[],"usage":{"cost":0}}`
				w := httptest.NewRecorder()
				if stream {
					CopyStream(t.Context(), w, strings.NewReader("data: "+body+"\n\ndata: [DONE]\n\n"), rec, nil)
				} else {
					resp := buildJSONResp(200, body, nil)
					defer resp.Body.Close()
					RelayBuffered(w, resp, nil, rec)
				}
				assert.Equal(t, "provider", rec.Snapshot().CostSource)
				assert.Zero(t, rec.Snapshot().Cost)
			})
		}
	})
}

func TestStreamCostMergePresence(t *testing.T) {
	first := UsageData{In: 19, Out: 3, CachedTokens: 2, ReasoningTokens: 1, Cost: 0.01, CostSource: CostSourceProvider}
	for _, tc := range []struct {
		name string
		body string
		want float64
	}{
		{"missing", `{"usage":{"completion_tokens":2}}`, 0.01},
		{"null", `{"usage":{"cost":null}}`, 0.01},
		{"invalid", `{"usage":{"cost":"0"}}`, 0.01},
		{"zero", `{"usage":{"cost":0}}`, 0},
		{"lower", `{"usage":{"cost":0.001}}`, 0.001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := first.merge(ParseUsageExtended([]byte(tc.body)))
			assert.Equal(t, tc.want, u.Cost)
			assert.Equal(t, first.In, u.In)
			assert.Equal(t, first.Out, u.Out)
			assert.Equal(t, first.CachedTokens, u.CachedTokens)
			assert.Equal(t, first.ReasoningTokens, u.ReasoningTokens)
			assert.Equal(t, CostSourceProvider, u.CostSource)
			rec := llm.NewInferenceRecorder(t.Context(), "m", "p")
			stream := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":19,\"completion_tokens\":3,\"cost\":0.01}}\n\ndata: " + tc.body + "\n\ndata: [DONE]\n\n"
			CopyStream(t.Context(), httptest.NewRecorder(), strings.NewReader(stream), rec, nil)
			assert.Equal(t, tc.want, rec.Snapshot().Cost)
			assert.Equal(t, "provider", rec.Snapshot().CostSource)
		})
	}
}

func TestProviderCostExtensionOrder(t *testing.T) {
	for _, body := range []string{
		`{"usage":{},"x_zulu":{"cost":0.02},"x_alpha":{"cost":0.01}}`,
		`{"x_alpha":{"cost":0.01},"usage":{},"x_zulu":{"cost":0.02}}`,
		`{"usage":{},"x_aardvark":{"cost":0},"x_zulu":{"cost":0.02},"x_alpha":{"cost":0.01}}`,
	} {
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(body), &raw))
		for range 100 {
			cost := extractProviderCost(raw)
			require.NotNil(t, cost)
			require.Equal(t, 0.01, *cost, "the first positive extension in sorted key order wins")
			assert.Equal(t, 0.01, ParseUsageExtended([]byte(body)).Cost)
		}
	}
}

func TestOpenRouterRecordedShapeStream(t *testing.T) {
	// Synthetic stream framing around the captured 2026-04-26 usage shape.
	const prefix = ": OPENROUTER PROCESSING\r\n\r\n" +
		"data: {\"id\":\"gen-1777174469-LxK6JdI3MdRTIzIjfS7e\", \"object\":\"chat.completion.chunk\",\"model\":\"openai/gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\r\n\r\n" +
		": OPENROUTER PROCESSING\r\n\r\n" +
		"data: {\"id\":\"gen-1777174469-LxK6JdI3MdRTIzIjfS7e\", \"object\":\"chat.completion.chunk\",\"model\":\"openai/gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello!\"},\"finish_reason\":null}]}\r\n\r\n"
	const final = `{"choices":[],"id":"gen-1777174469-LxK6JdI3MdRTIzIjfS7e","model":"openai/gpt-4o","object":"chat.completion.chunk","usage":{"completion_tokens":3,"prompt_tokens":19,"prompt_tokens_details":{"cached_tokens":0},"total_tokens":22}}`
	const suffix = " \t\r\n\r\ndata: [DONE]\r\n\r\n"
	for _, strip := range []bool{false, true} {
		t.Run(fmt.Sprint(strip), func(t *testing.T) {
			payload := final
			if strip {
				payload = strings.Replace(payload, `"completion_tokens":3`, `"completion_tokens":3,"cost":0,"cost_details":{"upstream_inference_cost":0.0021},"is_byok":true`, 1)
				payload = strings.Replace(payload, `"cached_tokens":0`, `"cache_write_tokens":0,"cached_tokens":0`, 1)
			}
			input := prefix + "data: " + payload + suffix
			// Split inside the final usage object, including a key when costs are present.
			split := len(prefix) + len("data: ") + strings.Index(payload, `"completion_tokens"`) + len(`"completion_tokens":3,"cost_de`)
			reader := io.MultiReader(strings.NewReader(input[:split]), strings.NewReader(input[split:]))
			norms := normalizer.NewRegistry()
			norms.CleanChatUsage = func() bool { return true }
			rec := llm.NewInferenceRecorder(t.Context(), "gpt-4o", "openrouter")
			w := httptest.NewRecorder()
			result := CopyStream(t.Context(), w, reader, rec, NewStreamTransform(norms.Stream("openrouter"), RoutingMetadata{}, rec).Func())
			require.Equal(t, Completed, result.Outcome)
			assert.Equal(t, prefix+"data: "+final+suffix, w.Body.String())
			assert.EqualValues(t, 3, rec.Snapshot().TokensOut)
			if strip {
				assert.Equal(t, 0.0021, rec.Snapshot().Cost)
				assert.Equal(t, "provider", rec.Snapshot().CostSource)
			} else {
				assert.Equal(t, input, w.Body.String())
				assert.Empty(t, rec.Snapshot().CostSource)
			}
		})
	}
}

func TestNormalizationFramingSplitReads(t *testing.T) {
	input := " : keep\r\n\t data: {\"choices\":[],\"usage\":{\"cost\":0,\"prompt_tokens\":5}} \t\r\n\r\ndata: [DONE]\r\n\r\n"
	want := " : keep\r\n\t data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5}} \t\r\n\r\ndata: [DONE]\r\n\r\n"
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			var r io.Reader = strings.NewReader(input)
			if split {
				r = iotest.OneByteReader(r)
			}
			norms := normalizer.NewRegistry()
			norms.CleanChatUsage = func() bool { return true }
			rec := llm.NewInferenceRecorder(t.Context(), "m", "p")
			w := httptest.NewRecorder()
			result := CopyStream(t.Context(), w, r, rec, NewStreamTransform(norms.Stream("p"), RoutingMetadata{}, rec).Func())
			require.Equal(t, Completed, result.Outcome)
			assert.Equal(t, want, w.Body.String())
			assert.Equal(t, "provider", rec.Snapshot().CostSource)
		})
	}
}
