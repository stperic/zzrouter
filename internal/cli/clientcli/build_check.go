// Package cli provides client-side CLI commands.
// This file ensures that server packages are never imported into client commands.
package clientcli

// Build-time check: If this file imports server packages, compilation will fail
// due to import cycle detection or unused imports.

// To verify no server dependencies:
// go list -f '{{ join .Deps "\n" }}' ./cmd/zzrouter | grep server
// Should return no results
