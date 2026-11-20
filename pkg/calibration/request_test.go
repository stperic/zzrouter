package calibration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func chatWorkload(concurrency ...int) *Workload {
	return &Workload{InputTokens: 10, OutputTokens: new(2), Concurrency: concurrency}
}

func testRequest(workloads ...*Workload) Request {
	r := Request{Node: "gpu-1"}
	for i, workload := range workloads {
		r.Runs = append(r.Runs, RunRequest{RunID: []string{"a", "b", "resident", "extra"}[i], Workload: workload})
	}
	return r
}

func TestValidateRequest(t *testing.T) {
	tests := []struct {
		name, field string
		change      func(*Request)
	}{
		{"node required", "node", func(r *Request) { r.Node = "  " }},
		{"runs required", "runs", func(r *Request) { r.Runs = nil }},
		{"too many runs", "runs", func(r *Request) { r.Runs = testRequest(chatWorkload(1), nil, nil, nil).Runs }},
		{"empty run ID", "runs[0].run_id", func(r *Request) { r.Runs[0].RunID = "" }},
		{"duplicate run ID", "runs[1].run_id", func(r *Request) { r.Runs[1].RunID = r.Runs[0].RunID }},
		{"resident only", "runs", func(r *Request) { r.Runs[0].Workload = nil }},
		{"all idle", "runs", func(r *Request) { r.Runs[0].Workload.Concurrency = []int{1, 0} }},
		{"different step count", "runs[1].workload.concurrency", func(r *Request) { r.Runs[1].Workload = chatWorkload(1, 1) }},
		{"zero steps", "runs[0].workload.concurrency", func(r *Request) { r.Runs[0].Workload.Concurrency = nil }},
		{"too many steps", "runs[0].workload.concurrency", func(r *Request) { r.Runs[0].Workload.Concurrency = make([]int, MaxSteps+1) }},
		{"negative concurrency", "runs[0].workload.concurrency[0]", func(r *Request) { r.Runs[0].Workload.Concurrency = []int{-1} }},
		{"concurrency over bound", "runs[0].workload.concurrency[0]", func(r *Request) { r.Runs[0].Workload.Concurrency = []int{257} }},
		{"zero input", "runs[0].workload.input_tokens", func(r *Request) { r.Runs[0].Workload.InputTokens = 0 }},
		{"zero output", "runs[0].workload.output_tokens", func(r *Request) { r.Runs[0].Workload.OutputTokens = new(0) }},
		{"zero request count", "runs[0].workload.requests_per_step", func(r *Request) { r.Runs[0].Workload.RequestsPerStep = new(0) }},
		{"request count below concurrency", "runs[0].workload.requests_per_step", func(r *Request) { r.Runs[0].Workload.RequestsPerStep = new(3) }},
		{"request count above bound", "runs[0].workload.requests_per_step", func(r *Request) { r.Runs[0].Workload.RequestsPerStep = new(1025) }},
		{"zero timeout", "step_timeout_seconds", func(r *Request) { r.StepTimeoutSeconds = new(0) }},
		{"timeout over limit", "step_timeout_seconds", func(r *Request) { r.StepTimeoutSeconds = new(1801) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testRequest(chatWorkload(4), nil)
			tt.change(&r)
			var field *FieldError
			require.ErrorAs(t, r.Validate(), &field)
			require.Equal(t, tt.field, field.Field)
		})
	}
}

func TestRequestLimitsAndDefaults(t *testing.T) {
	w := chatWorkload(1, 3, 4)
	require.Equal(t, 10, w.requestCount(1))
	require.Equal(t, 10, w.requestCount(3))
	require.Equal(t, 12, w.requestCount(4))
	w.RequestsPerStep = new(4)
	require.Equal(t, 4, w.requestCount(4))
	r := testRequest(w, chatWorkload(0, 1, 0), nil)
	require.NoError(t, r.Validate())
	require.Equal(t, 600*time.Second, r.stepTimeout())
	w.Concurrency = []int{256, 1, 1, 1, 1, 1, 1, 1}
	w.RequestsPerStep = nil
	r.Runs[1].Workload = nil
	r.StepTimeoutSeconds = new(1800)
	require.NoError(t, r.Validate())
	require.Equal(t, 768, w.requestCount(256))
	w.RequestsPerStep = new(1024)
	require.NoError(t, r.Validate())
	require.Equal(t, 1800*time.Second, r.stepTimeout())
}

func TestConcurrencyHasItsOwnLimit(t *testing.T) {
	for _, count := range []*int{nil, new(1025)} {
		w := chatWorkload(1, 257)
		w.RequestsPerStep = count
		var field *FieldError
		require.ErrorAs(t, testRequest(w).Validate(), &field)
		require.Equal(t, "runs[0].workload.concurrency[1]", field.Field)
		require.Contains(t, field.Message, "256")
	}
}

func TestEndpointTokenFields(t *testing.T) {
	w := chatWorkload(1)
	require.NoError(t, w.ValidateEndpoint(Chat))
	require.Error(t, w.ValidateEndpoint(Embeddings))
	w.OutputTokens = nil
	require.Error(t, w.ValidateEndpoint(Chat))
	require.NoError(t, w.ValidateEndpoint(Embeddings))
	require.Equal(t, "chat-v1", WorkloadRevision(Chat))
	require.Equal(t, "embeddings-v1", WorkloadRevision(Embeddings))
	require.Empty(t, WorkloadRevision("reranking"))
}

func TestGenerator(t *testing.T) {
	g := newGenerator()
	chat := Target{Run: RunReport{Endpoint: Chat, Model: "model"}, Output: map[string]any{
		"ignore_eos": true, "max_tokens": 999, "seed": 12, "stream": false, "model": "wrong",
	}}
	seen := map[string]bool{}
	for range 12 {
		body := g.body(chat, 512, 4)
		require.Equal(t, "model", body["model"])
		require.Equal(t, true, body["stream"])
		require.Equal(t, true, body["ignore_eos"])
		require.Equal(t, 4, body["max_tokens"])
		require.Equal(t, 12, body["seed"])
		messages, ok := body["messages"].([]map[string]string)
		require.True(t, ok)
		require.Len(t, messages, 1)
		require.Equal(t, "user", messages[0]["role"])
		prompt := messages[0]["content"]
		require.Len(t, prompt, 512)
		require.False(t, seen[prompt[:10]])
		seen[prompt[:10]] = true
	}
	require.Equal(t, 12, chat.Output["seed"])
	require.Equal(t, 999, chat.Output["max_tokens"])

	chat.Output = nil
	unforced := g.body(chat, 512, 3)
	require.Equal(t, 3, unforced["max_tokens"])
	require.NotContains(t, unforced, "ignore_eos")
	embeddings := Target{Run: RunReport{Endpoint: Embeddings, Model: "embed"}, Output: map[string]any{"seed": 4}}
	body := g.body(embeddings, 80, 0)
	input, ok := body["input"].(string)
	require.True(t, ok)
	require.Len(t, input, 80)
	require.Equal(t, map[string]any{"model": "embed", "input": input}, body)
}

func TestPromptSizing(t *testing.T) {
	const longCharacters = 2048
	promptSize := func(inputTokens int, short, long Sample) (int, *Reason) {
		return (Workload{InputTokens: inputTokens}).promptSize(nil, short, long, longCharacters)
	}
	short := Sample{InputTokens: new(90)}
	long := Sample{InputTokens: new(538)}
	size, failure := promptSize(97, short, long)
	require.Nil(t, failure)
	require.Equal(t, 300, size)
	size, failure = promptSize(20, short, long)
	require.Nil(t, failure)
	require.Zero(t, size, "the template alone already exceeds the target")

	size, failure = promptSize(18, Sample{InputTokens: new(20)}, Sample{InputTokens: new(27)})
	require.Nil(t, failure)
	require.Equal(t, 768, size)
	size, failure = promptSize(18, Sample{InputTokens: new(20)}, Sample{InputTokens: new(25)})
	require.Nil(t, failure)
	require.Equal(t, 973, size, "fractional character counts round up")

	size, failure = promptSize(1, Sample{InputTokens: new(0)}, Sample{InputTokens: new(224)})
	require.Nil(t, failure)
	require.Equal(t, 296, size, "zero reported tokens are a measured value")

	for _, sample := range []Sample{{}, {InputTokens: new(-1)}} {
		_, failure = promptSize(10, sample, long)
		require.Equal(t, "usage_unavailable", failure.Code)
	}
	_, failure = promptSize(1<<30, short, long)
	require.Equal(t, "requests_failed", failure.Code)
	require.Contains(t, failure.Message, "1 MiB")
	_, failure = promptSize(10, short, Sample{ErrorCode: "upstream_timeout"})
	require.Equal(t, "requests_failed", failure.Code)
	require.Contains(t, failure.Message, "upstream_timeout")
}

func TestPromptTokenMargin(t *testing.T) {
	for _, tt := range []struct {
		name           string
		workload       Workload
		context        *int
		tokensPerChar  int
		wantCharacters int
	}{
		{"four token minimum", Workload{InputTokens: 100}, nil, 1, 104},
		{"percentage rounded up", Workload{InputTokens: 30_001, OutputTokens: new(256)}, nil, 1, 30_062},
		{"partial margin at context limit", Workload{InputTokens: 30_001, OutputTokens: new(256)}, new(30_260), 1, 30_004},
		{"no margin at context limit", Workload{InputTokens: 30_001, OutputTokens: new(256)}, new(30_257), 1, 30_001},
		{"embeddings use full context", Workload{InputTokens: 100}, new(101), 1, 101},
		{"character rounding stays within context", Workload{InputTokens: 100, OutputTokens: new(2)}, new(105), 2, 51},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const longCharacters = 512
			short := Sample{InputTokens: new(256 * tt.tokensPerChar)}
			long := Sample{InputTokens: new(longCharacters * tt.tokensPerChar)}
			characters, failure := tt.workload.promptSize(tt.context, short, long, longCharacters)
			require.Nil(t, failure)
			require.Equal(t, tt.wantCharacters, characters)
		})
	}
}

func TestWarmupSizesEmbeddings(t *testing.T) {
	request := testRequest(&Workload{InputTokens: 100, Concurrency: []int{1}, RequestsPerStep: new(1)})
	runner, _, targets := runnerFixture(Embeddings)
	inputs := []string{}
	runner.Sender = senderFunc(func(_ context.Context, _ Clock, id, revision string, body map[string]any) Sample {
		require.Equal(t, "a", id)
		require.Equal(t, "a-original", revision)
		input, ok := body["input"].(string)
		require.True(t, ok)
		inputs = append(inputs, input)
		return Sample{InputTokens: new(8 + len(input)/8)}
	})
	report, err := runner.Run(t.Context(), request, targets)
	require.NoError(t, err)
	require.Equal(t, verified, report.Outcome)
	require.Len(t, inputs, 3)
	require.Len(t, inputs[0], 256)
	require.Len(t, inputs[1], 640)
	require.Len(t, inputs[2], 768)
	require.Equal(t, &TokenRange{104, 104}, report.Steps[0].Runs[0].InputTokens)
	require.Equal(t, "embeddings-v1", *report.Runs[0].WorkloadRevision)
}
