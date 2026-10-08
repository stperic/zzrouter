package httperr

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/openaicompat"
	"github.com/stperic/zzrouter/pkg/utils"
)

// maxUpstreamErrorBodyBytes bounds upstream errors, including decompressed bodies.
const maxUpstreamErrorBodyBytes = 64 * 1024

// readUpstreamErrorBody reads bounded plaintext or gzip errors. Oversized,
// unreadable or unsupported encodings return nil for status-based normalization.
func readUpstreamErrorBody(resp *http.Response) []byte {
	switch enc := resp.Header.Get("Content-Encoding"); {
	case enc == "", strings.EqualFold(enc, "identity"):
		return readCapped(resp.Body)
	case strings.EqualFold(enc, "gzip"):
		gz, err := gzip.NewReader(io.LimitReader(resp.Body, maxUpstreamErrorBodyBytes))
		if err != nil {
			return nil
		}
		defer func() { _ = gz.Close() }()
		return readCapped(gz)
	default:
		return nil
	}
}

// readCapped reads up to maxUpstreamErrorBodyBytes and returns nil if the
// reader had MORE than that available. Detecting saturation by
// reading cap+1 is cheaper than checking len after a full read and
// avoids the truncated-mid-token JSON pathology.
func readCapped(r io.Reader) []byte {
	b, err := io.ReadAll(io.LimitReader(r, maxUpstreamErrorBodyBytes+1))
	if err != nil || len(b) > maxUpstreamErrorBodyBytes {
		return nil
	}
	return b
}

// NormalizeUpstreamResponse owns bounded error reading, status classification
// and dialect conversion. Call before deciding whether an upstream is retriable.
// It replaces and closes the consumed body and returns the class for open streams.
func NormalizeUpstreamResponse(resp *http.Response, responder Responder) Error {
	raw := readUpstreamErrorBody(resp)
	_ = resp.Body.Close()
	message := upstreamMessage(raw)
	failure := Error{Status: resp.StatusCode, Type: openaicompat.StatusToErrorType(resp.StatusCode), Message: utils.SanitizeErrorMessage(message)}
	if failure.Message == "" {
		failure.Message = fmt.Sprintf("backend returned status %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusInternalServerError {
		if unsupportedImage(message) {
			failure.Code = "unsupported_input"
		} else if reason, ok := templateRefusal(message); ok {
			failure.Code = CodeChatTemplateRejected
			failure.Message = utils.SanitizeErrorMessage("the model's chat template rejected this request: " + reason + " " + ChatTemplateRemedy)
		}
	}
	// A refusal is the request's fault and retrying cannot change it.
	if failure.Code != "" {
		failure.Status, failure.Type = http.StatusBadRequest, "invalid_request_error"
		raw, _ = json.Marshal(map[string]any{"error": map[string]string{"message": failure.Message, "type": failure.Type, "code": failure.Code}})
		resp.Header.Del("Retry-After")
	}
	resp.TransferEncoding = nil
	resp.StatusCode = failure.Status
	resp.Status = fmt.Sprintf("%d %s", failure.Status, http.StatusText(failure.Status))
	if responder != nil {
		raw = responder.NormalizeUpstreamError(failure.Status, raw)
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	resp.ContentLength = int64(len(raw))
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return failure
}

func upstreamMessage(raw []byte) string {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return string(raw)
	}
	var text string
	if json.Unmarshal(envelope.Error, &text) == nil {
		return text
	}
	var inner struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(envelope.Error, &inner) == nil {
		if inner.Message != "" {
			return inner.Message
		}
		return inner.Error
	}
	return envelope.Message
}

// Match explicit capability refusals only. A server failure mentioning images
// (decoding, allocation, transport) is still a server failure.
func unsupportedImage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	for _, phrase := range []string{"image input is not supported", "image input not supported", "does not support image input", "does not support images", "images are not supported", "image inputs are not supported", "model does not support multimodal requests", "is not a multimodal model"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

// CodeChatTemplateRejected: the engine's chat template refused to render the
// request (vLLM answers such refusals 400 itself).
const CodeChatTemplateRejected = "chat_template_rejected"

// ChatTemplateRemedy is what a caller can do about a template refusal.
const ChatTemplateRemedy = "An admin can upload a chat template that accepts it and select it for this model; GET /zzrouter/v1 explains how under chat_templates."

// llama.cpp prefixes a template's own raise_exception with this marker:
// https://github.com/ggml-org/llama.cpp/blob/c811cb8f0ac91b8ac72a32f970bdd45037f20da7/common/jinja/value.cpp#L418
const llamacppTemplateRefusal = "Jinja Exception: "

func templateRefusal(message string) (string, bool) {
	_, reason, ok := strings.Cut(message, llamacppTemplateRefusal)
	return strings.TrimSpace(reason), ok
}
