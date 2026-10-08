package server

import (
	"fmt"
	"net"
	"runtime"
)

// ============================================================================
// Cross-cutting server helpers
// ============================================================================
//
// This file holds small utilities that don't fit in any of the
// feature-specific files: OS name normalization, local-IP
// discovery, and the canonical "ip / port / address" tuple used
// by every API response that reports a node endpoint.
//
// Everything here was previously in host_utils.go alongside the
// hardware DTOs and the (now-deleted) duplicate GPU detection
// pipeline; the file was dissolved once the GPU consolidation
// arc moved all hardware reads into pkg/discovery/gpu. The
// hardware DTOs landed in hosts.go next to their only consumer;
// these three helpers live here because they're called from
// both hosts.go and system_utils.go and don't belong in either.

// getOSName returns a user-friendly OS name.
func getOSName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
}

// nodeEndpointFields returns the standard ip_address, port, and
// address fields for API responses. address is the reachable
// host:port (using the actual IP, not the bind address — a
// 0.0.0.0 bind reports as the machine's routable IP so clients
// on the cluster can dial the returned value back).
func (s *Server) nodeEndpointFields() (ipAddress string, port int, address string) {
	ipAddress = getActualIPAddress(s.node.Bind())
	port = s.node.Port()
	address = fmt.Sprintf("%s:%d", ipAddress, port)
	return
}

// getActualIPAddress returns the actual IP address, not 0.0.0.0.
// On a 0.0.0.0 / empty / :: bind it dials an external host to
// learn which local interface Linux would route through, then
// returns that interface's address. Falls back to walking
// net.InterfaceAddrs() if the dial fails (offline host), and to
// 127.0.0.1 as a last resort so the return value is never blank.
func getActualIPAddress(configNode string) string {
	// If config host is not 0.0.0.0 or empty, use it
	if configNode != "" && configNode != "0.0.0.0" && configNode != "::" {
		return configNode
	}

	// Try to get the actual IP address
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		// Fallback: try to get local IP from interfaces
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return "127.0.0.1"
		}

		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					return ipnet.IP.String()
				}
			}
		}
		return "127.0.0.1"
	}
	defer func() { _ = conn.Close() }()

	if localAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return localAddr.IP.String()
	}
	return "127.0.0.1"
}
