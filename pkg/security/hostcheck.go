package security

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateDownloadURL checks that a URL uses HTTPS and its host is in the allowed list.
// Host matching supports exact matches and subdomain matches
// (e.g., "api.github.com" matches "github.com").
func ValidateDownloadURL(downloadURL string, allowedHosts []string) error {
	return validateDownloadURL(downloadURL, allowedHosts, false)
}

// ValidateLoopbackDownloadURL is ValidateDownloadURL with the HTTPS
// requirement lifted for loopback hosts only. The allowlist still
// applies, and a non-loopback host is still held to HTTPS.
//
// It exists for rehearsing an update against a release feed running on
// the same machine: that hop never reaches a network, and demanding a
// trusted certificate for it would mean installing one into the
// operator's system trust store. Callers must gate it on explicit
// configuration rather than reaching for it by default.
func ValidateLoopbackDownloadURL(downloadURL string, allowedHosts []string) error {
	return validateDownloadURL(downloadURL, allowedHosts, true)
}

func validateDownloadURL(downloadURL string, allowedHosts []string, allowLoopbackHTTP bool) error {
	parsed, err := url.Parse(downloadURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if parsed.Scheme != "https" {
		if !allowLoopbackHTTP || parsed.Scheme != "http" || !IsLoopbackHost(parsed.Hostname()) {
			return fmt.Errorf("only HTTPS URLs are allowed, got: %s", parsed.Scheme)
		}
	}

	host := strings.ToLower(parsed.Host)
	for _, allowed := range allowedHosts {
		allowed = strings.ToLower(allowed)
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}

	return fmt.Errorf("download host not allowed: %s", parsed.Host)
}

// IsLoopbackHost reports whether host is an address that cannot leave
// this machine.
//
// Only literals count. "localhost" is deliberately excluded: it is a
// name, resolved through /etc/hosts and whatever else the resolver
// consults, and an entry pointing it off-box would turn a
// loopback-only exemption into a remote one.
func IsLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
