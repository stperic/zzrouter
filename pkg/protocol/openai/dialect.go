package openai

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/openaicompat"
	"github.com/stperic/zzrouter/pkg/utils"
)

// The writer-level half of httperr.Responder: errors and stream frames
// written by code that holds no gin context.

// WriteError renders e as {"error":{...}} beside any e.Extra members.
func (r *Responder) WriteError(w http.ResponseWriter, _ *http.Request, e httperr.Error) {
	httperr.SetRetryAfter(w.Header(), e.RetryAfter)
	if r.opaque {
		w.WriteHeader(e.Status)
		return
	}
	body := map[string]any{}
	for k, v := range e.Extra {
		body[k] = v
	}
	body["error"] = errorObject(e)
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(data)
}

func errorObject(e httperr.Error) utils.OpenAIErrorDetail {
	var code *string
	if e.Code != "" {
		code = &e.Code
	}
	return utils.NewOpenAIError(e.Type, e.Message, nil, code).Error
}

func (*Responder) NormalizeUpstreamError(status int, raw []byte) []byte {
	return openaicompat.NormalizeError(status, raw)
}

// The OpenAI surface streams failures in band: OpenWebUI and similar chat
// UIs stall on a bare status mid-request but render an error chunk inside
// the stream, and show progress while a cold model loads.
var _ httperr.InBandStreamer = (*Responder)(nil)

// StreamStatus writes a status_update event, which the vLLM-style UIs
// render as progress.
func (*Responder) StreamStatus(w http.ResponseWriter, status, message string) {
	msg, _ := json.Marshal(message)
	st, _ := json.Marshal(status)
	_, _ = fmt.Fprintf(w, "event: status_update\ndata: {\"status\": %s, \"message\": %s}\n\n", st, msg)
	flush(w)
}

// StreamError preserves the pre-backend OpenWebUI terminator.
func (r *Responder) StreamError(w http.ResponseWriter, e httperr.Error) {
	r.EndStream(w, e)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flush(w)
}

// EndStream separates an upstream fragment from one vLLM-format error chunk.
func (r *Responder) EndStream(w http.ResponseWriter, e httperr.Error) {
	if !r.opaque {
		var code any
		if e.Code != "" {
			code = e.Code
		}
		data, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": e.Message, "type": e.Type, "param": nil, "code": code,
		}})
		_, _ = fmt.Fprintf(w, "\ndata: %s\n\n", data)
	}
	flush(w)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
