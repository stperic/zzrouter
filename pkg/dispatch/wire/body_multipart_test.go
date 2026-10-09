package wire

import (
	"bytes"
	"io"
	"mime/multipart"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A transcription upload names its model in a form field. Routing strips a
// provider prefix ("groq/whisper") before the upstream sees the name; the
// upload has to keep its boundary and file bytes while that field changes,
// or the provider is asked for a model it does not have.
func TestRewriteRequestModel_Multipart(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	require.NoError(t, w.WriteField("model", "groq/whisper-large-v3-turbo"))
	fw, err := w.CreateFormFile("file", "hello.wav")
	require.NoError(t, err)
	audio := []byte("RIFF\x00\x01binary")
	_, err = fw.Write(audio)
	require.NoError(t, err)
	require.NoError(t, w.WriteField("language", "en"))
	require.NoError(t, w.Close())

	out := RewriteRequestModel(buf.Bytes(), w.FormDataContentType(), "whisper-large-v3-turbo")

	r := multipart.NewReader(bytes.NewReader(out), w.Boundary())
	fields := map[string][]byte{}
	for {
		part, err := r.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(part)
		require.NoError(t, err)
		fields[part.FormName()] = data
	}
	assert.Equal(t, "whisper-large-v3-turbo", string(fields["model"]))
	assert.Equal(t, audio, fields["file"])
	assert.Equal(t, "en", string(fields["language"]))

	assert.JSONEq(t, `{"model":"b"}`, string(RewriteRequestModel([]byte(`{"model":"a"}`), "application/json", "b")))
	garbled := []byte("not multipart")
	assert.Equal(t, garbled, RewriteRequestModel(garbled, w.FormDataContentType(), "b"), "unparseable bodies pass unchanged")
}
