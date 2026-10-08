package llm

import (
	"sync"
	"sync/atomic"
)

// InferenceLogHook is called when an inference request completes.
// Implementations must be safe for concurrent use.
type InferenceLogHook interface {
	OnInferenceComplete(data InferenceLogData)
}

// CostSetter is the narrow surface a hook needs to push a resolved
// (cost, source) back onto the recorder after pricing-store fallback.
// Decouples hook implementations from the full recorder mutation
// surface (single consumer today: InferenceLogBridge).
type CostSetter interface {
	SetCost(cost float64, source string)
}

// InferenceLogData contains the data collected during an inference request,
// passed to the log hook for recording.
type InferenceLogData struct {
	Model string
	// ResponseModel is the model the upstream backend reported actually
	// using — distinct from Model when model-group routing resolves a
	// group alias to a concrete deployment, or when a backend
	// auto-versions (OpenAI's "gpt-4-turbo" → "gpt-4-turbo-2024-04-09").
	// Surfaced on the inference log so audit consumers can identify
	// which deployment served each request.
	ResponseModel   string
	App             string
	RequestType     string
	Stream          bool
	RoutingDecision string
	Node            string
	TokensIn        int64
	TokensOut       int64
	TokensCached    int64   // Prompt tokens served from KV cache
	TokensReasoning int64   // Tokens used for reasoning/thinking
	Cost            float64 // USD cost (from cloud providers)
	CostSource      string  // "" | "provider" | "zzrouter" — where Cost came from
	LatencyNs       int64
	TTFTNs          int64
	Status          string
	ErrorType       string
	ErrorMessage    string
	GroupName       string // Model group alias (e.g., "fast-chat")
	DeploymentName  string // Deployment that served the request
	FallbackCount   int    // Number of deployments tried before success
	FallbackFrom    string // First attempted deployment (if fallback occurred)
	KeyID           string // Virtual key ID (empty for static/anonymous)
	KeyAlias        string // LiteLLM `api_key_alias` — human-readable Name
	HashedKey       string // LiteLLM `hashed_api_key` — first4..last4 fingerprint
	TeamID          string // Team ID (empty if the key has no team)
	TeamAlias       string // LiteLLM `team_alias` — team Name
	ReservationID   uint64 // Stash ID returned by Enforce; zero = no reservation
	RequestBody     []byte // Raw request JSON for prompt extraction
	UpstreamBody    []byte // Request JSON as sent to the provider
	ResponseBody    []byte // Raw reply JSON; empty for streaming replies
	ResponseText    string // The assistant's reply, reassembled if streamed

	// CostSetter pushes a resolved (cost, source) back onto the live
	// recorder after pricing-store fallback so the (post-
	// RecordCompletion) response inject path observes the upgraded
	// value instead of the raw upstream-only one. May be nil in tests
	// that fire OnInferenceComplete directly.
	CostSetter CostSetter
}

var (
	logHookMu sync.RWMutex
	logHook   InferenceLogHook
)

// SetInferenceLogHook registers a global hook that is called on each inference completion.
func SetInferenceLogHook(hook InferenceLogHook) {
	logHookMu.Lock()
	logHook = hook
	logHookMu.Unlock()
}

// getInferenceLogHook returns the current log hook, or nil if none is set.
func getInferenceLogHook() InferenceLogHook {
	logHookMu.RLock()
	h := logHook
	logHookMu.RUnlock()
	return h
}

// maxResponseTextBytes caps the reply text kept per entry. The metadata
// ring holds 1000 of them, so an uncapped generation would dominate it.
const maxResponseTextBytes = 32 * 1024

// captureResponses gates reply capture. Off by default so a node that
// disables prompt capture pays neither the storage nor the per-chunk
// parse on the streaming path.
var captureResponses atomic.Bool

// SetCaptureResponses enables reply capture. Called once at startup from
// the same config that gates prompt capture.
func SetCaptureResponses(enabled bool) { captureResponses.Store(enabled) }

// CaptureResponses reports whether replies are being captured. Wire-level
// callers check it before parsing a chunk they would otherwise discard.
func CaptureResponses() bool { return captureResponses.Load() }
