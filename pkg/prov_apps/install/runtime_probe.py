# Fixed runtime checks; the argument contains validated declarations only.
import contextlib
import ctypes
import importlib
import importlib.metadata
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading

LIMIT = 4096
LIBRARY_DIAGNOSTIC_PATH_LIMIT = 4  # Bound output paths while checking every mapped copy.


class CheckFailure(RuntimeError):
    def __init__(self, reason, actual):
        super().__init__(reason)
        self.actual = actual


def command_output(args):
    process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    output = bytearray()

    def drain():
        while True:
            chunk = process.stdout.read(4096)
            if not chunk:
                break
            output.extend(chunk[:max(0, LIMIT - len(output))])

    reader = threading.Thread(target=drain, daemon=True)
    reader.start()
    try:
        code = process.wait(timeout=30)
    except subprocess.TimeoutExpired:
        process.kill()
        code = -1
    finally:
        reader.join(timeout=2)
        process.stdout.close()
    return code, output.decode(errors="replace").strip()


def cuda_tuple(version):
    if version is None or version <= 0:
        return None
    return (version // 1000, version % 1000 // 10)


def cuda_companions():
    torch = importlib.import_module("torch")
    importlib.import_module("torchaudio")
    importlib.import_module("torchvision")
    expected = tuple(map(int, torch.version.cuda.split(".")[:2])) if torch.version.cuda else None
    audio = cuda_tuple(torch.ops._torchaudio.cuda_version())
    vision = cuda_tuple(torch.ops.torchvision._cuda_version())
    actual = f"torch={torch.version.cuda}, torchaudio={audio}, torchvision={vision}"
    if expected is None or audio != expected or vision != expected:
        raise CheckFailure("CUDA build mismatch: companion builds must match torch", actual)
    return actual


def check(name):
    if name == "pip_check":
        code, output = command_output([sys.executable, "-I", "-B", "-m", "pip", "check"])
        if code != 0:
            raise CheckFailure(f"pip check exited {code}", output)
        return output
    if name == "cuda_companions":
        return cuda_companions()
    if name == "cuda_available":
        torch = importlib.import_module("torch")
        if not torch.cuda.is_available():
            raise RuntimeError("CUDA GPU unavailable to this runtime")
        return f"CUDA {torch.version.cuda}; {torch.cuda.device_count()} visible device(s)"
    if name == "managed_toolkit":
        root = os.environ.get("CUDA_MANAGED_ROOT")
        if not root or os.environ.get("CUDA_HOME") != root or os.environ.get("CUDA_PATH") != root:
            raise RuntimeError("managed CUDA compiler selection missing; host toolkit fallback refused")
        managed = os.path.realpath(sys.prefix)
        expected = os.path.join(root, "bin", "nvcc")
        compiler = os.path.realpath(expected)
        if os.path.commonpath([managed, compiler, os.path.realpath(root)]) != managed:
            raise RuntimeError("CUDA compiler escaped managed runtime")
        for selector in ("CUDACXX", "FLASHINFER_NVCC"):
            if os.environ.get(selector) != expected:
                raise RuntimeError(selector + " compiler override escaped managed runtime")
        resolved = shutil.which("nvcc")
        if not resolved or os.path.realpath(resolved) != compiler:
            raise RuntimeError("PATH does not resolve the managed CUDA compiler")
        code, output = command_output([compiler, "--version"])
        match = re.search(r"release\s+(\d+\.\d+)", output)
        build = importlib.import_module("torch").version.cuda
        if code or not match or match.group(1) != build:
            raise CheckFailure("managed compiler and loaded Torch CUDA build must match", output)
        loaded_version, library = loaded_cuda_runtime()
        if os.path.commonpath([managed, os.path.realpath(library)]) != managed:
            raise RuntimeError("loaded CUDA runtime library escaped managed runtime: " + os.path.realpath(library))
        if loaded_version != build:
            raise CheckFailure("loaded CUDA runtime and Torch CUDA build must match", loaded_version)
        if not shutil.which("c++"):
            raise RuntimeError("human-owned C++ compiler required; the API cannot install host packages")
        return "Managed CUDA " + build + "; host toolkit fallback disabled"
    if name == "metal_available":
        mx = importlib.import_module("mlx.core")
        if not mx.metal.is_available():
            raise RuntimeError("Metal GPU unavailable to this runtime")
        return "Metal available"
    raise RuntimeError("unknown fixed check: " + name)


def configure_probe(scratch):
    # Imports may create caches. Keep them outside the managed environment.
    for key in ("XDG_CACHE_HOME", "HF_HOME", "TRITON_CACHE_DIR", "TORCHINDUCTOR_CACHE_DIR", "FLASHINFER_WORKSPACE_BASE"):
        os.environ[key] = scratch
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"


def run(profile, scratch):
    results = []
    configure_probe(scratch)
    with open(os.devnull, "w") as sink, contextlib.redirect_stdout(sink), contextlib.redirect_stderr(sink):
        for kind in profile["checks"]:
            targets = profile["imports"] if kind == "imports" else [None]
            for target in targets:
                name = "import:" + target if target else kind
                try:
                    actual = importlib.import_module(target).__name__ if target else check(kind)
                    results.append(dict(name=name, passed=True, actual=str(actual)[:LIMIT], reason=""))
                except BaseException as error:
                    results.append(dict(name=name, passed=False, actual=str(getattr(error, "actual", ""))[:LIMIT], reason=(type(error).__name__ + ": " + str(error))[:LIMIT]))
    return results


def toolkit():
    root = os.getenv("CUDA_HOME") or os.getenv("CUDA_PATH")
    path = shutil.which("nvcc")
    source = "PATH"
    result = dict(path=path or "", version="", source=source, reason="")
    if root and path and os.path.realpath(path) != os.path.realpath(os.path.join(root, "bin", "nvcc")):
        result["reason"] = "PATH compiler differs from CUDA_HOME/CUDA_PATH selection"
        return result
    if not path or not os.path.isfile(path):
        result["reason"] = "CUDA compiler not found"
        return result
    try:
        code, output = command_output([path, "--version"])
        match = re.search(r"release\s+(\d+\.\d+)", output)
        if code == 0 and match:
            result["version"] = match.group(1)
        else:
            result["reason"] = "CUDA compiler version unavailable"
    except Exception as error:
        result["reason"] = str(error)[:LIMIT]
    return result


def library_path_details(paths):
    details = ", ".join(paths[:LIBRARY_DIAGNOSTIC_PATH_LIMIT])
    if len(paths) > LIBRARY_DIAGNOSTIC_PATH_LIMIT:
        details += f" (+{len(paths) - LIBRARY_DIAGNOSTIC_PATH_LIMIT} more)"
    return details


def loaded_cuda_runtime():
    # Force lazy CUDA initialization before inspecting the mapped libraries.
    importlib.import_module("torch").cuda.init()
    with open("/proc/self/maps") as maps:
        data = maps.read((4 << 20) + 1)
    if len(data) > 4 << 20:
        raise RuntimeError("loaded library map exceeds observation limit")
    paths = sorted({os.path.realpath(line.split(maxsplit=5)[-1])
                    for line in data.splitlines() if "/libcudart.so" in line})
    if not paths or len(paths) > 32:
        raise RuntimeError(f"loaded CUDA runtime library observation has {len(paths)} candidates")
    managed = os.path.realpath(sys.prefix)
    escaped = [path for path in paths
               if os.environ.get("CUDA_MANAGED_ROOT") and os.path.commonpath([managed, path]) != managed]
    if escaped:
        raise RuntimeError("loaded CUDA runtime library escaped managed runtime: " + library_path_details(escaped))
    versions = set()
    path_versions = []
    for path in paths:
        library = ctypes.CDLL(path, mode=os.RTLD_LOCAL | os.RTLD_NOLOAD)
        version = ctypes.c_int()
        query = library.cudaRuntimeGetVersion
        query.argtypes = [ctypes.POINTER(ctypes.c_int)]
        query.restype = ctypes.c_int
        code = query(ctypes.byref(version))
        if code != 0:
            raise RuntimeError(f"cudaRuntimeGetVersion returned {code}")
        major, minor = cuda_tuple(version.value)
        versions.add(f"{major}.{minor}")
        path_versions.append(f"{path} (CUDA {major}.{minor})")
    if len(versions) != 1:
        raise RuntimeError("loaded CUDA runtime libraries have conflicting versions " +
                           ", ".join(sorted(versions)) + ": " + library_path_details(path_versions))
    # Every mapped copy was checked; expose a deterministic representative path.
    return versions.pop(), paths[0]


def environment(profile):
    result = dict(packages={}, cuda_runtime=dict(build_version="", loaded_version="", library_path="", reason=""),
                  toolkit=toolkit(), cpp_compiler=dict(path="", version="", source="PATH", reason=""),
                  devices=[], metal_available=None, metal_reason="")
    if "managed_toolkit" in profile.get("checks", []):
        compiler = shutil.which("c++")
        result["cpp_compiler"]["path"] = compiler or ""
        if compiler:
            try:
                code, output = command_output([compiler, "--version"])
                if code == 0:
                    result["cpp_compiler"]["version"] = output.splitlines()[0][:LIMIT]
                else:
                    result["cpp_compiler"]["reason"] = "C++ compiler version unavailable"
            except Exception as error:
                result["cpp_compiler"]["reason"] = str(error)[:LIMIT]
        else:
            result["cpp_compiler"]["reason"] = "C++ compiler not found on the service PATH"
    for package in profile.get("packages", []):
        try:
            result["packages"][package] = dict(installed=True, version=importlib.metadata.version(package), reason="")
        except importlib.metadata.PackageNotFoundError:
            result["packages"][package] = dict(installed=False, version="", reason="package not installed")
    if "cuda_available" in profile.get("checks", []):
        try:
            torch = importlib.import_module("torch")
            result["cuda_runtime"]["build_version"] = torch.version.cuda or ""
            torch.cuda.init()
            for index in range(min(torch.cuda.device_count(), 64)):
                major, minor = torch.cuda.get_device_capability(index)
                result["devices"].append(dict(index=index, name=torch.cuda.get_device_name(index), capability=f"{major}.{minor}"))
            version, path = loaded_cuda_runtime()
            result["cuda_runtime"].update(loaded_version=version, library_path=path)
        except Exception as error:
            result["cuda_runtime"]["reason"] = (type(error).__name__ + ": " + str(error))[:LIMIT]
    if "metal_available" in profile.get("checks", []):
        try:
            mx = importlib.import_module("mlx.core")
            result["metal_available"] = mx.metal.is_available()
        except Exception as error:
            result["metal_reason"] = (type(error).__name__ + ": " + str(error))[:LIMIT]
    return result


if __name__ == "__main__":
    profile = json.loads(sys.argv[1])
    mode = sys.argv[3] if len(sys.argv) > 3 else "verify"
    if mode == "environment":
        configure_probe(sys.argv[2])
        checks = environment(profile)
    elif len(sys.argv) > 2:
        checks = run(profile, sys.argv[2])
    else:
        with tempfile.TemporaryDirectory(prefix="zzrouter-probe-") as scratch:
            checks = run(profile, scratch)
    print("ZZROUTER_RUNTIME_CHECKS=" + json.dumps(checks))
    sys.exit(0 if mode == "environment" or all(item["passed"] for item in checks) else 1)
