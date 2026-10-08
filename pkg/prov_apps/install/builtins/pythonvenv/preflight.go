package pythonvenv

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/security"
)

// resolvePackages creates only a disposable managed venv; it never changes host Python.
func resolvePackages(ctx context.Context, directory, interpreter string, environment []string, flags []string, roots map[string]string) ([]install.ResolvedPackage, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := fsroot.VerifySymlinkSafe(directory); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(directory, ".resolve-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := runResolver(ctx, interpreter, environment, "-m", "venv", filepath.Join(scratch, "venv")); err != nil {
		return nil, err
	}
	reportPath := filepath.Join(scratch, "report.json")
	args := append([]string{"-I", "-m", "pip"}, flags...)
	args = append(args, "--dry-run", "--ignore-installed", "--report", reportPath)
	if err := runResolver(ctx, venvPython(filepath.Join(scratch, "venv")), environment, args...); err != nil {
		return nil, err
	}
	file, err := os.Open(reportPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 8<<20 {
		return nil, fmt.Errorf("dependency report exceeds bound")
	}
	data, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
		return nil, fmt.Errorf("dependency report exceeds bound")
	}
	return install.ParsePipReport(data, roots)
}

func runResolver(ctx context.Context, python string, environment []string, args ...string) error {
	command := host.CommandContext(ctx, python, args...)
	command.Env = environment
	command.WaitDelay = 2 * time.Second
	tail := &boundedResolverOutput{}
	command.Stdout = tail
	command.Stderr = tail
	if err := process.RunOwnedCommand(ctx, command); err != nil {
		return fmt.Errorf("dependency preflight: %w: %s", err, security.RedactSensitive(string(tail.data)))
	}
	return nil
}

type boundedResolverOutput struct{ data []byte }

func (b *boundedResolverOutput) Write(data []byte) (int, error) {
	n := len(data)
	b.data = append(b.data, data...)
	if len(b.data) > 64<<10 {
		b.data = b.data[len(b.data)-(64<<10):]
	}
	return n, nil
}
