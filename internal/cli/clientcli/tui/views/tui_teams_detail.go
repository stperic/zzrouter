package views

import (
	"fmt"
	"sort"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
)

func (m *TeamsViewModel) viewDetail(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	d := shared.NewDetail(m.styles, width)

	t := m.selectedTeam()
	if t == nil {
		d.Write(" No team selected\n")
		return d.String()
	}

	// --- Configuration ---
	d.Section("Configuration")
	d.Field("ID", t.ID)
	d.Field("Name", t.Name)
	kind := t.Kind
	if kind == "" {
		kind = "shared"
	}
	d.Field("Kind", kind)
	if t.Suspended {
		if t.SuspendedBy != "" {
			d.Field("Status", fmt.Sprintf("suspended (by %s)", t.SuspendedBy))
		} else {
			d.Field("Status", "suspended")
		}
	} else {
		d.Field("Status", "active")
	}

	if len(t.AllowedModels) > 0 {
		d.Field("Allowed Models", strings.Join(t.AllowedModels, ", "))
	} else {
		d.Field("Allowed Models", "all")
	}

	// --- Rate Limits ---
	d.Section("Rate Limits")

	rpm := "unlimited"
	if t.RPMLimit > 0 {
		rpm = fmt.Sprintf("%d req/min", t.RPMLimit)
	}
	d.Field("RPM Limit", rpm)

	tpm := "unlimited"
	if t.TPMLimit > 0 {
		tpm = fmt.Sprintf("%d tok/min", t.TPMLimit)
	}
	d.Field("TPM Limit", tpm)

	concurrency := "unlimited"
	if t.MaxParallelRequests > 0 {
		concurrency = fmt.Sprintf("%d parallel requests", t.MaxParallelRequests)
	}
	d.Field("Concurrency", concurrency)

	// --- Budget ---
	if t.SpendLimit > 0 {
		d.Section("Budget")
		period := t.ResetPeriod
		if period == "" {
			period = "monthly"
		}
		d.Field("Period", period)
		d.Field("Max Budget", fmt.Sprintf("$%.2f", t.SpendLimit))

		if m.usageLoading {
			d.Write("  Loading usage...\n")
		} else if m.usage != nil {
			d.Field("Current Spend", fmt.Sprintf("$%.2f", m.usage.SpendUSD))
			pct := (m.usage.SpendUSD / t.SpendLimit) * 100
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

	// --- Keys in this team ---
	d.Section("Keys")
	members := keysInTeam(m.availableKeys, t.ID)
	if len(members) == 0 {
		d.Write("  " + m.styles.Help.Render("(no keys)") + "\n")
	} else {
		for _, k := range members {
			label := k.ID
			if k.Name != "" {
				label = fmt.Sprintf("%s (%s)", k.ID, k.Name)
			}
			d.Field(label, k.TeamRole)
		}
	}

	// --- Metadata ---
	if len(t.Metadata) > 0 {
		d.Section("Metadata")
		metaKeys := make([]string, 0, len(t.Metadata))
		for k := range t.Metadata {
			metaKeys = append(metaKeys, k)
		}
		sort.Strings(metaKeys)
		for _, mk := range metaKeys {
			d.Field(mk, t.Metadata[mk])
		}
	}

	// Confirmation dialogs
	if m.confirmPending {
		switch m.confirmAction {
		case "delete":
			d.Write("\n")
			d.Text("  ", m.styles.Error.Render(fmt.Sprintf("Delete team %q? (y/n)", t.ID)))
		case "reset-usage":
			d.Write("\n")
			d.Text("  ", m.styles.Error.Render("Reset usage counters? (y/n)"))
		case "suspend":
			d.Write("\n")
			d.Text("  ", m.styles.Error.Render(fmt.Sprintf("Suspend team %q? Every key in the team will be blocked. (y/n)", t.ID)))
		case "unsuspend":
			d.Write("\n")
			d.Text("  ", m.styles.Error.Render(fmt.Sprintf("Unsuspend team %q? (y/n)", t.ID)))
		}
	}

	if m.statusMsg != "" {
		d.Write("\n")
		d.Text("  ", m.styles.Error.Render(m.statusMsg))
	}

	d.Write("\n" + shared.RenderViewFooter(m.styles, width, tui.TeamsKeys.DetailHintsString()))

	return d.String()
}
