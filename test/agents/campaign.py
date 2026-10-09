#!/usr/bin/env python3
"""Local-engine test campaign for zzRouter, run as a matrix.

Every cell is one node and one local provider it supports (lab.json lists
them). Models are never named per node: each role (a small text model, a
small vision model, a big model for tool calling) is a family and size, and
the runner finds the build each cell loads through zzRouter's own model
search, sized to the node's hardware, and downloads it through the API.

Three tracks against a running coordinator:
  direct   the script drives /v1 in every cell: chat, stream, Responses,
           Anthropic Messages, tool calling on the big model, an image to a
           text-only model, an image to a vision model without and then with
           its projector, two models on one card, and two that contend.
  mcp      Claude Code (Haiku) and Codex operate each cell through the MCP
           server; a logging proxy records every tools/call as ground truth.
  backend  Claude Code and Codex use a local model behind zzRouter as their
           brain and must write and run a file (real tool calling).

Every agent confusion (a schema error, a failed or retried call, a fall
back to the zz_api escape hatch) is a finding: confusion.md lists them.

Usage: ZZR_CAMPAIGN_KEY=<admin key> campaign.py --lab lab.json  (copy lab.example.json)
       [--plan] [--tracks direct,mcp,backend] [--nodes ...] [--providers ...]
       [--agents claude,codex] [--only <scenario-substring>] [--analyze <run dir>]
--plan prints the matrix and the build search picked per cell, downloads nothing.
Needs claude and codex on PATH. Writes report.md and confusion.md under
$TMPDIR/zzr-campaign/<time>/ unless --out says otherwise.
"""
import argparse, base64, json, os, re, shlex, shutil, subprocess, sys, threading, time
import http.server as httpserver, urllib.request, urllib.error
from concurrent.futures import ThreadPoolExecutor
from urllib.parse import urlencode

COORD = os.environ.get("ZZR_COORDINATOR", "http://127.0.0.1:9090")
KEY = os.environ.get("ZZR_CAMPAIGN_KEY", "")
PROXY_PORT = 19390
CLAUDE_MODEL = "claude-haiku-5-5"
AGENT_TIMEOUT = 3600  # a big model's download happens inside a scenario

# The lab under test (--lab, filled in main()). "nodes" maps each node to the
# local providers it runs and an optional "projector_reset" command template
# ({repo}, {file}) that removes a vision projector out of band. "roles" maps
# each model role to a family, a size and, for ollama, its library name.
LAB = {}
ROLES = {}
BACKEND_MODELS = {}
MATRIX = {}  # (node, provider) -> {role: pick}, filled by plan()

WEATHER_TOOL = {"type": "function", "function": {"name": "get_weather", "description": "Get the current weather for a city",
                "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}
ANTH_TOOL = {"name": "get_weather", "description": "Get the current weather for a city",
             "input_schema": WEATHER_TOOL["function"]["parameters"]}


# ---------------------------------------------------------------- HTTP helpers
# LOAD_RETRY_WINDOW bounds how long a probe waits on a load the server says is still running;
# a big model's first load on vLLM compiles CUDA graphs for minutes.
LOAD_RETRY_WINDOW = 900
DEFAULT_RETRY_AFTER = 10


def http(method, path, body=None, headers=None, timeout=600, raw=False):
    """One API call; a load still running (503 model_load_timeout) is retried after Retry-After, as an agent would."""
    start, deadline = time.time(), time.time() + LOAD_RETRY_WINDOW
    while True:
        code, out, ms, retry_after = _http_once(method, path, body, headers, timeout, raw)
        loading = code == 503 and b"model_load_timeout" in (out if raw else json.dumps(out).encode())
        if not loading or time.time() + retry_after > deadline:
            return code, out, ms if not loading else int((time.time() - start) * 1000)
        time.sleep(retry_after)


def _http_once(method, path, body, headers, timeout, raw):
    h = {"X-API-Key": KEY}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        h["Content-Type"] = "application/json"
    h.update(headers or {})
    req = urllib.request.Request(COORD + path, data=data, method=method, headers=h)
    t = time.time()
    retry_after = DEFAULT_RETRY_AFTER
    try:
        r = urllib.request.urlopen(req, timeout=timeout)
        code, out = r.status, r.read()
    except urllib.error.HTTPError as e:
        code, out = e.code, e.read()
        retry_after = int(e.headers.get("Retry-After") or DEFAULT_RETRY_AFTER)
    except Exception as e:  # timeouts, resets
        return 0, {"error": repr(e)}, int((time.time() - t) * 1000), 0
    ms = int((time.time() - t) * 1000)
    if raw:
        return code, out, ms, retry_after
    try:
        return code, json.loads(out), ms, retry_after
    except ValueError:
        return code, {"_raw": out[:500].decode("utf-8", "replace")}, ms, retry_after


def runs(node=None):
    _, d, _ = http("GET", "/zzrouter/v1/runs")
    return [r for r in d.get("data", []) if node is None or r.get("node") == node]


def stop_runs(node, keep=()):
    for r in runs(node):
        if r["id"] not in keep:
            http("DELETE", f"/zzrouter/v1/runs/{r['id']}")


# ---------------------------------------------------------------- results
class Results:
    def __init__(self, out):
        self.out, self.rows, self.lock = out, [], threading.Lock()

    def add(self, track, node, scenario, check, ok, detail="", ms=None):
        row = {"track": track, "node": node, "scenario": scenario, "check": check,
               "ok": ok, "detail": str(detail)[:400], "ms": ms, "t": time.strftime("%H:%M:%S")}
        with self.lock:
            self.rows.append(row)
            with open(os.path.join(self.out, "results.jsonl"), "a") as f:
                f.write(json.dumps(row) + "\n")
        mark = {True: "PASS", False: "FAIL", None: "INFO"}[ok]
        print(f"[{row['t']}] {mark} {track}/{node}/{scenario}: {check} {row['detail'][:160]}", flush=True)

    def report(self):
        lines = ["# zzRouter local-engine campaign", "", f"Coordinator {COORD}, {time.strftime('%Y-%m-%d %H:%M')}", ""]
        for track in ("direct", "mcp", "backend"):
            rows = [r for r in self.rows if r["track"] == track]
            if not rows:
                continue
            p = sum(r["ok"] is True for r in rows); f = sum(r["ok"] is False for r in rows)
            lines += [f"## {track}: {p} pass, {f} fail", "", "| node | scenario | check | result | ms | detail |", "|---|---|---|---|---|---|"]
            for r in rows:
                res = {True: "pass", False: "**FAIL**", None: "info"}[r["ok"]]
                d = r["detail"].replace("|", "\\|").replace("\n", " ")[:200]
                lines.append(f"| {r['node']} | {r['scenario']} | {r['check']} | {res} | {r['ms'] or ''} | {d} |")
            lines.append("")
        path = os.path.join(self.out, "report.md")
        open(path, "w").write("\n".join(lines))
        return path



# ---------------------------------------------------------------- matrix: models found by search
# The Hugging Face library each provider loads; ollama has its own registry.
FORMATS = {"llamacpp": "gguf", "mlx": "mlx", "vllm": "transformers"}
# Community re-tunes rank high on downloads but are not the model under test.
RETUNE_MARKS = ("uncensored", "abliterat", "heretic", "fusion", "merge", "turbo", "-rco")
# A big model is quantized, not full precision: vLLM already covers bf16.
FULL_PRECISION = ("BF16", "F16", "F32")
GiB = 1 << 30
# vLLM keeps the weights and KV cache in GPU memory; this share leaves room for the cache.
VLLM_WEIGHT_SHARE = 0.7
# MLX shares unified memory with the OS and every other process.
MLX_WEIGHT_SHARE = 0.5
# llama.cpp and ollama spill layers to system RAM; half of the free RAM may hold them.
OFFLOAD_RAM_SHARE = 0.5
# A Q4 build of N billion parameters takes about this many GB, for ollama tags search does not size.
OLLAMA_GB_PER_B = 0.65


def hardware(node):
    d = http("GET", f"/zzrouter/v1/nodes/{node}")[1].get("data") or {}
    gpus = [g for g in (d.get("gpu") or {}).get("gpus") or [] if g.get("memory_total_gb")]
    mem = d.get("memory") or {}
    return {"os": d.get("os"), "gpu_gb": max((g["memory_total_gb"] for g in gpus), default=0),
            "ram_gb": mem.get("total_gb") or 0, "free_ram_gb": mem.get("available_gb") or 0}


def budgets(provider, hw):
    """Largest weights (GB) a cell may pick: on the GPU alone first, then with offload."""
    if provider == "vllm":
        return [VLLM_WEIGHT_SHARE * hw["gpu_gb"]]
    if provider == "mlx":
        return [MLX_WEIGHT_SHARE * hw["ram_gb"]]
    return [hw["gpu_gb"], hw["gpu_gb"] + OFFLOAD_RAM_SHARE * hw["free_ram_gb"]]


def billions(size):
    return float(re.sub(r"[^0-9.]", "", size) or 0)


def search(registry, query):
    _, d, _ = http("GET", "/zzrouter/v1/search?" + urlencode({"type": "models", "provider": registry, "q": query, "limit": 20}), timeout=120)
    return (d.get("data") or {}).get("results") or []


def hf_candidates(role, provider):
    fmt = FORMATS[provider]
    words = [role["family"], role["size"], role.get("variant", "")]
    query = " ".join(w for w in words + [{"gguf": "GGUF", "mlx": "MLX"}.get(fmt, "")] if w)
    out = []
    for r in search("huggingface", query):
        rid = r.get("id") or ""
        low = rid.lower()
        tags = {str(t).lower() for t in r.get("tags") or []} | {str(r.get("library_name") or "").lower()}
        if not all(w.lower() in low for w in words if w) or any(m in low for m in RETUNE_MARKS):
            continue
        if (fmt == "gguf") != ("gguf" in tags or low.endswith("gguf")) or (fmt == "mlx") != ("mlx" in tags or "mlx" in low):
            continue
        out.append(r)
    return sorted(out, key=lambda r: -(r.get("downloads") or 0))


def gguf_choice(variants, limit, largest):
    fits = sorted((v for v in variants if v.get("quantization") and v.get("size_bytes") and v["size_bytes"] / GiB <= limit
                   and "mtp" not in v["file"].lower() and not (largest and v.get("quantization") in FULL_PRECISION)),
                  key=lambda v: v["size_bytes"])
    if not fits:
        return None
    if largest:
        return fits[-1]
    return next((v for v in fits if v.get("recommended")), None) or next((v for v in fits if v.get("quantization") == "Q4_K_M"), fits[len(fits) // 2])


def pick_hf(provider, role, hw):
    """The most downloaded build of the role that fits: a GGUF file (the largest quantization
    for a big role), or a whole repo."""
    for limit in budgets(provider, hw):
        for r in hf_candidates(role, provider)[:5]:
            _, d, _ = http("GET", "/zzrouter/v1/registries/huggingface/variants?" + urlencode({"id": r["id"], "provider": provider}), timeout=120)
            d = d.get("data") or d
            if provider == "llamacpp":
                v = gguf_choice(d.get("variants") or [], limit, role.get("largest"))
                if not v:
                    continue
                vision = next((f for f in d.get("features") or [] if f.get("feature") == "vision"), None)
                if role.get("vision") and not vision:
                    continue  # a GGUF reads images only through a projector the repo ships
                return {"registry": "huggingface", "repo": r["id"], "file": v["file"], "size_gb": round(v["size_bytes"] / GiB, 1),
                        "projector": vision and vision["file"], "name": gguf_name(v["file"])}
            size = (d.get("total_size_bytes") or 0) / GiB
            if 0 < size <= limit:
                return {"registry": "huggingface", "repo": r["id"], "file": "", "size_gb": round(size, 1), "projector": None, "name": r["id"]}
    return None


def pick_ollama(role, hw):
    name = role.get("ollama")
    if not name or not any(r.get("name") == name for r in search("ollama", name)):
        return None
    size = round(billions(role["size"]) * OLLAMA_GB_PER_B, 1)
    if size > budgets("ollama", hw)[-1]:
        return None
    tag = f"{name}:{role['size'].lower()}"
    return {"registry": "ollama", "repo": tag, "file": "", "size_gb": size, "projector": None, "name": tag}


def gguf_name(file):
    """The catalog name of a GGUF: its file stem, one name for every shard."""
    stem = os.path.basename(file).removesuffix(".gguf")
    return re.sub(r"-0+1-of-\d+$", "", stem)


def plan(nodes, only_providers=()):
    """Fill MATRIX with every cell's pick per role and print it."""
    lines = ["| node | provider | role | build | size GB | projector | on node |", "|---|---|---|---|---|---|---|"]
    for node in nodes:
        hw = hardware(node)
        for provider in LAB[node]["providers"]:
            if only_providers and provider not in only_providers:
                continue
            cell = MATRIX.setdefault((node, provider), {})
            have = catalog(node, provider)
            for role_name, role in ROLES.items():
                pick = pick_ollama(role, hw) if provider == "ollama" else pick_hf(provider, role, hw)
                if pick:
                    pick["present"] = pick["name"] in have
                    cell[role_name] = pick
                build = f"{pick['repo']} {pick['file']}".strip() if pick else "none fits"
                lines.append(f"| {node} | {provider} | {role_name} | {build} | {pick['size_gb'] if pick else ''} | "
                             f"{pick and pick['projector'] or ''} | {pick['present'] if pick else ''} |")
    print("\n".join(lines), flush=True)
    return "\n".join(lines)


def catalog(node, provider=None):
    """The models a node can serve; an incomplete download is not one of them."""
    _, d, _ = http("GET", f"/zzrouter/v1/models?node={node}&limit=500&refresh=true")
    return {m["name"]: m for m in d.get("data") or [] if provider in (None, m.get("assigned_app")) and not m.get("incomplete")}


def vision_state(node, provider, name):
    m = catalog(node, provider).get(name) or {}
    return ((m.get("details") or {}).get("features") or {}).get("vision")


def deploy(node, provider, pick, features=None):
    """Download a pick to one node through the API and wait for its job."""
    body = {"model": pick["repo"], "registry": pick["registry"], "provider": provider, "nodes": [node]}
    if pick["file"]:
        body["file"] = pick["file"]
    if features is not None:
        body["features"] = features
    code, d, _ = http("POST", "/zzrouter/v1/deployments", body)
    jobs = [n.get("job_id") for n in ((d.get("data") or {}).get("nodes") or []) if n.get("job_id")]
    results = [wait_job(j, timeout=AGENT_TIMEOUT) for j in jobs]
    failed = [r for r in results if job_phase(r) not in JOB_SUCCESS]
    return code in (200, 201, 202) and not failed, f"{code} jobs={len(jobs)} {json.dumps(failed or d)[:240]}"


def ensure(R, node, provider, role, pick, features=None):
    if pick["present"] and features is None:
        return True
    ok, detail = deploy(node, provider, pick, features)
    pick["present"] = ok and pick["name"] in catalog(node, provider)
    R.add("direct", node, f"{provider}:{role}", f"download {pick['repo']} {pick['file']}".strip(), pick["present"], detail)
    return pick["present"]


JOB_SUCCESS = ("completed", "succeeded", "done")
JOB_TERMINAL = JOB_SUCCESS + ("failed", "cancelled", "error")


def job_phase(d):
    j = d.get("data") or d
    return j.get("phase") or j.get("state") or j.get("status")


def wait_job(job, timeout=900):
    end = time.time() + timeout
    while time.time() < end:
        _, d, _ = http("GET", f"/zzrouter/v1/jobs/{job}")
        if job_phase(d) in JOB_TERMINAL:
            return d
        time.sleep(3)
    return {"timeout": job}


# ---------------------------------------------------------------- direct track
def sse_events(raw):
    for line in raw.decode("utf-8", "replace").splitlines():
        if line.startswith("data:"):
            yield line[5:].strip()


# ANSWER_TOKENS leaves a thinking model room to reason before the answer a
# probe judges; with less, it stops on length with empty content.
ANSWER_TOKENS = 1024


def chat(name, content, max_tokens=ANSWER_TOKENS, **extra):
    return http("POST", "/v1/chat/completions", dict({"model": name, "max_tokens": max_tokens,
                "messages": [{"role": "user", "content": content}]}, **extra))


def answer(code, d):
    return ((d.get("choices") or [{}])[0].get("message") or {}).get("content") or "" if code == 200 else ""


def problem(d):
    e = d.get("error") if isinstance(d.get("error"), dict) else d
    return f"{e.get('code')} {str(e.get('message') or e.get('detail'))[:200]}"


def direct_basics(R, node, provider, name):
    sc = f"{provider}:text"
    code, d, ms = chat(f"{name}@{node}", "Reply with the single word: pong", 32)
    R.add("direct", node, sc, "chat (cold start)", code == 200 and bool(answer(code, d)), f"{code} {answer(code, d) or d!s:.120}", ms)
    if code != 200:
        return
    served = d.get("zzrouter", {})
    R.add("direct", node, sc, "served by this node and provider", served.get("node") == node and served.get("provider") == provider, served)
    code, raw, ms = http("POST", "/v1/chat/completions", {"model": f"{name}@{node}", "max_tokens": 32, "stream": True,
                         "messages": [{"role": "user", "content": "Count from 1 to 5."}]}, raw=True)
    ev = list(sse_events(raw)) if code == 200 else []
    R.add("direct", node, sc, "chat stream ends [DONE]", code == 200 and ev[-1:] == ["[DONE]"] and len(ev) > 2, f"{code} events={len(ev)}", ms)
    code, d, ms = http("POST", "/v1/messages", {"model": f"{name}@{node}", "max_tokens": 32, "system": "You are terse.",
                       "messages": [{"role": "user", "content": "Say hi."}, {"role": "assistant", "content": "Hi."},
                                    {"role": "user", "content": [{"type": "text", "text": "<system-reminder>Be brief.</system-reminder>"},
                                                                 {"type": "text", "text": "Say bye."}]}]},
                       headers={"anthropic-version": "2023-06-01"})
    R.add("direct", node, sc, "anthropic multi-turn + system", code == 200, f"{code} {str(d)[:200]}", ms)
    code, d, ms = http("POST", "/v1/responses", {"model": f"{name}@{node}", "input": "Reply with the single word: pong", "max_output_tokens": 32})
    R.add("direct", node, sc, "responses", code == 200 and d.get("status") in ("completed", "incomplete"), f"{code} {d.get('status') or str(d)[:200]}", ms)


def direct_tools(R, node, provider, name):
    """Tool calling on the big model: OpenAI tool_calls, the result round trip, Anthropic tool_use."""
    sc, model = f"{provider}:big", f"{name}@{node}"
    ask = [{"role": "user", "content": "What is the weather in Paris right now? Use the tool."}]
    code, d, ms = http("POST", "/v1/chat/completions", {"model": model, "max_tokens": ANSWER_TOKENS, "tools": [WEATHER_TOOL], "messages": ask})
    if code == 400 and (d.get("error") or {}).get("code") == "tool_calling_not_enabled":
        R.add("direct", node, sc, "tools refusal is classified", True, problem(d), ms)
        return
    tc = ((d.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") if code == 200 else None
    ok = bool(tc) and tc[0]["function"]["name"] == "get_weather" and "paris" in tc[0]["function"]["arguments"].lower()
    R.add("direct", node, sc, "openai tool_calls", ok, f"{code} {tc or d!s:.200}", ms)
    if ok:
        msgs = ask + [d["choices"][0]["message"], {"role": "tool", "tool_call_id": tc[0]["id"],
                      "content": json.dumps({"city": "Paris", "temp_c": 31, "sky": "sunny"})}]
        code, d2, ms = http("POST", "/v1/chat/completions", {"model": model, "max_tokens": ANSWER_TOKENS, "tools": [WEATHER_TOOL], "messages": msgs})
        R.add("direct", node, sc, "openai tool result round trip", code == 200 and "31" in answer(code, d2), f"{code} {answer(code, d2) or d2!s:.150}", ms)
    code, d, ms = http("POST", "/v1/messages", {"model": model, "max_tokens": ANSWER_TOKENS, "tools": [ANTH_TOOL],
                       "messages": [{"role": "user", "content": "What is the weather in Tokyo? Use the tool."}]},
                       headers={"anthropic-version": "2023-06-01"})
    tu = [b for b in d.get("content", []) if b.get("type") == "tool_use"] if code == 200 else []
    R.add("direct", node, sc, "anthropic messages tool_use", bool(tu) and tu[0].get("name") == "get_weather" and d.get("stop_reason") == "tool_use",
          f"{code} stop={d.get('stop_reason')} {tu or d!s:.200}", ms)


def image_request(model):
    b64 = base64.b64encode(open(IMAGE, "rb").read()).decode()
    return chat(model, [{"type": "text", "text": "What color is this square? One word."},
                        {"type": "image_url", "image_url": {"url": "data:image/png;base64," + b64}}])


def refused_for_images(code, d):
    return code == 400 and any(w in json.dumps(d).lower() for w in ("image", "vision", "multimodal"))


def direct_image_to_text(R, node, provider, name):
    """An image to a model that cannot read one is a clear 400, never a guess about pixels it never saw."""
    stop_runs(node)
    state = vision_state(node, provider, name)
    code, d, ms = image_request(f"{name}@{node}")
    R.add("direct", node, f"{provider}:text", "image to a text-only model refused", refused_for_images(code, d),
          f"{code} vision={state} {problem(d) if code != 200 else answer(code, d)[:120]}", ms)
    if state in ("unsupported", "available"):  # known before the request: refused without a launch
        R.add("direct", node, f"{provider}:text", "refused before any launch", not runs(node), [r["model"] for r in runs(node)])


def direct_vision(R, node, provider, pick):
    """A vision model whose projector is missing refuses images and says why; with it, it reads them.
    A run started before the projector arrived must not answer an image as text."""
    sc, model = f"{provider}:vision", f"{pick['name']}@{node}"
    stop_runs(node)
    if pick["projector"]:
        if vision_state(node, provider, pick["name"]) == "present" and not reset_projector(node, provider, pick):
            R.add("direct", node, sc, "projector removed for the test", False, "no projector_reset for this node")
            return
        code, d, ms = image_request(model)
        R.add("direct", node, sc, "image without projector refused with the fix", refused_for_images(code, d),
              f"{code} vision={vision_state(node, provider, pick['name'])} {problem(d) if code != 200 else answer(code, d)[:120]}", ms)
        chat(model, "Reply: ok", 16)  # a text run, launched before the projector exists
        ok, detail = deploy(node, provider, pick, ["vision"])
        # The text run above lacks the projector, so until it restarts the model cannot read images.
        state = vision_state(node, provider, pick["name"])
        R.add("direct", node, sc, "projector added through the API", ok and state == "available", f"vision={state} {detail}")
        code, d, ms = image_request(model)
        stale_ok = (code == 200 and "red" in answer(code, d).lower()) or (code in (400, 409) and "restart" in json.dumps(d).lower())
        R.add("direct", node, sc, "image to the run started before the projector", stale_ok,
              f"{code} {problem(d) if code != 200 else answer(code, d)[:120]}", ms)
        stop_runs(node)
    code, d, ms = image_request(model)
    R.add("direct", node, sc, "image answered correctly (red)", code == 200 and "red" in answer(code, d).lower(),
          f"{code} {problem(d) if code != 200 else answer(code, d)[:120]}", ms)


def reset_projector(node, provider, pick):
    tmpl = LAB[node].get("projector_reset")
    if not tmpl:
        return False
    done = subprocess.run(shlex.split(tmpl.format(repo=pick["repo"], file=pick["projector"])), capture_output=True, text=True, timeout=60)
    http("POST", f"/zzrouter/v1/models/scan?node={node}")
    return done.returncode == 0 and vision_state(node, provider, pick["name"]) != "present"


def direct_pair(R, node, provider, a, b):
    """Two models on one card at once: both answer while both run."""
    stop_runs(node)
    with ThreadPoolExecutor(2) as ex:
        res = list(ex.map(lambda p: chat(f"{p['name']}@{node}", "Reply: ok", 16), (a, b)))
    for p, (code, d, ms) in zip((a, b), res):
        R.add("direct", node, f"{provider}:pair", f"{p['name']} answers beside the other", code == 200, f"{code} {problem(d) if code != 200 else ''}", ms)
    if provider != "ollama":
        live = {r["model"] for r in runs(node) if r.get("status") == "running"}
        R.add("direct", node, f"{provider}:pair", "both running at once", all(any(p["name"] in l or p["repo"] in l for l in live) for p in (a, b)), sorted(live))
    stop_runs(node)


def memory_parameter(provider):
    """The launch parameter that sets a fraction of GPU memory, as the provider declares it."""
    mem = ((http("GET", f"/zzrouter/v1/providers/{provider}/schema")[1].get("diagnostics")) or {}).get("memory") or {}
    return mem.get("parameter") if mem.get("kind") == "fraction_total" else None


# Two launches at this share of GPU memory cannot both fit.
CONTENTION_SHARE = "0.6"


def launch(node, provider, pick, parameters):
    code, d, _ = http("POST", "/zzrouter/v1/runs", {"provider": provider, "launch_mode": "native", "node": node,
                      "model": pick["name"], "parameters": parameters})
    return code, d, ((d.get("data") or {}) if isinstance(d.get("data"), dict) else {}).get("job_id")


def direct_contention(R, node, provider, cell):
    """Two models that cannot both fit: the second is queued, then refused with the holder named,
    or both serve where the engine offloads. Never a crash, a hang or a run left behind.
    A provider that sizes runs by a GPU-memory fraction gets two small models at a share that
    cannot fit twice; the others get the big model and a second one beside it."""
    sc = f"{provider}:contention"
    param = memory_parameter(provider)
    a, b = (cell["text"], cell["vision"]) if param else (cell.get("big") or cell["text"], cell["vision"])
    stop_runs(node)
    if provider == "ollama":
        res = [chat(f"{p['name']}@{node}", "Reply: ok", 16) for p in (a, b)]
        R.add("direct", node, sc, "ollama swaps models it cannot hold together", all(c == 200 for c, _, _ in res),
              [f"{c} {problem(d) if c != 200 else ''}" for c, d, _ in res])
        return
    params = {param: CONTENTION_SHARE} if param else {}
    first, second = launch(node, provider, a, params), launch(node, provider, b, params)
    outcome = []
    for (code, d, job), p in zip((first, second), (a, b)):
        j = wait_job(job, timeout=AGENT_TIMEOUT) if job else d
        outcome.append((p["name"], code, job_phase(j), json.dumps(j)))
    refused = outcome[1][2] == "failed" and any(w in outcome[1][3] for w in ("cannot fit", "budget", "memory"))
    holder = refused and a["name"].split("/")[-1].lower() in outcome[1][3].lower()
    R.add("direct", node, sc, "first launch serves", outcome[0][2] in JOB_SUCCESS, outcome[0][3][:200])
    R.add("direct", node, sc, "second launch serves or is refused for memory", outcome[1][2] in JOB_SUCCESS or refused, outcome[1][3][:300])
    if refused:
        R.add("direct", node, sc, "refusal names the run holding the memory", holder, outcome[1][3][:300])
    stop_runs(node)
    time.sleep(2)
    R.add("direct", node, sc, "no run left behind", not runs(node), [r["model"] for r in runs(node)])


def direct_catalog(R, node):
    """A split GGUF is one model in the catalog, not one per shard."""
    names = list(catalog(node))
    shards = [n for n in names if "-of-0" in n]
    R.add("direct", node, "catalog", "no shard listed as its own model", not shards, shards or f"{len(names)} models")


def track_direct(R, node, provider):
    cell = MATRIX.get((node, provider)) or {}
    for role, pick in cell.items():
        # The vision model arrives without its projector, so the test starts from that state.
        if not ensure(R, node, provider, role, pick, [] if role == "vision" and pick["projector"] else None):
            cell[role] = None
    text, vision, big = cell.get("text"), cell.get("vision"), cell.get("big")
    stop_runs(node)
    steps = [(text, lambda: direct_basics(R, node, provider, text["name"])),
             (big, lambda: direct_tools(R, node, provider, big["name"])),
             (text, lambda: direct_image_to_text(R, node, provider, text["name"])),
             (vision, lambda: direct_vision(R, node, provider, vision)),
             (text and vision, lambda: direct_pair(R, node, provider, text, vision)),
             (text and vision, lambda: direct_contention(R, node, provider, cell))]
    for ready, step in steps:
        if not ready:
            continue
        try:
            step()
        except Exception as e:
            R.add("direct", node, provider, "exception", False, repr(e))
        stop_runs(node)


def direct_cloud(R, node):
    """Cloud media through the coordinator (lower priority than local)."""
    wav = os.environ.get("ZZR_CAMPAIGN_WAV", "")  # a short spoken "Hello from zz router"
    if wav and os.path.exists(wav):
        p = subprocess.run(["curl", "-s", "-m", "60", "-H", f"X-API-Key: {KEY}", COORD + "/v1/audio/transcriptions",
                            "-F", "model=groq/whisper-large-v3-turbo", "-F", f"file=@{wav}"], capture_output=True, text=True)
        R.add("direct", node, "cloud", "transcription with provider prefix", '"text"' in p.stdout and "zz router" in p.stdout.lower().replace("zzrouter", "zz router"), p.stdout[:200])


# ---------------------------------------------------------------- MCP proxy (ground truth)
CALLS, CALLS_LOCK = [], threading.Lock()


class Proxy(httpserver.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _go(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else None
        fwd = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "content-length", "x-campaign-scenario")}
        scen = self.headers.get("X-Campaign-Scenario", "?")
        rpc = {}
        try:
            rpc = json.loads(body) if body else {}
        except ValueError:
            pass
        req = urllib.request.Request(COORD + self.path, data=body, method=self.command, headers=fwd)
        t = time.time()
        try:
            r = urllib.request.urlopen(req, timeout=900); code, rb, rh = r.status, r.read(), r.headers
        except urllib.error.HTTPError as e:
            code, rb, rh = e.code, e.read(), e.headers
        if isinstance(rpc, dict) and rpc.get("method") == "tools/call":
            text = rb.decode("utf-8", "replace")
            payload = text
            for line in text.splitlines():  # SSE-framed reply
                if line.startswith("data:"):
                    payload = line[5:]
            is_err = None
            try:
                res = json.loads(payload)
                is_err = bool(res.get("error")) or bool((res.get("result") or {}).get("isError"))
            except ValueError:
                pass
            p = rpc.get("params") or {}
            with CALLS_LOCK:
                CALLS.append({"scenario": scen, "tool": p.get("name"), "args": p.get("arguments"), "status": code,
                              "is_error": is_err, "ms": int((time.time() - t) * 1000), "resp": payload[:600]})
        self.send_response(code)
        for k, v in rh.items():
            if k.lower() not in ("transfer-encoding", "content-length", "connection"):
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(rb))); self.end_headers(); self.wfile.write(rb)
    do_GET = do_POST = do_DELETE = _go


def start_proxy():
    srv = httpserver.ThreadingHTTPServer(("127.0.0.1", PROXY_PORT), Proxy)
    threading.Thread(target=srv.serve_forever, daemon=True).start()


def calls_for(scen):
    with CALLS_LOCK:
        return [c for c in CALLS if c["scenario"] == scen]


# ---------------------------------------------------------------- agent drivers
def run_claude_mcp(scen, prompt, wd):
    cfg = {"mcpServers": {"zzrouter": {"type": "http", "url": f"http://127.0.0.1:{PROXY_PORT}/zzrouter/v1/mcp",
           "headers": {"X-API-Key": "${ZZR_CAMPAIGN_KEY}", "X-Campaign-Scenario": scen}}}}
    p = os.path.join(wd, "mcp.json"); json.dump(cfg, open(p, "w"))
    cmd = ["claude", "-p", prompt, "--model", CLAUDE_MODEL, "--strict-mcp-config", "--mcp-config", p,
           "--allowedTools=mcp__zzrouter", "--output-format", "json"]
    return run_agent(cmd, wd, os.environ, parse_claude)


def run_codex_mcp(scen, prompt, wd):
    ans = os.path.join(wd, "answer.md")
    cmd = ["codex", "exec", "-s", "read-only", "--skip-git-repo-check",
           "-c", f'mcp_servers.zzrouter.url="http://127.0.0.1:{PROXY_PORT}/zzrouter/v1/mcp"',
           "-c", 'mcp_servers.zzrouter.env_http_headers={"X-API-Key"="ZZR_CAMPAIGN_KEY"}',
           "-c", f'mcp_servers.zzrouter.http_headers={{"X-Campaign-Scenario"="{scen}"}}',
           "-c", 'mcp_servers.zzrouter.default_tools_approval_mode="approve"',
           "-c", "mcp_servers.zzrouter.tool_timeout_sec=900", "-o", ans, prompt]
    return run_agent(cmd, wd, os.environ, lambda out: (open(ans).read() if os.path.exists(ans) else out[-3000:], None))


def parse_claude(out):
    try:
        d = json.loads(out.strip().splitlines()[-1])
        return str(d.get("result")), {"is_error": d.get("is_error"), "turns": d.get("num_turns"), "cost": d.get("total_cost_usd")}
    except Exception:
        return out[-3000:], None


def run_agent(cmd, wd, env, parse):
    t = time.time()
    try:
        p = subprocess.run(cmd, cwd=wd, env=env, capture_output=True, text=True, timeout=AGENT_TIMEOUT, stdin=subprocess.DEVNULL)
        out = p.stdout + ("\n" + p.stderr if p.returncode else "")
    except subprocess.TimeoutExpired as e:
        out = f"TIMEOUT after {AGENT_TIMEOUT}s\n{(e.stdout or b'')[-2000:]!r}"
    open(os.path.join(wd, "agent.log"), "w").write(out)
    text, meta = parse(out)
    open(os.path.join(wd, "answer.txt"), "w").write(text or "")
    return text or "", meta, int(time.time() - t)


def result_json(text):
    for line in reversed((text or "").splitlines()):
        i = line.find("RESULT:")
        if i >= 0:
            try:
                return json.loads(line[i + 7:].strip().strip("`"))
            except ValueError:
                return None
    return None



# ---------------------------------------------------------------- confusion (each item is a major bug)
ESCAPE_HATCH = ("zz_api", "zz_api_get", "zz_api_search")


def error_text(c):
    try:
        res = json.loads(c["resp"])
        if res.get("error"):
            return str(res["error"].get("message"))[:240]
        return " ".join(x.get("text", "") for x in res["result"].get("content", []))[:240]
    except Exception:
        return c["resp"][:240]


def confusion(calls, expect_error=None):
    """Every signal that an agent misread the interface, even when it recovered.
    An error carrying expect_error is the refusal the scenario asked for."""
    out, prev_err = [], None
    for c in calls:
        expected = c["is_error"] and bool(expect_error) and expect_error in error_text(c)
        if c["is_error"] and not expected:
            msg = error_text(c)
            kind = "schema" if "validating" in msg and "arguments" in msg else "unknown tool" if "unknown tool" in msg.lower() else "tool error"
            out.append({"kind": kind, "tool": c["tool"], "args": c["args"], "msg": msg})
        if c["tool"] in ESCAPE_HATCH:
            out.append({"kind": "escape hatch", "tool": c["tool"], "args": c["args"], "msg": "task tool missing or unclear"})
        if prev_err and c["tool"] == prev_err:
            out.append({"kind": "retry after error", "tool": c["tool"], "args": c["args"], "msg": ""})
        prev_err = c["tool"] if c["is_error"] and not expected else None
    return out


def analyze(out_dir):
    rows = []
    base = os.path.join(out_dir, "mcp")
    for sid in sorted(os.listdir(base)) if os.path.isdir(base) else []:
        p = os.path.join(base, sid, "calls.json")
        sp = os.path.join(base, sid, "scenario.json")
        expect = json.load(open(sp)).get("expect_error") if os.path.exists(sp) else None
        if os.path.exists(p):
            for item in confusion(json.load(open(p)), expect):
                rows.append(dict(item, run=sid))
    by = {}
    for r in rows:
        sig = (r["kind"], r["tool"], r["msg"][:120])
        by.setdefault(sig, []).append(r["run"])
    lines = ["# Agent confusion (each row is a major interface bug)", "", "| kind | tool | message | runs |", "|---|---|---|---|"]
    for (kind, tool, msg), rs in sorted(by.items(), key=lambda kv: -len(kv[1])):
        lines.append(f"| {kind} | {tool} | {msg.replace('|', '/')} | {len(rs)}: {', '.join(sorted(set(rs)))} |")
    path = os.path.join(out_dir, "confusion.md")
    open(path, "w").write("\n".join(lines) + "\n")
    json.dump(rows, open(os.path.join(out_dir, "confusion.json"), "w"), indent=1)
    return path, len(rows)



# ---------------------------------------------------------------- MCP scenarios (one set per cell)
def tail(provider):
    return (f"\nUse only the zzrouter tools and only the {provider} provider; no cloud providers. "
            "Finish with exactly one line: RESULT: <compact JSON>.")


def describe(role):
    return " ".join(w for w in (role["family"], role["size"], role.get("variant", "")) if w)


def scenarios(node, provider):
    cell = MATRIX.get((node, provider)) or {}
    out = []
    if cell.get("text"):
        out.append({
            "id": "find-run",
            "prompt": f"On node {node}, find {describe(ROLES['text'])} for the {provider} provider with the model search, pick a build "
                      f"that fits the node, download it to {node} if it is not there yet, start it with {provider}, smoke-test it "
                      f"with one chat, then stop it." + tail(provider) + " JSON keys: model, downloaded (bool), reply, stopped (bool).",
            "check": check_find_run,
        })
    vision = cell.get("vision")
    if vision and vision["projector"]:
        out.append({
            "id": "vision",
            "prompt": f"On node {node}, the model {vision['name']} ({provider}) cannot read images yet. Make it able to read images "
                      f"through zzRouter, then confirm the model catalog on {node} reports vision for it. Do not start any run."
                      + tail(provider) + " JSON keys: what_you_added, vision (bool).",
            "check": check_vision,
            "setup": lambda: vision_precondition(node, provider, vision),
            "pick": vision,
            "provider": provider,
        })
    if cell.get("text") and vision:
        param = memory_parameter(provider)
        a = cell["text"] if param else (cell.get("big") or cell["text"])
        share = f", each for this launch only with {param} set to {CONTENTION_SHARE} (do not change saved provider parameters)" if param else ""
        out.append({
            "id": "contention",
            "prompt": f"On node {node}, start {a['name']} and then {vision['name']} with {provider}, the second right after the first"
                      f"{share}. Follow both. If both serve, smoke-test each with one chat. If zzRouter refuses one, explain from its "
                      "error which run holds the memory and how much is needed. Then stop everything you started."
                      + tail(provider) + " JSON keys: both_served (bool), refused_model, holder, stopped (bool).",
            "check": check_contention,
            "expect_error": "cannot fit",
        })
    return out


def tool_calls(ctx, name, ok=None):
    cs = [c for c in ctx["calls"] if c["tool"] == name]
    if ok is not None:
        cs = [c for c in cs if (c["is_error"] is False) == ok]
    return cs


def check_inventory(ctx):
    rj = ctx["rj"] or {}
    want = set(LAB[ctx["node"]]["providers"])
    got = {p.lower() for p in rj.get("providers") or []}
    return [("RESULT line parsed", bool(rj), rj), ("local providers match", want <= got, f"want {want} got {got}")]


def check_find_run(ctx):
    rj = ctx["rj"] or {}
    return [("searched the registry", bool(tool_calls(ctx, "zz_model_search", True)), len(tool_calls(ctx, "zz_model_search"))),
            ("started a run", bool(tool_calls(ctx, "zz_run_start", True)), len(tool_calls(ctx, "zz_run_start"))),
            ("chat answered", bool(tool_calls(ctx, "zz_chat", True)), rj.get("reply")),
            ("stopped", bool(tool_calls(ctx, "zz_run_stop", True)) and rj.get("stopped") is True, rj),
            ("no runs left on node", not ctx["left"], ctx["left"])]


def check_contention(ctx):
    rj = ctx["rj"] or {}
    starts = tool_calls(ctx, "zz_run_start") + tool_calls(ctx, "zz_job")
    refused = [c for c in starts if "cannot fit" in c["resp"]]
    out = [(">= 2 launches", len(tool_calls(ctx, "zz_run_start")) >= 2, len(tool_calls(ctx, "zz_run_start"))),
           ("agent claims match zzRouter", bool(rj) and (rj.get("both_served") is True) == (not refused), rj),
           ("no runs left on node", not ctx["left"], ctx["left"])]
    if refused:
        out.append(("agent named the memory holder", bool(rj.get("holder")), rj.get("holder")))
    return out


def vision_precondition(node, provider, pick):
    """The model is on the node without its projector before the agent starts."""
    if vision_state(node, provider, pick["name"]) == "present" and not reset_projector(node, provider, pick):
        return False, "projector present and no projector_reset for this node"
    state = vision_state(node, provider, pick["name"])
    return state not in (None, "present"), f"vision={state}"


def check_vision(ctx):
    pick = ctx["scenario"]["pick"]
    state = vision_state(ctx["node"], ctx["scenario"]["provider"], pick["name"])
    out = [("catalog reports vision", state == "present", state),
           ("agent claim matches catalog", (ctx["rj"] or {}).get("vision") == (state == "present"), ctx["rj"])]
    if state == "present":
        code, d, _ = image_request(f"{pick['name']}@{ctx['node']}")
        out.append(("image answered correctly (red)", code == 200 and "red" in answer(code, d).lower(), f"{code} {answer(code, d)[:120] or problem(d)}"))
    return out


IMAGE = os.path.join(os.environ.get("TMPDIR", "/tmp"), "zzr_campaign_red_square.png")


def write_test_image():
    """A 64x64 solid red PNG, written rather than checked in."""
    import struct, zlib
    raw = b"".join(b"\x00" + b"\xff\x00\x00" * 64 for _ in range(64))
    chunk = lambda t, d: struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    with open(IMAGE, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 64, 64, 8, 2, 0, 0, 0))
                + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


def node_healthy(node):
    _, d, _ = http("GET", "/zzrouter/v1/nodes")
    return any(n["name"] == node and n.get("health_status") == "healthy" for n in d.get("data", []))


def run_scenario(R, node, label, sc, agents, only, out):
    for agent in agents:
        sid = f"{node}.{label}.{agent}"
        if only and only not in sid:
            continue
        wd = os.path.join(out, "mcp", sid); os.makedirs(wd, exist_ok=True)
        json.dump({"expect_error": sc.get("expect_error")}, open(os.path.join(wd, "scenario.json"), "w"))
        stop_runs(node)
        if "setup" in sc:
            ok, detail = sc["setup"]()
            R.add("mcp", node, f"{label}.{agent}", "precondition", ok, detail)
            if not ok:
                continue
        before = {r["id"] for r in runs(node)}
        drive = run_claude_mcp if agent == "claude" else run_codex_mcp
        text, meta, secs = drive(sid, sc["prompt"], wd)
        time.sleep(2)
        left = [f"{r['model']}({r['id']})" for r in runs(node) if r["id"] not in before]
        calls = calls_for(sid)
        json.dump(calls, open(os.path.join(wd, "calls.json"), "w"), indent=1)
        ctx = {"node": node, "calls": calls, "text": text, "rj": result_json(text), "left": left, "scenario": sc}
        failed = [f"{c['tool']}:{c['status']}" for c in calls if c["is_error"]]
        R.add("mcp", node, f"{label}.{agent}", "agent finished", bool(text) and not text.startswith(("TIMEOUT", "Error")) and not (meta or {}).get("is_error"),
              f"{secs}s calls={len(calls)} failed={failed} meta={meta}", secs * 1000)
        conf = confusion(calls, sc.get("expect_error"))
        R.add("mcp", node, f"{label}.{agent}", "no agent confusion", not conf,
              "; ".join(f"{c['kind']}:{c['tool']}:{c['msg'][:80]}" for c in conf))
        for name, ok, detail in sc["check"](ctx):
            R.add("mcp", node, f"{label}.{agent}", name, ok, detail)
        R.add("mcp", node, f"{label}.{agent}", "node still healthy", node_healthy(node), "")
        stop_runs(node)


def track_mcp(R, node, cells, agents, only, out):
    run_scenario(R, node, "inventory", {
        "prompt": f"Report node {node}'s operating system, GPU and free GPU memory, and its local providers ({', '.join(LAB[node]['providers'])}) "
                  "with the local models each can run.\nUse only the zzrouter tools. Finish with exactly one line: RESULT: <compact JSON>. "
                  "JSON keys: node, os, providers (list), model_count.",
        "check": check_inventory}, agents, only, out)
    for provider in cells:
        for sc in scenarios(node, provider):
            run_scenario(R, node, f"{provider}.{sc['id']}", sc, agents, only, out)


# ---------------------------------------------------------------- backend track
BACKEND_TASK = ("In the current working directory, create fib.py that prints the first 10 Fibonacci numbers starting 0 1, one per line. "
                "Run it with python3 and show the output.")
FIB = "0 1 1 2 3 5 8 13 21 34"


def track_backend(R, node, agents, out):
    model = BACKEND_MODELS.get(node)
    if not model:
        R.add("backend", node, "agent-on-zzrouter", "skipped: no local model large enough to drive an agent", None)
        return
    for agent in agents:
        wd = os.path.join(out, "backend", f"{node}.{agent}")
        shutil.rmtree(wd, ignore_errors=True); os.makedirs(wd)
        env = dict(os.environ)
        if agent == "claude":
            env.update({"ANTHROPIC_BASE_URL": COORD, "ANTHROPIC_API_KEY": KEY, "ANTHROPIC_DEFAULT_HAIKU_MODEL": model,
                        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"})
            cmd = ["claude", "-p", BACKEND_TASK, "--model", model, "--allowedTools=Write,Read,Bash(python3:*)", "--output-format", "json"]
            text, meta, secs = run_agent(cmd, wd, env, parse_claude)
        else:
            cmd = ["codex", "exec", "-s", "workspace-write", "--skip-git-repo-check", "-m", model,
                   "-c", 'model_provider="zz"',
                   "-c", f'model_providers.zz={{name="zzRouter",base_url="{COORD}/v1",env_key="ZZR_CAMPAIGN_KEY",wire_api="responses"}}',
                   "-o", os.path.join(wd, "answer.md"), BACKEND_TASK]
            text, meta, secs = run_agent(cmd, wd, env, lambda o: (o[-3000:], None))
        f = os.path.join(wd, "fib.py")
        got = ""
        if os.path.exists(f):
            got = subprocess.run(["python3", f], capture_output=True, text=True, timeout=30).stdout.split()
            got = " ".join(got)
        R.add("backend", node, f"{agent}@{model}", "agent wrote fib.py", os.path.exists(f), f"{secs}s meta={meta} {text[:200]}", secs * 1000)
        R.add("backend", node, f"{agent}@{model}", "fib.py output correct", got == FIB, got)



# ---------------------------------------------------------------- main
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--lab", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "lab.json"))
    ap.add_argument("--plan", action="store_true", help="print the matrix and the builds search picked, download nothing")
    ap.add_argument("--tracks", default="direct,mcp,backend")
    ap.add_argument("--nodes", default="")
    ap.add_argument("--providers", default="", help="limit the matrix to these providers")
    ap.add_argument("--agents", default="claude,codex")
    ap.add_argument("--only", default="")
    ap.add_argument("--analyze", default="", help="only rebuild confusion.md for an existing run dir")
    ap.add_argument("--out", default=os.path.join(os.environ.get("TMPDIR", "/tmp"), "zzr-campaign", time.strftime("%m%d-%H%M")))
    a = ap.parse_args()
    if a.analyze:
        print(analyze(os.path.abspath(a.analyze))); return
    if not KEY:
        sys.exit("set ZZR_CAMPAIGN_KEY to the coordinator's admin key")
    lab = json.load(open(a.lab))
    LAB.update(lab["nodes"])
    ROLES.update(lab["roles"])
    BACKEND_MODELS.update(lab.get("backend_models", {}))
    nodes = a.nodes.split(",") if a.nodes else list(LAB)
    only_providers = tuple(p for p in a.providers.split(",") if p)
    a.out = os.path.abspath(a.out)
    os.makedirs(a.out, exist_ok=True)
    open(os.path.join(a.out, "matrix.md"), "w").write(plan(nodes, only_providers) + "\n")
    if a.plan:
        return
    write_test_image()
    R = Results(a.out)
    tracks, agents = a.tracks.split(","), a.agents.split(",")
    if "mcp" in tracks:
        start_proxy()

    def per_node(node):  # nodes in parallel, everything within a node sequential
        cells = [p for (n, p) in MATRIX if n == node]
        if "direct" in tracks:
            for provider in cells:
                track_direct(R, node, provider)
            direct_catalog(R, node)
            if LAB[node].get("cloud"):
                direct_cloud(R, node)
        if "mcp" in tracks:
            track_mcp(R, node, cells, agents, a.only, a.out)
        if "backend" in tracks:
            track_backend(R, node, agents, a.out)
        stop_runs(node)
    with ThreadPoolExecutor(len(nodes)) as ex:
        for f in [ex.submit(per_node, n) for n in nodes]:
            f.result()
    print("report:", R.report())
    if "mcp" in tracks:
        print("confusion:", analyze(a.out))


if __name__ == "__main__":
    main()
