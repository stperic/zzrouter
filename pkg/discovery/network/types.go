// Package discovery provides mDNS-based host discovery for zzRouter
package network

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Constants for hostname validation
const (
	MaxNodenameLength = 253
)

// NodeEntry represents a discovered host
type NodeEntry struct {
	Name          string
	Node          string
	Port          int // Public admin/inference port (for `zzrouter connect`).
	ClusterPort   int // mTLS cluster port where pairing + cluster comms live; 0 when the advertiser didn't include it.
	IsCoordinator bool
	Version       string
	LastSeen      time.Time
}

// PairingURL returns the URL a worker should POST its pairing request
// to, or empty string when the entry lacks the fields needed to build
// one (not a coordinator, or no ClusterPort).
func (he *NodeEntry) PairingURL() string {
	if !he.IsCoordinator || he.ClusterPort <= 0 {
		return ""
	}
	return fmt.Sprintf("https://%s:%d", he.Node, he.ClusterPort)
}

// Validate checks if the host entry has valid data
func (he *NodeEntry) Validate() error {
	if he.Name == "" {
		return errors.New("host name cannot be empty")
	}
	if he.Node == "" {
		return errors.New("host address cannot be empty")
	}
	// Note: IP validation is redundant since AddrIPv4[0] is guaranteed to be valid
	if he.Port < MinPort || he.Port > MaxPort {
		return fmt.Errorf("invalid port number: %d", he.Port)
	}
	return nil
}

// IsValidNodename validates hostname format
func IsValidNodename(hostname string) bool {
	if hostname == "" || len(hostname) > MaxNodenameLength {
		return false
	}
	// Simple regex for hostname validation (allows alphanumeric, hyphens, dots)
	validNodename := regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?)*$`)
	return validNodename.MatchString(hostname)
}

// IsExpired checks if the host entry is older than the specified duration
func (he *NodeEntry) IsExpired(maxAge time.Duration) bool {
	return time.Since(he.LastSeen) > maxAge
}

// String returns a human-readable representation of the host entry
func (he *NodeEntry) String() string {
	roleStr := ""
	if he.IsCoordinator {
		roleStr = " (coordinator)"
	}
	return fmt.Sprintf("%s%s at %s:%d (v%s)", he.Name, roleStr, he.Node, he.Port, he.Version)
}
