// POST /v1/rerank — rerank endpoint for retrieval pipelines.
//
// Rerank is a standard inference operation for RAG: given a query and
// a list of candidate passages, return the passages sorted by
// relevance with a score attached. zzrouter exposes it on /v1/rerank
// next to embeddings, speaking the Cohere-shaped request/response
// body that most rerank-capable backends already accept.
//
// The handler extracts `model` and hands off to the shared
// model-field routing path; zzrouter does not re-implement rerank.
package server

import "github.com/gin-gonic/gin"

func (h *OpenAIInferenceHandlers) handleRerank(c *gin.Context) {
	h.routeByModelField(c, "reranking")
}
