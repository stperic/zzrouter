package quota

import (
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Usage is the current budget window, distinct from an invoice or lifetime usage.
type Usage struct {
	SpendUSD     float64  `json:"spend_usd"`
	ReservedUSD  float64  `json:"reserved_usd,omitempty"`
	RemainingUSD *float64 `json:"remaining_usd,omitempty"`
	Tracked      bool     `json:"tracked"`
	TokensIn     int64    `json:"tokens_in"`
	TokensOut    int64    `json:"tokens_out"`
	RequestCount int64    `json:"request_count"`
	PeriodStart  string   `json:"period_start,omitempty"`
	Period       string   `json:"period,omitempty"`
	ResetAt      string   `json:"reset_at,omitempty"`
	SpendLimit   float64  `json:"spend_limit,omitempty"`
	RPMLimit     int      `json:"rpm_limit,omitempty"`
	TPMLimit     int      `json:"tpm_limit,omitempty"`
}

// CurrentUsage projects the same lazy window rollover as budget admission.
// It never rewrites the ledger or invents history for an untracked scope.
func CurrentUsage(q QuotaConfig, state *SpendState) Usage {
	period := ParsePeriod(q.ResetPeriod)
	start := PeriodStartFor(utils.NowUTC(), period)
	u := Usage{SpendLimit: q.SpendLimit, RPMLimit: q.RPMLimit, TPMLimit: q.TPMLimit, Tracked: q.SpendLimit > 0 && state != nil}
	if state != nil && !IsExpired(state.PeriodStart, state.Period) {
		period, start = state.Period, state.PeriodStart
		u.SpendUSD, u.ReservedUSD = state.SpendUSD(), MicroToUSD(state.ReservedMicro)
		u.TokensIn, u.TokensOut, u.RequestCount = state.TokensIn, state.TokensOut, state.RequestCount
	}
	u.Period = string(period)
	if state != nil {
		u.PeriodStart = start.Format(time.RFC3339)
	}
	if q.SpendLimit > 0 {
		u.ResetAt = NextPeriodStart(start, period).Format(time.RFC3339)
		if u.Tracked {
			remaining := max(0, q.SpendLimit-u.SpendUSD-u.ReservedUSD)
			u.RemainingUSD = &remaining
		}
	}
	return u
}
