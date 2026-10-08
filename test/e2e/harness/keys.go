package harness

import (
	"context"
	"encoding/json"
	"fmt"
)

// VirtualKeyRequest is the harness-side input for CreateVirtualKey.
// Mirrors internal/server.CreateKeyRequest, but only fields ring2 +
// later slices actually drive are typed. Defaults: role=user, no
// limits (unenforced).
type VirtualKeyRequest struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	Role                string  `json:"role,omitempty"`
	TeamID              string  `json:"team_id,omitempty"`
	TeamRole            string  `json:"team_role,omitempty"`
	MaxParallelRequests int     `json:"max_parallel_requests,omitempty"`
	RPMLimit            int     `json:"rpm_limit,omitempty"`
	TPMLimit            int     `json:"tpm_limit,omitempty"`
	SpendLimit          float64 `json:"spend_limit,omitempty"`
	ResetPeriod         string  `json:"reset_period,omitempty"`
	DefaultMaxTokens    int     `json:"default_max_tokens,omitempty"`
}

// VirtualKey is the parsed creation response. RawKey is the only
// chance to read the secret — the server doesn't echo it on
// subsequent GETs (Argon2id hash only).
type VirtualKey struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	TeamID   string `json:"team_id,omitempty"`
	TeamRole string `json:"team_role,omitempty"`
	RawKey   string `json:"key"`
}

// keyEnvelope mirrors response_helpers.SuccessResponse — POST /keys
// returns 201 Created with {success, message, data: CreateKeyResponse}.
// Decode in two steps so the test code reads the typed inner struct
// directly.
type keyEnvelope struct {
	Success bool       `json:"success"`
	Message string     `json:"message"`
	Data    VirtualKey `json:"data"`
}

// CreateVirtualKey posts /zzrouter/v1/keys with admin auth. Returns
// the typed VirtualKey including RawKey, which is the only credential
// the test can use against the user-tier path going forward.
//
// Caller is responsible for cleanup via DeleteVirtualKey — the file
// store persists across server restarts, so a leaked key from a
// flaky test outlives the run.
func CreateVirtualKey(ctx context.Context, c *Client, req VirtualKeyRequest) (*VirtualKey, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("CreateVirtualKey: name is required")
	}
	if req.ID == "" {
		req.ID = req.Name
	}
	if req.Role == "" {
		req.Role = "user"
	}
	resp, err := c.POST(ctx, "/zzrouter/v1/keys", req)
	if err != nil {
		return nil, fmt.Errorf("POST /keys: %w", err)
	}
	if resp.Status != 201 {
		return nil, fmt.Errorf("POST /keys: status %d body=%s", resp.Status, resp.Body)
	}
	var env keyEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode key envelope: %w (body=%s)", err, resp.Body)
	}
	if env.Data.ID == "" {
		return nil, fmt.Errorf("response missing id (body=%s)", resp.Body)
	}
	if env.Data.RawKey == "" {
		return nil, fmt.Errorf("response missing key (body=%s)", resp.Body)
	}
	return &env.Data, nil
}

// KeyUsageSnapshot mirrors internal/server.KeyUsageResponse — the
// per-key spend ledger snapshot returned by GET /keys/:id/usage and
// embedded in /spend/report.
type KeyUsageSnapshot struct {
	KeyID        string  `json:"key_id"`
	SpendUSD     float64 `json:"spend_usd"`
	ReservedUSD  float64 `json:"reserved_usd,omitempty"`
	TokensIn     int64   `json:"tokens_in"`
	TokensOut    int64   `json:"tokens_out"`
	RequestCount int64   `json:"request_count"`
}

type keyUsageEnvelope struct {
	Data KeyUsageSnapshot `json:"data"`
}

// KeyUsage fetches the live spend snapshot for a key. Returns a zero
// snapshot (with KeyID populated) when the key has no recorded
// activity, since the server returns the key's row even with all-zero
// counters.
func KeyUsage(ctx context.Context, c *Client, id string) (*KeyUsageSnapshot, error) {
	if id == "" {
		return nil, fmt.Errorf("KeyUsage: id required")
	}
	resp, err := c.GET(ctx, "/zzrouter/v1/keys/"+id+"/usage")
	if err != nil {
		return nil, fmt.Errorf("GET /keys/%s/usage: %w", id, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("GET /keys/%s/usage: status %d body=%s", id, resp.Status, resp.Body)
	}
	var env keyUsageEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode key usage: %w (body=%s)", err, resp.Body)
	}
	if env.Data.KeyID == "" {
		env.Data.KeyID = id
	}
	return &env.Data, nil
}

// KeyDetails mirrors the typed safe representation of a key returned
// by GET /keys and GET /keys/:id. Subset of internal/server.KeyResponse —
// only fields the harness reads. UpdatedAt is a string here to dodge
// the time-decode dance (tests compare structurally).
type KeyDetails struct {
	ID          string  `json:"id"`
	UUID        string  `json:"uuid,omitempty"`
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Role        string  `json:"role"`
	Suspended   bool    `json:"suspended,omitempty"`
	CreatedAt   string  `json:"created_at,omitempty"`
	UpdatedAt   string  `json:"updated_at,omitempty"`
	CreatedBy   string  `json:"created_by,omitempty"`
	UpdatedBy   string  `json:"updated_by,omitempty"`
	SpendLimit  float64 `json:"spend_limit,omitempty"`
	RPMLimit    int     `json:"rpm_limit,omitempty"`
}

type keyDetailsEnvelope struct {
	Data KeyDetails `json:"data"`
}

type keyListEnvelope struct {
	Data []KeyDetails `json:"data"`
}

// ListKeys fetches GET /zzrouter/v1/keys.
func ListKeys(ctx context.Context, c *Client) ([]KeyDetails, error) {
	resp, err := c.GET(ctx, "/zzrouter/v1/keys")
	if err != nil {
		return nil, fmt.Errorf("GET /keys: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("GET /keys: status %d body=%s", resp.Status, resp.Body)
	}
	var env keyListEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode keys list: %w", err)
	}
	return env.Data, nil
}

// GetKey fetches GET /zzrouter/v1/keys/:id.
func GetKey(ctx context.Context, c *Client, id string) (*KeyDetails, error) {
	if id == "" {
		return nil, fmt.Errorf("GetKey: id required")
	}
	resp, err := c.GET(ctx, "/zzrouter/v1/keys/"+id)
	if err != nil {
		return nil, fmt.Errorf("GET /keys/%s: %w", id, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("GET /keys/%s: status %d body=%s", id, resp.Status, resp.Body)
	}
	var env keyDetailsEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	return &env.Data, nil
}

// UpdateKeyRequest is the partial-update body for PATCH /keys/:id.
// Mirrors internal/server.UpdateKeyRequest (pointer fields = optional
// in JSON, distinguishing "unset" from "set to zero value").
type UpdateKeyRequest struct {
	Name        *string  `json:"name,omitempty"`
	Description *string  `json:"description,omitempty"`
	Suspended   *bool    `json:"suspended,omitempty"`
	SpendLimit  *float64 `json:"spend_limit,omitempty"`
	RPMLimit    *int     `json:"rpm_limit,omitempty"`
	TPMLimit    *int     `json:"tpm_limit,omitempty"`
}

// UpdateKey PATCHes /zzrouter/v1/keys/:id and returns the updated entity.
func UpdateKey(ctx context.Context, c *Client, id string, req UpdateKeyRequest) (*KeyDetails, error) {
	if id == "" {
		return nil, fmt.Errorf("UpdateKey: id required")
	}
	resp, err := c.PATCH(ctx, "/zzrouter/v1/keys/"+id, req)
	if err != nil {
		return nil, fmt.Errorf("PATCH /keys/%s: %w", id, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("PATCH /keys/%s: status %d body=%s", id, resp.Status, resp.Body)
	}
	var env keyDetailsEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	return &env.Data, nil
}

// SpendReport mirrors internal/server.SpendReportResponse.
type SpendReport struct {
	Keys       []KeyUsageSnapshot `json:"keys"`
	TotalSpend float64            `json:"total_spend_usd"`
}

type spendReportEnvelope struct {
	Data SpendReport `json:"data"`
}

// GetSpendReport fetches GET /zzrouter/v1/spend/report.
func GetSpendReport(ctx context.Context, c *Client) (*SpendReport, error) {
	resp, err := c.GET(ctx, "/zzrouter/v1/spend/report")
	if err != nil {
		return nil, fmt.Errorf("GET /spend/report: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("GET /spend/report: status %d body=%s", resp.Status, resp.Body)
	}
	var env spendReportEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode spend report: %w", err)
	}
	return &env.Data, nil
}

// CreateTeamRequest mirrors internal/server.CreateTeamRequest (subset).
type CreateTeamRequest struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	SpendLimit    float64  `json:"spend_limit,omitempty"`
	RPMLimit      int      `json:"rpm_limit,omitempty"`
}

// TeamDetails mirrors internal/server.TeamResponse (subset).
type TeamDetails struct {
	ID            string   `json:"id"`
	UUID          string   `json:"uuid,omitempty"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	Suspended     bool     `json:"suspended,omitempty"`
	SpendLimit    float64  `json:"spend_limit,omitempty"`
	RPMLimit      int      `json:"rpm_limit,omitempty"`
}

type teamEnvelope struct {
	Data TeamDetails `json:"data"`
}

type teamListEnvelope struct {
	Data []TeamDetails `json:"data"`
}

// CreateTeam POSTs /zzrouter/v1/teams.
func CreateTeam(ctx context.Context, c *Client, req CreateTeamRequest) (*TeamDetails, error) {
	if req.ID == "" || req.Name == "" {
		return nil, fmt.Errorf("CreateTeam: id+name required")
	}
	resp, err := c.POST(ctx, "/zzrouter/v1/teams", req)
	if err != nil {
		return nil, fmt.Errorf("POST /teams: %w", err)
	}
	if resp.Status != 201 {
		return nil, fmt.Errorf("POST /teams: status %d body=%s", resp.Status, resp.Body)
	}
	var env teamEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode team: %w", err)
	}
	return &env.Data, nil
}

// ListTeams fetches GET /zzrouter/v1/teams.
func ListTeams(ctx context.Context, c *Client) ([]TeamDetails, error) {
	resp, err := c.GET(ctx, "/zzrouter/v1/teams")
	if err != nil {
		return nil, fmt.Errorf("GET /teams: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("GET /teams: status %d body=%s", resp.Status, resp.Body)
	}
	var env teamListEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode teams list: %w", err)
	}
	return env.Data, nil
}

// GetTeam fetches GET /zzrouter/v1/teams/:id.
func GetTeam(ctx context.Context, c *Client, id string) (*TeamDetails, error) {
	resp, err := c.GET(ctx, "/zzrouter/v1/teams/"+id)
	if err != nil {
		return nil, fmt.Errorf("GET /teams/%s: %w", id, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("GET /teams/%s: status %d body=%s", id, resp.Status, resp.Body)
	}
	var env teamEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode team: %w", err)
	}
	return &env.Data, nil
}

// UpdateTeamRequest mirrors internal/server.UpdateTeamRequest (subset).
type UpdateTeamRequest struct {
	Name       *string  `json:"name,omitempty"`
	Suspended  *bool    `json:"suspended,omitempty"`
	SpendLimit *float64 `json:"spend_limit,omitempty"`
	RPMLimit   *int     `json:"rpm_limit,omitempty"`
}

// UpdateTeam PATCHes /zzrouter/v1/teams/:id.
func UpdateTeam(ctx context.Context, c *Client, id string, req UpdateTeamRequest) (*TeamDetails, error) {
	resp, err := c.PATCH(ctx, "/zzrouter/v1/teams/"+id, req)
	if err != nil {
		return nil, fmt.Errorf("PATCH /teams/%s: %w", id, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("PATCH /teams/%s: status %d body=%s", id, resp.Status, resp.Body)
	}
	var env teamEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode team: %w", err)
	}
	return &env.Data, nil
}

// DeleteTeam DELETEs /zzrouter/v1/teams/:id. Idempotent — 404 = success.
func DeleteTeam(ctx context.Context, c *Client, id string) error {
	resp, err := c.DELETE(ctx, "/zzrouter/v1/teams/"+id)
	if err != nil {
		return fmt.Errorf("DELETE /teams/%s: %w", id, err)
	}
	if resp.Status == 200 || resp.Status == 204 || resp.Status == 404 {
		return nil
	}
	return fmt.Errorf("DELETE /teams/%s: status %d body=%s", id, resp.Status, resp.Body)
}

// TeamKey is one entry in GET /teams/:id/keys.
type TeamKey struct {
	KeyID string `json:"key_id"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role"`
}

type teamKeysEnvelope struct {
	Data struct {
		TeamID string    `json:"team_id"`
		Keys   []TeamKey `json:"keys"`
	} `json:"data"`
}

// GetTeamKeys fetches GET /zzrouter/v1/teams/:id/keys.
func GetTeamKeys(ctx context.Context, c *Client, id string) ([]TeamKey, error) {
	resp, err := c.GET(ctx, "/zzrouter/v1/teams/"+id+"/keys")
	if err != nil {
		return nil, fmt.Errorf("GET /teams/%s/keys: %w", id, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("GET /teams/%s/keys: status %d body=%s", id, resp.Status, resp.Body)
	}
	var env teamKeysEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode team keys: %w", err)
	}
	return env.Data.Keys, nil
}

// DeleteVirtualKey removes a virtual key by ID. Idempotent — a 404
// is treated as success since the only goal is "no longer present".
func DeleteVirtualKey(ctx context.Context, c *Client, id string) error {
	if id == "" {
		return fmt.Errorf("DeleteVirtualKey: id required")
	}
	resp, err := c.DELETE(ctx, "/zzrouter/v1/keys/"+id)
	if err != nil {
		return fmt.Errorf("DELETE /keys/%s: %w", id, err)
	}
	if resp.Status == 200 || resp.Status == 204 || resp.Status == 404 {
		return nil
	}
	return fmt.Errorf("DELETE /keys/%s: status %d body=%s", id, resp.Status, resp.Body)
}
