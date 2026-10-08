package server

import (
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ProviderStatusController exposes provider-level health/cooldown status.
type ProviderStatusController struct {
	appsConfig        func() *pkgConfig.AppsConfig // Always returns current provider config
	providerCooldowns *fallback.CooldownManager
	rateLimitTracker  *fallback.RateLimitTracker
}

// NewProviderStatusController creates a new provider status controller.
func NewProviderStatusController(
	appsConfig func() *pkgConfig.AppsConfig,
	providerCooldowns *fallback.CooldownManager,
	rateLimitTracker *fallback.RateLimitTracker,
) *ProviderStatusController {
	return &ProviderStatusController{
		appsConfig:        appsConfig,
		providerCooldowns: providerCooldowns,
		rateLimitTracker:  rateLimitTracker,
	}
}

// RegisterPublicRoutes registers provider status routes on the public API.
func (ctrl *ProviderStatusController) RegisterPublicRoutes(router *gin.RouterGroup) {
	router.GET("/providers/status", ctrl.ListProviderStatus)
}

// rateLimitResponse is the API representation of the last-seen rate limit headers.
type rateLimitResponse struct {
	LimitRequests        int     `json:"limit_requests,omitempty"`
	RemainingRequests    int     `json:"remaining_requests,omitempty"`
	LimitTokens          int     `json:"limit_tokens,omitempty"`
	RemainingTokens      int     `json:"remaining_tokens,omitempty"`
	ResetRequestsSeconds float64 `json:"reset_requests_seconds,omitempty"`
	ResetTokensSeconds   float64 `json:"reset_tokens_seconds,omitempty"`
	ObservedAt           string  `json:"observed_at"`          // RFC3339
	ObservedAgoSeconds   float64 `json:"observed_ago_seconds"` // seconds since observation
}

// providerStatusResponse is the API representation of a single provider's status.
type providerStatusResponse struct {
	Name            string             `json:"name"`
	Status          string             `json:"status"`                     // "ok" or "cooldown"
	CooldownSeconds float64            `json:"cooldown_seconds,omitempty"` // remaining cooldown (0 if ok)
	CooldownReason  string             `json:"cooldown_reason,omitempty"`  // "rate_limit", "quota", "unavailable", "transport"
	RateLimit       *rateLimitResponse `json:"rate_limit,omitempty"`       // last-seen rate limit snapshot
}

// ListProviderStatus handles GET /zzrouter/v1/providers/status
func (ctrl *ProviderStatusController) ListProviderStatus(c *gin.Context) {
	// Every other list endpoint honors limit/offset through this pair.
	// This one accepted ?limit and returned everything anyway, so a
	// caller paging through providers silently got the full set and no
	// way to tell that it had.
	params, ok := ParsePagination(c)
	if !ok {
		return
	}

	cfg := ctrl.appsConfig()
	if cfg == nil {
		respondList(c, []providerStatusResponse{}, 0, false)
		return
	}

	now := utils.Now()

	// Collect all configured apps
	names := cfg.AppNames()
	sort.Strings(names)

	result := make([]providerStatusResponse, 0, len(names))
	for _, name := range names {
		app, ok := cfg.LookupApp(name)
		if !ok || !app.IsEnabled() {
			continue
		}

		resp := providerStatusResponse{
			Name:   name,
			Status: "ok",
		}

		if ctrl.providerCooldowns != nil {
			if remaining, reason := ctrl.providerCooldowns.GetEntry(name); remaining > 0 {
				resp.Status = "cooldown"
				resp.CooldownSeconds = remaining.Seconds()
				resp.CooldownReason = reason
			}
		}

		if ctrl.rateLimitTracker != nil {
			if snap, ok := ctrl.rateLimitTracker.Get(name); ok {
				resp.RateLimit = &rateLimitResponse{
					LimitRequests:        snap.LimitRequests,
					RemainingRequests:    snap.RemainingRequests,
					LimitTokens:          snap.LimitTokens,
					RemainingTokens:      snap.RemainingTokens,
					ResetRequestsSeconds: snap.ResetRequestsSeconds,
					ResetTokensSeconds:   snap.ResetTokensSeconds,
					ObservedAt:           snap.ObservedAt.Format(time.RFC3339),
					ObservedAgoSeconds:   now.Sub(snap.ObservedAt).Seconds(),
				}
			}
		}

		result = append(result, resp)
	}

	page, total, hasMore := ApplyPagination(result, params)
	respondList(c, page, total, hasMore)
}
