// Package nativewire hosts helpers for the native-wire provider
// passthrough (Anthropic Messages, Vertex AI, Bedrock, Azure OpenAI,
// Google AI Studio, Mistral, Cohere, vLLM-native, OpenAI pinned
// bypass).
//
// HTTP route wiring and the handler bound to *server.Server live in
// internal/server/routes_nativewire.go. Pure helpers (config
// validation, path-stripping, target URL construction) migrate here
// as they get extracted.
package nativewire
