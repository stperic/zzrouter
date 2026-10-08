package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildMultipartRequest constructs a multipart/form-data HTTP request with the
// given fields and an optional file part. fileFirst=true emits the file part
// BEFORE the form fields, mimicking clients that stream the file up-front.
// Returns the Gin context plus the underlying ResponseRecorder so tests can
// inspect the response directly (Gin wraps the writer so a type assertion on
// c.Writer does not work).
func buildMultipartRequest(t *testing.T, formFields map[string]string, filePart string, fileContent []byte, fileFirst bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	writeFile := func() {
		if filePart == "" {
			return
		}
		fw, err := writer.CreateFormFile(filePart, "sample.wav")
		require.NoError(t, err)
		_, err = fw.Write(fileContent)
		require.NoError(t, err)
	}

	writeFields := func() {
		for k, v := range formFields {
			require.NoError(t, writer.WriteField(k, v))
		}
	}

	if fileFirst {
		writeFile()
		writeFields()
	} else {
		writeFields()
		writeFile()
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c, w
}

func TestReadOpenAIMultipartBody_HappyPath_ModelBeforeFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, w := buildMultipartRequest(t,
		map[string]string{"model": "whisper-1", "language": "en"},
		"file",
		[]byte("fake audio bytes"),
		false,
	)

	body, model, ok := readOpenAIMultipartBody(c, 1<<20)
	require.True(t, ok, "expected ok=true, got response: %s", w.Body.String())
	assert.Equal(t, "whisper-1", model)
	assert.NotEmpty(t, body)
	// The returned body must still be a parseable multipart payload with the
	// file part intact — callers forward these bytes verbatim.
	assert.Contains(t, string(body), "fake audio bytes")
	assert.Contains(t, string(body), `name="model"`)
	assert.Contains(t, string(body), `name="file"`)
}

func TestReadOpenAIMultipartBody_HappyPath_ModelAfterFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := buildMultipartRequest(t,
		map[string]string{"model": "whisper-1"},
		"file",
		[]byte("fake audio bytes"),
		true, // file first, model comes after
	)

	body, model, ok := readOpenAIMultipartBody(c, 1<<20)
	require.True(t, ok)
	assert.Equal(t, "whisper-1", model)
	assert.Contains(t, string(body), "fake audio bytes")
}

func TestReadOpenAIMultipartBody_MissingModel_IsPermissive(t *testing.T) {
	// OpenAI endpoints like /v1/images/edits and /v1/images/variations
	// treat `model` as optional (default dall-e-2). The helper must not
	// reject these: it returns ok=true with an empty model string and
	// lets the caller decide how to route.
	gin.SetMode(gin.TestMode)
	c, _ := buildMultipartRequest(t,
		map[string]string{"size": "512x512"},
		"image",
		[]byte("fake png bytes"),
		false,
	)

	body, model, ok := readOpenAIMultipartBody(c, 1<<20)
	assert.True(t, ok)
	assert.Empty(t, model)
	assert.NotEmpty(t, body)
	assert.Contains(t, string(body), "fake png bytes")
}

func TestReadOpenAIMultipartBody_NonMultipartContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader(`{"model":"whisper-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	_, _, ok := readOpenAIMultipartBody(c, 1<<20)
	assert.False(t, ok)
	assert.Equal(t, 400, rec.Code)
}

func TestReadOpenAIMultipartBody_MissingBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader("garbage"))
	req.Header.Set("Content-Type", "multipart/form-data") // no boundary
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	_, _, ok := readOpenAIMultipartBody(c, 1<<20)
	assert.False(t, ok)
	assert.Equal(t, 400, rec.Code)
	assert.Contains(t, rec.Body.String(), "boundary")
}

func TestReadOpenAIMultipartBody_BodyTooLarge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Build a body that's larger than the limit we'll pass.
	big := bytes.Repeat([]byte("A"), 4096)
	c, w := buildMultipartRequest(t,
		map[string]string{"model": "whisper-1"},
		"file",
		big,
		false,
	)

	_, _, ok := readOpenAIMultipartBody(c, 512) // tiny cap
	assert.False(t, ok)
	assert.Equal(t, 413, w.Code)
	assert.Contains(t, w.Body.String(), "request_too_large")
}

func TestReadOpenAIMultipartBody_WhitespaceOnlyModelField(t *testing.T) {
	// Whitespace-only model is treated as absent (TrimSpace), so the
	// helper returns an empty model string. Caller falls back to the
	// default backend.
	gin.SetMode(gin.TestMode)
	c, _ := buildMultipartRequest(t,
		map[string]string{"model": "   "},
		"file",
		[]byte("x"),
		false,
	)

	_, model, ok := readOpenAIMultipartBody(c, 1<<20)
	assert.True(t, ok)
	assert.Empty(t, model)
}

// Sanity check: Go's multipart.NewReader round-trips the buffered body for
// re-parsing downstream. Guards against a silent regression where we might
// accidentally rewrite the buffer before returning it.
func TestReadOpenAIMultipartBody_BufferIsReparseable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := buildMultipartRequest(t,
		map[string]string{"model": "whisper-1", "language": "fr"},
		"file",
		[]byte("round-trip"),
		false,
	)

	body, _, ok := readOpenAIMultipartBody(c, 1<<20)
	require.True(t, ok)

	// Re-parse using the original boundary parsed from the request header.
	ct := c.Request.Header.Get("Content-Type")
	const prefix = "multipart/form-data; boundary="
	require.True(t, strings.HasPrefix(ct, prefix))
	boundary := ct[len(prefix):]

	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	seen := map[string]string{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data, _ := io.ReadAll(p)
		seen[p.FormName()] = string(data)
		_ = p.Close()
	}
	assert.Equal(t, "whisper-1", seen["model"])
	assert.Equal(t, "fr", seen["language"])
	assert.Equal(t, "round-trip", seen["file"])
}
