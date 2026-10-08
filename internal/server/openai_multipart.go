// OpenAI multipart body handling for /v1/* pass-through routes.
//
// Some OpenAI endpoints accept multipart/form-data bodies that carry a `model`
// form field alongside a file part (audio transcription, audio translation,
// image edits, image variations). zzRouter needs the model field for routing
// but must forward the rest of the body — including the multipart boundary in
// the Content-Type header — verbatim to the backend.
//
// readOpenAIMultipartBody buffers the full body (capped by maxBytes), extracts
// the `model` form field, and returns the raw bytes so the caller can hand
// them to proxyToBackend unchanged. upstreamHeaders preserves the original
// Content-Type (including boundary) so the backend can re-parse the form.
package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/utils"
)

// readOpenAIMultipartBody buffers a multipart/form-data request body up to
// maxBytes, extracts the `model` form field if present, and returns the raw
// body for downstream pass-through. On parse or size errors it writes an
// OpenAI-shape error response and returns ok=false.
//
// The returned model string may be empty when the request omits the field —
// this is legal for some endpoints (e.g. /v1/images/edits, /v1/images/variations
// where model defaults server-side). Callers that require a model MUST check
// explicitly.
//
// The body is returned as an opaque byte slice; callers MUST NOT re-encode it.
// The original Content-Type header (with boundary parameter) remains on the
// request and will be copied verbatim by upstreamHeaders during proxying.
func readOpenAIMultipartBody(c *gin.Context, maxBytes int64) (body []byte, model string, ok bool) {
	contentType := c.Request.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		OpenAIInvalidRequest(c, "Content-Type must be multipart/form-data", nil)
		return nil, "", false
	}
	boundary := params["boundary"]
	if boundary == "" {
		OpenAIInvalidRequest(c, "Multipart Content-Type is missing boundary parameter", nil)
		return nil, "", false
	}

	// Cap body size. http.MaxBytesReader writes a 413 itself if invoked
	// with a ResponseWriter, but we want to emit an OpenAI-shape error so
	// we inspect the error instead and write our own envelope.
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	buf, err := io.ReadAll(limited)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			code := "request_too_large"
			c.JSON(http.StatusRequestEntityTooLarge, utils.NewOpenAIError(
				"invalid_request_error",
				fmt.Sprintf("Request body exceeds maximum upload size of %d bytes", maxBytes),
				nil, &code,
			))
			return nil, "", false
		}
		OpenAIInvalidRequest(c, "Failed to read request body", nil)
		return nil, "", false
	}

	// Parse the buffered body to find the `model` form field. We re-parse
	// the same bytes we will forward — the backend will parse them again
	// on its end with the original boundary, which stays in the preserved
	// Content-Type header.
	mr := multipart.NewReader(bytes.NewReader(buf), boundary)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			OpenAIInvalidRequest(c, fmt.Sprintf("Invalid multipart body: %v", err), nil)
			return nil, "", false
		}
		// A form field has FormName != "" and FileName == "".
		if part.FormName() == "model" && part.FileName() == "" {
			data, readErr := io.ReadAll(part)
			_ = part.Close()
			if readErr != nil {
				OpenAIInvalidRequest(c, "Failed to read model form field", nil)
				return nil, "", false
			}
			model = strings.TrimSpace(string(data))
			// First `model` field wins, mirroring OpenAI server behaviour.
			break
		}
		_ = part.Close()
	}

	return buf, model, true
}
