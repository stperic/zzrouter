package server

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// RegistriesController surfaces registry-discovery endpoints intended for
// AI-agent consumption: chunky, decision-ready responses rather than raw
// API dumps, so a calling agent can pick a variant in one turn without
// regex-parsing filenames.
type RegistriesController struct {
	registry func() *modelregistry.Registry
}

// NewRegistriesController creates the controller.
func NewRegistriesController(reg func() *modelregistry.Registry) *RegistriesController {
	return &RegistriesController{registry: reg}
}

// RegisterPublicRoutes wires the /registries/* surface.
func (ctrl *RegistriesController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.GET("/registries/huggingface/variants", ctrl.ListHuggingFaceVariants)
}

// GGUFVariant is one pickable GGUF file in a HuggingFace repo, with the
// parsed metadata an agent needs to choose between quantizations without
// scraping filenames. `File` is the exact string to echo back to
// POST /pulls's `file` field.
type GGUFVariant struct {
	File                 string `json:"file"`
	Quantization         string `json:"quantization"`
	Bits                 int    `json:"bits"`
	SizeBytes            int64  `json:"size_bytes"`
	SizeHuman            string `json:"size_human"`
	Recommended          bool   `json:"recommended,omitempty"`
	RecommendationReason string `json:"recommendation_reason,omitempty"`
}

// HuggingFaceVariantsResponse is the response envelope. Counts (not lists)
// for non-GGUF files keeps the payload focused on the decision-making
// surface while still signalling "this repo has docs/configs too".
type HuggingFaceVariantsResponse struct {
	ModelID        string        `json:"model_id"`
	Variants       []GGUFVariant `json:"variants"`
	NonGGUFFiles   int           `json:"non_gguf_files"`
	TotalSizeBytes int64         `json:"total_size_bytes"`
}

// ListHuggingFaceVariants handles GET /registries/huggingface/variants?id=<org/repo>.
// The `id` goes in a query param (not the path) because HuggingFace IDs
// contain a slash and path-escaping round-trips badly through some
// proxies and client SDKs.
func (ctrl *RegistriesController) ListHuggingFaceVariants(c *gin.Context) {
	modelID := strings.TrimSpace(c.Query("id"))
	if modelID == "" {
		BadRequest(c, "query parameter 'id' is required (e.g., unsloth/SmolLM2-135M-Instruct-GGUF)")
		return
	}
	if !strings.Contains(modelID, "/") {
		BadRequest(c, fmt.Sprintf("HuggingFace model id must be in 'org/repo' form, got %q", modelID))
		return
	}

	reg := ctrl.registry()
	if reg == nil {
		RespondToError(c, newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "model registry not initialized"))
		return
	}

	files, err := reg.ListHuggingFaceRepoFiles(c.Request.Context(), modelID)
	if err != nil {
		RespondToError(c, newProblemError(http.StatusBadGateway, "Bad Gateway",
			fmt.Sprintf("fetch %s: %s", modelID, err)))
		return
	}

	resp := buildVariantsResponse(modelID, files)
	respondSuccess(c, "HuggingFace variants retrieved", resp)
}

// buildVariantsResponse is pure — no HTTP, no registry — for easy testing.
func buildVariantsResponse(modelID string, files []metadata.TreeFileEntry) HuggingFaceVariantsResponse {
	out := HuggingFaceVariantsResponse{ModelID: modelID}
	for _, f := range files {
		out.TotalSizeBytes += f.Size
		if !isGGUF(f.Name) {
			out.NonGGUFFiles++
			continue
		}
		quant, bits := parseQuant(f.Name)
		out.Variants = append(out.Variants, GGUFVariant{
			File:         f.Name,
			Quantization: quant,
			Bits:         bits,
			SizeBytes:    f.Size,
			SizeHuman:    humanizeBytes(f.Size),
		})
	}
	markRecommended(out.Variants)
	return out
}

func isGGUF(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".gguf")
}

// quantPattern matches the last quant tag in a filename (Q4_K_M, IQ3_XS,
// Q8_0, F16, F32, BF16). Anchored at a dash or dot boundary so model
// names that happen to contain "Q4" don't false-match.
var quantPattern = regexp.MustCompile(`(?i)[.\-_](IQ\d_[A-Z]+|Q\d(?:_[A-Z0-9]+)*|BF16|F16|F32)(?:[.\-_]|$)`)

// parseQuant extracts the quantization tag and a coarse bit count.
// Returns ("", 0) when the pattern doesn't match — callers should treat
// that as "unknown, don't surface as a quant choice".
func parseQuant(filename string) (tag string, bits int) {
	base := strings.TrimSuffix(filename, ".gguf")
	m := quantPattern.FindStringSubmatch(base)
	if len(m) < 2 {
		return "", 0
	}
	tag = strings.ToUpper(m[1])
	switch {
	case strings.HasPrefix(tag, "IQ2"), strings.HasPrefix(tag, "Q2"):
		bits = 2
	case strings.HasPrefix(tag, "IQ3"), strings.HasPrefix(tag, "Q3"):
		bits = 3
	case strings.HasPrefix(tag, "IQ4"), strings.HasPrefix(tag, "Q4"):
		bits = 4
	case strings.HasPrefix(tag, "Q5"):
		bits = 5
	case strings.HasPrefix(tag, "Q6"):
		bits = 6
	case strings.HasPrefix(tag, "Q8"):
		bits = 8
	case tag == "F16", tag == "BF16":
		bits = 16
	case tag == "F32":
		bits = 32
	}
	return tag, bits
}

// markRecommended picks a single "safe default" variant for agents that
// lack a preference. Heuristic: prefer Q4_K_M (de-facto balanced pick);
// fall back to the smallest file at bits >= 4 to avoid quality cliffs
// of Q2/Q3; fall back to the largest non-F32 variant if nothing else
// qualifies. Only ever sets one variant's Recommended=true.
func markRecommended(variants []GGUFVariant) {
	if len(variants) == 0 {
		return
	}
	for i := range variants {
		if strings.EqualFold(variants[i].Quantization, "Q4_K_M") {
			variants[i].Recommended = true
			variants[i].RecommendationReason = "balanced quality/size (Q4_K_M)"
			return
		}
	}
	bestIdx := -1
	for i, v := range variants {
		if v.Bits < 4 {
			continue
		}
		if bestIdx == -1 || v.SizeBytes < variants[bestIdx].SizeBytes { //nolint:gosec // short-circuit: bestIdx used only after -1 guard
			bestIdx = i
		}
	}
	if bestIdx >= 0 {
		variants[bestIdx].Recommended = true                                                       //nolint:gosec // bestIdx < len(variants) by construction
		variants[bestIdx].RecommendationReason = "smallest >=4-bit variant (Q4_K_M not available)" //nolint:gosec // bestIdx < len(variants) by construction
		return
	}
	// Everything is <4 bits (unusual). Pick the largest so the agent
	// gets the best-quality option available rather than a deep-quant
	// surprise.
	bestIdx = 0
	for i, v := range variants {
		if v.SizeBytes > variants[bestIdx].SizeBytes { //nolint:gosec // bestIdx=0 initialized above, len(variants)>0 by caller
			bestIdx = i
		}
	}
	variants[bestIdx].Recommended = true                                                         //nolint:gosec // len(variants) > 0 checked above
	variants[bestIdx].RecommendationReason = "largest available (only sub-4-bit quants offered)" //nolint:gosec // len(variants) > 0 checked above
}

func humanizeBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
