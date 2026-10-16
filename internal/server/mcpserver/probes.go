package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/stperic/zzrouter/pkg/httperr"
)

type probeKind string
type probeVerdict string

const (
	probeChat         probeKind    = "chat"
	probeTools        probeKind    = "tools"
	probeEmbed        probeKind    = "embed"
	probeVision       probeKind    = "vision"
	probeVerified     probeVerdict = "verified"
	probeNotSupported probeVerdict = "not_supported"
	probeInconclusive probeVerdict = "inconclusive"
	visionQuestion                 = "What is the solid color in this image? Answer with one color word."
	visionRed                      = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAIAAACQkWg2AAAAF0lEQVR4nGP4z8BAEiJN9aiGUQ1DSgMAkPn/Afnh+ngAAAAASUVORK5CYII="
	visionBlue                     = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAIAAACQkWg2AAAAFUlEQVR4nGNgYPhPIhrVMKph2GoAAJLb/wFh5Z4RAAAAAElFTkSuQmCC"
)

func (d dispatcher) chat(ctx context.Context, in chatInput) (out agentOutput, err error) {
	if in.Probe == "" {
		in.Probe = probeChat
	}
	defer func() {
		var failure *toolError
		if errors.As(err, &failure) && (failure.problem.Code == httperr.CodeUnsupportedInput || failure.problem.Code == httperr.CodeToolCallingNotEnabled) {
			failure.probe = &probeOutput{Probe: in.Probe, Verdict: probeNotSupported, NextSteps: failure.problem.Detail}
		}
	}()
	if in.Node != "" {
		in.Model += "@" + in.Node
	}
	body := map[string]any{"model": in.Model, "stream": false}
	checks := 1
	switch in.Probe {
	case probeEmbed:
		return d.embeddingProbe(ctx, in.Model)
	case probeChat:
		if in.Message == "" {
			return out, newToolError(http.StatusBadRequest, codeInvalidInput, "message is required for plain chat", nil)
		}
		if in.MaxTokens == 0 {
			in.MaxTokens = DefaultChatTokens
		}
		if in.MaxTokens < 1 {
			return out, newToolError(http.StatusBadRequest, codeInvalidInput, "max_tokens must be positive", nil)
		}
		body["max_tokens"] = min(in.MaxTokens, MaxChatTokens)
	case probeTools:
		in.Message = probeMessage
		body["max_tokens"] = ToolProbeTokens
		// Automatic choice exercises the engine's parser rather than guided decoding.
		body["tool_choice"] = "auto"
		body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{
			"name": probeFunction, "description": "Look up the current weather in a city.",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"city"}},
		}}}
	case probeVision:
		checks, body["max_tokens"] = 2, MaxChatTokens
	default:
		return out, newToolError(http.StatusBadRequest, codeInvalidInput, "probe must be chat, tools, embed or vision", nil)
	}
	out = agentOutput{fields: map[string]any{"probe": in.Probe}, text: "Reply verified for this request; use zz_logs to diagnose later failures."}
	verified := true
	usage := tokenUsage{}
	for i := range checks {
		var content any = in.Message
		expected := "red"
		if in.Probe == probeVision {
			image := visionRed
			if i == 1 {
				image, expected = visionBlue, "blue"
			}
			content = []any{map[string]any{"type": "text", "text": visionQuestion}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": image}}}
		}
		body["messages"] = []map[string]any{{"role": "user", "content": content}}
		var reply struct {
			Model   string       `json:"model"`
			Choices []chatChoice `json:"choices"`
			Usage   tokenUsage   `json:"usage"`
		}
		if err := d.call(ctx, request{method: http.MethodPost, path: chatRoute.path(), jsonBody: body, contentType: "application/json"}, &reply); err != nil {
			return agentOutput{}, err
		}
		if len(reply.Choices) == 0 {
			return agentOutput{}, newToolError(http.StatusBadGateway, codeResponseDecode, "chat response has no choices; inspect zz_logs", nil)
		}
		choice := reply.Choices[0]
		out.fields["reply"], out.fields["model"] = choice.Message.Content, reply.Model
		usage.PromptTokens += reply.Usage.PromptTokens
		usage.CompletionTokens += reply.Usage.CompletionTokens
		usage.TotalTokens += reply.Usage.TotalTokens
		answered := strings.TrimSpace(choice.Message.Content) != "" && choice.FinishReason != "length"
		switch in.Probe {
		case probeTools:
			outcome, text := choice.probeResult()
			out.fields["tool_call"], out.text = outcome, text
			answered = outcome == toolCallVerified
		case probeVision:
			answered = answered && strings.EqualFold(strings.TrimSpace(choice.Message.Content), expected)
		}
		verified = verified && answered
	}
	out.fields["usage"] = usage
	out.fields["verdict"] = probeInconclusive
	if verified {
		out.fields["verdict"] = probeVerified
	} else if in.Probe != probeTools {
		out.text = "Probe inconclusive for this request; inspect zz_logs and the model's serving configuration, then probe again."
	}
	out.fields["next_steps"] = out.text
	return out, nil
}

func (d dispatcher) embeddingProbe(ctx context.Context, model string) (agentOutput, error) {
	var reply struct {
		Model string `json:"model"`
		Data  []struct {
			Embedding []any `json:"embedding"`
		} `json:"data"`
		Usage tokenUsage `json:"usage"`
	}
	body := map[string]any{"model": model, "input": "The sky is clear today.", "encoding_format": "float"}
	if err := d.call(ctx, request{method: http.MethodPost, path: routePrefix("/v1/embeddings").path(), jsonBody: body, contentType: "application/json"}, &reply); err != nil {
		return agentOutput{}, err
	}
	if len(reply.Data) != 1 || len(reply.Data[0].Embedding) == 0 {
		return agentOutput{}, newToolError(http.StatusBadGateway, codeResponseDecode, "embedding probe requires one nonempty numeric vector", nil)
	}
	for _, value := range reply.Data[0].Embedding {
		n, numeric := value.(json.Number)
		if _, err := n.Float64(); !numeric || err != nil {
			return agentOutput{}, newToolError(http.StatusBadGateway, codeResponseDecode, "embedding probe requires finite values", nil)
		}
	}
	text := "Embedding verified for this request; use this model for embedding requests."
	return agentOutput{fields: map[string]any{"probe": probeEmbed, "verdict": probeVerified, "next_steps": text, "model": reply.Model, "dimensions": len(reply.Data[0].Embedding), "usage": reply.Usage}, text: text}, nil
}

type toolCallOutcome string

const (
	toolCallVerified      toolCallOutcome = "verified"
	toolCallParserMissing toolCallOutcome = "parser_missing"
	toolCallTruncated     toolCallOutcome = "truncated"
	toolCallInconclusive  toolCallOutcome = "inconclusive"
)

type chatChoice struct {
	FinishReason string `json:"finish_reason"`
	Message      struct {
		Content   string `json:"content"`
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
}

func (choice chatChoice) probeResult() (toolCallOutcome, string) {
	if choice.FinishReason == "length" {
		return toolCallTruncated, "Tool probe truncated at its token budget; inspect zz_logs and the model publisher's reasoning settings, then probe again."
	}
	for _, call := range choice.Message.ToolCalls {
		if call.Function.Name != probeFunction {
			continue
		}
		var arguments string
		var args map[string]json.RawMessage
		var city string
		// The OpenAI wire carries arguments as a JSON string; an object breaks SDK clients, so it does not verify.
		if json.Unmarshal(call.Function.Arguments, &arguments) == nil && json.Unmarshal([]byte(arguments), &args) == nil && json.Unmarshal(args["city"], &city) == nil && strings.TrimSpace(city) != "" {
			return toolCallVerified, "Tool calling verified by a structured probe call; use this model for tool requests."
		}
	}
	if len(choice.Message.ToolCalls) == 0 && strings.Contains(choice.Message.Content, probeFunction) {
		return toolCallParserMissing, "Tool probe appeared as text: set the provider's tool-calling parameters for this model with zz_params_set using the publisher's serving instructions, restart the run with zz_run_restart, then probe again."
	}
	return toolCallInconclusive, "Tool probe inconclusive; probe again or read the model publisher's model card for tool-calling support and serving instructions."
}
