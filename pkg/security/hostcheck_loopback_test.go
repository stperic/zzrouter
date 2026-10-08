package security

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.5.4.3", "::1"} {
		assert.True(t, IsLoopbackHost(host), host)
	}
	// A name is not an address: "localhost" resolves through /etc/hosts,
	// which an attacker with write access there can point off-box.
	for _, host := range []string{"localhost", "LOCALHOST", "github.com", "192.0.2.10", "localhost.attacker.test", ""} {
		assert.False(t, IsLoopbackHost(host), host)
	}
}

func TestValidateLoopbackDownloadURL(t *testing.T) {
	allowed := []string{"127.0.0.1:8099", "github.com"}

	// Plaintext is accepted for loopback, and only for loopback.
	require.NoError(t, ValidateLoopbackDownloadURL("http://127.0.0.1:8099/a.tar.gz", allowed))
	require.NoError(t, ValidateLoopbackDownloadURL("https://github.com/a.tar.gz", allowed))

	assert.Error(t, ValidateLoopbackDownloadURL("http://github.com/a.tar.gz", allowed),
		"lifting HTTPS for loopback must not lift it for anything else")
	assert.Error(t, ValidateLoopbackDownloadURL("http://192.0.2.10:8099/a.tar.gz", allowed))

	// The allowlist still applies to loopback.
	assert.Error(t, ValidateLoopbackDownloadURL("http://127.0.0.1:9999/a.tar.gz", allowed))

	// The strict entry point is unaffected.
	assert.Error(t, ValidateDownloadURL("http://127.0.0.1:8099/a.tar.gz", allowed))
}
