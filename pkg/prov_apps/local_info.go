package prov_apps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
)

// LifecycleState is the composed runtime state of a provider on the local
// node. Producers (ProviderAppManager.LocalInfo) derive it from the install
// coordinator, protocol registry, and apps config in one place; consumers
// (HTTP handlers, TUI) render from it directly and MUST NOT re-query those
// subsystems to recover the state — the snapshot is authoritative, and
// cross-component re-derivation is exactly the drift this type prevents.
//
// The five states are mutually exclusive and exhaustive for a provider
// declared in apps config:
//
//   - not-installed   — no install on disk, no credentials. UI: install.
//   - installing      — install coordinator reports an in-flight job.
//     UI: show progress, no chat/uninstall until terminal.
//   - dormant         — IsInstalled() true, but provider is not enabled in
//     config (finalize failed, user disabled, partial
//     install wrote the version file before crashing).
//     UI: uninstall or enable. Not routable.
//   - live            — enabled in config AND registered in the runtime
//     protocol registry. UI: chat, disable, uninstall.
//   - cloud-available — cloud provider with resolvable credentials. UI:
//     chat. No install/uninstall — credentials-only.
//
// "installed" as a boolean is derivable from State (dormant/live/cloud-
// available are all installed in some sense); the wire format keeps a
// derived `installed` field for back-compat with older clients.
type LifecycleState string

// String helpers let switch statements read cleanly without quoting.
const (
	StateNotInstalled   LifecycleState = "not-installed"
	StateInstalling     LifecycleState = "installing"
	StateDormant        LifecycleState = "dormant"
	StateLive           LifecycleState = "live"
	StateCloudAvailable LifecycleState = "cloud-available"
)

// IsInstalled collapses the five-state enum to the back-compat boolean
// the wire format and older TUI code expect. live, dormant, and
// cloud-available are all "installed" — not-installed and installing
// are not. We intentionally return false for installing: a mid-flight
// job isn't installed yet, and callers that render "installed?"
// checkmarks should wait for terminal state before claiming success.
func (s LifecycleState) IsInstalled() bool {
	switch s {
	case StateLive, StateDormant, StateCloudAvailable:
		return true
	default:
		return false
	}
}

// IsRoutable reports whether the provider can accept inference requests
// right now. Used by dispatchers that must filter the listing before
// routing — listing-presence alone is not a routability guarantee since
// dormant and installing states surface in the same list.
func (s LifecycleState) IsRoutable() bool {
	return s == StateLive || s == StateCloudAvailable
}

// versionUnknown is what a provider reports when no version could be
// established. It is a sentinel, not a version: code deciding whether
// something is installed must exclude it explicitly.
const versionUnknown = "unknown"

// LocalProviderInfo is the composed snapshot of a single provider on the
// local node. The producer is ProviderAppManager.LocalInfo; the consumer
// set is HTTP shell (wire map), TUI (display), and cluster gossip (node
// snapshot). Nothing in the consumer set re-derives State — if a new
// display decision depends on a fact that isn't captured here, add it
// here rather than reaching back into Protocols/Install/AppsConfig at
// the call site.
type LocalProviderInfo struct {
	Key   string         `json:"key"`
	Name  string         `json:"name"`
	Type  string         `json:"type"`
	Kind  string         `json:"kind"`
	Mode  string         `json:"mode"`
	State LifecycleState `json:"state"`
	// Managed reports that zzRouter put this install here (marker file
	// plus a binary), which is what makes uninstalling it our business.
	// It is a real field rather than one of the derived booleans below
	// because State cannot carry it: a provider is equally Dormant
	// whether zzRouter installed it or it was found already present.
	Managed            bool                   `json:"managed"`
	Node               string                 `json:"node,omitempty"`
	Version            string                 `json:"version"`
	Endpoint           string                 `json:"endpoint,omitempty"`
	Formats            []string               `json:"formats,omitempty"`
	FormatNote         string                 `json:"format_note,omitempty"`
	Description        string                 `json:"description,omitempty"`
	SupportsRouting    bool                   `json:"supports_routing"`
	SupportsDownload   bool                   `json:"supports_download"`
	SupportsLoadUnload bool                   `json:"supports_load_unload"`
	Service            *ProviderServiceStatus `json:"service,omitempty"`
}

// MarshalJSON emits the struct plus three derived booleans
// (enabled/installed/cloud) so decoders that predate the State enum keep
// working. State is the single source of truth for those three — never
// hold them as struct fields. Managed is not among them precisely
// because it is not derivable from State; see the field.
func (l LocalProviderInfo) MarshalJSON() ([]byte, error) {
	type alias LocalProviderInfo
	return json.Marshal(struct {
		alias
		Enabled   bool `json:"enabled"`
		Installed bool `json:"installed"`
		Cloud     bool `json:"cloud"`
	}{
		alias:     alias(l),
		Enabled:   l.State == StateLive || l.State == StateCloudAvailable,
		Installed: l.State.IsInstalled(),
		Cloud:     l.Kind == string(config.KindCloud),
	})
}

// LocalInfo returns every provider declared in this node's apps config as
// a LocalProviderInfo, with its LifecycleState composed from the three
// canonical sources of truth (install coordinator for in-flight jobs,
// protocol registry for enabled/routable, installer for on-disk state,
// apps config for credentials). Callers render; they do not re-compose.
//
// Ordering: registries are filtered out (they aren't providers in the
// user-facing sense). Cloud providers without credentials are surfaced
// as not-installed so the UI can still offer "set up" even though the
// cloud kind has no on-disk install step.
//
// Concurrency: each sub-lookup (Progress, IsInstalled, IsEnabled,
// IsAvailable) is individually thread-safe; the composition is a
// point-in-time snapshot. A racing install that transitions between
// installing → dormant → live mid-range produces a slice in which one
// entry can reflect any of those states, which is correct — the next
// listing call sees the newer state.
func (m *ProviderAppManager) LocalInfo(nodeName string) []LocalProviderInfo {
	m.mu.RLock()
	cfg := m.appsConfig
	m.mu.RUnlock()
	if cfg == nil {
		return nil
	}

	out := make([]LocalProviderInfo, 0)
	cfg.RangeApps(func(key string, svc config.ServiceConfig) bool {
		p := cfg.Find(key)
		if p == nil {
			return true
		}
		if svc.IsModelHubRegistry() {
			return true // registries aren't providers in this view
		}

		state, managed := m.lifecycleFor(key, p)
		var endpoint string
		if svc.Runtime != nil {
			endpoint = svc.Runtime.Endpoint
		}
		info := LocalProviderInfo{
			Key:              key,
			Name:             svc.Name,
			Type:             svc.Name,
			Kind:             string(p.Kind()),
			Mode:             svc.Mode,
			State:            state,
			Managed:          managed,
			Node:             nodeName,
			Version:          m.ProviderVersion(key),
			Endpoint:         endpoint,
			Formats:          formatsFor(p),
			FormatNote:       formatNoteFor(p),
			Description:      describe(svc.Name, state),
			SupportsRouting:  state.IsRoutable(),
			SupportsDownload: state.IsRoutable(),
		}
		if info.Version == "" {
			info.Version = versionUnknown
		}
		// Ollama's load/unload flag is part of its protocol contract; preserving
		// the previous hosts.go behavior so the TUI keeps offering load/unload
		// buttons only for ollama-type providers.
		if strings.EqualFold(svc.Name, constants.AppOllama) {
			info.SupportsLoadUnload = true
		}
		out = append(out, info)
		return true
	})
	return out
}

// lifecycleFor composes a single provider's state. Precedence is ordered:
//
//  1. In-flight install → installing (most recent action wins; a racing
//     re-install during a dormant-recovery must surface as installing,
//     not dormant).
//  2. Cloud kind → cloud-available if credentials resolve, else not-installed.
//  3. Enabled in config AND registered in protocol registry → live.
//  4. IsInstalled on disk → dormant.
//  5. Otherwise → not-installed.
//
// The Enabled-without-protocol case is reachable during startup before
// the registry finishes syncing; treating it as dormant briefly is
// acceptable — the next listing call after syncVersions completes will
// flip it to live.
func (m *ProviderAppManager) lifecycleFor(key string, p config.Provider) (LifecycleState, bool) {
	if prog := m.installs.Progress(key); prog != nil && prog.Status == install.StatusRunning {
		return StateInstalling, false
	}
	if cp, ok := p.(*config.CloudProvider); ok {
		// IsEnabled gate comes first: a disabled cloud provider is
		// "not installed" from the UI's perspective even if credentials
		// remain resolvable in-process (env vars survive .env rewrites
		// until the next server restart).
		if !cp.IsEnabled() || !cp.IsAvailable() {
			return StateNotInstalled, false
		}
		// A cloud provider has no on-disk install to own.
		return StateCloudAvailable, false
	}
	// Non-cloud providers. Live = enabled + routable + actually installed
	// on disk. Dropping the IsInstalled gate (as a prior version did)
	// makes a provider that was enabled-by-default in the template but
	// never installed (vllm on macOS, for example) lie as installed:true
	// + version:unknown — the UI then offers a Stop button for a binary
	// that was never spawned. Three signals must agree:
	//   1. Config says enabled (operator opted in)
	//   2. Protocol registry has a routable handler (sync has run)
	//   3. Installer reports a real on-disk install (binary present)
	inst, dispErr := m.installs.Dispatcher().Get(key)
	managed := dispErr == nil && inst.IsInstalled()
	// A provider zzRouter did not install is still installed. IsInstalled
	// on the installer means "we put it here" (marker file + binary), so
	// reading it as the whole answer made an operator's own Ollama report
	// installed:false here while /providers/{name}/status, which asks the
	// broader question, reported installed:true for the same binary.
	//
	// The version cache is the detection channel, and the "unknown" guard
	// is what keeps this honest: a provider enabled by default in the
	// template but never installed (vllm on macOS) has no version, and
	// must not be promoted to installed on the strength of being enabled.
	version := m.ProviderVersion(key)
	installed := managed || (version != "" && version != versionUnknown)
	_, routable := m.protocols.Get(key)
	if p.IsEnabled() && routable == nil && installed {
		return StateLive, managed
	}
	if installed {
		return StateDormant, managed
	}
	return StateNotInstalled, managed
}

// describe returns the UI-visible description. A "(dormant)" suffix on
// dormant providers is how the TUI telegraphs "this is recoverable, not
// active" to the user without needing a separate icon column.
func describe(name string, state LifecycleState) string {
	base := fmt.Sprintf("%s app", name)
	if state == StateDormant {
		return base + " (dormant)"
	}
	return base
}

// formatsFor extracts the provider's declared model formats from the
// typed provider. OnDemand/External providers carry Capabilities; Cloud
// providers don't declare formats in this field (their "format" is the
// cloud API), so we return nil — the UI renders an em-dash.
func formatsFor(p config.Provider) []string {
	switch tp := p.(type) {
	case *config.OnDemandProvider:
		if tp.Capabilities != nil {
			return append([]string(nil), tp.Capabilities.Formats...)
		}
	case *config.ExternalProvider:
		if tp.Capabilities != nil {
			return append([]string(nil), tp.Capabilities.Formats...)
		}
	}
	return nil
}

func formatNoteFor(p config.Provider) string {
	switch tp := p.(type) {
	case *config.OnDemandProvider:
		if tp.Capabilities != nil {
			return tp.Capabilities.FormatNote
		}
	case *config.ExternalProvider:
		if tp.Capabilities != nil {
			return tp.Capabilities.FormatNote
		}
	}
	return ""
}
