// Package discovery provides mDNS-based host discovery for zzRouter
// Discovery functionality for finding and processing mDNS services
package network

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// FindCoordinator runs a one-shot mDNS discovery and returns the
// first entry advertising `coordinator=true` along with a usable
// pairing URL (ClusterPort > 0). Returns (nil, nil) when discovery
// succeeds but no usable coordinator is found — callers fall back to
// interactive prompt for URL (and fingerprint under --secure).
// Returns (nil, err) on transport failure.
func (hd *NodeDiscovery) FindCoordinator(ctx context.Context) (*NodeEntry, error) {
	nodes, err := hd.DiscoverNodesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.IsCoordinator && n.PairingURL() != "" {
			return n, nil
		}
	}
	return nil, nil
}

// DiscoverNodes discovers other zzRouter nodes on the network
// If background discovery is running, returns current cached hosts.
// Otherwise performs a one-time discovery.
func (hd *NodeDiscovery) DiscoverNodes() ([]*NodeEntry, error) {
	return hd.DiscoverNodesWithContext(context.Background())
}

// DiscoverNodesWithContext discovers hosts with context for cancellation
// If background discovery is running, returns current cached hosts.
// Otherwise performs a one-time discovery with the provided context.
func (hd *NodeDiscovery) DiscoverNodesWithContext(ctx context.Context) ([]*NodeEntry, error) {
	// If background discovery is running, just return current cached hosts
	if hd.IsBackgroundDiscoveryRunning() {
		utils.LogDebugf("📋 Returning cached hosts from background discovery (%d hosts)", hd.GetNodeCount())
		return hd.GetNodes(), nil
	}

	// Background discovery not running, perform one-time discovery
	utils.LogDebugf("Performing one-time mDNS discovery for service '%s' on domain '%s'", ServiceType, ServiceDomain)

	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create resolver: %w", err)
	}

	entries := make(chan *zeroconf.ServiceEntry, ChannelBufferSize)

	// Create a context with timeout if none provided
	if ctx == context.Background() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
		utils.LogDebugf("⏱️  Starting discovery with %v timeout", DefaultTimeout)
	}

	// Start processing entries in background
	var wg sync.WaitGroup
	wg.Go(func() {
		entriesProcessed := 0
		for entry := range entries {
			entriesProcessed++
			select {
			case <-ctx.Done():
				utils.LogDebugf("Discovery cancelled after processing %d entries", entriesProcessed)
				return
			default:
				hd.processServiceEntry(entry)
			}
		}
		utils.LogDebugf("Processed %d mDNS entries total", entriesProcessed)
	})

	// Start browsing for services - this blocks until context is done
	utils.LogDebugf("Browsing for mDNS services...")
	err = resolver.Browse(ctx, ServiceType, ServiceDomain, entries)
	if err != nil {
		utils.LogDebugf("Browse failed: %v", err)
		return nil, fmt.Errorf("failed to browse services: %w", err)
	}

	// Wait for the processing goroutine to complete
	wg.Wait()

	// Browse has completed, return the discovered hosts
	return hd.GetNodes(), nil
}

// processServiceEntry processes a discovered service entry
func (hd *NodeDiscovery) processServiceEntry(entry *zeroconf.ServiceEntry) {
	if len(entry.AddrIPv4) == 0 {
		utils.LogDebugf("No IPv4 addresses found for service %s", entry.ServiceInstanceName())
		return
	}

	hostEntry := &NodeEntry{
		Name:     entry.ServiceInstanceName(),
		Node:     entry.AddrIPv4[0].String(),
		Port:     entry.Port,
		LastSeen: utils.Now(),
	}

	// Parse TXT records with better error handling
	for _, txt := range entry.Text {
		if err := hd.parseTXTRecord(txt, hostEntry); err != nil {
			utils.LogDebugf("Failed to parse TXT record '%s': %v", txt, err)
		}
	}

	// Validate the host entry before storing
	if err := hostEntry.Validate(); err != nil {
		utils.LogDebugf("Invalid host entry %s: %v", hostEntry.Name, err)
		return
	}

	hd.mu.Lock()
	// Check if this is a new host or an update
	isNew := hd.entries[hostEntry.Name] == nil
	hd.entries[hostEntry.Name] = hostEntry
	hd.mu.Unlock()

	// Print new discoveries immediately
	if isNew {
		fmt.Printf("Found host: %s\n", hostEntry.String())
	}
}

// parseTXTRecord parses a single TXT record and updates the host entry
func (hd *NodeDiscovery) parseTXTRecord(txt string, entry *NodeEntry) error {
	parts := strings.SplitN(txt, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid TXT record format: %s", txt)
	}

	key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])

	switch key {
	case string(config.ClusterModeCoordinator):
		// Critical field - log warning but continue with safe default
		isCoordinator, err := strconv.ParseBool(value)
		if err != nil {
			utils.LogDebugf("Failed to parse critical field 'coordinator'='%s', defaulting to false: %v", value, err)
			entry.IsCoordinator = false // Safe default for critical field
		} else {
			entry.IsCoordinator = isCoordinator
		}

	case "master":
		// Legacy support - parse master as coordinator
		isCoordinator, err := strconv.ParseBool(value)
		if err != nil {
			utils.LogDebugf("Failed to parse legacy field 'master'='%s', defaulting to false: %v", value, err)
			entry.IsCoordinator = false
		} else {
			entry.IsCoordinator = isCoordinator
		}

	case "version":
		// Non-critical field - silently handle invalid values
		if value == "" {
			entry.Version = "unknown"
		} else {
			entry.Version = value
		}

	case "nodename", "hostname":
		// Validation field - log if invalid but don't fail
		// Supports both new "nodename" and legacy "hostname" keys
		if !IsValidNodename(value) {
			utils.LogDebugf("Invalid nodename in TXT record: %s", value)
		} // Could store this for additional validation if needed

	case "port":
		// Consistency check - log mismatch but don't override service port
		if parsedPort, err := strconv.Atoi(value); err == nil {
			if parsedPort != entry.Port {
				utils.LogDebugf("Port mismatch: service announces %d but TXT says %d", entry.Port, parsedPort)
			}
		} else {
			utils.LogDebugf("Failed to parse port from TXT record: %s", value)
		}

	case "timestamp":
		// Optional metadata - silently ignore parse failures
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			utils.LogDebugf("Invalid timestamp in TXT record: %s", value)
		}

	case "service":
		// Validation field - log unexpected service types
		if value != "zzrouter" {
			utils.LogDebugf("Unexpected service type: %s", value)
		}

	case "cluster_port":
		// mTLS pairing port advertised by coordinators so browse-only
		// workers can construct the pairing URL without pre-config.
		if port, err := strconv.Atoi(value); err == nil {
			entry.ClusterPort = port
		} else {
			utils.LogDebugf("Failed to parse cluster_port from TXT record: %s", value)
		}

	default:
		// Unknown fields - log for debugging but don't treat as errors
		utils.LogDebugf("ℹ️ Unknown TXT record: %s=%s", key, value)
	}

	return nil
}

// StartBackgroundDiscovery starts a continuous background discovery process
func (hd *NodeDiscovery) StartBackgroundDiscovery() error {
	hd.mu.Lock()
	defer hd.mu.Unlock()

	if hd.isRunning {
		return fmt.Errorf("background discovery is already running")
	}

	hd.backgroundCtx, hd.backgroundCancel = context.WithCancel(context.Background())
	hd.isRunning = true

	go hd.backgroundDiscoveryLoop()
	utils.LogDebugf("🔄 Started background host discovery")
	return nil
}

// StopBackgroundDiscovery stops the background discovery process
func (hd *NodeDiscovery) StopBackgroundDiscovery() {
	hd.mu.Lock()
	defer hd.mu.Unlock()

	if !hd.isRunning {
		return
	}

	hd.backgroundCancel()
	<-hd.backgroundDone
	hd.isRunning = false
	utils.LogDebugf("🛑 Stopped background host discovery")
}

// IsBackgroundDiscoveryRunning returns whether background discovery is active
func (hd *NodeDiscovery) IsBackgroundDiscoveryRunning() bool {
	hd.mu.RLock()
	defer hd.mu.RUnlock()
	return hd.isRunning
}

// backgroundDiscoveryLoop runs the continuous discovery process
func (hd *NodeDiscovery) backgroundDiscoveryLoop() {
	defer close(hd.backgroundDone)

	ticker := time.NewTicker(constants.HealthCheckInterval) // Check for new hosts every 30 seconds
	defer ticker.Stop()

	for {
		select {
		case <-hd.backgroundCtx.Done():
			return
		default:
			// Perform one discovery cycle
			if err := hd.performDiscoveryCycle(hd.backgroundCtx); err != nil {
				utils.LogDebugf("Background discovery cycle failed: %v", err)
			}

			// Clean up stale entries
			hd.Cleanup(constants.PeriodicScanInterval) // Remove hosts not seen for 5 minutes

			// Wait for next cycle or cancellation
			select {
			case <-hd.backgroundCtx.Done():
				return
			case <-ticker.C:
				// Continue to next discovery cycle
			}
		}
	}
}

// performDiscoveryCycle performs a single discovery operation
func (hd *NodeDiscovery) performDiscoveryCycle(ctx context.Context) error {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return fmt.Errorf("failed to create resolver: %w", err)
	}

	entries := make(chan *zeroconf.ServiceEntry, ChannelBufferSize)

	// Start processing entries in background. zeroconf.Resolver owns the
	// channel close — grandcat/zeroconf closes `entries` when Browse
	// completes or its ctx cancels (LookupParams.done → close(Entries)).
	// We must NOT close it ourselves; doing so would panic with
	// "close of closed channel" on the goroutine side, which
	// RecoverAndLog would then silently swallow.
	go func() {
		defer utils.RecoverAndLog("discovery.processServiceEntries")
		for entry := range entries {
			select {
			case <-ctx.Done():
				return
			default:
				hd.processServiceEntry(entry)
			}
		}
	}()

	// Browse with a shorter timeout for background discovery
	discoveryCtx, cancel := context.WithTimeout(ctx, constants.DiscoveryTimeout)
	defer cancel()

	err = resolver.Browse(discoveryCtx, ServiceType, ServiceDomain, entries)
	if err != nil {
		return fmt.Errorf("failed to browse services: %w", err)
	}

	return nil
}
