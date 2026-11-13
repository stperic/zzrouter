package server

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// connectionStore keeps storage policy outside the reusable encrypted store.
type connectionStore interface {
	Replace(context.Context, string, uint64, map[string]string) (secrets.Metadata, error)
	Remove(context.Context, string, uint64) (secrets.Metadata, error)
	UseExternal(context.Context, string, uint64) (secrets.Metadata, error)
}

// ProviderConnectionService owns node-local connection changes and their effects.
// The mutex orders activation/publication after each revision; Test never enters it.
type ProviderConnectionService struct {
	resolver    *backend.Resolver
	store       connectionStore
	registrar   ProviderRegistrar
	client      *http.Client
	coordinator func() bool
	changed     func(string)
	publish     func() error
	mu          sync.Mutex
}

// ProviderConnectionStatus contains the editable schema and secret-free state.
type ProviderConnectionStatus struct {
	*backend.ConnectionStatus
	ManagedAvailable bool `json:"managed_available"`
}

// ConnectionTestResult describes only safe upstream observations.
type ConnectionTestResult struct {
	Status     string `json:"status"`
	Code       string `json:"code"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// ConnectionApplyResult distinguishes durable saving from activation/publication.
type ConnectionApplyResult struct {
	Connection *ProviderConnectionStatus `json:"connection"`
	Saved      bool                      `json:"saved"`
	Durable    bool                      `json:"durable"`
	Active     bool                      `json:"active"`
	Published  bool                      `json:"published"`
	Outcome    string                    `json:"outcome"`
}

// Status returns connection metadata without stored secret values.
func (s *ProviderConnectionService) Status(name string) (*ProviderConnectionStatus, error) {
	if s.coordinator == nil || !s.coordinator() {
		return nil, httperr.Error{Status: 409, Code: "provider_connection_scope", Message: "provider connections belong to the coordinator"}
	}
	status, err := s.resolver.Connection(name)
	if err != nil {
		return nil, connectionResolutionError(err)
	}
	return &ProviderConnectionStatus{ConnectionStatus: status, ManagedAvailable: s.store != nil}, nil
}

// Test checks a complete candidate without persisting it, enabling a provider, or publishing.
func (s *ProviderConnectionService) Test(ctx context.Context, name string, values map[string]string) (*ConnectionTestResult, error) {
	if _, err := s.Status(name); err != nil {
		return nil, err
	}
	target, err := s.resolver.Candidate(name, values)
	if err != nil {
		return nil, connectionResolutionError(err)
	}
	return probeConnection(ctx, s.client, target), nil
}

// connectionResolutionError preserves availability refusals if configuration
// changes between the status preflight and validation of a complete candidate.
func connectionResolutionError(err error) error {
	switch {
	case errors.Is(err, backend.ErrConnectionUnsupported):
		return httperr.Error{Status: 404, Code: "provider_connection_unsupported", Message: "provider has no declared connection"}
	case errors.Is(err, backend.ErrConnectionUnavailable):
		return httperr.Error{Status: 503, Code: string(httperr.CodeProviderConnectionUnavailable), Message: "provider configuration or protected connection state is unavailable"}
	default:
		return httperr.Error{Status: 400, Code: "invalid_provider_connection", Message: err.Error()}
	}
}

func probeConnection(ctx context.Context, client *http.Client, target *backend.Resolved) *ConnectionTestResult {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := target.NewRequest(ctx, http.MethodGet, target.TestPath, nil)
	if err != nil {
		return &ConnectionTestResult{Status: "failed", Code: "invalid_endpoint"}
	}
	// A candidate credential is bound to the submitted destination. Redirects are
	// not evidence that this destination accepted it, even on the same host.
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req)
	if err != nil {
		return &ConnectionTestResult{Status: "failed", Code: "unreachable"}
	}
	defer func() { _ = resp.Body.Close() }()
	result := &ConnectionTestResult{Status: "failed", Code: "upstream_rejected", HTTPStatus: resp.StatusCode}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.Status = "ok"
		result.Code = "accepted"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		result.Code = "authentication_failed"
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		result.Code = "redirect_refused"
	case resp.StatusCode == 429:
		result.Code = "rate_limited"
	}
	return result
}

// Save replaces the whole endpoint/account/credential snapshot. No secret is
// implicitly reused when a client changes a destination or ownership mode.
func (s *ProviderConnectionService) Save(ctx context.Context, name string, revision uint64, values map[string]string, activate bool) (*ConnectionApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(name); err != nil {
		return nil, err
	}
	candidate, err := s.resolver.Candidate(name, values)
	if err != nil {
		return nil, connectionResolutionError(err)
	}
	recordValues := maps.Clone(values)
	if recordValues == nil {
		recordValues = map[string]string{}
	}
	recordValues[backend.ConnectionEndpointField] = candidate.Endpoint
	_, err = s.store.Replace(ctx, name, revision, recordValues)
	return s.applied(name, activate, err)
}

// Remove disables the connection and removes managed values at revision.
func (s *ProviderConnectionService) Remove(ctx context.Context, name string, revision uint64) (*ConnectionApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(name); err != nil {
		return nil, err
	}
	_, err := s.store.Remove(ctx, name, revision)
	return s.applied(name, false, err)
}

// UseExternal switches to operator-provided configuration at revision.
func (s *ProviderConnectionService) UseExternal(ctx context.Context, name string, revision uint64) (*ConnectionApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(name); err != nil {
		return nil, err
	}
	_, err := s.store.UseExternal(ctx, name, revision)
	return s.applied(name, false, err)
}

func (s *ProviderConnectionService) writable(name string) error {
	if _, err := s.Status(name); err != nil {
		return err
	}
	if s.store == nil {
		return httperr.Error{Status: 503, Code: "managed_credentials_unavailable", Message: "managed connections require security.credentials.key_file at server startup"}
	}
	return nil
}

func (s *ProviderConnectionService) applied(name string, activate bool, writeErr error) (*ConnectionApplyResult, error) {
	committed := errors.Is(writeErr, atomicfile.ErrPostRenameDurability)
	switch {
	case errors.Is(writeErr, secrets.ErrConflict):
		return nil, httperr.Error{Status: 409, Code: "provider_connection_conflict", Message: "connection changed; reload its revision before retrying"}
	case writeErr != nil && !committed:
		return nil, httperr.Error{Status: 503, Code: "provider_connection_persistence_failed", Message: "connection was not saved; protected storage is unavailable"}
	}
	result := &ConnectionApplyResult{Saved: true, Durable: !committed, Outcome: "saved"}
	if s.changed != nil {
		s.changed(name)
	}
	if activate && s.registrar != nil {
		if err := s.registrar.FinalizeOnboarding(name); err != nil {
			result.Outcome = "saved_activation_failed"
		}
	}
	result.Connection, _ = s.Status(name)
	if result.Connection != nil {
		result.Active = result.Connection.Enabled && result.Connection.Configured
	}
	if s.publish != nil {
		if err := s.publish(); err != nil {
			if result.Outcome == "saved" {
				result.Outcome = "saved_publication_failed"
			}
		} else {
			result.Published = true
		}
	}
	if committed {
		result.Outcome = "saved_durability_uncertain"
	}
	return result, nil
}
