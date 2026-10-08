package views

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// Per-model usage for the model detail pane.
//
// These totals are attribution-free: they come off the request record itself,
// so a locally deployed model that nobody authenticates against still reports
// real numbers. The window is node start, which is what the totals accumulate
// from — there is no earlier point they could be reported from.

// modelUsageMsg carries fetched usage for one model.
//
// idle marks the case where the node has simply served nothing for this model
// yet. It is kept apart from err because a panel that renders "we could not
// reach the node" as "no requests" is the same lie as rendering a failed
// version check as "up to date".
type modelUsageMsg struct {
	model string
	usage *pkgClient.ModelUsageResponse
	idle  bool
	err   error
}

// fetchModelUsage loads running usage for a model. An empty provider sums
// every provider serving that name, which is what "this model" means when the
// row is not scoped to one deployment.
func (m *ModelsViewModel) fetchModelUsage(model, provider string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		usage, err := client.GetModelUsage(model, provider)
		if errors.Is(err, pkgClient.ErrNoUsageRecorded) {
			return modelUsageMsg{model: model, idle: true}
		}
		return modelUsageMsg{model: model, usage: usage, err: err}
	}
}

// usageDetailLines renders the usage section of the model detail pane.
//
// Cost appears only when the model is priced. Self-hosted engines have no
// pricing, and a $0.00 there would read as a broken number rather than as
// free, so the line is replaced by a short statement of why.
func usageDetailLines(resp *pkgClient.ModelUsageResponse) [][2]string {
	if resp == nil || resp.Model == nil {
		return nil
	}
	u := resp.Model

	out := [][2]string{
		{"Since", formatUptime(resp.UptimeSecs) + " ago (node start)"},
		{"Requests", formatUsageCount(u.Requests)},
	}
	if u.Errors > 0 {
		out = append(out, [2]string{"Errors", formatUsageCount(u.Errors)})
	}

	out = append(out,
		[2]string{"Tokens in", formatUsageCount(u.TokensIn)},
		[2]string{"Tokens out", formatUsageCount(u.TokensOut)},
	)

	if u.AvgTokensPerSec > 0 {
		throughput := fmt.Sprintf("%.1f tok/s avg", u.AvgTokensPerSec)
		if u.PeakTokensPerSec > 0 {
			throughput += fmt.Sprintf(", %.1f peak", u.PeakTokensPerSec)
		}
		out = append(out, [2]string{"Throughput", throughput})
	}

	if u.Priced {
		out = append(out, [2]string{"Cost", fmt.Sprintf("$%.4f", u.CostUSD)})
	} else {
		out = append(out, [2]string{"Cost", "not priced (self-hosted)"})
	}

	if len(u.Nodes) > 1 {
		out = append(out, [2]string{"Nodes", strings.Join(u.Nodes, ", ")})
	}

	return out
}

// formatUsageCount renders a count with thousands separators, because these
// run to seven figures on a busy node and unseparated digits are unreadable.
func formatUsageCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}

	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// formatUptime renders a duration in the coarsest useful unit. Sub-second
// precision is noise for a window measured in hours.
func formatUptime(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// usageProviderFor returns the provider to scope usage to. A deployment row
// names one engine; a bare registry row does not, and summing every provider
// serving the name is the honest answer there.
func usageProviderFor(r *modelRow) string {
	if r == nil {
		return ""
	}
	return r.Provider
}

// idleUsageLines renders the panel for a model the node has served nothing
// for. Stating it explicitly beats omitting the section: a model page with no
// usage block at all reads as a missing feature rather than as no traffic.
func idleUsageLines() [][2]string {
	return [][2]string{{"Requests", "0 since restart"}}
}
