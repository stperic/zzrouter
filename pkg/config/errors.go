package config

import "errors"

// Package-level sentinels for the config surface. Two categories:
//
//   - Validation-phase (*Config.Validate / schema-side rules that are ours,
//     not the jsonschema library's): raise with fmt.Errorf("%w: <field
//     hint>: <detail>", ErrInvalidConfig, …). Callers discriminate "your
//     config is malformed" from other failure modes via errors.Is.
//
//   - Env-var resolution: ErrMissingEnvKey fires when a config field
//     references an environment variable that isn't set. Distinct from
//     ErrInvalidConfig because the config itself is structurally valid;
//     only the runtime resolution fails.
//
// Existing sentinels live next to their raise sites by convention and
// are not relocated here: ErrProviderExists, ErrProviderNotFound
// (apps_config.go), ErrEndpointExists (node_config_store.go),
// ErrFilePermsTooOpen (manager.go).
//
// Schema-library errors from pkg/config/schema/ remain string-matched in
// their tests; those strings come from a third-party jsonschema library
// and are not ours to sentinelize.
var (
	ErrInvalidConfig = errors.New("invalid configuration")
	ErrMissingEnvKey = errors.New("environment variable not set")
)
