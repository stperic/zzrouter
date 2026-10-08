//go:build windows

package pythonvenv

import (
	"fmt"
	"os"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stretchr/testify/require"
)

func TestAcceptedIndexSurvivesWindowsCommandParsing(t *testing.T) {
	for _, value := range []string{"https://pypi.org/simple", "https://wheels.example.org:443/a-b_c.d+e~f", "https://[2001:db8::1]/simple"} {
		t.Run(value, func(t *testing.T) {
			require.NoError(t, config.ValidateInstallIndex(value))
			t.Setenv("ZZROUTER_TEST_INDEX_HELPER", "1")
			command := quoteCommand(os.Args[0], "-test.run=^TestWindowsIndexArgumentHelper$", "--", value)
			plan := install.Plan{Steps: []install.Step{{Number: 1, Command: command, Verify: install.StepVerify{Type: "command_output", Command: command, Expected: value}}}}
			result := plan.ExecuteStep(t.Context(), 1)
			require.True(t, result.Passed, result.Message)
			require.Equal(t, value, result.Actual)
		})
	}
}

func TestWindowsIndexArgumentHelper(t *testing.T) {
	if os.Getenv("ZZROUTER_TEST_INDEX_HELPER") != "1" {
		return
	}
	if len(os.Args) != 4 || os.Args[2] != "--" {
		os.Exit(1)
	}
	fmt.Println(os.Args[3])
	os.Exit(0)
}
