// Package protocol hosts the per-surface compatibility code that is not
// tied to the HTTP server struct.
//
// Each third-party protocol zzrouter speaks is a subpackage:
//
//   - openai     — OpenAI /v1/* error envelope, closed-vocabulary
//     error types/codes, and the Responder that emits them
//   - ollama     — Ollama /api/* compatibility
//   - mcp        — Model Context Protocol transport gateway
//   - realtime   — OpenAI Realtime WebSocket proxy helpers
//   - nativewire — native-wire provider passthrough (Anthropic, Vertex, …)
//
// HTTP route wiring and the handler methods that depend on the full
// server state still live in internal/server/. The subpackages here hold
// the pieces that are pure functions or pure data structures — they can
// be unit tested without spinning up a Server, and they make it
// structurally obvious which protocol owns which file.
package protocol
