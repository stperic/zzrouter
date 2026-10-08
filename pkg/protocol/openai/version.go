package openai

// SpecVersion identifies the OpenAI REST API reference snapshot that
// this package's envelope, error.type vocabulary, and error.code
// vocabulary target.
//
// Format: YYYY-MM-DD, matching the revision date on the OpenAI API
// reference pages (https://platform.openai.com/docs/api-reference).
//
// # Upgrade protocol
//
// When OpenAI publishes a revision of the API reference that affects
// the error contract (new error codes, new error.type values, a new
// field on the envelope, a changed parameter name):
//
//  1. Update SpecVersion to the new revision date.
//  2. Add the new ErrorType / ErrorCode constant(s) in vocab.go with a
//     `// Added in YYYY-MM-DD.` doc comment on the line above.
//  3. Never delete a constant. Mark removed values with `// Deprecated:
//     removed from OpenAI reference in YYYY-MM-DD, kept for legacy
//     client compatibility.` so downstream enum consumers do not break.
//  4. If the envelope adds a new field, extend the Envelope / ErrorBody
//     struct in envelope.go with an `omitempty` JSON tag so existing
//     callers continue to round-trip without the field.
//  5. Add a Changelog entry below.
//  6. Bump the zzRouter version (not the Go module path — see breaking
//     change protocol below) and update the release notes.
//
// # Breaking change protocol
//
// If OpenAI restructures the envelope in a way that cannot be expressed
// as an additive change (e.g. `error` becomes an array, `type` is
// renamed, `code` becomes an integer):
//
//  1. Do NOT modify the existing Envelope / ErrorBody types in place.
//  2. Create pkg/protocol/openai/v2 with the new shape.
//  3. Keep this package at its last good SpecVersion so in-flight
//     clients continue to work.
//  4. Migrate internal/server wiring to v2 on a flag and switch the
//     default after one release cycle.
//
// The goal is that bumping to a new OpenAI revision is a localized
// change: edit version.go, add constants in vocab.go, update Changelog,
// and run the existing SDK smoke tests. Nothing outside this package
// should need to know the SpecVersion.
const SpecVersion = "2026-04-11"

// Changelog records the revisions of the OpenAI API reference that
// this package has tracked. Each entry names the SpecVersion value and
// lists the reason for the bump. Entries are in reverse chronological
// order; the current SpecVersion is always the first entry.
//
// The Changelog exists so reviewers of a SpecVersion bump can see the
// rationale without leaving the source tree, and so future maintainers
// can correlate a wire-level change with the revision that introduced
// it.
//
//	2026-04-11 — Initial release. Tracks the OpenAI API reference as of
//	             April 2026. Vocabulary covers the eight error.type
//	             values documented on the error-codes page and the
//	             error.code values observed in openai-python 2.31.0.
const Changelog = `
2026-04-11: Initial release pinned to the OpenAI API reference snapshot
             current as of April 2026. Vocabulary covers the eight
             error.type values documented on the error-codes page and
             the error.code values observed in openai-python 2.31.0.
`
