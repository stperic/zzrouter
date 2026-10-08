package inferencelog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// maxInlineBlobBytes is the length past which a base64-shaped string is
// treated as attached media rather than prose. Prose carries whitespace
// and punctuation so the charset test does the discriminating; the
// length only keeps short tokens (IDs, hashes, tool-call args) inline.
const maxInlineBlobBytes = 1024

// base64Marker separates a data URL's media type from its payload.
const base64Marker = ";base64,"

// ElidedBlob describes attached media that was replaced by a placeholder.
type ElidedBlob struct {
	// Path locates the blob in the request, e.g.
	// "messages[0].content[1].image_url.url".
	Path  string `json:"path"`
	Media string `json:"media,omitempty"` // media type when the blob was a data URL
	Bytes int    `json:"bytes"`
}

// elideMedia replaces base64 media (images, audio, uploaded files) with
// size-annotated placeholders so a payload records what was attached
// without retaining megabytes of binary. Returns the body unchanged when
// nothing matched, which keeps text-only payloads byte-exact.
func elideMedia(body []byte) ([]byte, []ElidedBlob) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // large integers must survive the decode/encode round trip
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return body, nil
	}

	var blobs []ElidedBlob
	elided := elideValue(doc, "", "", &blobs)
	if len(blobs) == 0 {
		return body, nil
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // placeholders and prompts stay readable to copy
	if err := enc.Encode(elided); err != nil {
		return body, nil
	}
	return bytes.TrimRight(out.Bytes(), "\n"), blobs
}

// elideValue walks the decoded JSON, replacing media strings in place and
// recording where each one was found. mediaHint carries the media type
// declared by an enclosing object for providers that put it beside the
// blob rather than in a data URL.
func elideValue(v any, path, mediaHint string, blobs *[]ElidedBlob) any {
	switch t := v.(type) {
	case map[string]any:
		hint := siblingMediaType(t)
		for k, child := range t {
			t[k] = elideValue(child, joinPath(path, k), hint, blobs)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = elideValue(child, fmt.Sprintf("%s[%d]", path, i), mediaHint, blobs)
		}
		return t
	case string:
		placeholder, blob, ok := elideBlob(t)
		if !ok {
			return v
		}
		blob.Path = path
		if blob.Media == "" {
			blob.Media = mediaHint
		}
		*blobs = append(*blobs, blob)
		return placeholder
	default:
		return v
	}
}

// mediaTypeKeys are the sibling keys providers use to type an adjacent
// base64 blob: Anthropic sends source.media_type, Gemini inlineData.mimeType.
var mediaTypeKeys = []string{"media_type", "mimeType", "mime_type"}

// siblingMediaType reads the media type an object declares for the blob it
// holds, so a non-data-URL attachment is still identified.
func siblingMediaType(obj map[string]any) string {
	for _, key := range mediaTypeKeys {
		if v, ok := obj[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// elideBlob reports whether s is attached media and, if so, returns the
// placeholder that replaces it. Data URLs keep their prefix so the media
// type stays readable in the payload itself.
func elideBlob(s string) (string, ElidedBlob, bool) {
	if marker := strings.Index(s, base64Marker); marker >= 0 {
		data := s[marker+len(base64Marker):]
		if len(data) <= maxInlineBlobBytes {
			return "", ElidedBlob{}, false
		}
		prefix := s[:marker+len(base64Marker)]
		return prefix + placeholder(len(data)),
			ElidedBlob{Media: mediaTypeFromDataURL(s[:marker]), Bytes: len(data)}, true
	}
	if len(s) > maxInlineBlobBytes && isBase64(s) {
		return placeholder(len(s)), ElidedBlob{Bytes: len(s)}, true
	}
	return "", ElidedBlob{}, false
}

func placeholder(n int) string {
	return fmt.Sprintf("<elided %d bytes>", n)
}

// mediaTypeFromDataURL pulls "image/png" out of a "data:image/png" prefix.
func mediaTypeFromDataURL(prefix string) string {
	return strings.TrimPrefix(prefix, "data:")
}

// isBase64 reports whether s is entirely base64 alphabet (standard or
// URL-safe), allowing the line breaks MIME encoders insert every 76
// chars. Spaces stay disqualifying — prose is letters and spaces, so
// accepting them would elide long prompts.
func isBase64(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '+', r == '/', r == '=', r == '-', r == '_':
		case r == '\r', r == '\n':
		default:
			return false
		}
	}
	return true
}

// hasElidableBlob reports whether body contains a base64 run long enough
// for elideMedia to shrink. Cheaper than the decode it guards.
func hasElidableBlob(body []byte) bool {
	run := 0
	for _, b := range body {
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9',
			b == '+', b == '/', b == '=', b == '-', b == '_', b == '\r', b == '\n':
			run++
			if run > maxInlineBlobBytes {
				return true
			}
		default:
			run = 0
		}
	}
	return false
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}
