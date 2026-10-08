// Package discovery provides mDNS-based host discovery for zzRouter
// Node filtering and retrieval functionality
package network

import (
	"sort"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

// GetNodesSorted returns hosts sorted by the specified criteria
func (hd *NodeDiscovery) GetNodesSorted(sortBy string) []*NodeEntry {
	hosts := hd.GetNodes()

	switch sortBy {
	case "name":
		sort.Slice(hosts, func(i, j int) bool {
			return hosts[i].Name < hosts[j].Name
		})
	case "node":
		sort.Slice(hosts, func(i, j int) bool {
			return hosts[i].Node < hosts[j].Node
		})
	case "port":
		sort.Slice(hosts, func(i, j int) bool {
			return hosts[i].Port < hosts[j].Port
		})
	case "lastseen", "age":
		sort.Slice(hosts, func(i, j int) bool {
			return hosts[i].LastSeen.After(hosts[j].LastSeen)
		})
	case string(config.ClusterModeCoordinator):
		// Sort coordinators first, then by name
		sort.Slice(hosts, func(i, j int) bool {
			if hosts[i].IsCoordinator != hosts[j].IsCoordinator {
				return hosts[i].IsCoordinator
			}
			return hosts[i].Name < hosts[j].Name
		})
	default:
		// Default sort by last seen (most recent first)
		sort.Slice(hosts, func(i, j int) bool {
			return hosts[i].LastSeen.After(hosts[j].LastSeen)
		})
	}

	return hosts
}

// FilterNodes filters hosts based on criteria and returns deep copies
func (hd *NodeDiscovery) FilterNodes(filter func(*NodeEntry) bool) []*NodeEntry {
	hd.mu.RLock()
	defer hd.mu.RUnlock()

	var filtered []*NodeEntry
	for _, entry := range hd.entries {
		if filter(entry) {
			// Deep copy the NodeEntry
			hostCopy := &NodeEntry{
				Name:          entry.Name,
				Node:          entry.Node,
				Port:          entry.Port,
				IsCoordinator: entry.IsCoordinator,
				Version:       entry.Version,
				LastSeen:      entry.LastSeen,
			}
			filtered = append(filtered, hostCopy)
		}
	}

	return filtered
}

// GetNodesByIP returns hosts with the specified IP address
func (hd *NodeDiscovery) GetNodesByIP(ip string) []*NodeEntry {
	return hd.FilterNodes(func(entry *NodeEntry) bool {
		return entry.Node == ip
	})
}

// GetNodesByPort returns hosts running on the specified port
func (hd *NodeDiscovery) GetNodesByPort(port int) []*NodeEntry {
	return hd.FilterNodes(func(entry *NodeEntry) bool {
		return entry.Port == port
	})
}

// GetNodesByName returns hosts with names containing the specified string
func (hd *NodeDiscovery) GetNodesByName(namePattern string) []*NodeEntry {
	return hd.FilterNodes(func(entry *NodeEntry) bool {
		return strings.Contains(strings.ToLower(entry.Name), strings.ToLower(namePattern))
	})
}

// GetRecentNodes returns hosts discovered within the specified duration
func (hd *NodeDiscovery) GetRecentNodes(maxAge time.Duration) []*NodeEntry {
	return hd.FilterNodes(func(entry *NodeEntry) bool {
		return !entry.IsExpired(maxAge)
	})
}

// GetNodeCount returns the total number of discovered hosts
func (hd *NodeDiscovery) GetNodeCount() int {
	hd.mu.RLock()
	defer hd.mu.RUnlock()
	return len(hd.entries)
}

// GetNodeCountByType returns counts of coordinator and worker hosts
func (hd *NodeDiscovery) GetNodeCountByType() (coordinators, workers int) {
	hd.mu.RLock()
	defer hd.mu.RUnlock()

	for _, entry := range hd.entries {
		if entry.IsCoordinator {
			coordinators++
		} else {
			workers++
		}
	}
	return coordinators, workers
}

// GetCoordinatorNodes returns only coordinator hosts
func (hd *NodeDiscovery) GetCoordinatorNodes() []*NodeEntry {
	return hd.FilterNodes(func(entry *NodeEntry) bool {
		return entry.IsCoordinator
	})
}

// Cleanup removes stale entries (older than specified duration)
func (hd *NodeDiscovery) Cleanup(maxAge time.Duration) {
	hd.mu.Lock()
	defer hd.mu.Unlock()

	removed := 0
	for name, entry := range hd.entries {
		if entry.IsExpired(maxAge) {
			delete(hd.entries, name)
			removed++
		}
	}

	if removed > 0 {
		utils.LogInfof("Removed %d stale host entries", removed)
	}
}

// Clear removes all discovered hosts
func (hd *NodeDiscovery) Clear() {
	hd.mu.Lock()
	defer hd.mu.Unlock()

	count := len(hd.entries)
	hd.entries = make(map[string]*NodeEntry)
	utils.LogInfof("Cleared all %d host entries", count)
}

// GetNode retrieves a deep copy of a specific host by name
func (hd *NodeDiscovery) GetNode(name string) (*NodeEntry, bool) {
	hd.mu.RLock()
	defer hd.mu.RUnlock()

	entry, exists := hd.entries[name]
	if !exists {
		return nil, false
	}

	// Return deep copy
	hostCopy := &NodeEntry{
		Name:          entry.Name,
		Node:          entry.Node,
		Port:          entry.Port,
		IsCoordinator: entry.IsCoordinator,
		Version:       entry.Version,
		LastSeen:      entry.LastSeen,
	}

	return hostCopy, true
}
