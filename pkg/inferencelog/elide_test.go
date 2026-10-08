package inferencelog

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// base64Blob returns a base64-alphabet string of n bytes.
func base64Blob(n int) string {
	return strings.Repeat("iVBORw0KGgoAAAANSUhEUg", n/22+1)[:n]
}

func TestElideMedia_OpenAIImageURL(t *testing.T) {
	blob := base64Blob(400 * 1024)
	body := []byte(fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":[`+
		`{"type":"text","text":"what is in this picture?"},`+
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`, blob))

	out, elided := elideMedia(body)

	require.Len(t, elided, 1)
	assert.Equal(t, "messages[0].content[1].image_url.url", elided[0].Path)
	assert.Equal(t, "image/png", elided[0].Media)
	assert.Equal(t, len(blob), elided[0].Bytes)

	assert.Less(t, len(out), 1024, "the binary is gone")
	assert.Contains(t, string(out), "what is in this picture?", "the prompt survives")
	assert.Contains(t, string(out), "data:image/png;base64,<elided ", "the media type stays readable")
	assert.NotContains(t, string(out), blob)
	assert.True(t, json.Valid(out))
}

func TestElideMedia_OllamaImagesArray(t *testing.T) {
	blob := base64Blob(200 * 1024)
	body := []byte(fmt.Sprintf(`{"model":"llava","messages":[{"role":"user","content":"describe","images":["%s"]}]}`, blob))

	out, elided := elideMedia(body)

	require.Len(t, elided, 1)
	assert.Equal(t, "messages[0].images[0]", elided[0].Path)
	assert.Equal(t, len(blob), elided[0].Bytes)
	assert.Empty(t, elided[0].Media, "a raw base64 blob carries no media type")
	assert.NotContains(t, string(out), blob)
}

func TestElideMedia_UploadedFileKeepsFilename(t *testing.T) {
	blob := base64Blob(300 * 1024)
	body := []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":[`+
		`{"type":"file","file":{"filename":"q3-report.pdf","file_data":"data:application/pdf;base64,%s"}}]}]}`, blob))

	out, elided := elideMedia(body)

	require.Len(t, elided, 1)
	assert.Equal(t, "application/pdf", elided[0].Media)
	assert.Contains(t, string(out), "q3-report.pdf", "the filename is the point — only the bytes go")
	assert.NotContains(t, string(out), blob)
}

// Anthropic and Gemini type the blob on a sibling key rather than in a
// data URL, so the walk must read it from the enclosing object.
func TestElideMedia_SiblingMediaType(t *testing.T) {
	blob := base64Blob(200 * 1024)

	tests := []struct {
		name string
		body string
		path string
		want string
	}{
		{
			name: "anthropic",
			body: `{"messages":[{"content":[{"type":"image","source":` +
				`{"type":"base64","media_type":"image/jpeg","data":"%s"}}]}]}`,
			path: "messages[0].content[0].source.data",
			want: "image/jpeg",
		},
		{
			name: "gemini",
			body: `{"contents":[{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"%s"}}]}]}`,
			path: "contents[0].parts[0].inlineData.data",
			want: "audio/wav",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, elided := elideMedia([]byte(fmt.Sprintf(tt.body, blob)))

			require.Len(t, elided, 1)
			assert.Equal(t, tt.path, elided[0].Path)
			assert.Equal(t, tt.want, elided[0].Media)
			assert.NotContains(t, string(out), blob)
		})
	}
}

// MIME encoders wrap base64 every 76 chars; the line breaks must not
// disguise the blob as prose.
func TestElideMedia_LineWrappedBase64(t *testing.T) {
	var wrapped strings.Builder
	for i := 0; i < 200; i++ {
		wrapped.WriteString(base64Blob(76))
		wrapped.WriteString("\r\n")
	}
	body := []byte(fmt.Sprintf(`{"images":[%q]}`, wrapped.String()))

	_, elided := elideMedia(body)

	require.Len(t, elided, 1)
	assert.Equal(t, "images[0]", elided[0].Path)
}

func TestHasElidableBlob(t *testing.T) {
	prose := []byte(fmt.Sprintf(`{"content":%q}`, strings.Repeat("a thoughtful sentence. ", 500)))
	assert.False(t, hasElidableBlob(prose), "prose must skip the decode entirely")
	assert.True(t, hasElidableBlob([]byte(fmt.Sprintf(`{"images":[%q]}`, base64Blob(2*maxInlineBlobBytes)))))
}

func TestElideMedia_LeavesProseAlone(t *testing.T) {
	// A long system prompt is exactly what an operator wants to read back.
	prompt := strings.Repeat("You are a careful assistant. ", 500)
	body := []byte(fmt.Sprintf(`{"messages":[{"role":"system","content":%q}]}`, prompt))

	out, elided := elideMedia(body)

	assert.Empty(t, elided)
	assert.Equal(t, string(body), string(out), "untouched bodies stay byte-exact")
}

func TestElideMedia_LeavesShortBlobsInline(t *testing.T) {
	body := []byte(fmt.Sprintf(`{"images":["%s"]}`, base64Blob(maxInlineBlobBytes)))

	out, elided := elideMedia(body)

	assert.Empty(t, elided)
	assert.Equal(t, string(body), string(out))
}

func TestElideMedia_MultipleBlobs(t *testing.T) {
	blob := base64Blob(2 * maxInlineBlobBytes)
	body := []byte(fmt.Sprintf(`{"images":["%s","%s"]}`, blob, blob))

	_, elided := elideMedia(body)

	require.Len(t, elided, 2)
	assert.Equal(t, "images[0]", elided[0].Path)
	assert.Equal(t, "images[1]", elided[1].Path)
}

func TestElideMedia_PreservesLargeIntegers(t *testing.T) {
	body := []byte(fmt.Sprintf(`{"seed":9007199254740993,"images":["%s"]}`, base64Blob(2*maxInlineBlobBytes)))

	out, _ := elideMedia(body)

	assert.Contains(t, string(out), "9007199254740993", "float64 round-tripping would corrupt this")
}

func TestNewPayload_VisionRequestSurvivesTheSizeCap(t *testing.T) {
	blob := base64Blob(4 * MaxPayloadBytes)
	body := []byte(fmt.Sprintf(`{"model":"llava","messages":[{"role":"user","content":"describe","images":["%s"]}]}`, blob))

	p := NewPayload(body, nil, nil)

	require.NotNil(t, p)
	assert.False(t, p.Oversize, "eliding the image brings the body back under the cap")
	assert.Contains(t, string(p.Request), "describe")
	require.Len(t, p.Elided, 1)
	assert.Equal(t, len(blob), p.Elided[0].Bytes)
}
