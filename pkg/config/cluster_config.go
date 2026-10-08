package config

// ClusterMode defines the cluster operation mode
type ClusterMode string

const (
	ClusterModeDisabled    ClusterMode = "disabled"    // Standalone mode, no clustering
	ClusterModeCoordinator ClusterMode = "coordinator" // Coordinator node, manages cluster
	ClusterModeWorker      ClusterMode = "worker"      // Worker node, joins cluster
	ClusterModeStandalone  ClusterMode = "standalone"  // Explicit standalone (no cluster)
)

// ClusterConfig holds cluster networking configuration
type ClusterConfig struct {
	// Cluster mode: "disabled" (standalone), "coordinator" (manages cluster), "worker" (joins cluster)
	Mode ClusterMode `mapstructure:"mode" yaml:"mode,omitempty"`

	BindAddr         string            `mapstructure:"bind_addr" yaml:"bind_addr,omitempty"`
	BindPort         int               `mapstructure:"bind_port" yaml:"bind_port,omitempty"`
	AdvertisePort    int               `mapstructure:"advertise_port" yaml:"advertise_port,omitempty"`
	Members          []string          `mapstructure:"members" yaml:"members,omitempty"`
	Endpoints        ClusterPeers      `mapstructure:"endpoints" yaml:"endpoints,omitempty"`
	StrategyDefaults []StrategyDefault `mapstructure:"strategy_defaults" yaml:"strategy_defaults,omitempty"`

	// AdvertiseIPs and AdvertiseDNSNames populate the worker/coordinator
	// identity cert's Subject Alternative Names. The coordinator dials
	// `https://<endpoint-host>:bind_port` — the stdlib TLS stack verifies
	// that <endpoint-host> matches a SAN on the peer cert.
	// When unset, clusternode defaults to [NodeName] + [127.0.0.1, ::1],
	// which is fine for loopback tests but breaks real-network dispatch.
	// Deployment rule: list every hostname / IP the peer will be reached
	// by. Cert is regenerated when this changes (worker re-claim or
	// coordinator restart required to re-sign).
	AdvertiseIPs      []string `mapstructure:"advertise_ips" yaml:"advertise_ips,omitempty"`
	AdvertiseDNSNames []string `mapstructure:"advertise_dns_names" yaml:"advertise_dns_names,omitempty"`

	// TLS CA certificate for verifying public-port HTTPS cluster node
	// certificates. Required when nodes use self-signed or internal CA
	// certs on their PUBLIC (admin/inference) port. Separate from the
	// mTLS cluster-listener CA, which is generated per-coordinator by
	// clusternode itself. If empty, the system trust store is used.
	TLSCACert string `mapstructure:"tls_ca_cert" yaml:"tls_ca_cert,omitempty"`

	// Note: coordinator URL + CA fingerprint are NOT persisted in user
	// config. They are pairing-time inputs only — supplied via
	// `zzrouter cluster pair --coordinator-url ... --ca-fingerprint ...`,
	// discovered via mDNS from the coordinator's TXT records, or
	// entered interactively through the install wizard. Post-pairing,
	// the coord URL + CA cert live in clusterDir (written from the
	// pairing approval response); user config stays clean.

	// AdvertiseURL is the coord-side override for the coordinator URL
	// workers persist on pairing-accept and dial on every renewal.
	// Required when the coord's bind address isn't what workers
	// should dial — e.g. bound on 0.0.0.0 behind a reverse proxy, or
	// NAT'd where the internal bind host doesn't resolve externally.
	//
	// Format: full URL including scheme, e.g.
	// "https://coord-01.internal:9091". When empty, the coord falls
	// back to deriving "<scheme>://<bind-host>:<bind-port>" which
	// only works when bind-host is already reachable by workers.
	//
	// Wrong or unset AdvertiseURL on a deploy is a latent time bomb:
	// pairing completes successfully, the worker persists a bad URL,
	// and renewal dials start failing 15 days later when the
	// refresh threshold hits. Validate at startup via url.Parse so
	// operators find out at boot, not at first renewal.
	AdvertiseURL string `mapstructure:"advertise_url" yaml:"advertise_url,omitempty"`

	// RequireSecurePairing makes the coordinator reject worker pair
	// requests that did NOT pin the coordinator's CA fingerprint on
	// the bootstrap handshake. Home-lab default is false (accept
	// TOFU). Set to true on hostile or multi-tenant networks to
	// require every pairing worker to prove it obtained the coord
	// fingerprint out of band before initiating the request — the
	// worker echoes the fingerprint in its pair request body and the
	// coordinator verifies it matches its own identity.
	//
	// Zero-value (false) is the permissive home-lab default.
	RequireSecurePairing bool `mapstructure:"require_secure_pairing" yaml:"require_secure_pairing,omitempty"`
}

// IsCoordinator returns true if this node is configured as a cluster coordinator
func (cc *ClusterConfig) IsCoordinator() bool {
	return cc.Mode == ClusterModeCoordinator
}

// IsMaster is an alias for IsCoordinator (backwards compatibility)
func (cc *ClusterConfig) IsMaster() bool {
	return cc.IsCoordinator()
}

// IsWorker returns true if this node is configured as a cluster worker
func (cc *ClusterConfig) IsWorker() bool {
	return cc.Mode == ClusterModeWorker
}

// IsDisabled returns true if clustering is disabled (standalone mode)
func (cc *ClusterConfig) IsDisabled() bool {
	// Empty mode defaults to disabled
	if cc.Mode == "" {
		return true
	}
	return cc.Mode == ClusterModeDisabled
}

// SetMode sets the cluster mode
func (cc *ClusterConfig) SetMode(mode ClusterMode) {
	cc.Mode = mode
}

// StrategyDefault defines default strategy for path patterns
type StrategyDefault struct {
	PathPrefix string `mapstructure:"path_prefix" yaml:"path_prefix"`
	Default    string `mapstructure:"default" yaml:"default"`
}

// ToClusterConfig converts ClusterConfig to cluster.Config to avoid import cycles
// The cluster package has its own Config type to prevent circular dependencies
func (cc *ClusterConfig) ToClusterConfig() any {
	// Return as interface{} to avoid importing pkg/cluster
	// The caller will type assert to cluster.Config
	// Derive Enabled from Mode: enabled when mode is coordinator or worker
	enabled := cc.Mode == ClusterModeCoordinator || cc.Mode == ClusterModeWorker

	return struct {
		Enabled          bool
		BindAddr         string
		BindPort         int
		AdvertisePort    int
		Members          []string
		Endpoints        []string
		StrategyDefaults []struct {
			PathPrefix string
			Default    string
		}
	}{
		Enabled:       enabled,
		BindAddr:      cc.BindAddr,
		BindPort:      cc.BindPort,
		AdvertisePort: cc.AdvertisePort,
		Members:       cc.Members,
		Endpoints:     cc.Endpoints.Addresses(),
		StrategyDefaults: func() []struct {
			PathPrefix string
			Default    string
		} {
			result := make([]struct {
				PathPrefix string
				Default    string
			}, len(cc.StrategyDefaults))
			for i, sd := range cc.StrategyDefaults {
				result[i] = struct {
					PathPrefix string
					Default    string
				}{
					PathPrefix: sd.PathPrefix,
					Default:    sd.Default,
				}
			}
			return result
		}(),
	}
}

// ClusterPeer is one peer in the coordinator's membership list.
//
// Name is the peer's own node name, learned from a successful probe and
// written back so it survives a restart. Without it the coordinator has
// only the address until it has probed, and GET /nodes has nothing to
// report but the URL — an identifier the rest of the API rejects, since
// every `node` selector takes a name.
//
// Name is a cache, never authoritative: the peer owns its name, and a
// stale value is corrected by the next probe.
type ClusterPeer struct {
	Address string `mapstructure:"address" yaml:"address"`
	Name    string `mapstructure:"name" yaml:"name,omitempty"`
}

// MarshalYAML writes a bare string when no name is known, so an
// unprobed membership list stays as readable as it was before names
// existed, and only gains structure once there is something to hold.
func (e ClusterPeer) MarshalYAML() (any, error) {
	if e.Name == "" {
		return e.Address, nil
	}
	type plain ClusterPeer // avoid recursing into this method
	return plain(e), nil
}

// ClusterPeers is the coordinator's membership list.
type ClusterPeers []ClusterPeer

// Addresses projects the host:port values, which is what every consumer
// that predates names still wants.
func (c ClusterPeers) Addresses() []string {
	if len(c) == 0 {
		return nil
	}
	out := make([]string, len(c))
	for i, e := range c {
		out[i] = e.Address
	}
	return out
}

// IndexOf returns the position of address, or -1.
func (c ClusterPeers) IndexOf(address string) int {
	for i, e := range c {
		if e.Address == address {
			return i
		}
	}
	return -1
}

// NameFor returns the cached node name for address, or "" when the
// address is unknown or has not been probed yet.
func (c ClusterPeers) NameFor(address string) string {
	if i := c.IndexOf(address); i >= 0 {
		return c[i].Name
	}
	return ""
}

// PeersFromAddresses builds a membership list from bare addresses, for
// callers (and tests) that have no names to record yet.
func PeersFromAddresses(addresses ...string) ClusterPeers {
	if len(addresses) == 0 {
		return nil
	}
	out := make(ClusterPeers, len(addresses))
	for i, a := range addresses {
		out[i] = ClusterPeer{Address: a}
	}
	return out
}
