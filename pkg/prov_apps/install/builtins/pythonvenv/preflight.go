package pythonvenv

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if err := runPython(ctx, interpreter, "", environment, "-m", "venv", filepath.Join(scratch, "venv")); err != nil {
		return nil, err
	}
	reportPath := filepath.Join(scratch, "report.json")
	args := append([]string{"-I", "-m", "pip"}, flags...)
	args = append(args, "--dry-run", "--ignore-installed", "--report", reportPath)
	if err := runPython(ctx, venvPython(filepath.Join(scratch, "venv")), "", environment, args...); err != nil {
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
	python := venvPython(filepath.Join(scratch, "venv"))
	return install.ResolvePipReport(ctx, data, roots, func(ctx context.Context, artifactURL string) (string, error) {
		return resolveWheelHash(ctx, scratch, python, environment, artifactURL)
	})
}

// resolverWheelLimit bounds accepted hashless wheels, including multi-GiB CUDA wheels.
// Pip's download is bounded by the resolver deadline, not this post-download limit.
const resolverWheelLimit int64 = 8 << 30

func resolveWheelHash(ctx context.Context, scratch, python string, environment []string, artifactURL string) (string, error) {
	directory, err := os.MkdirTemp(scratch, "wheel-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	if err := runPython(ctx, python, "", environment, "-I", "-m", "pip", "--disable-pip-version-check", "download", "--no-input", "--no-index", "--no-deps", "--only-binary=:all:", "--no-cache-dir", "--dest", directory, artifactURL); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || !entries[0].Type().IsRegular() || !strings.HasSuffix(entries[0].Name(), ".whl") {
		return "", fmt.Errorf("resolver must download exactly one regular wheel")
	}
	file, err := os.Open(filepath.Join(directory, entries[0].Name()))
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > resolverWheelLimit {
		return "", fmt.Errorf("resolved wheel exceeds size bound or is empty")
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := file.Read(buffer)
		size += int64(n)
		if size > resolverWheelLimit {
			return "", fmt.Errorf("resolved wheel exceeds size bound")
		}
		_, _ = hash.Write(buffer[:n])
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func runPython(ctx context.Context, python, directory string, environment []string, args ...string) error {
	command := host.CommandContext(ctx, python, args...)
	command.Dir = directory
	command.Env = environment
	command.WaitDelay = 2 * time.Second
	tail := &boundedResolverOutput{}
	command.Stdout = tail
	command.Stderr = tail
	if err := process.RunOwnedCommand(ctx, command); err != nil {
		// Python on Windows writes CRLF; errors read the same on every platform.
		output := security.RedactSensitive(strings.ReplaceAll(string(tail.data), "\r\n", "\n"))
		var diagnostics []string
		for _, line := range strings.Split(output, "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "ERROR:") {
				diagnostics = append(diagnostics, line)
			}
		}
		if len(diagnostics) > 0 {
			output = strings.Join(diagnostics, "\n") + "\n\n" + output
		}
		return fmt.Errorf("%s\nmanaged Python command: %w", output, err)
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
