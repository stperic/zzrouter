package security

import (
	"strings"
	"testing"
)

// The regex pass alone cannot redact JSON: `(token\s*[:=]\s*)` expects
// a bare key name, and the closing quote in `"token":` breaks the
// match. These tests pin the JSON-aware path against exactly the
// payloads the e2e example capture stores.

func TestRedactJSON_SensitiveFieldsByName(t *testing.T) {
	for _, tc := range []struct{ name, in, mustLose string }{
		{"token", `{"type":"ollama-connect","token":"hf_notARealTokenButSecret00"}`, "hf_notARealTokenButSecret00"},
		{"password", `{"password":"hunter2secret"}`, "hunter2secret"},
		{"nested secret", `{"config":{"client_secret":"deep-dark-value"}}`, "deep-dark-value"},
		{"array element", `[{"api_key":"abcd1234efgh5678"}]`, "abcd1234efgh5678"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(RedactJSON([]byte(tc.in)))
			if strings.Contains(got, tc.mustLose) {
				t.Errorf("secret survived redaction: %s", got)
			}
			if !strings.Contains(got, RedactedPlaceholder) {
				t.Errorf("no placeholder in output: %s", got)
			}
		})
	}
}

// "key" names a parameter almost everywhere in this API (ParamError.key,
// config keys); redacting it by name would make every validation-error
// example read [REDACTED] where a flag name belongs.
func TestRedactJSON_BenignKeyFieldsSurvive(t *testing.T) {
	// cost_per_token caught a live regression: the _token suffix rule
	// redacted every pricing rate in the generated examples.
	in := `{"errors":[{"key":"threads","code":"wrong_type"}],"key_id":"agent-7",` +
		`"input_cost_per_token":0.002,"output_cost_per_reasoning_token":0.004}`
	got := string(RedactJSON([]byte(in)))
	for _, want := range []string{`"threads"`, `"agent-7"`, `0.002`, `0.004`} {
		if !strings.Contains(got, want) {
			t.Errorf("benign value %s was redacted: %s", want, got)
		}
	}
}

// A raw credential in a benign-named field is caught by VALUE: the
// created-key response carries the zzr_ secret under json "key".
func TestRedactJSON_CredentialValuesCaughtInBenignFields(t *testing.T) {
	in := `{"key":"zzr_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ab","id":"agent-7"}`
	got := string(RedactJSON([]byte(in)))
	if strings.Contains(got, "zzr_") {
		t.Errorf("raw virtual key survived: %s", got)
	}
	if !strings.Contains(got, `"agent-7"`) {
		t.Errorf("benign id was lost: %s", got)
	}
}

func TestRedactJSON_NonJSONFallsBackToRegex(t *testing.T) {
	got := string(RedactJSON([]byte("data: bearer zzr_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ab\n\n")))
	if strings.Contains(got, "zzr_A") {
		t.Errorf("zzr_ key survived in non-JSON body: %s", got)
	}
}

func TestRedactSensitive_KnowsBothKeyPrefixes(t *testing.T) {
	for _, secret := range []string{
		"zzr_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ab",
		"hf_AbCdEfGhIjKlMnOpQrStUv",
	} {
		if got := RedactSensitive("value=" + secret); strings.Contains(got, secret) {
			t.Errorf("%s survived RedactSensitive: %s", secret[:4], got)
		}
	}
}

// The output documents the wire: int64-scale ids and timestamps must
// not round through float64, and "<" must stay itself.
func TestRedactJSON_PreservesNumbersAndAngleBrackets(t *testing.T) {
	in := `{"ts":1756100000123456789,"html":"<a>&"}`
	got := string(RedactJSON([]byte(in)))
	if !strings.Contains(got, "1756100000123456789") {
		t.Errorf("nanosecond timestamp lost precision: %s", got)
	}
	if !strings.Contains(got, "<a>&") {
		t.Errorf("HTML-escaped on re-marshal: %s", got)
	}
}

// A body truncated mid-JSON falls back to the regex pass; credential
// VALUES must still be caught there. The key-name predicate is lost on
// that path by design - value prefixes are the safety net.
func TestRedactJSON_TruncatedBodyStillCatchesValues(t *testing.T) {
	truncated := `{"items":[{"key":"zzr_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ab","na`
	got := string(RedactJSON([]byte(truncated)))
	if strings.Contains(got, "zzr_A") {
		t.Errorf("raw key survived in a truncated body: %s", got)
	}
}
