package openai

// Envelope is the top-level OpenAI error response shape:
//
//	{
//	  "error": {
//	    "message": "...",
//	    "type":    "...",
//	    "code":    "...",
//	    "param":   "..."
//	  }
//	}
//
// It is the single JSON shape that every /v1/* error response MUST
// conform to. openai-python, openai-node, and every third-party SDK
// that speaks OpenAI classify errors by reading Error.Type and
// Error.Code — anything that deviates silently falls through to a
// generic APIError and breaks retry logic.
//
// The envelope is intentionally tiny. If a future OpenAI revision
// adds a sibling field (for example `event_id` on streaming errors,
// which Bifrost tracks), extend ErrorBody with an `omitempty` JSON
// tag rather than introducing a second envelope type.
type Envelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the nested object documented at
// https://platform.openai.com/docs/guides/error-codes.
//
// Field semantics, verbatim from the OpenAI reference:
//
//   - Message: human-readable description of the error. Safe to show
//     to end users. MUST be sanitized on the server side before
//     emitting — see sanitize.go.
//   - Type: closed-vocabulary error family. SDKs switch on this
//     string to construct typed exception classes. MUST be a known
//     ErrorType value; see vocab.go.
//   - Code: closed-vocabulary machine-readable sub-code. SDKs use
//     this to distinguish retryable from fatal failures within a
//     single type. MUST be a known ErrorCode value; see vocab.go.
//   - Param: name of the offending request parameter when the error
//     is attributable to a specific field (e.g. "messages[2].role").
//     Omitted from the wire format when empty.
//   - ProviderSpecificFields: optional bag of backend-specific context
//     (e.g. Anthropic's request_id, Vertex quota details). Omitted when
//     empty to preserve strict byte-compatibility with vanilla OpenAI
//     responses, and populated only when the backend surfaces details
//     worth forwarding to the caller.
//
// All fields except Message and ProviderSpecificFields are represented as
// string-backed typed constants at construction time and serialized as plain
// strings on the wire. This keeps the wire format exactly byte-compatible
// with OpenAI while preserving compile-time safety at the Go level.
type ErrorBody struct {
	Message                string         `json:"message"`
	Type                   string         `json:"type"`
	Code                   string         `json:"code,omitempty"`
	Param                  string         `json:"param,omitempty"`
	ProviderSpecificFields map[string]any `json:"provider_specific_fields,omitempty"`
}

// NewEnvelope constructs an Envelope from typed dialect constants.
// It is the single constructor; callers MUST NOT instantiate Envelope
// struct literals directly so the package can enforce vocabulary
// invariants in one place.
//
// The `message` argument is accepted as-is — callers are expected to
// have already sanitized it via sanitize(), which is the contract
// Responder relies on. Passing an unsanitized string here is a bug.
func NewEnvelope(errType ErrorType, code ErrorCode, message, param string) Envelope {
	return Envelope{
		Error: ErrorBody{
			Message: message,
			Type:    string(errType),
			Code:    string(code),
			Param:   param,
		},
	}
}
