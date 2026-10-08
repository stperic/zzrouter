package fsroot

import (
	"fmt"
	"sync/atomic"
)

// Process-scoped install knobs, populated at server boot from node.yaml
// `providers.*`. Each has an atomic setter + a local accessor used by the
// install-plan builders and the step executor. Zero values leave each
// subsystem at its built-in default (harden_venv = true, no proxy env).

// hardenVenvEnabled gates the `chmod -R a-w` step in PythonVenvInstaller.
// Default true; set false on mixed-ownership hosts where the chmod would
// fail legitimately (shared pip cache, service-user installs).
var hardenVenvEnabled atomic.Bool

func init() { hardenVenvEnabled.Store(true) }

// SetHardenVenvDefault configures whether new PythonVenv install plans
// include the read-only hardening step. Called at server boot.
func SetHardenVenvDefault(enabled bool) { hardenVenvEnabled.Store(enabled) }

// IsHardenVenvEnabled reports the current hardening policy.
func IsHardenVenvEnabled() bool { return hardenVenvEnabled.Load() }

// installExecEnv holds pre-formatted "K=V" strings merged into every install
// step's subprocess environment (in addition to the parent env). Used for
// corporate proxy + CA bundle settings so curl/pip/Invoke-WebRequest all
// pick them up uniformly — we never have to teach every plan-builder about
// proxy plumbing.
var installExecEnv atomic.Pointer[[]string]

// SetExecEnv installs process-scoped environment variables that the step
// executor merges into every install subprocess. Passing an empty map clears
// the override. Callers typically build the map from node.yaml providers.proxy.
func SetExecEnv(env map[string]string) {
	if len(env) == 0 {
		installExecEnv.Store(nil)
		return
	}
	formatted := make([]string, 0, len(env))
	for k, v := range env {
		formatted = append(formatted, fmt.Sprintf("%s=%s", k, v))
	}
	installExecEnv.Store(&formatted)
}

// ExecEnv returns the currently-configured install env, or nil if unset.
// The returned slice must not be mutated by callers.
func ExecEnv() []string {
	p := installExecEnv.Load()
	if p == nil {
		return nil
	}
	return *p
}

// BuildProxyEnv translates a proxy config (http/https/no_proxy + CA bundle)
// into the canonical env-var set consumed by install subprocesses:
//
//   - HTTP_PROXY / http_proxy           (curl, wget, pip, most Go stdlib)
//   - HTTPS_PROXY / https_proxy
//   - NO_PROXY / no_proxy
//   - SSL_CERT_FILE                     (OpenSSL / libssl, requests via certifi)
//   - CURL_CA_BUNDLE                    (curl)
//   - PIP_CERT                          (pip, bypasses pip's own certifi)
//
// Both upper- and lower-case forms are set because tools disagree on which
// they honor. An empty caFile leaves SSL_CERT_FILE/CURL_CA_BUNDLE/PIP_CERT
// unset, letting each subprocess fall back to the system trust store.
func BuildProxyEnv(httpProxy, httpsProxy, noProxy, caFile string) map[string]string {
	env := map[string]string{}
	if httpProxy != "" {
		env["HTTP_PROXY"] = httpProxy
		env["http_proxy"] = httpProxy
	}
	if httpsProxy != "" {
		env["HTTPS_PROXY"] = httpsProxy
		env["https_proxy"] = httpsProxy
	}
	if noProxy != "" {
		env["NO_PROXY"] = noProxy
		env["no_proxy"] = noProxy
	}
	if caFile != "" {
		env["SSL_CERT_FILE"] = caFile
		env["CURL_CA_BUNDLE"] = caFile
		env["PIP_CERT"] = caFile
	}
	return env
}
