package calibration

import (
	"crypto/rand"
	"fmt"
	"maps"
	"math"
	"strings"
	"sync/atomic"
)

const (
	// Chat is the endpoint for the chat-v1 workload.
	Chat = "chat"
	// Embeddings is the endpoint for the embeddings-v1 workload.
	Embeddings            = "embeddings"
	shortWarmupCharacters = 256
	maxPromptCharacters   = 1 << 20
	promptFiller          = " Quartz granite basalt marble shale slate sandstone limestone."
)

// ValidateEndpoint checks token fields that depend on the checked run's endpoint.
func (w Workload) ValidateEndpoint(endpoint string) error {
	switch endpoint {
	case Chat:
		if w.OutputTokens == nil {
			return &FieldError{"output_tokens", "Chat workloads require output tokens."}
		}
	case Embeddings:
		if w.OutputTokens != nil {
			return &FieldError{"output_tokens", "Embeddings workloads omit output tokens."}
		}
	}
	return nil
}

// WorkloadRevision returns the generator version, or an empty string for unsupported endpoints.
func WorkloadRevision(endpoint string) string {
	switch endpoint {
	case Chat:
		return "chat-v1"
	case Embeddings:
		return "embeddings-v1"
	default:
		return ""
	}
}

type generator struct {
	nonce string
	seq   atomic.Uint64
}

func newGenerator() *generator { return &generator{nonce: rand.Text()} }

func (g *generator) prompt(n int) string {
	// Put the changing part first so the shared filler cannot become a cached prefix.
	prefix := fmt.Sprintf("%x %s ", g.seq.Add(1), g.nonce)
	filler := max(0, n-len(prefix))
	return prefix + strings.Repeat(promptFiller, (filler+len(promptFiller)-1)/len(promptFiller))[:filler]
}

func (g *generator) body(target Target, characters, outputTokens int) map[string]any {
	prompt := g.prompt(characters)
	if target.Run.Endpoint == Embeddings {
		return map[string]any{"model": target.Run.Model, "input": prompt}
	}
	body := make(map[string]any, len(target.Output)+4)
	maps.Copy(body, target.Output)
	body["model"] = target.Run.Model
	body["stream"] = true
	body["messages"] = []map[string]string{{"role": "user", "content": prompt}}
	body["max_tokens"] = outputTokens
	return body
}

func longWarmupSize(inputTokens int, short Sample) int {
	if short.InputTokens == nil || *short.InputTokens <= 0 {
		return 8 * shortWarmupCharacters
	}
	characters := math.Ceil(float64(inputTokens) * shortWarmupCharacters / float64(*short.InputTokens))
	if characters <= shortWarmupCharacters {
		return 2 * shortWarmupCharacters
	}
	return int(min(maxPromptCharacters, characters))
}

func (w Workload) promptSize(contextTokens *int, short, long Sample, longCharacters int) (int, *Reason) {
	switch {
	case short.ErrorCode != "":
		return 0, requestFailure(short.ErrorCode)
	case long.ErrorCode != "":
		return 0, requestFailure(long.ErrorCode)
	case short.InputTokens == nil || long.InputTokens == nil:
		return 0, reason(usageUnavailable)
	case *short.InputTokens < 0 || *long.InputTokens <= *short.InputTokens:
		return 0, reason(usageUnavailable)
	}
	tokenDelta := float64(*long.InputTokens - *short.InputTokens)
	characterDelta := float64(longCharacters - shortWarmupCharacters)
	charactersForTokens := func(tokens float64) float64 {
		return shortWarmupCharacters + (tokens-float64(*short.InputTokens))*characterDelta/tokenDelta
	}
	input := float64(w.InputTokens)
	// Leave room for tokenization to differ from the fitted line.
	characters := math.Ceil(charactersForTokens(input + max(4, math.Ceil(input/500))))
	if contextTokens != nil {
		inputLimit := *contextTokens
		if w.OutputTokens != nil {
			inputLimit -= *w.OutputTokens
		}
		characters = min(characters, math.Floor(charactersForTokens(float64(inputLimit))))
	}
	if characters > maxPromptCharacters {
		failure := reason(requestsFailed)
		failure.Message = "The warm-up fit sizes the prompt above the generator's 1 MiB limit."
		return 0, failure
	}
	return int(max(0, characters)), nil
}
