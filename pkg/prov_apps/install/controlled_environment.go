package install

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// ControlledEnvironment snapshots install inputs and disables every pip config source.
func ControlledEnvironment() ([]string, error) {
	inherited, err := process.ChildEnvironment(nil)
	if err != nil {
		return nil, err
	}
	result := []string{}
	for _, entry := range inherited {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains([]string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "SystemRoot", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT"}, key) {
			result = append(result, entry)
		}
	}
	result = append(result, fsroot.ExecEnv()...)
	result = append(result, "PIP_CONFIG_FILE="+os.DevNull, "PYTHONNOUSERSITE=1")
	slices.Sort(result)
	return result, nil
}

// MergeExecutionEnvironment applies explicit runtime values to an immutable base.
func MergeExecutionEnvironment(base []string, overlay map[string]string) ([]string, error) {
	if err := ValidateRuntimeEnvironment(overlay); err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overlay {
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	slices.Sort(result)
	return result, nil
}

// ValidateRuntimeEnvironment preserves process safety and operator-owned install inputs.
func ValidateRuntimeEnvironment(environment map[string]string) error {
	for key := range environment {
		if process.IsDangerousEnvVar(key) {
			return fmt.Errorf("%w: %s", process.ErrDangerousEnvVar, key)
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "PIP_") || slices.Contains([]string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE"}, upper) {
			return fmt.Errorf("operator-owned install environment variable: %s", key)
		}
	}
	return nil
}
