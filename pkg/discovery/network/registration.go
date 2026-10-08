// Package discovery provides mDNS-based host discovery for zzRouter
// Registration functionality for mDNS service registration
package network

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/grandcat/zeroconf"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// Start registers this host via mDNS. Alias for Register — satisfies
// the subsystem lifecycle convention where the server calls Start.
func (hd *NodeDiscovery) Start(port int) error {
	if hd == nil {
		return nil
	}
	return hd.Register(port)
}

// Register registers this host via mDNS.
//
// Workers are browse-only: they skip registration entirely so the
// coordinator is the sole broadcaster on the LAN. This lets a worker
// discover the coordinator and read its URL + CA fingerprint from TXT
// records without any pre-configured coordinator_url.
func (hd *NodeDiscovery) Register(port int) error {
	if !hd.isCoordinator {
		utils.LogDebugf("mDNS: skipping registration (worker is browse-only; coordinator is sole broadcaster)")
		return nil
	}

	// Intentionally NOT advertising the CA fingerprint. mDNS is an
	// unauthenticated LAN broadcast — any attacker on the segment can
	// both spoof the URL and the fingerprint, so "pinning" against
	// an mDNS-delivered fingerprint is security theater indistinguishable
	// from TOFU. The operator pastes the fingerprint via an OOB
	// channel (--ca-fingerprint or the `--secure` wizard prompt) when
	// real pinning is wanted; otherwise pairing runs as TOFU.

	// Validate port
	if port < MinPort || port > MaxPort {
		return fmt.Errorf("invalid port number: %d", port)
	}

	// Use configured node name, fall back to OS hostname
	nodeName := hd.nodeName
	if nodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("failed to get hostname: %w", err)
		}
		nodeName = hostname
	}

	// Validate node name
	if !IsValidNodename(nodeName) {
		nodeName = "zzrouter-node" // Fallback to safe default
		utils.LogWarnf("Invalid node name '%s', using fallback 'zzrouter-node'", nodeName)
	}

	// Create service name with coordinator suffix if applicable
	serviceName := nodeName
	if hd.isCoordinator {
		serviceName = fmt.Sprintf("%s-coordinator", nodeName)
	}

	// Validate and intelligently truncate service name (mDNS limit is 63 chars for labels)
	if len(serviceName) > 63 {
		serviceName = hd.truncateServiceName(serviceName, 63)
		utils.LogWarnf("Service name truncated to: %s", serviceName)
	}

	txtRecords := buildTXTRecords(nodeName, port, hd.clusterPort, hd.isCoordinator, version.Current.String(), utils.Now().Unix())

	// Try zeroconf first (works on macOS, bare metal Linux).
	// Use interfaces cached at init time to avoid gopsutil netlink corruption.
	ifaces := CachedMulticastInterfaces()
	var ifaceArg []net.Interface
	if len(ifaces) > 0 {
		ifaceArg = ifaces
	}

	server, err := zeroconf.Register(serviceName, ServiceType, ServiceDomain, port, txtRecords, ifaceArg)
	if err == nil {
		hd.server = server
		utils.LogDebugf("Node registered via mDNS: %s on port %d", serviceName, port)
		return nil
	}

	// zeroconf failed (common in LXC containers where gopsutil corrupts netlink).
	// Fall back to avahi-publish-service if available.
	utils.LogWarnf("zeroconf registration failed: %v; trying avahi fallback", err)

	if avahiErr := hd.registerViaAvahi(serviceName, port, txtRecords); avahiErr != nil {
		return fmt.Errorf("mDNS registration failed (zeroconf: %v, avahi: %v)", err, avahiErr)
	}

	return nil
}

// buildTXTRecords assembles the mDNS TXT records a coordinator
// advertises. Pure function — no side effects, no package globals —
// so registration.go can stay focused on zeroconf/avahi plumbing and
// the TXT-shape invariants can be unit-tested directly.
//
// Browse-only workers parse `cluster_port` to dial the pairing TLS
// endpoint directly, without any pre-configured coordinator_url.
// `port` is the public zzRouter admin/inference port (existing
// contract for `zzrouter connect`); `cluster_port` is the separate
// mTLS port where pairing lives. The CA fingerprint is intentionally
// NOT broadcast — mDNS is unauthenticated, so operators supply the
// pin via an OOB channel when real verification is required.
func buildTXTRecords(nodeName string, port, clusterPort int, isCoordinator bool, ver string, ts int64) []string {
	records := []string{
		fmt.Sprintf("nodename=%s", nodeName),
		fmt.Sprintf("port=%d", port),
		fmt.Sprintf("coordinator=%t", isCoordinator),
		fmt.Sprintf("version=%s", ver),
		"service=zzrouter",
		fmt.Sprintf("timestamp=%d", ts),
	}
	if clusterPort > 0 {
		records = append(records, fmt.Sprintf("cluster_port=%d", clusterPort))
	}
	return records
}

// truncateServiceName intelligently truncates a service name to fit within the limit
func (hd *NodeDiscovery) truncateServiceName(name string, maxLength int) string {
	if len(name) <= maxLength {
		return name
	}

	// Safety check for very small maxLength values
	if maxLength <= 0 {
		return ""
	}

	// If it's a coordinator node, try to preserve the "-coordinator" suffix
	if hd.isCoordinator && strings.HasSuffix(name, "-coordinator") {
		baseName := strings.TrimSuffix(name, "-coordinator")
		coordinatorSuffix := "-coordinator"

		// Ensure we don't go below the length of "-coordinator"
		if maxLength <= len(coordinatorSuffix) {
			// Can't fit "-coordinator", just truncate normally
			return name[:maxLength]
		}

		if len(baseName) <= maxLength-len(coordinatorSuffix) {
			// Can fit the full name with -coordinator
			return baseName + coordinatorSuffix
		}
		// Truncate the base name but keep -coordinator
		truncatedBase := baseName[:maxLength-len(coordinatorSuffix)]
		return truncatedBase + coordinatorSuffix
	}

	// For regular names, just truncate at the limit
	return name[:maxLength]
}

// registerViaAvahi uses avahi-publish-service as a fallback for mDNS registration.
// This works in LXC containers where Go's netlink is corrupted by gopsutil.
func (hd *NodeDiscovery) registerViaAvahi(serviceName string, port int, txtRecords []string) error {
	avahiPath, err := exec.LookPath("avahi-publish-service")
	if err != nil {
		return fmt.Errorf("avahi-publish-service not found: %w", err)
	}

	// Build args: avahi-publish-service <name> <type> <port> [txt...]
	args := []string{serviceName, ServiceType, fmt.Sprintf("%d", port)}
	args = append(args, txtRecords...)

	cmd := host.Command(avahiPath, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start avahi-publish-service: %w", err)
	}

	hd.avahiProcess = cmd.Process
	utils.LogInfof("Node registered via avahi: %s on port %d", serviceName, port)
	return nil
}

// Stop shuts down the mDNS service.
func (hd *NodeDiscovery) Stop() {
	if hd == nil {
		return
	}
	if hd.avahiProcess != nil {
		_ = hd.avahiProcess.Kill()
		_, _ = hd.avahiProcess.Wait()
		utils.LogInfof("Avahi mDNS service shut down")
	}
	if hd.server != nil {
		hd.server.Shutdown()
		utils.LogInfof("Node discovery service shut down")
	}
}
