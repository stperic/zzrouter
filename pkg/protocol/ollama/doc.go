// Package ollama hosts Ollama /api/* protocol helpers that are not
// coupled to the HTTP server struct.
//
// Handler methods bound to *server.Server (ollama_service.go,
// ollama_handlers.go, ollama_helpers.go) stay in internal/server/ for
// now. Type definitions and pure helpers migrate here as they get
// decoupled.
package ollama
