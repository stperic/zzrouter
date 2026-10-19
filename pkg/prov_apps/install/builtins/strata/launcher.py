#!/usr/bin/env python3
"""Start Strata's server for one model, the way zzRouter starts any engine binary.

strata-launch --model-path <shard 1> --name <model> --port <port> -- <engine flags>

On each launch it builds the model's pack with Strata's own tool when it is missing, writes the server's config and
execs the server, so the process zzRouter tracks is Strata's server and a stop reaches the whole process group.
"""
import fcntl
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

HOME = Path(__file__).resolve().parent.parent      # providers/strata
DATA = HOME / "data"
SHARD = re.compile(r"^(.*)-(\d{5})-of-(\d{5})\.gguf$")


def say(msg):
    print(f"[strata-launch] {msg}", file=sys.stderr, flush=True)


def fail(msg):
    say(msg)
    sys.exit(2)


def parse(argv):
    if "--" in argv:
        own, engine = argv[: argv.index("--")], argv[argv.index("--") + 1:]
    else:
        own, engine = argv, []
    if len(own) % 2 or any(not k.startswith("--") for k in own[::2]):
        fail(f"usage: {Path(sys.argv[0]).name} --model-path <gguf> --name <model> --port <port> -- <engine flags>")
    opts = dict(zip(own[::2], own[1::2]))
    missing = [k for k in ("--model-path", "--name", "--port") if k not in opts]
    if missing:
        fail(f"missing {', '.join(missing)}")
    return Path(opts["--model-path"]), opts["--name"], int(opts["--port"]), engine


def shards_of(first):
    m = SHARD.match(first.name)
    if not m:
        return [first]
    if int(m.group(2)) != 1:
        fail(f"{first} is not the first shard of its set")
    shards = [first.with_name(f"{m.group(1)}-{i:05d}-of-{m.group(3)}.gguf") for i in range(1, int(m.group(3)) + 1)]
    absent = [s.name for s in shards if not s.is_file()]
    if absent:
        fail(f"shards missing beside {first.name}: {', '.join(absent)}")
    return shards


def pack_key(version, gguf):
    st = gguf.stat()
    return hashlib.sha256(f"{version}\0{gguf.resolve()}\0{st.st_size}\0{st.st_mtime_ns}".encode()).hexdigest()[:16]


def packs_in_use():
    """Pack dirs a running engine names on its command line."""
    used = set()
    for proc in Path("/proc").iterdir():
        try:
            argv = (proc / "cmdline").read_bytes().split(b"\0") if proc.name.isdigit() else []
        except OSError:
            continue
        used.update(argv[i + 1].decode(errors="replace") for i, a in enumerate(argv[:-1]) if a == b"--pack")
    return used


def prune_packs(packs, version):
    """Drops dead packers' temp dirs and the packs no running engine uses whose model or engine version changed.
    The caller holds the packs lock, so no other packer is running."""
    for tmp in packs.glob("*.tmp-*"):
        shutil.rmtree(tmp, ignore_errors=True)
    used = packs_in_use()
    for src in packs.glob("*.json"):
        if str(packs / src.stem) in used:
            continue
        gguf = Path(json.loads(src.read_text()).get("gguf", ""))
        if not gguf.is_file() or pack_key(version, gguf) != src.stem:
            shutil.rmtree(packs / src.stem, ignore_errors=True)
            src.unlink(missing_ok=True)


def complete(pack):
    return (pack / "native_experts.txt").is_file() and (pack / "tokenizer" / "vocab.json").is_file()


def ensure_pack(release, version, python, gguf, name):
    packs = DATA / "packs"
    packs.mkdir(parents=True, exist_ok=True)
    key = pack_key(version, gguf)
    pack = packs / key
    with open(packs / ".lock", "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)            # one packer per node: a base and its variant can start at once
        prune_packs(packs, version)
        if complete(pack):
            return pack
        say(f"building the Strata pack for {name} (one time per model and engine version)")
        (packs / f"{key}.json").write_text(json.dumps({"gguf": str(gguf.resolve())}))   # before the pack: prunable
        tmp = packs / f"{key}.tmp-{os.getpid()}"
        env = dict(os.environ, STRATA_GGUF_PY=str(release / "third_party" / "llama.cpp" / "gguf-py"))
        rc = subprocess.call([str(python), str(release / "tools" / "iq_pack.py"), "--gguf", str(gguf),
                              "--out", str(tmp)], cwd=release, env=env, stdout=sys.stderr)
        if rc != 0:
            shutil.rmtree(tmp, ignore_errors=True)
            fail(f"Strata's pack tool refused {gguf.name} (exit {rc})")
        shutil.rmtree(pack, ignore_errors=True)     # an incomplete leftover: no engine starts from one
        tmp.rename(pack)
    return pack


def main():
    gguf, name, port, engine = parse(sys.argv[1:])
    version = (HOME / "version").read_text().strip()
    release = HOME / "releases" / version
    python = release / ".venv" / "bin" / "python"
    shards = shards_of(gguf)

    sys.path.insert(0, str(release / "tools"))
    from gguf_reader import GGUFFile                # Strata's own header reader, standard library only
    ple = next((s for s in shards if any(t.name == "per_layer_token_embd.weight" for t in GGUFFile(s).tensors)), None)
    if ple is None:
        fail(f"{gguf.name} has no per_layer_token_embd tensor: not a model Strata runs")
    mtp = DATA / "mtp" / "rt"
    if not (mtp / "experts.bin").is_file():
        fail(f"the MTP draft layer is missing ({mtp}): reinstall Strata")

    pack = ensure_pack(release, version, python, shards[0], name)
    args = ["--pack", str(pack), "--native", str(shards[0])]
    if len(shards) <= 2:                            # with more shards the engine finds the table from shard 1
        args += ["--ple-gguf", str(ple)]
    defaults = {"--expert-profile": release / "data" / "expert-profile.bin", "--mtp": mtp}
    for flag, value in defaults.items():
        if flag not in engine:
            args += [flag, str(value)]

    run = DATA / "run"
    run.mkdir(parents=True, exist_ok=True)
    log = run / f"{port}.log"
    log.write_text("")                              # one run's engine log; the server narrates it to stdout
    build = json.loads((release / "engine" / "BUILD.json").read_text())
    config = run / f"{port}.json"
    config.write_text(json.dumps({
        "exe": str(release / "engine" / "strata"), "args": args + engine, "cwd": str(release),
        "tokenizer": str(pack / "tokenizer"), "model_name": name, "log": str(log),
        "lib_dirs": build.get("cuda_dirs") or [], "host": "127.0.0.1", "port": port,
    }, indent=1))
    os.chdir(release)
    server = [str(python), str(release / "serve" / "server.py"), "--engine", "strata", "--config", str(config),
              "--host", "127.0.0.1", "--port", str(port)]
    os.execv(server[0], server)


if __name__ == "__main__":
    main()
