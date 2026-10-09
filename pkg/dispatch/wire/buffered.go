package wire

import (
	"io"
	"net/http"
	"strconv"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// RelayBuffered reads before committing headers so an incomplete body can still become a gateway error.
func RelayBuffered(w http.ResponseWriter, resp *http.Response, shape func([]byte) []byte, recorder *llm.InferenceRecorder) Result {
	body, err := io.ReadAll(resp.Body)
	if err == nil && resp.ContentLength > int64(len(body)) && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified && (resp.Request == nil || resp.Request.Method != http.MethodHead) {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		RecordPreByteCopyError(recorder, "upstream_body_error", err.Error())
		return Result{Outcome: Truncated, Err: err}
	}
	tagRecorderFromStatus(recorder, resp.StatusCode)
	recorder.SetResponseBody(body)
	recorder.AppendResponseText(ResponseText(body))
	// Shaping reads the completed snapshot, including cost settled by the completion hook.
	recordProxyMetrics(body, recorder)
	if shape != nil {
		body = shape(body)
	}
	w.Header().Del("Transfer-Encoding")
	w.Header().Del("Content-Encoding")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	if len(body) == 0 {
		return Result{Outcome: Completed}
	}
	n, err := w.Write(body)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return Result{Outcome: ClientGone, Err: err}
	}
	return Result{Outcome: Completed}
}
