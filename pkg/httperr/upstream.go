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
// hasImages is evaluated only for matched unsupported-input phrases; nil means false.
func NormalizeUpstreamResponse(resp *http.Response, responder Responder, classifier *Classifier, hasImages func() bool) Error {
	raw := readUpstreamErrorBody(resp)
	_ = resp.Body.Close()
	message, code := upstreamError(raw)
	failure := Error{Status: resp.StatusCode, Type: openaicompat.StatusToErrorType(resp.StatusCode), Message: utils.SanitizeErrorMessage(message)}
	if failure.Message == "" {
		failure.Message = fmt.Sprintf("backend returned status %d", resp.StatusCode)
	}
	if class, ok := classForCode(code); ok {
		failure.Code = code
		failure.Status = failureClasses[class].status
		failure.Type = "invalid_request_error"
	} else if cause := classifier.match(message, func(class FailureClass) bool {
		return class != FailureUnsupportedInput || (hasImages != nil && hasImages())
	}); cause != nil {
		failure = cause.HTTPError()
	}
	if failure.Code != "" {
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

// upstreamError returns an error body's message and, when it carries one as
// a string, its code.
func upstreamError(raw []byte) (message, code string) {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Code    any             `json:"code"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return string(raw), ""
	}
	var text string
	if json.Unmarshal(envelope.Error, &text) == nil {
		code, _ := envelope.Code.(string)
		return text, code
	}
	var inner struct {
		Message string `json:"message"`
		Error   string `json:"error"`
		Code    any    `json:"code"`
	}
	if json.Unmarshal(envelope.Error, &inner) == nil {
		code, _ = inner.Code.(string)
		if inner.Message != "" {
			return inner.Message, code
		}
		return inner.Error, code
	}
	return envelope.Message, ""
}
