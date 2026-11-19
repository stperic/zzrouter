//go:build darwin

package interpreter

import (
	"context"
	"errors"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

func TestSelectFindsInstalledPythonWithoutShellPath(t *testing.T) {
	for _, path := range []string{
		"/opt/homebrew/opt/python@3.13/bin/python3.13",
		"/usr/local/opt/python@3.13/bin/python3.13",
		"/Library/Frameworks/Python.framework/Versions/3.13/bin/python3.13",
	} {
		t.Run(path, func(t *testing.T) {
			opts := seams([]fakeInterp{
				{"python3", "/usr/bin/python3", "3.9.6"},
				{path, path, "3.13.17"},
			})
			choice, err := Select(t.Context(), &config.PythonRequirement{Min: "3.9", Max: "3.14"}, opts)
			require.NoError(t, err)
			require.Equal(t, path, choice.Path)
			require.Equal(t, "3.13.17", choice.Version)
		})
	}
}

func TestSelectInstalledPythonStillHonorsRuntimeRange(t *testing.T) {
	path := "/opt/homebrew/opt/python@3.13/bin/python3.13"
	opts := seams([]fakeInterp{
		{"python3", "/usr/bin/python3", "3.9.6"},
		{path, path, "3.13.17"},
	})
	choice, err := Select(t.Context(), &config.PythonRequirement{Min: "3.9", Max: "3.13"}, opts)
	require.NoError(t, err)
	require.Equal(t, "/usr/bin/python3", choice.Path)
}

func TestDarwinInterpreterProbeRejectsAnotherArchitecture(t *testing.T) {
	choice, err := Select(t.Context(), nil, Options{})
	if errors.Is(err, ErrNoMatch) {
		t.Skip("native Python fixture unavailable")
	}
	require.NoError(t, err)
	output, err := runVersionDefault(t.Context(), choice.Path)
	require.NoError(t, err)
	require.Contains(t, output, choice.Version)
	args := versionProbeArgs()
	wrong := "x86_64"
	if args[len(args)-1] == wrong {
		wrong = "arm64"
	}
	args[len(args)-1] = wrong
	outputBytes, err := host.CommandContext(t.Context(), choice.Path, args...).CombinedOutput()
	require.Error(t, err, string(outputBytes))
	require.Empty(t, outputBytes, "an incompatible interpreter must not become a version candidate")

	newer := "/fixture/incompatible-python"
	opts := seams([]fakeInterp{{choice.Path, choice.Path, choice.Version}, {newer, newer, "3.99.1"}})
	opts.Candidates = []string{newer, choice.Path}
	runVersion := opts.RunVersion
	opts.RunVersion = func(ctx context.Context, path string) (string, error) {
		if path == newer {
			return "", errors.New("incompatible interpreter architecture")
		}
		return runVersion(ctx, path)
	}
	selected, err := Select(t.Context(), nil, opts)
	require.NoError(t, err)
	require.Equal(t, choice.Path, selected.Path)
}
