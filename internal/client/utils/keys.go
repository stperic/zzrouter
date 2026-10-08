package client

import "github.com/stperic/zzrouter/pkg/apipath"

// KeyResponse is the client-side safe representation of a virtual key.
// Mirrors the server's zzrouter/v1/keys response shape.
type KeyResponse struct {
	ID   string `json:"id"`
	UUID string `json:"uuid,omitempty"`
	Name string `json:"name"`
	Role string `json:"role"`

	TeamID   string `json:"team_id"`
	TeamRole string `json:"team_role"`

	// Lifecycle / identity
	ExpiresAt  *string `json:"expires_at,omitempty"`
	CreatedAt  string  `json:"created_at,omitempty"`
	UpdatedAt  string  `json:"updated_at,omitempty"`
	LastSeenAt string  `json:"last_seen_at,omitempty"`
	CreatedBy  string  `json:"created_by,omitempty"`
	UpdatedBy  string  `json:"updated_by,omitempty"`

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

	Metadata  map[string]string `json:"metadata,omitempty"`
	IsExpired bool              `json:"is_expired"`
}

// KeyListResponse is the envelope for GET /keys.
type KeyListResponse struct {
	Data    []*KeyResponse `json:"data"`
	Total   int            `json:"total"`
	HasMore bool           `json:"has_more,omitempty"`
}

// KeyDetailResponse is the envelope for single-key responses.
type KeyDetailResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Data    KeyResponse `json:"data"`
}

// CreateKeyRequest is the input for creating a virtual key.
// Omit TeamID to get a personal team auto-created for the new key.
type CreateKeyRequest struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Description         string            `json:"description,omitempty"`
	Role                string            `json:"role,omitempty"`
	TeamID              string            `json:"team_id,omitempty"`
	TeamRole            string            `json:"team_role,omitempty"`
	ExpiresAt           *string           `json:"expires_at,omitempty"`
	MaxParallelRequests int               `json:"max_parallel_requests,omitempty"`
	RPMLimit            int               `json:"rpm_limit,omitempty"`
	TPMLimit            int               `json:"tpm_limit,omitempty"`
	SpendLimit          float64           `json:"spend_limit,omitempty"`
	ResetPeriod         string            `json:"reset_period,omitempty"`
	DefaultMaxTokens    int               `json:"default_max_tokens,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

// CreateKeyResponse is returned when a key is created (contains raw key shown once).
type CreateKeyResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Role     string `json:"role"`
		TeamID   string `json:"team_id"`
		TeamRole string `json:"team_role"`
		RawKey   string `json:"key"`
	} `json:"data"`
}

// UpdateKeyRequest is the input for partially updating a virtual key.
// TeamID and TeamRole are not updatable — keys cannot move between teams.
type UpdateKeyRequest struct {
	Name                *string           `json:"name,omitempty"`
	Role                *string           `json:"role,omitempty"`
	Suspended           *bool             `json:"suspended,omitempty"`
	ExpiresAt           *string           `json:"expires_at,omitempty"`
	MaxParallelRequests *int              `json:"max_parallel_requests,omitempty"`
	RPMLimit            *int              `json:"rpm_limit,omitempty"`
	TPMLimit            *int              `json:"tpm_limit,omitempty"`
	SpendLimit          *float64          `json:"spend_limit,omitempty"`
	ResetPeriod         *string           `json:"reset_period,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

// KeyUsageResponse is the client-side usage/spend data for a key.
type KeyUsageResponse struct {
	KeyID        string  `json:"key_id"`
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

// KeyUsageDetailResponse is the envelope for GET /keys/:id/usage.
type KeyUsageDetailResponse struct {
	Success bool             `json:"success"`
	Message string           `json:"message"`
	Data    KeyUsageResponse `json:"data"`
}

// ListKeys retrieves all virtual keys.
// GET /zzrouter/v1/keys
func (c *Client) ListKeys() ([]*KeyResponse, error) {
	var result KeyListResponse
	if err := c.doJSON("GET", apipath.Keys, nil, &result, "list keys"); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// GetKey retrieves a single virtual key by ID.
// GET /zzrouter/v1/keys/:id
func (c *Client) GetKey(id string) (*KeyResponse, error) {
	var result KeyDetailResponse
	if err := c.doJSON("GET", apipath.Key(id), nil, &result, "get key"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// CreateKey creates a new virtual key.
// POST /zzrouter/v1/keys
func (c *Client) CreateKey(req *CreateKeyRequest) (*CreateKeyResponse, error) {
	var result CreateKeyResponse
	if err := c.doJSON("POST", apipath.Keys, req, &result, "create key"); err != nil {
		return nil, err
	}
	return &result, nil
}

// DeleteKey deletes a virtual key by ID.
// DELETE /zzrouter/v1/keys/:id
func (c *Client) DeleteKey(id string) error {
	return c.doJSON("DELETE", apipath.Key(id), nil, nil, "delete key")
}

// RotateKey generates a new raw key value for an existing key.
// POST /zzrouter/v1/keys/:id/rotate
func (c *Client) RotateKey(id string) (*CreateKeyResponse, error) {
	var result CreateKeyResponse
	if err := c.doJSON("POST", apipath.KeyRotate(id), nil, &result, "rotate key"); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetKeyUsage retrieves usage/spend data for a key.
// GET /zzrouter/v1/keys/:id/usage
func (c *Client) GetKeyUsage(id string) (*KeyUsageResponse, error) {
	var result KeyUsageDetailResponse
	if err := c.doJSON("GET", apipath.KeyUsage(id), nil, &result, "get key usage"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// UpdateKey partially updates a virtual key.
// PATCH /zzrouter/v1/keys/:id
func (c *Client) UpdateKey(id string, req *UpdateKeyRequest) (*KeyResponse, error) {
	var result KeyDetailResponse
	if err := c.doJSON("PATCH", apipath.Key(id), req, &result, "update key"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// SpendReportResponse is the aggregated spend data across all keys.
type SpendReportResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    SpendReportData `json:"data"`
}

// SpendReportData holds the actual spend report fields.
type SpendReportData struct {
	Keys       []KeyUsageResponse `json:"keys"`
	TotalSpend float64            `json:"total_spend_usd"`
}

// GetSpendReport retrieves aggregated spend data across all keys.
// GET /zzrouter/v1/spend/report
func (c *Client) GetSpendReport() (*SpendReportData, error) {
	var result SpendReportResponse
	if err := c.doJSON("GET", apipath.SpendReport, nil, &result, "get spend report"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// ResetKeyUsage clears usage/spend counters for a key.
// POST /zzrouter/v1/keys/:id/usage/reset
func (c *Client) ResetKeyUsage(id string) error {
	return c.doJSON("POST", apipath.KeyUsageReset(id), nil, nil, "reset key usage")
}
