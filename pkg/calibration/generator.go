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
	longWarmupCharacters  = 8 * shortWarmupCharacters
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
	delete(body, "seed")
	body["model"] = target.Run.Model
	body["stream"] = true
	body["messages"] = []map[string]string{{"role": "user", "content": prompt}}
	body["max_tokens"] = outputTokens
	return body
}

func promptSize(inputTokens int, short, long Sample) (int, *Reason) {
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
	ratio := float64(*long.InputTokens-*short.InputTokens) / (longWarmupCharacters - shortWarmupCharacters)
	overhead := float64(*short.InputTokens) - ratio*shortWarmupCharacters
	characters := math.Ceil((float64(inputTokens) - overhead) / ratio)
	if characters > maxPromptCharacters {
		failure := reason(requestsFailed)
		failure.Message = "The warm-up fit sizes the prompt above the generator's 1 MiB limit."
		return 0, failure
	}
	return int(max(0, characters)), nil
}
