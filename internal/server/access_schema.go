package server

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/quota"
)

// accessFieldDTO mirrors schemaParamDTO for the keys + teams admin surface.
type accessFieldDTO struct {
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	// Numeric Min/Max with int(0) deliberately serialize: "min: 0" is the
	// schema's way of advertising "0 = unenforced" for quota fields.
	Min         any      `json:"min,omitempty"`
	Max         any      `json:"max,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type accessSchemaResponse struct {
	Create map[string]accessFieldDTO `json:"create"`
	Update map[string]accessFieldDTO `json:"update"`
}

// Enum builders return fresh slices so a caller mutating the response
// can't corrupt the next one. Sources of truth:
//
//	pkg/access/control/types.go (RoleAdmin/RoleUser)
//	pkg/access/teams/types.go   (RoleOwner/RoleMember)
//	pkg/access/quota/types.go   (Period{Daily,Weekly,Monthly})
func keyRoleEnum() []string  { return []string{string(control.RoleAdmin), string(control.RoleUser)} }
func teamRoleEnum() []string { return []string{"owner", "member"} }
func resetPeriodEnum() []string {
	return []string{string(quota.PeriodDaily), string(quota.PeriodWeekly), string(quota.PeriodMonthly)}
}

// validateKeyRole rejects values not in the closed enum the schema
// advertises. Empty is allowed because CreateKey defaults it to "user".
func validateKeyRole(role string) error {
	if role == "" || role == string(control.RoleAdmin) || role == string(control.RoleUser) {
		return nil
	}
	return invalidInputf("invalid role %q (expected %q or %q)", role, control.RoleAdmin, control.RoleUser)
}

// validateResetPeriod rejects values not in the closed enum. Empty is
// allowed because Create paths only consult ResetPeriod when SpendLimit > 0.
func validateResetPeriod(p string) error {
	if p == "" || p == string(quota.PeriodDaily) || p == string(quota.PeriodWeekly) || p == string(quota.PeriodMonthly) {
		return nil
	}
	return invalidInputf("invalid reset_period %q (expected %q, %q, or %q)",
		p, quota.PeriodDaily, quota.PeriodWeekly, quota.PeriodMonthly)
}

func keysSchema() accessSchemaResponse {
	return accessSchemaResponse{
		Create: map[string]accessFieldDTO{
			"id":                    {Type: "string", Required: true, Description: "Unique virtual key ID; immutable after creation."},
			"name":                  {Type: "string", Required: true, Description: "Human-readable display name."},
			"description":           {Type: "string", Description: "Free-form notes."},
			"role":                  {Type: "string", Enum: keyRoleEnum(), Description: "Auth role; defaults to user (read-only inference). admin grants full management."},
			"team_id":               {Type: "string", Description: "Existing shared team to join; omit for auto-created personal team. Model access comes from the team, and a personal team is immutable: pass a shared team here to scope this key to a subset of models."},
			"team_role":             {Type: "string", Enum: teamRoleEnum(), Description: "Role within team_id; defaults to member. Ignored when team_id is omitted (auto-personal-team forces owner)."},
			"expires_at":            {Type: "string", Description: "RFC 3339 timestamp; omit for no expiry."},
			"max_parallel_requests": {Type: "int", Min: 0, Description: "Concurrent in-flight cap; 0 = unenforced."},
			"rpm_limit":             {Type: "int", Min: 0, Description: "Requests per minute; 0 = unenforced."},
			"tpm_limit":             {Type: "int", Min: 0, Description: "Tokens per minute; 0 = unenforced."},
			"spend_limit":           {Type: "float", Min: 0, Description: "USD budget per reset_period; 0 = unenforced."},
			"reset_period":          {Type: "string", Enum: resetPeriodEnum(), Description: "Spend window; ignored unless spend_limit > 0."},
			"default_max_tokens":    {Type: "int", Min: 0, Description: defaultMaxTokensDescription()},
			"metadata":              {Type: "object<string,string>", Description: "Free-form labels."},
		},
		Update: map[string]accessFieldDTO{
			"name":                  {Type: "string"},
			"description":           {Type: "string"},
			"role":                  {Type: "string", Enum: keyRoleEnum()},
			"suspended":             {Type: "bool", Description: "Block all auth attempts immediately when true."},
			"expires_at":            {Type: "string", Description: "RFC 3339 timestamp. Cannot be cleared via PATCH today: omit to leave unchanged; pass a far-future date to effectively unblock."},
			"max_parallel_requests": {Type: "int", Min: 0},
			"rpm_limit":             {Type: "int", Min: 0},
			"tpm_limit":             {Type: "int", Min: 0},
			"spend_limit":           {Type: "float", Min: 0},
			"reset_period":          {Type: "string", Enum: resetPeriodEnum()},
			"default_max_tokens":    {Type: "int", Min: 0},
			"metadata":              {Type: "object<string,string>", Description: "Replaces the prior map; send {} to clear."},
		},
	}
}

func teamsSchema() accessSchemaResponse {
	return accessSchemaResponse{
		Create: map[string]accessFieldDTO{
			"id":                    {Type: "string", Required: true, Description: "Unique team ID; immutable after creation."},
			"name":                  {Type: "string", Required: true, Description: "Human-readable display name."},
			"allowed_models":        {Type: "array<string>", Description: allowedModelsDescription},
			"max_parallel_requests": {Type: "int", Min: 0, Description: "Concurrent in-flight cap across all team members; 0 = unenforced."},
			"rpm_limit":             {Type: "int", Min: 0, Description: "Requests per minute across team; 0 = unenforced."},
			"tpm_limit":             {Type: "int", Min: 0, Description: "Tokens per minute across team; 0 = unenforced."},
			"spend_limit":           {Type: "float", Min: 0, Description: "USD budget per reset_period across team; 0 = unenforced."},
			"reset_period":          {Type: "string", Enum: resetPeriodEnum(), Description: "Spend window; ignored unless spend_limit > 0."},
			"default_max_tokens":    {Type: "int", Min: 0, Description: defaultMaxTokensDescription()},
			"metadata":              {Type: "object<string,string>", Description: "Free-form labels."},
		},
		Update: map[string]accessFieldDTO{
			"name":                  {Type: "string"},
			"allowed_models":        {Type: "array<string>", Description: "Replaces the prior list; send [] to clear. " + allowedModelsDescription},
			"suspended":             {Type: "bool", Description: "Block all team members from authenticating when true."},
			"max_parallel_requests": {Type: "int", Min: 0},
			"rpm_limit":             {Type: "int", Min: 0},
			"tpm_limit":             {Type: "int", Min: 0},
			"spend_limit":           {Type: "float", Min: 0},
			"reset_period":          {Type: "string", Enum: resetPeriodEnum()},
			"default_max_tokens":    {Type: "int", Min: 0},
			"metadata":              {Type: "object<string,string>", Description: "Replaces the prior map; send {} to clear."},
		},
	}
}

// allowedModelsDescription documents both what the list gates and which
// id space it is written in. Entries and requests are compared on model
// identity, which is what the id parser returns after dropping the
// placement decorations: the "@node" routing hint and the "#file" GGUF
// variant selector. Naming either therefore grants the model, not the
// node or the variant — worth saying outright, because an operator
// writing "repo:tag#Q4_K_M" is plainly trying to narrow the grant and
// would otherwise never learn it did not.
const allowedModelsDescription = "Model allowlist for every key on this team; empty = all models. " +
	"Accepts catalog ids or bare model names. Matching is on model identity, so an \"@node\" suffix " +
	"or a \"#file\" variant selector is ignored: an entry grants the model on every node and in every " +
	"variant, and per-node or per-variant grants cannot be expressed here. " +
	"A model-group name grants its replicas. Model access is a team property; keys do not carry one."

// Cost-per-token is fixed at 10 microUSD by quota.Check; surface the
// constant + derived dollar cost so the description tracks the source.
func defaultMaxTokensDescription() string {
	return fmt.Sprintf("Per-request token reservation estimate (cost = N × 10 microUSD). Default %d reserves $%.5f per call: set explicitly when spend_limit is tight.",
		quota.DefaultMaxTokensFallback, float64(quota.DefaultMaxTokensFallback)*10/1e6)
}

func handleKeysSchema(c *gin.Context) {
	respondSuccess(c, "Keys schema retrieved", keysSchema())
}

func handleTeamsSchema(c *gin.Context) {
	respondSuccess(c, "Teams schema retrieved", teamsSchema())
}
