//go:build windows

package interpreter

import (
	"errors"
	"path/filepath"

	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"

	"golang.org/x/sys/windows/registry"

	"github.com/stretchr/testify/require"
)

func TestRegisteredInterpreterPathsAreBoundedAndAbsolute(t *testing.T) {
	path := `Software\zzRouterInterpreterTests\` + filepath.Base(filepath.Dir(t.TempDir()))
	root, existed, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS)
	require.NoError(t, err)
	require.False(t, existed)
	t.Cleanup(func() {
		_ = root.Close()
		_ = registry.DeleteKey(registry.CURRENT_USER, path)
	})
	t.Cleanup(func() { _ = registry.DeleteKey(root, "PythonCore") })
	want := `C:\Program Files\Python313\python.exe`
	for name, values := range map[string]map[string]string{
		"3.13": {"ExecutablePath": want},
		"3.12": {"": `C:\Python312`},
		"3.11": {"ExecutablePath": `python.exe`},
		"3.10": {"ExecutablePath": `"C:\Python310\python.exe" -I`},
	} {
		tagPath := `PythonCore\` + name
		key, _, err := registry.CreateKey(root, tagPath+`\InstallPath`, registry.ALL_ACCESS)
		require.NoError(t, err)
		for keyName, value := range values {
			require.NoError(t, key.SetStringValue(keyName, value))
		}
		require.NoError(t, key.Close())
		t.Cleanup(func() {
			_ = registry.DeleteKey(root, tagPath+`\InstallPath`)
			_ = registry.DeleteKey(root, tagPath)
		})
	}
	require.ElementsMatch(t, []string{want, `C:\Python312\python.exe`}, registeredCandidates(root))
	key, err := registry.OpenKey(root, `PythonCore\3.13\InstallPath`, registry.ALL_ACCESS)
	require.NoError(t, err)
	defer key.Close()
	require.NoError(t, key.SetStringValue("oversized", string(make([]rune, 32769))))
	_, err = registryString(key, "oversized")
	require.Error(t, err)
}

func TestWindowsInterpreterProbeRejectsAnotherArchitecture(t *testing.T) {
	choice, err := Select(t.Context(), &config.PythonRequirement{Min: "3.10", Max: "3.15"}, Options{})
	if errors.Is(err, ErrNoMatch) {
		t.Skip("native Python fixture unavailable")
	}
	require.NoError(t, err)
	output, err := runVersionDefault(t.Context(), choice.Path)
	require.NoError(t, err)
	require.Contains(t, output, choice.Version)
	args := versionProbeArgs()
	wrong := "win32"
	if args[len(args)-1] == wrong {
		wrong = "win-amd64"
	}
	args[len(args)-1] = wrong
	outputBytes, err := host.CommandContext(t.Context(), choice.Path, args...).CombinedOutput()
	require.Error(t, err, string(outputBytes))
	require.Empty(t, outputBytes, "a mismatched interpreter must not become a version candidate")
}
