package openai

import "testing"

// TestAllErrorTypes_Stable verifies the closed vocabulary is returned
// in a stable, defensive-copy form. Callers MUST NOT be able to
// mutate the package's internal slice through the returned value.
func TestAllErrorTypes_Stable(t *testing.T) {
	first := AllErrorTypes()
	second := AllErrorTypes()
	if len(first) != len(second) {
		t.Fatalf("AllErrorTypes length drift: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("AllErrorTypes order drift at %d: %q vs %q", i, first[i], second[i])
		}
	}
	// Mutation of the returned slice must not leak into a subsequent call.
	first[0] = ErrorType("mutated")
	third := AllErrorTypes()
	for _, v := range third {
		if v == ErrorType("mutated") {
			t.Fatal("AllErrorTypes leaked internal backing array")
		}
	}
}

// TestErrorType_ValidCoversAllConstants ensures every named ErrorType
// constant is marked Valid. A new constant added to the package
// without adding it to allErrorTypes will trip this test.
func TestErrorType_ValidCoversAllConstants(t *testing.T) {
	cases := []ErrorType{
		ErrorTypeInvalidRequest,
		ErrorTypeAuthentication,
		ErrorTypePermission,
		ErrorTypeNotFound,
		ErrorTypeRateLimit,
		ErrorTypeServer,
		ErrorTypeAPI,
		ErrorTypeInsufficientQuota,
		ErrorTypeOverloaded,
	}
	for _, c := range cases {
		if !c.Valid() {
			t.Errorf("ErrorType %q reported as invalid", c)
		}
	}
}

// TestErrorType_ValidRejectsUnknown guards against future regressions
// that loosen Valid to accept unknown strings.
func TestErrorType_ValidRejectsUnknown(t *testing.T) {
	for _, bad := range []ErrorType{"", "nope", "InvalidRequest"} {
		if bad.Valid() {
			t.Errorf("ErrorType %q unexpectedly reported as valid", bad)
		}
	}
}

// TestErrorCode_ValidCoversAllConstants mirrors the ErrorType check.
func TestErrorCode_ValidCoversAllConstants(t *testing.T) {
	cases := []ErrorCode{
		ErrorCodeInvalidAPIKey,
		ErrorCodeInsufficientPermissions,
		ErrorCodeInvalidRequest,
		ErrorCodeUnknownURL,
		ErrorCodeMethodNotAllowed,
		ErrorCodeRequestTooLarge,
		ErrorCodeModelNotFound,
		ErrorCodeEndpointNotSupported,
		ErrorCodeRateLimitExceeded,
		ErrorCodeInternalError,
		ErrorCodeUpstreamError,
		ErrorCodeUpstreamTimeout,
		ErrorCodeFeatureDisabled,
		ErrorCodeDraining,
		ErrorCodeNoDefaultBackend,
	}
	for _, c := range cases {
		if !c.Valid() {
			t.Errorf("ErrorCode %q reported as invalid", c)
		}
	}
}

// TestSpecVersion_Shape verifies SpecVersion is set to a plausible
// YYYY-MM-DD string. The test deliberately does NOT assert a specific
// date so bumping SpecVersion to track a new OpenAI reference
// revision is a single-file edit.
func TestSpecVersion_Shape(t *testing.T) {
	if len(SpecVersion) != 10 {
		t.Fatalf("SpecVersion %q is not YYYY-MM-DD", SpecVersion)
	}
	if SpecVersion[4] != '-' || SpecVersion[7] != '-' {
		t.Fatalf("SpecVersion %q is not YYYY-MM-DD", SpecVersion)
	}
}
