package views

import (
	"fmt"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
)

func (m *VkeysViewModel) viewDetail(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	d := shared.NewDetail(m.styles, width)

	k := m.selectedKey()
	if k == nil {
		d.Write(" No key selected\n")
		return d.String()
	}

	// --- Configuration ---
	d.Section("Configuration")
	d.Field("ID", k.ID)
	d.Field("Name", k.Name)
	d.Field("Role", k.Role)

	status := "active"
	if k.Suspended {
		if k.SuspendedBy != "" {
			status = fmt.Sprintf("suspended (by %s)", k.SuspendedBy)
		} else {
			status = "suspended"
		}
	}
	d.Field("Status", status)

	if k.ExpiresAt != nil {
		exp := *k.ExpiresAt
		if k.IsExpired {
			exp += " (expired)"
		}
		d.Field("Expires", exp)
	} else {
		d.Field("Expires", shared.EmptyValue)
	}

	d.Field("Team", k.TeamID+" ("+k.TeamRole+")")

	// --- Rate Limits ---
	d.Section("Rate Limits")

	rpm := "unlimited"
	if k.RPMLimit > 0 {
		rpm = fmt.Sprintf("%d req/min", k.RPMLimit)
	}
	d.Field("RPM Limit", rpm)

	tpm := "unlimited"
	if k.TPMLimit > 0 {
		tpm = fmt.Sprintf("%d tok/min", k.TPMLimit)
	}
	d.Field("TPM Limit", tpm)

	concurrency := "unlimited"
	if k.MaxParallelRequests > 0 {
		concurrency = fmt.Sprintf("%d parallel requests", k.MaxParallelRequests)
	}
	d.Field("Concurrency", concurrency)

	// --- Budget ---
	if k.SpendLimit > 0 {
		d.Section("Budget")
		period := k.ResetPeriod
		if period == "" {
			period = "monthly"
		}
		d.Field("Period", period)
		d.Field("Max Budget", fmt.Sprintf("$%.2f", k.SpendLimit))

		if m.usageLoading {
			d.Write("  Loading usage...\n")
		} else if m.usage != nil {
			d.Field("Current Spend", fmt.Sprintf("$%.2f", m.usage.SpendUSD))
			pct := (m.usage.SpendUSD / k.SpendLimit) * 100
			bar := shared.RenderBudgetBar(pct, width-6)
			d.Write("  " + bar + "\n")
		}
	}

	// --- Usage ---
	d.Section("Usage")
	if m.usageLoading {
		d.Write("  Loading...\n")
	} else if m.usageErr != nil {
		d.Write("  " + m.styles.Help.Render("Could not load usage") + "\n")
	} else if m.usage != nil {
		d.Field("Requests", fmt.Sprintf("%d", m.usage.RequestCount))
		d.Field("Tokens In", shared.FormatTokenCount(m.usage.TokensIn))
		d.Field("Tokens Out", shared.FormatTokenCount(m.usage.TokensOut))
		d.Field("Spend", fmt.Sprintf("$%.2f", m.usage.SpendUSD))
	} else {
		d.Write("  " + m.styles.Help.Render("No usage data") + "\n")
	}

	// --- Metadata ---
	if len(k.Metadata) > 0 {
		d.Section("Metadata")
		for mk, mv := range k.Metadata {
			d.Field(mk, mv)
		}
	}

	// Confirmation dialogs
	if m.confirmPending {
		switch m.confirmAction {
		case "rotate":
			d.Write("\n")
			d.Text("  ", m.styles.Error.Render("Rotate key? Old key will stop working immediately. (y/n)"))
		case "delete":
			d.Write("\n")
			d.Text("  ", m.styles.Error.Render(fmt.Sprintf("Delete key %q? (y/n)", k.ID)))
		case "reset-usage":
			d.Write("\n")
			d.Text("  ", m.styles.Error.Render("Reset usage counters? (y/n)"))
		case "suspend":
			d.Write("\n")
			d.Text("  ", m.styles.Error.Render(fmt.Sprintf("Suspend key %q? Requests using it will be rejected immediately. (y/n)", k.ID)))
		case "unsuspend":
			d.Write("\n")
			d.Text("  ", m.styles.Error.Render(fmt.Sprintf("Unsuspend key %q? (y/n)", k.ID)))
		}
	}

	if m.statusMsg != "" {
		d.Write("\n")
		d.Text("  ", m.styles.Error.Render(m.statusMsg))
	}

	d.Write("\n" + shared.RenderViewFooter(m.styles, width, tui.VkeysKeys.DetailHintsString()))

	return d.String()
}
