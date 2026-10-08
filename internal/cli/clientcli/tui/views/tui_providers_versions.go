package views

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// Upstream release reporting for the providers view.
//
// The column pairs the installed version with the newest one upstream
// publishes. Both are rendered as raw upstream tags: the pin is stored raw
// ("b10453") while the normalized version has strip_prefix applied ("10502"),
// so pairing the two forms would render "b10453 -> 10502".
//
// A failed or absent check renders nothing rather than an "up to date" that
// was never verified.

// versionStatus mirrors the closed enum the API returns.
const (
	versionStatusNewer   = "newer"
	versionStatusSame    = "same"
	versionStatusOlder   = "older"
	versionStatusUnknown = "unknown"
)

// providerVersionsMsg carries a fetched upstream report.
type providerVersionsMsg struct {
	provider string
	report   *pkgClient.ProviderVersionsResponse
	err      error
}

// fetchProviderVersions loads the upstream report for one provider.
func (m *ProvidersViewModel) fetchProviderVersions(provider string, refresh bool) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		report, err := client.GetProviderVersions(provider, refresh)
		return providerVersionsMsg{provider: provider, report: report, err: err}
	}
}

// latestForDisplay renders the upstream version in the same shape as the
// installed one, so the two sides of the arrow are comparable at a glance.
//
// The list path gets this shape from the server, which knows both forms. This
// local copy serves the detail path, which reads the versions endpoint
// directly and so has to choose for itself.
func latestForDisplay(installed, latestTag, latestVersion string) string {
	if latestTag == "" {
		return latestVersion
	}
	if latestVersion == "" || latestTag == latestVersion {
		return latestTag
	}
	// Whatever the tag carries beyond the normalized version is the prefix
	// strip_prefix removes: "v" for ollama, "b" for llama.cpp.
	prefix := strings.TrimSuffix(latestTag, latestVersion)
	if prefix != "" && strings.HasPrefix(installed, prefix) {
		return latestTag
	}
	return latestVersion
}

// versionCell renders the VERSION column: the installed version, plus an
// arrow to the newest upstream release when one is genuinely newer.
//
// Only "newer" earns the arrow. "same" needs no annotation, and "older" or
// "unknown" would be noise in a list whose job is to show what is running.
func versionCell(app shared.ProviderInfo) string {
	installed := app.Version
	if installed == "" {
		installed = "unknown"
	}
	if app.VersionStatus != versionStatusNewer || app.LatestVersion == "" {
		return installed
	}
	return fmt.Sprintf("%s → %s", installed, app.LatestVersion)
}

// upgradeTargetFor returns the version the upgrade prompt should offer for a
// provider, or "" when there is nothing newer.
//
// "" is left for the server to resolve rather than guessed at here: the
// coordinator resolves a blank upgrade to the newest upstream release, and
// re-deriving that from a possibly stale cached row would be a second
// answer to the same question.
func upgradeTargetFor(app shared.ProviderInfo) string {
	if app.VersionStatus != versionStatusNewer {
		return ""
	}
	return app.LatestVersion
}

// applyVersionsReport folds a fetched report into the cached provider rows so
// the list column and the detail pane agree.
func (m *ProvidersViewModel) applyVersionsReport(msg providerVersionsMsg) {
	if msg.err != nil || msg.report == nil {
		return
	}
	m.versionsReport = msg.report

	for i := range m.providers {
		if m.providers[i].Name != msg.provider {
			continue
		}
		latest, status := versionForNode(msg.report, m.providers[i].Node, m.providers[i].Version)
		m.providers[i].LatestVersion = latest
		m.providers[i].VersionCheckedAt = msg.report.CheckedAt
		m.providers[i].VersionStatus = status
	}
}

// versionForNode returns the ceiling a node answers to and how its installed
// version compares, falling back to the pin comparison when the report
// carries no row for that node.
//
// A node's own ceiling wins over the headline. The headline is what the
// project published; a node installing through a packager is capped by that
// packager instead, and the upgrade prompt is prefilled from this value. Take
// the headline there and the prompt offers a release the node's installer
// cannot fetch, which is the failure this whole distinction exists to stop.
//
// The display form is chosen against the node's installed version so the
// arrow never mixes prefixed and bare forms.
func versionForNode(report *pkgClient.ProviderVersionsResponse, node, installed string) (latest, status string) {
	headline := latestForDisplay(installed, report.LatestTag, report.Latest)
	for _, n := range report.Nodes {
		if n.Node != node {
			continue
		}
		if n.InstallableLatest != "" {
			return n.InstallableLatest, n.Status
		}
		return headline, n.Status
	}
	return headline, report.PinnedStatus
}

// versionDetailLines renders the upstream section of the provider detail
// pane. Returns nil when there is nothing meaningful to say yet.
func versionDetailLines(report *pkgClient.ProviderVersionsResponse) [][2]string {
	if report == nil {
		return nil
	}

	var out [][2]string
	if report.Pinned != "" {
		out = append(out, [2]string{"Pinned", report.Pinned + "  (seeds new installs)"})
	}

	if report.Latest == "" {
		// Say why rather than leaving a blank that reads as "up to date".
		reason := report.Reason
		if reason == "" {
			reason = "no upstream check has completed"
		}
		out = append(out, [2]string{"Upstream", "unknown: " + reason})
		return out
	}

	latest := report.LatestTag
	switch report.PinnedStatus {
	case versionStatusNewer:
		latest += "  (newer than the pin)"
	case versionStatusSame:
		latest += "  (matches the pin)"
	case versionStatusOlder:
		latest += "  (pin is ahead of upstream)"
	}
	out = append(out, [2]string{"Upstream", latest})

	if report.Source != nil {
		src := report.Source.Type
		switch {
		case report.Source.Repo != "":
			src += "  " + report.Source.Repo
		case report.Source.Package != "":
			src += "  " + report.Source.Package
		}
		out = append(out, [2]string{"Source", src})
	}
	if report.ReleaseURL != "" {
		out = append(out, [2]string{"Release", report.ReleaseURL})
	}
	if report.CheckedAt != "" {
		out = append(out, [2]string{"Checked", report.CheckedAt})
	}

	if len(report.Nodes) > 0 {
		var parts []string
		for _, n := range report.Nodes {
			installed := n.Installed
			if installed == "" {
				installed = "unknown"
			}
			parts = append(parts, fmt.Sprintf("%s %s (%s)", n.Node, installed, n.Status))
		}
		out = append(out, [2]string{"Installed", strings.Join(parts, ", ")})
	}

	return out
}
