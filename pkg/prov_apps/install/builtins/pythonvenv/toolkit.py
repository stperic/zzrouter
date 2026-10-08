# Release-owned CUDA 13 layout, confined to the selected managed interpreter.
import ctypes
import os
from pathlib import Path
import re
import subprocess
import sys
import sysconfig


def contained(path, root):
    resolved = path.resolve(strict=True)
    resolved.relative_to(root)
    return resolved


root = Path(sys.prefix).resolve(strict=True)
site = contained(Path(sysconfig.get_path("purelib")), root)
cuda = contained(site / "nvidia" / "cu13", root)
for directory in ("bin", "lib", "nvvm", "nvvm/bin", "nvvm/libdevice", "include", "include/crt", "include/cccl"):
    contained(cuda / directory, root)
for part in ("bin/nvcc", "bin/ptxas", "bin/nvlink", "bin/fatbinary", "nvvm/bin/cicc",
             "include/cuda_runtime.h", "include/crt/host_config.h", "include/cccl/cuda",
             "nvvm/libdevice/libdevice.10.bc", "lib/libnvvm.so.4", "lib/libcudart.so.13",
             "lib/libcudart_static.a", "lib/libcudadevrt.a"):
    contained(cuda / part, root)

for link, target in ((cuda / "lib64", "lib"), (cuda / "lib" / "libcudart.so", "libcudart.so.13")):
    if link.is_symlink():
        if os.readlink(link) != target:
            raise RuntimeError("managed toolkit link changed")
    elif link.exists():
        raise RuntimeError("managed toolkit layout collision")
    else:
        link.symlink_to(target)

# The human-owned driver stays external; a linker script references its loaded library.
ctypes.CDLL("libcuda.so.1")
with open("/proc/self/maps") as maps:
    data = maps.read((4 << 20) + 1)
if len(data) > 4 << 20:
    raise RuntimeError("driver mapping exceeds diagnostic bound")
driver = None
for line in data.splitlines():
    fields = line.split(maxsplit=5)
    if len(fields) == 6 and re.search(r"/libcuda\.so\.[0-9.]+$", fields[5]):
        candidate = Path(fields[5]).resolve(strict=True)
        if candidate.stat().st_uid != 0 or candidate.stat().st_mode & 0o022:
            raise RuntimeError("driver library must be operator-owned and protected")
        driver = candidate
        break
if driver is None or any(char in str(driver) for char in "\n\r\"\\"):
    raise RuntimeError("protected NVIDIA driver library unavailable")
linker = cuda / "lib" / "libcuda.so"
if linker.is_symlink():
    raise RuntimeError("managed driver linker script cannot be a symlink")
linker.write_text('INPUT ( "' + str(driver) + '" )\n')

nvcc = subprocess.run([str(cuda / "bin/nvcc"), "--version"], check=True,
                      stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=15)
if b"release 13.0," not in nvcc.stdout:
    raise RuntimeError("managed compiler must match CUDA 13.0")
print("Managed CUDA 13.0 compiler layout prepared; host C++ compiler and driver remain prerequisites")
