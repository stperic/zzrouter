package pythonvenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stretchr/testify/require"
)

func TestResolveWheelHashAcquisitionAndCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("synthetic Python executable uses a POSIX shebang; native acquisition is exercised through the install API")
	}
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	for _, mode := range []string{"success", "multiple", "source", "symlink", "empty", "oversize", "failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			scratch := t.TempDir()
			helper := filepath.Join(t.TempDir(), "resolver python")
			// This synthetic executable tests pip argv/output handling without network or installations.
			script := `
import os, pathlib, sys
args = sys.argv[1:]
expected = ["-I", "-m", "pip", "--disable-pip-version-check", "download", "--no-input", "--no-index", "--no-deps", "--only-binary=:all:", "--no-cache-dir", "--dest"]
assert args[:len(expected)] == expected, args
assert len(args) == len(expected) + 2, args
assert args[-1] == "https://download.pytorch.org/whl/jinja2-3.1.6-py3-none-any.whl", args
dest = pathlib.Path(args[-2])
mode = os.environ["ZZROUTER_WHEEL_TEST_MODE"]
wheel = dest / "jinja2-3.1.6-py3-none-any.whl"
wheel.write_bytes(b"wheel bytes")
if mode == "multiple": (dest / "extra.whl").write_bytes(b"extra")
if mode == "source": wheel.rename(dest / "source.tar.gz")
if mode == "symlink":
    wheel.unlink()
    wheel.symlink_to(__file__)
if mode == "empty": wheel.write_bytes(b"")
if mode == "oversize":
    with wheel.open("wb") as stream: stream.truncate((8 << 30) + 1)
if mode == "failure": sys.exit(3)
`
			require.NoError(t, os.WriteFile(helper, []byte("#!"+python+"\n"+script), 0700))
			t.Setenv("ZZROUTER_WHEEL_TEST_MODE", mode)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			hash, err := resolveWheelHash(ctx, scratch, helper, os.Environ(), "https://download.pytorch.org/whl/jinja2-3.1.6-py3-none-any.whl")
			if mode == "success" {
				require.NoError(t, err)
				require.Equal(t, "67c0d8f7de19e30c2d5891030a0b37cbfcdd240852b53055c0b28290ad52290b", hash)
			} else {
				require.Error(t, err)
				require.Empty(t, hash)
				if mode == "cancelled" {
					require.ErrorIs(t, err, context.Canceled)
				}
			}
			entries, err := os.ReadDir(scratch)
			require.NoError(t, err)
			require.Empty(t, entries, "acquisition scratch must be removed after success and failure")
		})
	}
}

func TestResolvePackagesHashlessReportAndInFlightCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("synthetic Python executable uses a POSIX shebang; native resolution is exercised through the install API")
	}
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	fixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "pip-report-pep658.json"))
	require.NoError(t, err)
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "cancelled acquisition"}[cancelled], func(t *testing.T) {
			directory := t.TempDir()
			helper := filepath.Join(t.TempDir(), "python-fixture")
			ready := filepath.Join(t.TempDir(), "download-started")
			// Synthetic executable consumes the trimmed real report and returns synthetic wheel bytes.
			script := `
import os, pathlib, shutil, sys, time
args = sys.argv[1:]
if args[:2] == ["-m", "venv"]:
    target = pathlib.Path(args[2]) / "bin" / "python"
    target.parent.mkdir(parents=True)
    shutil.copyfile(__file__, target)
    target.chmod(0o700)
elif "--dry-run" in args:
    shutil.copyfile(os.environ["ZZROUTER_REPORT_FIXTURE"], args[args.index("--report") + 1])
else:
    assert "--no-index" in args and "--no-deps" in args and "--only-binary=:all:" in args
    assert args[-1] == "https://download.pytorch.org/whl/jinja2-3.1.6-py3-none-any.whl"
    dest = pathlib.Path(args[args.index("--dest") + 1])
    (dest / "jinja2-3.1.6-py3-none-any.whl").write_bytes(b"wheel bytes")
    pathlib.Path(os.environ["ZZROUTER_DOWNLOAD_READY"]).write_text("started")
    if os.environ["ZZROUTER_CANCEL_DOWNLOAD"] == "true": time.sleep(60)
`
			require.NoError(t, os.WriteFile(helper, []byte("#!"+python+"\n"+script), 0700))
			environment := append(os.Environ(), "ZZROUTER_REPORT_FIXTURE="+fixture, "ZZROUTER_DOWNLOAD_READY="+ready, "ZZROUTER_CANCEL_DOWNLOAD="+map[bool]string{false: "false", true: "true"}[cancelled])
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			type result struct {
				inventory []install.ResolvedPackage
				err       error
			}
			done := make(chan result, 1)
			go func() {
				inventory, err := resolvePackages(ctx, directory, helper, environment, []string{"install", "jinja2"}, map[string]string{"jinja2": ""})
				done <- result{inventory, err}
			}()
			if cancelled {
				require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second, 10*time.Millisecond)
				cancel()
			}
			select {
			case outcome := <-done:
				if cancelled {
					require.ErrorIs(t, outcome.err, context.Canceled)
					require.Nil(t, outcome.inventory)
				} else {
					require.NoError(t, outcome.err)
					require.Equal(t, "jinja2==3.1.6 --hash=sha256:67c0d8f7de19e30c2d5891030a0b37cbfcdd240852b53055c0b28290ad52290b\n", install.InventoryRequirements(outcome.inventory))
				}
			case <-time.After(20 * time.Second):
				t.Fatal("resolver did not return after its deadline")
			}
			entries, err := os.ReadDir(directory)
			require.NoError(t, err)
			require.Empty(t, entries, "resolver venv, report and partial wheel must be removed")
		})
	}
}
