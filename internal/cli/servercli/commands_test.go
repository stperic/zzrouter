package servercli

import (
	"os"
	"testing"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/service"
	"github.com/stretchr/testify/require"
)

type startServiceManager struct {
	service.ServiceManager
	dir        string
	firstRun   bool
	startCalls int
}

func (m *startServiceManager) GetConfigDir() string { return m.dir }
func (m *startServiceManager) IsFirstRun() bool     { return m.firstRun }
func (m *startServiceManager) GetStatus() (*service.Status, error) {
	return &service.Status{Running: m.startCalls > 0}, nil
}
func (m *startServiceManager) StartService() error { m.startCalls++; return nil }

func TestPrivilegedStartRejectsUnsupportedNewConfigBeforeMutation(t *testing.T) {
	for _, source := range []string{"flag", "environment"} {
		t.Run(source, func(t *testing.T) {
			manager := &startServiceManager{dir: t.TempDir(), firstRun: true}
			dir := t.TempDir()
			t.Setenv("ZZROUTER_CONFIG_DIR", "")
			if source == "environment" {
				t.Setenv("ZZROUTER_CONFIG_DIR", dir)
				dir = ""
			}
			err := runPrivilegedStart(manager, dir)
			require.ErrorContains(t, err, "--no-setup")
			require.Zero(t, manager.startCalls)
		})
	}
}

func TestPrivilegedStartPreservesExistingServiceDefinition(t *testing.T) {
	manager := &startServiceManager{dir: t.TempDir()}
	t.Setenv("ZZROUTER_CONFIG_DIR", t.TempDir())
	// A selection cannot rewrite an installed service; every mutation except Start hits the nil interface.
	require.NoError(t, runPrivilegedStart(manager, t.TempDir()))
	require.Equal(t, 1, manager.startCalls)
}

func TestNewStartCmd_Node(t *testing.T) {
	cmd := NewStartCmd()

	if cmd == nil {
		t.Fatal("NewStartCmd() returned nil")
	}

	if cmd.Use != "start" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "start")
	}

	// Verify expected flags
	flags := cmd.Flags()

	portFlag := flags.Lookup("port")
	if portFlag == nil {
		t.Error("expected 'port' flag to exist")
	}

	debugFlag := flags.Lookup("debug")
	if debugFlag == nil {
		t.Error("expected 'debug' flag to exist")
	}

}

func TestNewStopCmd(t *testing.T) {
	cmd := NewStopCmd()

	if cmd == nil {
		t.Fatal("NewStopCmd() returned nil")
	}

	if cmd.Use != "stop" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "stop")
	}

	if cmd.RunE == nil {
		t.Error("cmd.RunE should not be nil")
	}
}

func TestNewStatusCmd(t *testing.T) {
	cmd := NewStatusCmd()

	if cmd == nil {
		t.Fatal("NewStatusCmd() returned nil")
	}

	if cmd.Use != "status" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "status")
	}

	if cmd.RunE == nil {
		t.Error("cmd.RunE should not be nil")
	}
}

func TestNewVersionCmd(t *testing.T) {
	cmd := NewVersionCmd()

	if cmd == nil {
		t.Fatal("NewVersionCmd() returned nil")
	}

	// Version command should exist
	if cmd.Short == "" {
		t.Error("cmd.Short should not be empty")
	}
}

func TestNewConfigCmd(t *testing.T) {
	cmd := NewConfigCmd()

	if cmd == nil {
		t.Fatal("NewConfigCmd() returned nil")
	}

	if cmd.Use != "config" {
		t.Errorf("cmd.Use = %q, want %q", cmd.Use, "config")
	}

	// Config command should have subcommands
	subCommands := cmd.Commands()
	if len(subCommands) == 0 {
		t.Error("expected config command to have subcommands")
	}
}

func TestGetNodename(t *testing.T) {
	result := getNodename()

	// Should return a non-empty string
	if result == "" {
		t.Error("getNodename() should not return empty string")
	}

	// Should return either the system hostname or the default
	hostname, err := os.Hostname()
	if err != nil {
		// If os.Hostname fails, should return default
		if result != "zzrouter-host" {
			t.Errorf("getNodename() = %q, want %q when hostname unavailable", result, "zzrouter-host")
		}
	} else {
		if result != hostname {
			t.Errorf("getNodename() = %q, want %q", result, hostname)
		}
	}
}

func TestDefaultConstants(t *testing.T) {
	if insecureDevAdminKey != "dev-key-please-change-in-production" {
		t.Errorf("insecureDevAdminKey = %q, want 'dev-key-please-change-in-production'", insecureDevAdminKey)
	}
	if constants.DefaultZZROUTERPort != 9090 {
		t.Errorf("constants.DefaultZZROUTERPort = %d, want 9090", constants.DefaultZZROUTERPort)
	}
}
