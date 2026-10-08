package security

import (
	"bytes"
	"encoding/json"
	"strings"
)

// jsonSensitiveKeys marks JSON field names whose values must never
// leave a capture pipeline. Deliberately stricter than IsSensitiveKey:
// that predicate is contains-based ("key" matches anything), which is
// right for header/env maps but would gut API bodies where "key" names
// a parameter, not a secret. A raw credential in a benign-named field
// is still caught by the value-prefix patterns in SensitivePatterns.
var jsonSensitiveKeys = []string{
	"token", "secret", "password", "passwd", "credential",
	"api_key", "api-key", "apikey", "bearer", "authorization", "raw_key",
}

func isJSONSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	// Pricing rates (input_cost_per_token, output_cost_per_reasoning_token,
	// ...) end in "token" but are not credentials; redacting them makes
	// every pricing example lie, and turns a number into a string.
	if strings.Contains(k, "cost_per_") {
		return false
	}
	for _, s := range jsonSensitiveKeys {
		if k == s || strings.HasSuffix(k, "_"+s) || strings.HasSuffix(k, "-"+s) {
			return true
		}
	}
	return false
}

// RedactJSON redacts a captured HTTP body for storage or display.
//
// JSON input is parsed and walked: values under sensitive field names
// are replaced wholesale, then the re-marshalled text gets the
// value-prefix pass so a credential in an unremarkable field is still
// caught. The regex pass alone is NOT enough for JSON: every
// key[:=]value pattern in SensitivePatterns expects the bare key name,
// and the closing quote in "token": breaks the match.
//
// Non-JSON input falls back to RedactSensitive unchanged.
func RedactJSON(raw []byte) []byte {
	// UseNumber and no HTML escaping: the output documents the wire, so
	// a nanosecond timestamp must not lose precision to float64 and "<"
	// must not become \u003c.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return []byte(RedactSensitive(string(raw)))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redactValue(v)); err != nil {
		// Cannot happen for a value built from Decode output, but a
		// silent empty return would look like an empty body.
		return []byte(RedactedPlaceholder)
	}
	return []byte(RedactSensitive(strings.TrimSuffix(buf.String(), "\n")))
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if isJSONSensitiveKey(k) {
				t[k] = RedactedPlaceholder
				continue
			}
			t[k] = redactValue(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = redactValue(val)
		}
		return t
	default:
		return v
	}
}
