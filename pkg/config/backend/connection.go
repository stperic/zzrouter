package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config"
)

// ConnectionEndpointField binds a managed record to its effective destination.
const ConnectionEndpointField = "_endpoint"

// ConnectionRecords supplies node-owned protected connection values.
type ConnectionRecords interface {
	Snapshot(string) (secrets.Record, error)
}

// ErrConnectionUnavailable refuses unreadable configuration or protected state.
var ErrConnectionUnavailable = errors.New("provider connection unavailable")

// ErrConnectionUnsupported identifies a provider without a declared connection.
var ErrConnectionUnsupported = errors.New("provider connection unsupported")

// ConnectionStatus is the safe client projection of the effective connection.
type ConnectionStatus struct {
	Provider   string                   `json:"provider"`
	Scope      string                   `json:"scope"`
	Mode       secrets.Mode             `json:"mode"`
	Revision   uint64                   `json:"revision"`
	Configured bool                     `json:"configured"`
	Enabled    bool                     `json:"enabled"`
	Fields     []config.ConnectionField `json:"fields"`
	Values     map[string]string        `json:"values"`
	Present    []string                 `json:"present"`
}

// Connection reads declared fields without revealing their secret values.
func (r *Resolver) Connection(name string) (*ConnectionStatus, error) {
	svc, record, values, err := r.connection(name)
	if err != nil {
		return nil, err
	}
	if svc.Connection == nil {
		return nil, ErrConnectionUnsupported
	}
	status := &ConnectionStatus{Provider: name, Scope: "coordinator", Mode: record.Mode, Revision: record.Revision, Enabled: svc.IsEnabled(), Fields: slices.Clone(svc.Connection.Fields), Values: map[string]string{}, Present: []string{}}
	for i := range status.Fields {
		status.Fields[i].EnvAliases = slices.Clone(status.Fields[i].EnvAliases)
	}
	for _, field := range svc.Connection.Fields {
		if values[field.ID] != "" {
			status.Present = append(status.Present, field.ID)
		}
		if !field.Secret {
			if field.ValidateValue(values[field.ID], false) == nil {
				status.Values[field.ID] = values[field.ID]
			}
		}
	}
	target := connectionTarget(svc, record, values)
	status.Configured = target != nil && target.Configured
	return status, nil
}

// Target captures a provider's endpoint and authentication even before activation.
func (r *Resolver) Target(name string) (*Resolved, bool) {
	svc, record, values, err := r.connection(name)
	if err != nil {
		return nil, false
	}
	target := connectionTarget(svc, record, values)
	return target, target != nil
}

// Candidate validates a complete connection without changing its stored state.
func (r *Resolver) Candidate(name string, values map[string]string) (*Resolved, error) {
	svc, _, _, err := r.connection(name)
	if err != nil {
		return nil, err
	}
	if svc.Connection == nil {
		return nil, ErrConnectionUnsupported
	}
	provided := maps.Clone(values)
	if !managedAuthDeclared(svc) {
		return nil, fmt.Errorf("managed authentication requires runtime.api to reference its declared secret fields")
	}
	for _, field := range svc.Connection.Fields {
		value := values[field.ID]
		if err := field.ValidateValue(value, true); err != nil {
			return nil, err
		}
		delete(provided, field.ID)
	}
	if len(provided) != 0 {
		return nil, fmt.Errorf("connection contains undeclared fields")
	}
	target := connectionTarget(svc, secrets.Record{Mode: secrets.Managed}, values)
	if target == nil || !config.ValidConnectionEndpoint(target.Endpoint) {
		return nil, fmt.Errorf("connection requires an HTTPS endpoint (HTTP is allowed on loopback)")
	}
	if svc.IsCloudProvider() && !target.Configured {
		return nil, fmt.Errorf("connection requires its declared credentials")
	}
	return target, nil
}

// Available checks runtime availability through the same connection as requests.
func (r *Resolver) Available(name string) bool {
	cfg := r.appsConfig()
	if cfg == nil {
		return false
	}
	svc, ok := cfg.LookupApp(name)
	if !ok || !svc.IsEnabled() {
		return false
	}
	if !svc.IsCloudProvider() {
		return true
	}
	target, ok := r.Target(name)
	return ok && target.Configured
}

func (r *Resolver) connection(name string) (*config.ServiceConfig, secrets.Record, map[string]string, error) {
	if r == nil || r.appsConfig == nil {
		return nil, secrets.Record{}, nil, ErrConnectionUnavailable
	}
	cfg := r.appsConfig()
	if cfg == nil {
		return nil, secrets.Record{}, nil, ErrConnectionUnavailable
	}
	svc, ok := cfg.LookupApp(name)
	if !ok {
		return nil, secrets.Record{}, nil, ErrConnectionUnsupported
	}
	if svc.Runtime == nil {
		if svc.Connection == nil {
			return nil, secrets.Record{}, nil, ErrConnectionUnsupported
		}
		return nil, secrets.Record{}, nil, ErrConnectionUnavailable
	}
	record := secrets.Record{Mode: secrets.External}
	if r.records != nil && svc.Connection != nil {
		var err error
		record, err = r.records.Snapshot(name)
		if err != nil {
			return nil, record, nil, ErrConnectionUnavailable
		}
	}
	values := map[string]string{}
	if svc.Connection != nil {
		for _, field := range svc.Connection.Fields {
			switch record.Mode {
			case secrets.Managed:
				values[field.ID] = record.Values[field.ID]
			case secrets.External:
				values[field.ID] = externalValue(field)
			case secrets.Disabled:
			default:
				return nil, record, nil, ErrConnectionUnavailable
			}
			if values[field.ID] == "" && !field.Secret {
				values[field.ID] = field.Default
			}
		}
	}
	return &svc, record, values, nil
}

func externalValue(field config.ConnectionField) string {
	for _, name := range append([]string{field.Env}, field.EnvAliases...) {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	path := field.FallbackFile
	if path == "" {
		return field.Default
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		} else {
			return ""
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, (16<<10)+1))
	if err != nil || len(data) > 16<<10 {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func connectionTarget(svc *config.ServiceConfig, record secrets.Record, values map[string]string) *Resolved {
	if svc.Runtime == nil || svc.Runtime.Endpoint == "" {
		return nil
	}
	bound := map[string]string{}
	if svc.Connection != nil {
		for _, field := range svc.Connection.Fields {
			bound[field.Env] = values[field.ID]
			for _, alias := range field.EnvAliases {
				bound[alias] = values[field.ID]
			}
		}
	}
	// Capture every environment expansion once, including operator-added fields.
	lookup := func(name string) string {
		if value, ok := bound[name]; ok {
			return value
		}
		value := ""
		if record.Mode == secrets.External {
			value = os.Getenv(name)
		}
		bound[name] = value
		return value
	}
	endpoint := config.ExpandEnvWith(svc.Runtime.Endpoint, lookup)
	if svc.IsCloudProvider() {
		endpoint = config.NormalizeEndpointWith(svc.Runtime.Endpoint, lookup)
	}
	if endpoint == "" {
		return nil
	}
	if record.Mode == secrets.Managed && record.Revision > 0 && (!managedAuthDeclared(svc) || !SameEndpoint(record.Values[ConnectionEndpointField], endpoint)) {
		return nil
	}
	upstream := Engine()
	if svc.Runtime.API == nil || svc.Runtime.API.AuthType == config.AuthTypeCaller {
		upstream = CallerKey()
	}
	if svc.Runtime.API.SendsOwnCredential() {
		upstream = credentialWithLookup(svc.Runtime.API, lookup)
	}
	configured := svc.Runtime.API.HasCredentialsWith(lookup)
	if svc.Connection != nil {
		for _, f := range svc.Connection.Fields {
			if f.Required && values[f.ID] == "" && f.Default == "" {
				configured = false
			}
		}
	}
	if record.Mode == secrets.Disabled {
		configured = false
		if svc.IsCloudProvider() {
			return &Resolved{Endpoint: endpoint, Cloud: true, Configured: false, Revision: fmt.Sprintf("disabled:%d", record.Revision)}
		}
		upstream = Engine()
	}
	testPath := svc.Runtime.HealthCheck.Path
	if svc.Connection != nil && svc.Connection.TestPath != "" {
		testPath = svc.Connection.TestPath
	}
	if testPath == "" {
		testPath = "/v1/models"
	}
	header := upstream.Header()
	encoded, _ := json.Marshal(struct {
		Endpoint          string
		ModelsPath        string
		ModelsResponseKey string
		Header            http.Header
		Revision          uint64
		Mode              secrets.Mode
	}{endpoint, svc.Runtime.ModelsPath, svc.Runtime.ModelsResponseKey, header, record.Revision, record.Mode})
	digest := sha256.Sum256(encoded)
	return &Resolved{Endpoint: endpoint, Upstream: upstream, Cloud: svc.IsCloudProvider(), Configured: configured, Revision: hex.EncodeToString(digest[:]), ModelsPath: svc.Runtime.ModelsPath, ModelsResponseKey: svc.Runtime.ModelsResponseKey, TestPath: testPath}
}

// A managed form cannot silently ignore its candidate because an operator's
// auth declaration still points at a literal or another external variable.
func managedAuthDeclared(svc *config.ServiceConfig) bool {
	if svc.Connection == nil || !svc.Runtime.API.SendsOwnCredential() {
		return false
	}
	referenced := map[string]bool{}
	observe := func(name string) string { referenced[name] = true; return "" }
	_ = config.ExpandEnvWith(svc.Runtime.API.Token, observe)
	for _, value := range svc.Runtime.API.CustomHeaders {
		_ = config.ExpandEnvWith(value, observe)
	}
	for _, field := range svc.Connection.Fields {
		if !field.Secret {
			continue
		}
		bound := referenced[field.Env]
		for _, alias := range field.EnvAliases {
			bound = bound || referenced[alias]
		}
		if !bound {
			return false
		}
	}
	return true
}
