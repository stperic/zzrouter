package openai

import (
	"strings"
	"testing"
)

// TestSanitize_StripsGoTypeNames covers the routeAndParse leak case:
// unmarshal errors from helpers.go used to reach the client carrying
// the internal Go type name. The sanitizer MUST strip them.
func TestSanitize_StripsGoTypeNames(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantSub string // substring that must appear
		badSub  string // substring that must NOT appear
	}{
		{
			name:    "unmarshal phrase with types.InstanceStatus",
			in:      "failed to parse response: json: cannot unmarshal number into Go struct field types.InstanceStatus of type string",
			wantSub: "upstream response had unexpected shape",
			badSub:  "types.InstanceStatus",
		},
		{
			name:    "bare dotted type reference",
			in:      "expected runs.Request, got nil",
			wantSub: "<type>",
			badSub:  "runs.Request",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitize(tc.in)
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("missing wanted substring %q in %q", tc.wantSub, got)
			}
			if strings.Contains(got, tc.badSub) {
				t.Errorf("sanitizer left %q in %q", tc.badSub, got)
			}
		})
	}
}

// TestSanitize_PreservesLegitimateModelNames guards against the
// goTypeNameRe pattern accidentally stripping strings that look like
// qualified identifiers but are actually user-facing model names.
func TestSanitize_PreservesLegitimateModelNames(t *testing.T) {
	// Model names use hyphens and digits, start with a lowercase
	// letter, and have no ASCII uppercase package-name boundary —
	// they should not match goTypeNameRe.
	cases := []string{
		"model gpt-4o-mini not found",
		"requested llama-3.1-8b-instruct",
	}
	for _, in := range cases {
		got := sanitize(in)
		if !strings.Contains(got, in[:6]) { // cheap sanity check on prefix
			t.Errorf("sanitize(%q) = %q unexpectedly lost leading content", in, got)
		}
		if strings.Contains(got, "<type>") {
			t.Errorf("sanitize(%q) = %q wrongly stripped a model name", in, got)
		}
	}
}

// TestSanitize_Empty exercises the empty-input fast path.
func TestSanitize_Empty(t *testing.T) {
	if got := sanitize(""); got != "" {
		t.Errorf("sanitize(\"\") = %q, want empty", got)
	}
}
