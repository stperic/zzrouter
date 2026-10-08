package server

import (
	"net"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// NodeIdentityReport answers "which config is this process actually
// running?".
//
// It exists because a node can have more than one node.yaml. A server
// installed as a service runs under another account and reads that
// account's file, while an operator's CLI reads their own. Both are
// valid configs, they simply are not the same one, and every symptom of
// the split surfaces somewhere else entirely: a pair attempt refused as
// "this node is a coordinator", a stop that undoes itself. Having the
// server state its own answer is what collapses that into one check.
type NodeIdentityReport struct {
	// Name is the resolved node name (never empty on a loaded config;
	// see NodeConfig.applyLoadDefaults).
	Name string `json:"node_name"`

	// Mode is the effective cluster mode, with an unset mode rendered
	// as "disabled" so callers compare values rather than guessing at
	// an empty string.
	Mode string `json:"cluster_mode"`

	// ConfigPath is the file the config was read from. It names the OS
	// account the server runs under, so it is withheld from callers
	// that are neither admin-authenticated nor local.
	ConfigPath string `json:"config_path,omitempty"`
}

// nodeIdentityReportFrom builds the report from a loaded config.
func nodeIdentityReportFrom(cfg *pkgConfig.NodeConfig) NodeIdentityReport {
	mode := cfg.Cluster.Mode
	if mode == "" {
		mode = pkgConfig.ClusterModeDisabled
	}
	return NodeIdentityReport{
		Name:       cfg.Node.Name,
		Mode:       string(mode),
		ConfigPath: cfg.SourcePath,
	}
}

// nodeIdentityReport builds the report from the config this server
// loaded.
func (s *Server) nodeIdentityReport() NodeIdentityReport {
	return nodeIdentityReportFrom(s.config)
}

// forRemoteAddr withholds the fields an off-box caller has no business
// reading. The unauthenticated health route is reachable from the LAN,
// but the caller that needs the config path is this node's own CLI, so
// loopback is the line.
//
// addr is read from Request.RemoteAddr rather than gin's RemoteIP,
// which honours forwarded-for headers a caller controls.
func (r NodeIdentityReport) forRemoteAddr(addr string) NodeIdentityReport {
	if !isLoopbackAddr(addr) {
		r.ConfigPath = ""
	}
	return r
}

// isLoopbackAddr reports whether a "host:port" came from this machine.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
