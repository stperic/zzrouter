package views

import (
	"fmt"
	"sort"
	"strings"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// teamPolicyLines describes stored API policies without calculating access or
// combining limits in the UI. Unknown modes and saved identities stay visible.
func teamPolicyLines(team *pkgClient.TeamResponse) []string {
	providers := "All local and cloud providers"
	switch team.ProviderAccess.Mode {
	case "", "all":
	case "none":
		providers = "No providers"
	case "selected":
		providers = strings.Join(team.ProviderAccess.Providers, ", ")
	default:
		providers = "Unknown policy: " + team.ProviderAccess.Mode
	}
	routes := "Direct models and named routes"
	switch team.RouteAccess.Mode {
	case "", "direct":
	case "none":
		routes = "No routes"
	case "selected":
		routes = "Only " + strings.Join(team.RouteAccess.Routes, ", ")
	default:
		routes = "Unknown policy: " + team.RouteAccess.Mode
	}
	models := "All models"
	if len(team.AllowedModels) > 0 {
		models = strings.Join(team.AllowedModels, ", ")
	}
	budget := "No team spending limit"
	if team.SpendLimit > 0 {
		period := team.ResetPeriod
		if period == "" {
			period = "monthly"
		}
		budget = fmt.Sprintf("$%.2f / %s", team.SpendLimit, period)
	}
	return []string{
		"Providers: " + providers, "Routes: " + routes, "Models: " + models,
		"Budget: " + budget,
		fmt.Sprintf("Limits: RPM %s, TPM %s, parallel %s; output reservation %d", policyLimit(team.RPMLimit), policyLimit(team.TPMLimit), policyLimit(team.MaxParallelRequests), team.DefaultMaxTokens),
	}
}

func policyLimit(value int) string {
	if value == 0 {
		return "unlimited"
	}
	return fmt.Sprint(value)
}

func keysInTeam(keys []*pkgClient.KeyResponse, teamID string) []*pkgClient.KeyResponse {
	var result []*pkgClient.KeyResponse
	for _, key := range keys {
		if key.TeamID == teamID {
			result = append(result, key)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func countTeamKeys(keys []*pkgClient.KeyResponse, teamID string) int {
	count := 0
	for _, key := range keys {
		if key.TeamID == teamID {
			count++
		}
	}
	return count
}
