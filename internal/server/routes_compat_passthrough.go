// Phase 9 — OpenAI /v1/* stateful pass-through route registrations.
//
// Every route in this file is a pure reverse proxy to the provider configured
// as openai_compat.default_backend in node.yaml. None of them inspect or
// translate the request body; zzRouter streams bytes through proxyToBackend
// and returns the backend's response verbatim.
//
// These endpoints create stateful resources (files, batches, assistants,
// threads, vector stores, uploads, responses retrieval). State is NOT
// synchronised across cluster nodes — a file uploaded to backend A is not
// visible on backend B. This is documented on the OpenAICompatConfig struct
// and in the Phase 9 plan.
//
// Route registration style is deliberately explicit (one line per route):
// Gin's radix tree matches faster, and debug traces / route listings remain
// grep-friendly. There are ~45 routes here; organise them by resource group.
package server

import (
	"github.com/gin-gonic/gin"
)

// registerOpenAIStatefulRoutes wires every spec-defined stateful /v1/* route
// to routeToDefaultOpenAIBackend. Returns the number of routes registered
// (used by the compat-route logger).
func (s *Server) registerOpenAIStatefulRoutes(openai *gin.RouterGroup) int {
	h := s.routeToDefaultOpenAIBackend
	count := 0
	add := func(method, path string) {
		openai.Handle(method, path, h)
		count++
	}

	// ------------------------------------------------------------------
	// Responses retrieval/cancel (POST /v1/responses is handled earlier
	// via handleResponses because it carries a model field and records
	// the new response id in the affinity map).
	//
	// Retrieval goes through routeToResponsesBackend so that when the
	// affinity map has an entry for the requested response id, the call
	// lands on the backend that originally served the create — not the
	// default backend. On a cache miss the handler falls through to
	// routeToDefaultOpenAIBackend, preserving the single-backend
	// behaviour that predated session affinity.
	// ------------------------------------------------------------------
	openai.Handle("GET", "/responses/:response_id", s.routeToResponsesBackend)
	openai.Handle("DELETE", "/responses/:response_id", s.routeToResponsesBackend)
	openai.Handle("POST", "/responses/:response_id/cancel", s.routeToResponsesBackend)
	openai.Handle("GET", "/responses/:response_id/input_items", s.routeToResponsesBackend)
	count += 4

	// ------------------------------------------------------------------
	// Files
	// ------------------------------------------------------------------
	add("POST", "/files") // multipart upload with `purpose` field
	add("GET", "/files")  // list
	add("GET", "/files/:file_id")
	add("DELETE", "/files/:file_id")
	add("GET", "/files/:file_id/content") // raw bytes

	// ------------------------------------------------------------------
	// Batches
	// ------------------------------------------------------------------
	add("POST", "/batches")
	add("GET", "/batches")
	add("GET", "/batches/:batch_id")
	add("POST", "/batches/:batch_id/cancel")

	// ------------------------------------------------------------------
	// Fine-tuning jobs. POST /fine_tuning/jobs has a `model` field but
	// still routes to the default backend: creation has stateful side
	// effects (the job record lives on a specific backend) and later
	// retrievals by job ID must hit the same backend that created it.
	// ------------------------------------------------------------------
	add("POST", "/fine_tuning/jobs")
	add("GET", "/fine_tuning/jobs")
	add("GET", "/fine_tuning/jobs/:job_id")
	add("POST", "/fine_tuning/jobs/:job_id/cancel")
	add("GET", "/fine_tuning/jobs/:job_id/events")
	add("GET", "/fine_tuning/jobs/:job_id/checkpoints")
	add("GET", "/fine_tuning/checkpoints/:checkpoint_id/permissions")
	add("POST", "/fine_tuning/checkpoints/:checkpoint_id/permissions")
	add("DELETE", "/fine_tuning/checkpoints/:checkpoint_id/permissions/:permission_id")

	// ------------------------------------------------------------------
	// Assistants
	// ------------------------------------------------------------------
	add("POST", "/assistants")
	add("GET", "/assistants")
	add("GET", "/assistants/:assistant_id")
	add("POST", "/assistants/:assistant_id") // modify
	add("DELETE", "/assistants/:assistant_id")

	// ------------------------------------------------------------------
	// Threads. Note: /v1/threads/runs (create thread + run) is a static
	// sibling of /v1/threads/:thread_id. Gin's radix tree accepts mixed
	// static + param children at the same level since v1.9, with static
	// taking precedence. If that ever regresses, migrate this whole
	// group to a wildcard "/threads/*path" dispatcher.
	// ------------------------------------------------------------------
	add("POST", "/threads")
	add("POST", "/threads/runs") // create thread + run in one call
	add("GET", "/threads/:thread_id")
	add("POST", "/threads/:thread_id") // modify
	add("DELETE", "/threads/:thread_id")
	add("POST", "/threads/:thread_id/messages")
	add("GET", "/threads/:thread_id/messages")
	add("GET", "/threads/:thread_id/messages/:message_id")
	add("POST", "/threads/:thread_id/messages/:message_id")
	add("POST", "/threads/:thread_id/runs")
	add("GET", "/threads/:thread_id/runs")
	add("GET", "/threads/:thread_id/runs/:run_id")
	add("POST", "/threads/:thread_id/runs/:run_id")
	add("POST", "/threads/:thread_id/runs/:run_id/cancel")
	add("POST", "/threads/:thread_id/runs/:run_id/submit_tool_outputs")
	add("GET", "/threads/:thread_id/runs/:run_id/steps")
	add("GET", "/threads/:thread_id/runs/:run_id/steps/:step_id")

	// ------------------------------------------------------------------
	// Vector Stores
	// ------------------------------------------------------------------
	add("POST", "/vector_stores")
	add("GET", "/vector_stores")
	add("GET", "/vector_stores/:vector_store_id")
	add("POST", "/vector_stores/:vector_store_id") // modify
	add("DELETE", "/vector_stores/:vector_store_id")
	add("POST", "/vector_stores/:vector_store_id/files")
	add("GET", "/vector_stores/:vector_store_id/files")
	add("GET", "/vector_stores/:vector_store_id/files/:file_id")
	add("DELETE", "/vector_stores/:vector_store_id/files/:file_id")
	add("POST", "/vector_stores/:vector_store_id/search")
	add("POST", "/vector_stores/:vector_store_id/file_batches")
	add("GET", "/vector_stores/:vector_store_id/file_batches/:batch_id")
	add("POST", "/vector_stores/:vector_store_id/file_batches/:batch_id/cancel")
	add("GET", "/vector_stores/:vector_store_id/file_batches/:batch_id/files")

	// ------------------------------------------------------------------
	// Uploads — multipart resumable upload flow.
	// ------------------------------------------------------------------
	add("POST", "/uploads")
	add("POST", "/uploads/:upload_id/parts")
	add("POST", "/uploads/:upload_id/complete")
	add("POST", "/uploads/:upload_id/cancel")

	// ------------------------------------------------------------------
	// Stored chat completions. POST /v1/chat/completions is the core
	// inference handler (not this file) and GET /v1/chat/completions is
	// a connectivity probe; we add only the per-id CRUD that OpenAI
	// added when `store: true` shipped. Side-by-side static + param
	// routes work here for the same gin radix tree reason the threads
	// block relies on.
	// ------------------------------------------------------------------
	add("GET", "/chat/completions/:completion_id")
	add("GET", "/chat/completions/:completion_id/messages")
	add("POST", "/chat/completions/:completion_id") // update metadata
	add("DELETE", "/chat/completions/:completion_id")

	// ------------------------------------------------------------------
	// Conversations (companion to the Responses API). Create has no
	// model field so it routes to the default backend like the rest of
	// the stateful block; retrieval/update/delete follow the same path.
	// Session affinity is not wired here because a single default
	// backend owns the conversation resource today — if multi-backend
	// conversations become a thing, reuse responseAffinity keyed by
	// conversation_id the same way /v1/responses does.
	// ------------------------------------------------------------------
	add("POST", "/conversations")
	add("GET", "/conversations/:conversation_id")
	add("POST", "/conversations/:conversation_id") // update
	add("DELETE", "/conversations/:conversation_id")
	add("GET", "/conversations/:conversation_id/items")
	add("POST", "/conversations/:conversation_id/items")
	add("GET", "/conversations/:conversation_id/items/:item_id")
	add("DELETE", "/conversations/:conversation_id/items/:item_id")

	// ------------------------------------------------------------------
	// Evals — CRUD on eval definitions, runs, and per-run output items.
	// ------------------------------------------------------------------
	add("POST", "/evals")
	add("GET", "/evals")
	add("GET", "/evals/:eval_id")
	add("POST", "/evals/:eval_id") // update
	add("DELETE", "/evals/:eval_id")
	add("POST", "/evals/:eval_id/runs")
	add("GET", "/evals/:eval_id/runs")
	add("GET", "/evals/:eval_id/runs/:run_id")
	add("POST", "/evals/:eval_id/runs/:run_id/cancel")
	add("DELETE", "/evals/:eval_id/runs/:run_id")
	add("GET", "/evals/:eval_id/runs/:run_id/output_items")
	add("GET", "/evals/:eval_id/runs/:run_id/output_items/:output_item_id")

	// ------------------------------------------------------------------
	// Containers (Code Interpreter sandbox used by the Responses API's
	// code_interpreter tool). File upload/download + container CRUD.
	// ------------------------------------------------------------------
	add("POST", "/containers")
	add("GET", "/containers")
	add("GET", "/containers/:container_id")
	add("DELETE", "/containers/:container_id")
	add("POST", "/containers/:container_id/files")
	add("GET", "/containers/:container_id/files")
	add("GET", "/containers/:container_id/files/:file_id")
	add("DELETE", "/containers/:container_id/files/:file_id")
	add("GET", "/containers/:container_id/files/:file_id/content")

	// ------------------------------------------------------------------
	// Videos (Sora / gpt-video). Pure pass-through: create/retrieve/
	// delete/extend/remix use JSON bodies, and GET /content streams
	// binary video bytes — proxyToBackend's non-streaming branch uses
	// io.Copy which relays the body chunk-by-chunk without buffering
	// the whole file, so multi-megabyte responses flow straight
	// through without blowing memory.
	// ------------------------------------------------------------------
	add("POST", "/videos")
	add("GET", "/videos/:video_id")
	add("DELETE", "/videos/:video_id")
	add("GET", "/videos/:video_id/content")
	add("POST", "/videos/:video_id/extend")
	add("POST", "/videos/:video_id/remix")

	return count
}
