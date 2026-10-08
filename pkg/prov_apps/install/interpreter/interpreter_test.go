package interpreter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
)

// fakeInterp describes a fake interpreter the test harness pretends is
// installed on PATH: LookPath maps name → abs, RunVersion maps abs → version.
type fakeInterp struct {
	name, abs, version string
}

// build a pair of injected functions that simulate a given candidate table.
func seams(table []fakeInterp) Options {
	byName := map[string]string{}
	byAbs := map[string]string{}
	for _, f := range table {
		byName[f.name] = f.abs
		byAbs[f.abs] = f.version
	}
	return Options{
		LookPath: func(name string) (string, error) {
			if p, ok := byName[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		RunVersion: func(_ context.Context, path string) (string, error) {
			if v, ok := byAbs[path]; ok {
				return "Python " + v, nil
			}
			return "", errors.New("unexpected interpreter")
		},
		EvalSymlinks: func(p string) (string, error) { return p, nil },
	}
}

func TestSelect_PrefersHighestMatchingMinor(t *testing.T) {
	opts := seams([]fakeInterp{
		{"python3.11", "/u/bin/python3.11", "3.11.9"},
		{"python3.13", "/u/bin/python3.13", "3.13.4"},
		{"python3", "/u/bin/python3", "3.14.2"}, // out of range, should be skipped
	})
	opts.Candidates = []string{"python3.13", "python3.12", "python3.11", "python3"}

	got, err := Select(context.Background(), &config.PythonRequirement{Min: "3.9", Max: "3.14"}, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Path != "/u/bin/python3.13" {
		t.Fatalf("want /u/bin/python3.13, got %q", got.Path)
	}
	if got.Version != "3.13.4" {
		t.Fatalf("want 3.13.4, got %q", got.Version)
	}
}

func TestSelect_RejectsAboveMax(t *testing.T) {
	// This is the incident condition: every python on PATH is 3.14 (the brew
	// default shifted). Select must refuse, not silently build a broken venv.
	opts := seams([]fakeInterp{
		{"python3", "/opt/python@3.14/bin/python3", "3.14.4"},
		{"python3.14", "/opt/python@3.14/bin/python3.14", "3.14.4"},
	})
	opts.Candidates = []string{"python3.13", "python3", "python3.14"}

	_, err := Select(context.Background(), &config.PythonRequirement{Min: "3.9", Max: "3.14"}, opts)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "<3.14") {
		t.Fatalf("error should name the range; got %q", err.Error())
	}
}

func TestSelect_BelowMinRejected(t *testing.T) {
	opts := seams([]fakeInterp{
		{"python3.8", "/u/bin/python3.8", "3.8.10"},
	})
	opts.Candidates = []string{"python3.13", "python3.8"}

	_, err := Select(context.Background(), &config.PythonRequirement{Min: "3.9"}, opts)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

func TestSelect_NilReqAcceptsAnyPython3(t *testing.T) {
	opts := seams([]fakeInterp{
		{"python3", "/u/bin/python3", "3.12.1"},
	})
	opts.Candidates = []string{"python3"}

	got, err := Select(context.Background(), nil, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Version != "3.12.1" {
		t.Fatalf("want 3.12.1, got %q", got.Version)
	}
}

func TestSelect_DedupesAliasesPointingAtSameBinary(t *testing.T) {
	// `python3` and `python3.13` symlinking to the same underlying binary
	// — realistic on distro layouts — must be considered once, not twice.
	opts := Options{
		LookPath: func(name string) (string, error) {
			switch name {
			case "python3", "python3.13":
				return "/u/bin/python3.13", nil
			}
			return "", errors.New("not found")
		},
		RunVersion: func(_ context.Context, _ string) (string, error) {
			return "Python 3.13.4", nil
		},
		EvalSymlinks: func(_ string) (string, error) { return "/u/bin/python3.13", nil },
		Candidates:   []string{"python3.13", "python3"},
	}
	got, err := Select(context.Background(), &config.PythonRequirement{Min: "3.9", Max: "3.14"}, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Path != "/u/bin/python3.13" {
		t.Fatalf("want /u/bin/python3.13 (first matching candidate), got %q", got.Path)
	}
}

func TestInstallHint_SuggestsMaxMinusOne(t *testing.T) {
	got := InstallHint(&config.PythonRequirement{Max: "3.14"})
	if !strings.Contains(got, "3.13") {
		t.Fatalf("hint should suggest 3.13 when max=3.14; got %q", got)
	}
}
