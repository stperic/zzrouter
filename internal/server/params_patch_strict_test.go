package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

func decodePatch(t *testing.T, body string) (*paramPatchBody, int, utils.ProblemDetails) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	// BadRequest reads the request path for the Problem Details instance,
	// so the malformed-JSON branch needs a real request on the context.
	c.Request = httptest.NewRequest(http.MethodPatch, "/zzrouter/v1/providers/ollama/parameters", nil)
	patch, ok := decodePatchBody(c, []byte(body))
	if ok {
		return patch, 200, utils.ProblemDetails{}
	}
	var errBody utils.ProblemDetails
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	return nil, rec.Code, errBody
}

// The merge-patch body is {defaults|models|nodes} → {parameters|
// environment|endpoints}. The flat guess is the natural one: it is the
// shape of the retired PUT, and it is how GET /resolved answers. Under
// json.Unmarshal that body returned 200 with the unchanged resolved view
// and wrote nothing to disk -- a report of success for a change that
// never happened.
func TestPatchBodyRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"flat environment": `{"environment":{"OLLAMA_NUM_PARALLEL":"3"}}`,
		"flat parameters":  `{"parameters":{"num-ctx":"4096"}}`,
		"misspelled tier":  `{"default":{"environment":{"A":"1"}}}`,
		"nested typo":      `{"defaults":{"env":{"A":"1"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			patch, code, errBody := decodePatch(t, body)

			require.Nil(t, patch)
			assert.Equal(t, 400, code)
			require.Len(t, errBody.Errors, 1)
			assert.Equal(t, string(httperr.CodeUnknownField), errBody.Errors[0].Code)
			assert.NotEmpty(t, errBody.Errors[0].Key, "the caller needs to know which field")
			assert.Contains(t, errBody.Errors[0].Want, "defaults",
				"a rejection has to say what the shape is, or the caller guesses again")
		})
	}
}

// Strictness stops at the struct level. The maps inside a leaf block hold
// parameter and environment names, which are open by construction -- an
// unrecognized one is the schema's business (CodeUnknownFlag), not a
// structural error. Rejecting those here would break every valid patch.
func TestPatchBodyAcceptsEveryTierAndOpenLeafKeys(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"defaults":       `{"defaults":{"environment":{"ANY_KEY":"v"},"parameters":{"any-flag":"v"}}}`,
		"model tier":     `{"models":{"qwen":{"parameters":{"num-ctx":"4096"}}}}`,
		"node tier":      `{"nodes":{"worker-1":{"environment":{"A":"1"}}}}`,
		"node x model":   `{"nodes":{"worker-1":{"models":{"qwen":{"parameters":{"num-ctx":"1"}}}}}}`,
		"endpoints":      `{"defaults":{"endpoints":{"embeddings":{"parameters":{"pooling":"mean"}}}}}`,
		"null deletes":   `{"defaults":{"environment":{"A":null}}}`,
		"empty is a nop": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			patch, code, errBody := decodePatch(t, body)

			require.NotNil(t, patch, "code %d, errors %+v", code, errBody.Errors)
		})
	}
}

// Malformed JSON is not an unknown field and must not be reported as one.
func TestPatchBodyMalformedJSONIsNotAnUnknownField(t *testing.T) {
	t.Parallel()

	patch, code, errBody := decodePatch(t, `{"defaults":`)

	require.Nil(t, patch)
	assert.Equal(t, 400, code)
	for _, e := range errBody.Errors {
		assert.NotEqual(t, httperr.CodeUnknownField, e.Code)
	}
}

// encoding/json stops at the first document, so a body carrying two
// objects had the second silently dropped -- report-success-for-nothing,
// the same failure DisallowUnknownFields is here to stop.
func TestPatchBodyRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	patch, code, _ := decodePatch(t, `{"defaults":{"environment":{"A":"1"}}} {"defaults":{"environment":{"A":"2"}}}`)

	require.Nil(t, patch, "the second object would have been dropped without a word")
	assert.Equal(t, 400, code)
}

// The advice has to fit the mistake. A flat body needs wrapping; a typo
// INSIDE a tier does not, and telling that caller to "send
// {"defaults":{...}}" is advice they already followed.
func TestUnknownFieldAdviceFitsTheMistake(t *testing.T) {
	t.Parallel()

	t.Run("a leaf block at the top level is told to wrap it", func(t *testing.T) {
		t.Parallel()
		_, _, errBody := decodePatch(t, `{"environment":{"A":"1"}}`)

		require.Len(t, errBody.Errors, 1)
		assert.Contains(t, errBody.Errors[0].Message, `{"defaults":{"environment":{...}}}`)
	})

	t.Run("a typo inside a tier is not told to wrap it", func(t *testing.T) {
		t.Parallel()
		_, _, errBody := decodePatch(t, `{"defaults":{"env":{"A":"1"}}}`)

		require.Len(t, errBody.Errors, 1)
		assert.Equal(t, "env", errBody.Errors[0].Key)
		assert.NotContains(t, errBody.Errors[0].Message, "Wrap it",
			"this body already has a tier; the advice would be nonsense")
		assert.Contains(t, errBody.Errors[0].Message, "rejects fields it does not define")
	})
}
