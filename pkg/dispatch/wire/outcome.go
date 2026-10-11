package wire

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// Outcome is how a relay ended.
type Outcome int

const (
	Unknown        Outcome = iota // not finished yet: a zero Result is never a success
	Completed                     // the whole body or terminal stream frame arrived
	Truncated                     // incomplete body or stream, with the read error in Err
	UpstreamFailed                // the upstream ended the stream itself with an in-band error
	ClientGone                    // the client stopped reading
)

// Result is a relay's outcome and the error behind it, if any.
type Result struct {
	Outcome Outcome
	Err     error
	MidLine bool // the stream stopped inside a line, so an ending must start a new one
}

// Failure reports the error the caller must end the stream with. Only Truncated asks for one:
// UpstreamFailed already ended in the upstream's own protocol, and ClientGone has nobody to tell.
func (r Result) Failure() (httperr.Error, bool) {
	if r.Outcome != Truncated {
		return httperr.Error{}, false
	}
	return httperr.Error{
		Status:  http.StatusBadGateway,
		Type:    "api_error",
		Code:    string(httperr.CodeUpstreamStreamInterrupted),
		Message: "The upstream stream ended before completing.",
	}, true
}

// terminal is the only judge of whether a whole-line frame ends its stream or reports a failure.
func terminal(frame []byte) (done, failed bool) {
	frame = bytes.TrimSpace(frame)
	if event, ok := bytes.CutPrefix(frame, []byte("event:")); ok {
		switch string(bytes.TrimSpace(event)) {
		case "message_stop", "response.completed", "response.incomplete":
			return true, false
		case "error", "response.failed":
			return false, true
		}
		return false, false
	}
	if data, ok := bytes.CutPrefix(frame, []byte("data:")); ok {
		frame = bytes.TrimSpace(data)
		if bytes.Equal(frame, []byte("[DONE]")) {
			return true, false
		}
	}
	if !bytes.Contains(frame, []byte(`"finish_reason"`)) &&
		!bytes.Contains(frame, []byte(`"done"`)) &&
		!bytes.Contains(frame, []byte(`"type"`)) &&
		!bytes.Contains(frame, []byte(`"error"`)) &&
		!bytes.Contains(frame, []byte(`"status"`)) {
		return false, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(frame, &object) != nil {
		return false, false
	}
	if e, ok := object["error"]; ok && !bytes.Equal(e, []byte("null")) {
		return false, true
	}
	var kind string
	_ = json.Unmarshal(object["type"], &kind)
	switch kind {
	case "response.failed":
		return false, true
	case "message_stop", "response.completed", "response.incomplete":
		return true, false
	}
	if bytes.Equal(object["done"], []byte("true")) || bytes.Equal(object["status"], []byte(`"success"`)) {
		return true, false
	}
	var choices []struct {
		FinishReason json.RawMessage `json:"finish_reason"`
	}
	if json.Unmarshal(object["choices"], &choices) == nil {
		for _, choice := range choices {
			if len(choice.FinishReason) > 0 && !bytes.Equal(choice.FinishReason, []byte("null")) && !bytes.Equal(choice.FinishReason, []byte(`""`)) {
				return true, false
			}
		}
	}
	return false, false
}
