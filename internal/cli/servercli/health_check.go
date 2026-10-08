package servercli

import (
	"fmt"
	"net/http"
	"time"
)

// HealthCheckResult represents the result of a health check
type HealthCheckResult struct {
	Reachable bool
	Address   string // The address that succeeded (if any)
}

// CheckHealth performs a health check on the given URL
// Returns true if the endpoint responds with HTTP 200
func CheckHealth(client *http.Client, url string) bool {
	resp, err := client.Get(url)
	if err == nil && resp.StatusCode == 200 {
		_ = resp.Body.Close()
		return true
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	return false
}

// CheckLocalNode checks if a server is running on the specified port
// Tries localhost first, then 127.0.0.1 as fallback
func CheckLocalNode(port int, endpoint string, timeout time.Duration) HealthCheckResult {
	client := &http.Client{Timeout: timeout}

	// Try localhost first (most common)
	localhostURL := fmt.Sprintf("http://localhost:%d%s", port, endpoint)
	if CheckHealth(client, localhostURL) {
		return HealthCheckResult{Reachable: true, Address: "localhost"}
	}

	// Try 127.0.0.1 as fallback
	loopbackURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, endpoint)
	if CheckHealth(client, loopbackURL) {
		return HealthCheckResult{Reachable: true, Address: "127.0.0.1"}
	}

	return HealthCheckResult{Reachable: false}
}

// CheckRemoteNode checks if a remote host is reachable
func CheckRemoteNode(address string, endpoint string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	url := fmt.Sprintf("http://%s%s", address, endpoint)
	return CheckHealth(client, url)
}
