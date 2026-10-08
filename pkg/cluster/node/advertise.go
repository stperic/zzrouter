package clusternode

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// noisyInterfacePrefixes is a curated blocklist of interface-name prefixes
// whose IPs we never want in a cluster mTLS cert SAN, even if they're
// nominally routable unicast. Filters fall into three groups:
//
//   - Apple-specific virtual interfaces (awdl, llw, anpi, ap-numeric)
//     that AirDrop / Continuity / privacy-relay layer on top of Wi-Fi.
//     These are alive but cluster traffic has no business there.
//   - VM bridge networks (vmnet, vboxnet, bridge) that VMware Fusion,
//     VirtualBox, and macOS Internet Sharing expose with private RFC1918
//     IPs. Operator intent is "give VMs a network", not "advertise as
//     a cluster member" — adding these to the SAN bloats certs and
//     leaks deployment topology.
//   - Container bridges (docker, br-, veth) on Linux. Same rationale as
//     VM bridges.
//
// gif/stf are BSD generic + 6to4 tunnel pseudo-interfaces; they
// occasionally pick up routable IPs on macOS but are never the right
// answer for cluster mesh.
//
// utun* is intentionally NOT here: Tailscale + WireGuard surface as
// utun on macOS, and that IS a legitimate cluster path. The link-local
// filter in detectRoutableIPs already drops the inactive utuns macOS
// keeps for IPv6 routing.
var noisyInterfacePrefixes = []string{
	"awdl",    // Apple AirDrop / Continuity
	"llw",     // Apple low-latency wireless
	"anpi",    // Apple Network Privacy Interface
	"gif",     // BSD generic tunnel
	"stf",     // BSD 6to4 tunnel
	"bridge",  // macOS bridge0 (Internet Sharing, virtualization bridges)
	"vmnet",   // VMware Fusion / Workstation
	"vboxnet", // VirtualBox host-only
	"docker",  // Docker default bridge on Linux
	"br-",     // Linux Docker user-defined bridges
	"veth",    // Linux container virtual ethernet
}

// isNoisyInterface reports whether name matches a known-noisy prefix
// from noisyInterfacePrefixes (plus the apN apple-wireless-P2P pattern,
// which is name-length-sensitive and handled separately to avoid
// collisions with operator-named interfaces starting with "ap").
func isNoisyInterface(name string) bool {
	for _, p := range noisyInterfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	// Apple AirPlay/wireless P2P: ap0, ap1, … Strict shape (ap + digits)
	// avoids matching e.g. "ap-prod-01" should an operator name an
	// interface that way.
	if strings.HasPrefix(name, "ap") && len(name) > 2 {
		allDigits := true
		for _, r := range name[2:] {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return true
		}
	}
	return false
}

// CoordinatorSelfCoordURL returns the full URL that workers should
// persist at pairing-accept time and dial on every renewal. Two
// resolution paths:
//
//  1. advertiseURL non-empty → use it verbatim (after parse
//     validation). The operator-configured value wins because
//     derived-from-bind can't capture reverse-proxy or NAT
//     topologies. Validated at Server Start via ValidateAdvertiseURL;
//     this re-parse is defense-in-depth for hot-reload scenarios.
//
//  2. advertiseURL empty → derive "https://<host>:<port>" from the
//     listener's bound address. host is bindHost unless the bind is
//     a wildcard (0.0.0.0, ::); when wildcard, prefer the first
//     non-loopback entry from the coordinator's own
//     cluster.advertise_ips so a paired worker can dial the
//     coordinator directly by IP without needing DNS. Falls back to
//     nodeName only when no usable coordinator IP is configured —
//     that path requires the worker to resolve the coordinator's
//     hostname out of band, which is fragile across subnets. port
//     comes from the actual bound address so an OS-assigned port
//     (test harness) is still captured.
//
// Scheme is hardcoded https — the cluster port is definitionally
// mTLS TLS 1.3 regardless of the admin-port's TLS toggle. Using the
// admin-port's scheme would ship http:// to workers when a coord
// has admin-port TLS off, breaking their renewal handshake.
//
// Returns an error if the cluster listener isn't bound yet —
// persisting a port-zero URL would silently break all future
// renewal dials, so we prefer 503 "not ready" over a malformed
// coordinator_url on disk at the worker.
func CoordinatorSelfCoordURL(n *Node, bindHost, nodeName, advertiseURL string) (string, error) {
	if advertiseURL != "" {
		if err := ValidateAdvertiseURL(advertiseURL); err != nil {
			return "", err
		}
		return advertiseURL, nil
	}

	addr := n.Addr()
	if addr == nil {
		return "", errors.New("cluster listener not bound")
	}
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil || port == "" {
		return "", fmt.Errorf("parse listener addr %q: %w", addr.String(), err)
	}
	return "https://" + net.JoinHostPort(ResolveSelfHost(bindHost, nodeName, n.cfg.AdvertiseIPs), port), nil
}

// ResolveSelfHost picks the host a peer should dial us on: bindHost if
// concrete, first non-loopback advertise IP if bindHost is wildcard,
// nodeName as last resort. Shared by CoordinatorSelfCoordURL and the
// worker-side public URL synthesis.
func ResolveSelfHost(bindHost, nodeName string, ips []net.IP) string {
	if bindHost != "" && bindHost != "0.0.0.0" && bindHost != "::" {
		return bindHost
	}
	for _, ip := range ips {
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		return ip.String()
	}
	return nodeName
}

// detectRoutableIPs returns the IPs the host is reachable on for cert
// SAN defaulting. When bindHost is a concrete non-loopback address, it
// IS the answer — we trust the operator's bind. When bindHost is empty
// or a wildcard (0.0.0.0, ::), enumerate interfaces and keep the
// routable IPs from non-noisy interfaces.
//
// Two layers of filtering:
//
//  1. Interface-level: skip down interfaces (FlagUp clear) and
//     skip well-known noisy interface names (Apple AirDrop / VM
//     bridges / container bridges — see noisyInterfacePrefixes).
//     A bridge0 with a private RFC1918 IP from VMware Fusion has no
//     business in a cluster cert SAN even though it's nominally
//     routable.
//
//  2. Address-level: skip loopback / unspecified / multicast /
//     link-local-unicast. IPv6 fe80::/10 is dialable but only on the
//     originating link, useless for cluster mesh and prone to the
//     "bad certificate" foot-gun once zone IDs strip.
//
// Returns nil on enumeration failure; advertiseIPs() always layers
// loopback on top so the SAN is never empty.
func detectRoutableIPs(bindHost string) []net.IP {
	if bindHost != "" && bindHost != "0.0.0.0" && bindHost != "::" {
		if ip := net.ParseIP(bindHost); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return []net.IP{ip}
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if isNoisyInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}

// ValidateAdvertiseURL parses s and returns an error if it isn't a
// well-formed absolute URL with scheme and host. Called at Server
// Start (fail-fast) and again from the pairing-accept handler
// (defense-in-depth for any hot-reload path that mutates the field
// after Start).
func ValidateAdvertiseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("parse advertise_url %q: %w", s, err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("advertise_url missing scheme: %q", s)
	}
	if u.Host == "" {
		return fmt.Errorf("advertise_url missing host: %q", s)
	}
	return nil
}
