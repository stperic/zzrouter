package client

import "github.com/stperic/zzrouter/pkg/apipath"

// TeamResponse is the client-side safe representation of a team.
// Mirrors the server's zzrouter/v1/teams response shape.
//
// The member list lives on keys now; call GetTeamKeys to see who's in the team.
type TeamResponse struct {
	ID   string `json:"id"`
	UUID string `json:"uuid,omitempty"`
	Name string `json:"name"`
	Kind string `json:"kind"` // "personal" | "shared"

	AllowedModels []string `json:"allowed_models,omitempty"`

	// Suspension state
	Suspended   bool    `json:"suspended"`
	SuspendedAt *string `json:"suspended_at,omitempty"`
	SuspendedBy string  `json:"suspended_by,omitempty"`

	// Quota config
	MaxParallelRequests int     `json:"max_parallel_requests,omitempty"`
	RPMLimit            int     `json:"rpm_limit,omitempty"`
	TPMLimit            int     `json:"tpm_limit,omitempty"`
	SpendLimit          float64 `json:"spend_limit,omitempty"`
	ResetPeriod         string  `json:"reset_period,omitempty"`
	DefaultMaxTokens    int     `json:"default_max_tokens,omitempty"`

	Metadata map[string]string `json:"metadata,omitempty"`

	// Audit
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

// TeamListResponse is the envelope for GET /teams.
type TeamListResponse struct {
	Data    []*TeamResponse `json:"data"`
	Total   int             `json:"total"`
	HasMore bool            `json:"has_more,omitempty"`
}

// TeamDetailResponse is the envelope for single-team responses.
type TeamDetailResponse struct {
	Success bool         `json:"success"`
	Message string       `json:"message"`
	Data    TeamResponse `json:"data"`
}

// CreateTeamRequest is the payload for POST /zzrouter/v1/teams. Only shared
// teams can be created this way — personal teams are auto-created alongside
// a key when CreateKey omits team_id.
type CreateTeamRequest struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	AllowedModels       []string          `json:"allowed_models,omitempty"`
	MaxParallelRequests int               `json:"max_parallel_requests,omitempty"`
	RPMLimit            int               `json:"rpm_limit,omitempty"`
	TPMLimit            int               `json:"tpm_limit,omitempty"`
	SpendLimit          float64           `json:"spend_limit,omitempty"`
	ResetPeriod         string            `json:"reset_period,omitempty"`
	DefaultMaxTokens    int               `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

// UpdateTeamRequest is the payload for PATCH /zzrouter/v1/teams/:id.
// Nil fields are omitted and leave stored values untouched. Personal teams
// reject updates outright (409).
type UpdateTeamRequest struct {
	Name                *string           `json:"name,omitempty"`
	AllowedModels       *[]string         `json:"allowed_models,omitempty"`
	Suspended           *bool             `json:"suspended,omitempty"`
	MaxParallelRequests *int              `json:"max_parallel_requests,omitempty"`
	RPMLimit            *int              `json:"rpm_limit,omitempty"`
	TPMLimit            *int              `json:"tpm_limit,omitempty"`
	SpendLimit          *float64          `json:"spend_limit,omitempty"`
	ResetPeriod         *string           `json:"reset_period,omitempty"`
	DefaultMaxTokens    *int              `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

// TeamKeyItem is a single row in a TeamKeysResponse.
type TeamKeyItem struct {
	KeyID string `json:"key_id"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role"`
}

// TeamKeysResponse is the response body for GET /zzrouter/v1/teams/:id/keys.
type TeamKeysResponse struct {
	TeamID string        `json:"team_id"`
	Keys   []TeamKeyItem `json:"keys"`
}

// TeamKeysDetailResponse is the envelope for GET /zzrouter/v1/teams/:id/keys.
type TeamKeysDetailResponse struct {
	Success bool             `json:"success"`
	Message string           `json:"message"`
	Data    TeamKeysResponse `json:"data"`
}

// TeamUsageResponse is the client-side usage/spend data for a team.
type TeamUsageResponse struct {
	TeamID       string  `json:"team_id"`
	SpendUSD     float64 `json:"spend_usd"`
	ReservedUSD  float64 `json:"reserved_usd,omitempty"`
	TokensIn     int64   `json:"tokens_in"`
	TokensOut    int64   `json:"tokens_out"`
	RequestCount int64   `json:"request_count"`
	PeriodStart  string  `json:"period_start,omitempty"`
	Period       string  `json:"period,omitempty"`
	SpendLimit   float64 `json:"spend_limit,omitempty"`
	RPMLimit     int     `json:"rpm_limit,omitempty"`
	TPMLimit     int     `json:"tpm_limit,omitempty"`
}

// TeamUsageDetailResponse is the envelope for GET /zzrouter/v1/teams/:id/usage.
type TeamUsageDetailResponse struct {
	Success bool              `json:"success"`
	Message string            `json:"message"`
	Data    TeamUsageResponse `json:"data"`
}

// ListTeams retrieves all teams.
// GET /zzrouter/v1/teams
func (c *Client) ListTeams() ([]*TeamResponse, error) {
	var result TeamListResponse
	if err := c.doJSON("GET", apipath.Teams, nil, &result, "list teams"); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// GetTeam retrieves a single team by ID.
// GET /zzrouter/v1/teams/:id
func (c *Client) GetTeam(id string) (*TeamResponse, error) {
	var result TeamDetailResponse
	if err := c.doJSON("GET", apipath.Team(id), nil, &result, "get team"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// CreateTeam creates a new team.
// POST /zzrouter/v1/teams
func (c *Client) CreateTeam(req *CreateTeamRequest) (*TeamResponse, error) {
	var result TeamDetailResponse
	if err := c.doJSON("POST", apipath.Teams, req, &result, "create team"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// UpdateTeam partially updates a team.
// PATCH /zzrouter/v1/teams/:id
func (c *Client) UpdateTeam(id string, req *UpdateTeamRequest) (*TeamResponse, error) {
	var result TeamDetailResponse
	if err := c.doJSON("PATCH", apipath.Team(id), req, &result, "update team"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// DeleteTeam deletes a team by ID. Fails with 409 if the team still has members.
// DELETE /zzrouter/v1/teams/:id
func (c *Client) DeleteTeam(id string) error {
	return c.doJSON("DELETE", apipath.Team(id), nil, nil, "delete team")
}

// GetTeamKeys returns the keys that belong to a team.
// GET /zzrouter/v1/teams/:id/keys
func (c *Client) GetTeamKeys(id string) (*TeamKeysResponse, error) {
	var result TeamKeysDetailResponse
	if err := c.doJSON("GET", apipath.TeamKeys(id), nil, &result, "get team keys"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// GetTeamUsage retrieves usage/spend data for a team.
// GET /zzrouter/v1/teams/:id/usage
func (c *Client) GetTeamUsage(id string) (*TeamUsageResponse, error) {
	var result TeamUsageDetailResponse
	if err := c.doJSON("GET", apipath.TeamUsage(id), nil, &result, "get team usage"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// ResetTeamUsage clears usage/spend counters for a team.
// POST /zzrouter/v1/teams/:id/usage/reset
func (c *Client) ResetTeamUsage(id string) error {
	return c.doJSON("POST", apipath.TeamUsageReset(id), nil, nil, "reset team usage")
}
