package constants

import (
	"net/url"
	"strings"
)

// Host-related constants
const (
	// Localhost is the standard localhost hostname
	Localhost = "localhost"

	// LocalhostIP is the loopback IP address
	LocalhostIP = "127.0.0.1"

	// WildcardHost matches all hosts
	WildcardHost = "*"
)

// NodeIdentifier represents a parsed host identifier
type NodeIdentifier struct {
	Nodename string // Just the hostname/IP (e.g., "mac", "192.0.2.10")
	Port     string // Port if specified (e.g., "9090")
	Scheme   string // http or https if specified
	Original string // Original input
}

// ParseNodeIdentifier parses various host formats into a normalized NodeIdentifier
// Supports:
//   - Empty or "@master": Current/master host (default)
//   - "localhost": Local host
//   - "*": Wildcard (all hosts)
//   - Simple hostname: "mac", "gpu-server"
//   - IP address: "192.0.2.10"
//   - Node with port: "mac:9090", "192.0.2.10:9090"
//   - Full URL: "http://mac:9090", "https://192.0.2.10:9090"
func ParseNodeIdentifier(host string) *NodeIdentifier {
	if host == "" {
		return &NodeIdentifier{
			Nodename: "",
			Original: "",
		}
	}

	original := host
	host = strings.TrimSpace(host)

	// Special notation: @master means current/master host
	if host == "@master" {
		return &NodeIdentifier{
			Nodename: "localhost",
			Original: original,
		}
	}

	// Check if it's a wildcard
	if host == "*" || host == "/*" || host == "/*/*" {
		return &NodeIdentifier{
			Nodename: "*",
			Original: original,
		}
	}

	// Try to parse as URL first
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		if parsed, err := url.Parse(host); err == nil {
			return &NodeIdentifier{
				Nodename: parsed.Hostname(),
				Port:     parsed.Port(),
				Scheme:   parsed.Scheme,
				Original: original,
			}
		}
	}

	// Check if it has a port (contains : but not IPv6)
	if strings.Contains(host, ":") && !strings.Contains(host, "::") {
		parts := strings.Split(host, ":")
		if len(parts) == 2 {
			return &NodeIdentifier{
				Nodename: parts[0],
				Port:     parts[1],
				Original: original,
			}
		}
	}

	// Simple hostname or IP
	return &NodeIdentifier{
		Nodename: host,
		Original: original,
	}
}

// IsWildcard returns true if this is a wildcard host
func (h *NodeIdentifier) IsWildcard() bool {
	return h.Nodename == "*"
}

// IsLocalhost returns true if this refers to localhost
func (h *NodeIdentifier) IsLocalhost() bool {
	lower := strings.ToLower(h.Nodename)
	return lower == "localhost" ||
		lower == "127.0.0.1" ||
		lower == "::1" ||
		lower == "0.0.0.0" ||
		strings.Contains(lower, "localhost") ||
		strings.HasSuffix(lower, ".local") ||
		strings.HasSuffix(lower, ".localdomain")
}

// IsMaster returns true if this refers to the master/current host
// Checks for empty string, @master, or localhost
func (h *NodeIdentifier) IsMaster() bool {
	if h.Nodename == "" {
		return true // Empty means default to master
	}
	if h.Original == "@master" {
		return true
	}
	return h.IsLocalhost()
}
