package openai

import (
	"regexp"
	"strings"

	"github.com/stperic/zzrouter/pkg/utils"
)

// sanitize scrubs a caller-provided detail string before it lands in
// the OpenAI envelope's `message` field.
//
// It layers three transformations in order:
//
//  1. pkg/utils.SanitizeErrorMessage removes absolute paths, stack
//     traces, and internal module paths. This is the shared sanitizer
//     also used by the Problem Details dialect.
//
//  2. goTypeNameRe replaces Go type name leaks of the form
//     "types.InstanceStatus" and "types.Foo{...}" with the placeholder
//     "<type>". The leak most commonly originates in helpers.go's
//     routeAndParse unmarshal error path.
//
//  3. unmarshalPhraseRe collapses the characteristic Go JSON unmarshal
//     error phrase "cannot unmarshal ... into Go struct field ..." into
//     a generic shape-mismatch message so the error surface does not
//     identify internal struct layouts.
//
// The function is OpenAI-specific rather than living in pkg/utils
// because the replacement placeholders ("<type>") use syntax that is
// safe inside a JSON string literal but that other dialects (RFC 7807
// HTML-mimetype rendering, Ollama flat strings) may treat differently.
// Keeping the sanitizer beside the responder that uses it also means
// future additions to the OpenAI message vocabulary can be reviewed
// in one place.
//
// An empty input returns an empty string. The function is pure and
// safe for concurrent use.
func sanitize(msg string) string {
	if msg == "" {
		return ""
	}
	out := utils.SanitizeErrorMessage(msg)
	out = goTypeNameRe.ReplaceAllString(out, "<type>")
	out = unmarshalPhraseRe.ReplaceAllString(out, "upstream response had unexpected shape")
	return strings.TrimSpace(out)
}

// goTypeNameRe matches dotted Go type references — the package-qualified
// identifier form the fmt verbs ("%T", "%v" of typed errors) emit when
// an internal type leaks into a user-facing string.
//
// The pattern is restricted to lowercase-starting package names
// (\b[a-z][a-z0-9_]*) followed by a dot and an exported identifier
// (\.[A-Z][A-Za-z0-9_]*) so it does not spuriously strip legitimate
// model names like "gpt-4o.0613" (dot after a digit, no package-name
// shape) or sentence fragments like "Model found. The".
var goTypeNameRe = regexp.MustCompile(`\b[a-z][a-z0-9_]*\.[A-Z][A-Za-z0-9_]*\b`)

// unmarshalPhraseRe matches the canonical Go JSON unmarshal error
// phrase. The pattern is deliberately narrow so it does not catch
// arbitrary user messages that happen to contain "cannot" or "unmarshal".
var unmarshalPhraseRe = regexp.MustCompile(`cannot unmarshal [^ ]+ into Go struct field [^ ]+ of type [^ ]+`)
