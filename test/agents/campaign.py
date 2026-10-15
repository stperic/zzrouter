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
import argparse, base64, json, os, re, shlex, shutil, subprocess, sys, threading, time, uuid
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
                   and not any(mark in v["file"].lower() for mark in ("mtp", "dflash", "draft", "speculative")) and not (largest and v.get("quantization") in FULL_PRECISION)),
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
    return code in (200, 201, 202) and not failed and pick["name"] in catalog(node, provider), f"{code} jobs={len(jobs)} {json.dumps(failed or d)[:240]}"


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


def stream_completed(events):
    """[DONE] or a finish reason ends a reply; an error frame or a bare stop does not."""
    done = False
    for e in events:
        if e == "[DONE]":
            done = True
            continue
        try:
            chunk = json.loads(e)
        except ValueError:
            continue
        if chunk.get("error"):
            return False
        done = done or any(c.get("finish_reason") for c in chunk.get("choices") or [])
    return done


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
    R.add("direct", node, sc, "chat stream completes", code == 200 and len(ev) > 2 and stream_completed(ev), f"{code} events={len(ev)} last={ev[-1:]!s:.120}", ms)
    code, d, ms = http("POST", "/v1/messages", {"model": f"{name}@{node}", "max_tokens": 32, "system": "You are terse.",
                       "messages": [{"role": "user", "content": "Say hi."}, {"role": "assistant", "content": "Hi."},
                                    {"role": "user", "content": [{"type": "text", "text": "<system-reminder>Be brief.</system-reminder>"},
                                                                 {"type": "text", "text": "Say bye."}]}]},
                       headers={"anthropic-version": "2023-06-01"})
    R.add("direct", node, sc, "anthropic multi-turn + system", code == 200, f"{code} {str(d)[:200]}", ms)
    code, d, ms = http("POST", "/v1/responses", {"model": f"{name}@{node}", "input": "Reply with the single word: pong", "max_output_tokens": 32})
    R.add("direct", node, sc, "responses", code == 200 and d.get("status") in ("completed", "incomplete"), f"{code} {d.get('status') or str(d)[:200]}", ms)


WEATHER_ASK = [{"role": "user", "content": "What is the weather in Paris right now? Use the tool."}]


def openai_tool_call(model):
    """One /v1 chat with the weather tool; ok when the model calls it for Paris."""
    code, d, ms = http("POST", "/v1/chat/completions", {"model": model, "max_tokens": ANSWER_TOKENS, "tools": [WEATHER_TOOL], "messages": WEATHER_ASK})
    tc = ((d.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") if code == 200 else None
    ok = bool(tc) and tc[0]["function"]["name"] == "get_weather" and "paris" in tc[0]["function"]["arguments"].lower()
    return code, d, ms, tc, ok


def direct_tools(R, node, provider, name):
    """Tool calling on the big model: OpenAI tool_calls, the result round trip, Anthropic tool_use."""
    sc, model = f"{provider}:big", f"{name}@{node}"
    code, d, ms, tc, ok = openai_tool_call(model)
    if code == 400 and (d.get("error") or {}).get("code") == "tool_calling_not_enabled":
        R.add("direct", node, sc, "tools refusal is classified", True, problem(d), ms)
        return
    R.add("direct", node, sc, "openai tool_calls", ok, f"{code} {tc or d!s:.200}", ms)
    if ok:
        msgs = WEATHER_ASK + [d["choices"][0]["message"], {"role": "tool", "tool_call_id": tc[0]["id"],
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


def call_evidence(fields):
    if not isinstance(fields, dict):
        return {}
    evidence = {k: fields[k] for k in ("probe", "verdict", "tool_call", "dimensions", "node", "total", "has_more", "model", "deleted", "errors", "job_id", "state", "run_id", "status", "code", "detail", "deployment_id", "accepted") if k in fields}
    if isinstance(fields.get("jobs"), list):
        evidence["jobs"] = [{k: row[k] for k in ("job_id", "node", "kind", "state") if k in row} for row in fields["jobs"][:2] if isinstance(row, dict)]
    if "nodes" in fields:
        evidence["nodes"] = []  # Marker presence blocks refusal inference even when its contents cannot be captured.
        if isinstance(fields["nodes"], list):
            evidence["nodes"] = [{k: row[k] for k in ("node", "job_id", "status", "error") if k in row} for row in fields["nodes"][:4] if isinstance(row, dict)]
    if isinstance(fields.get("models"), list):
        evidence["models"] = []
        for entry in fields["models"][:2]:
            if not isinstance(entry, dict):
                continue
            item = {k: entry[k] for k in ("name", "source_id", "source_repo", "node", "provider", "show_unavailable", "variant_of", "is_cloud", "details") if k in entry}
            show = entry.get("show")
            if isinstance(show, dict):
                item["show"] = {k: show[k] for k in ("name", "node", "assigned_app", "source_id", "source_repo", "model_info", "template_present", "capabilities") if k in show}
                item["show"]["has_parameters"] = "parameters" in show
            if len(json.dumps(item)) <= 16384:
                evidence["models"].append(item)
            else:
                evidence["capture_overflow"] = True
    return evidence if len(json.dumps(evidence)) <= 32768 else {"capture_overflow": True}


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
        params = rpc.get("params") or {}
        args = params.get("arguments") or {}
        blocked = rpc.get("method") == "tools/call" and (params.get("name") == "zz_model_delete" and not delete_ownership(calls_for(scen), args) or params.get("name") == "zz_api" and args.get("method", "").upper() == "DELETE")
        t = time.time()
        if blocked:
            code, rh = 200, {"Content-Type": "application/json"}
            result = {"isError": True, "content": [{"type": "text", "text": "Campaign ownership guard: deletion requires node-wide exact source_id absence, a completed zz_model_download with that same exact source_id as model plus the selected node and provider, and a post-download zz_models read with model equal to the physical name or source_id, node selected, total=1 and has_more=false. Wildcards do not prove exact identity."}], "structuredContent": {"status": 409, "code": "campaign_ownership_guard"}}
            protocol = self.headers.get("MCP-Protocol-Version") or (params.get("_meta") or {}).get("io.modelcontextprotocol/protocolVersion", "")
            if isinstance(protocol, str) and protocol >= "2026-07-28":
                result["resultType"] = "complete"
            rb = json.dumps({"jsonrpc": "2.0", "id": rpc.get("id"), "result": result}).encode()
        else:
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
            is_err, evidence = None, {}
            try:
                res = json.loads(payload)
                is_err = bool(res.get("error")) or bool((res.get("result") or {}).get("isError"))
                fields = (res.get("result") or {}).get("structuredContent") or {}
                evidence = call_evidence(fields)
            except ValueError:
                pass
            p = rpc.get("params") or {}
            with CALLS_LOCK:
                CALLS.append({"scenario": scen, "tool": p.get("name"), "args": p.get("arguments"), "status": code,
                              "is_error": is_err, "ms": int((time.time() - t) * 1000), "resp": payload[:600], "evidence": evidence})
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
    return (f"\nUse only the zzrouter tools and the {provider} inference backend; no cloud providers. Model search's provider selects a registry such as huggingface or ollama. "
            "A stop reply with status=stopped confirms its run_id was stopped; each evicted_runs entry confirms its listed run_id was stopped. Verify those IDs are absent from the node's zz_runs collection; do not query or stop those IDs, or chat their models to verify absence. "
            "Finish with exactly one line: RESULT: <compact JSON>.")


def describe(role):
    return " ".join(w for w in (role["family"], role["size"], role.get("variant", "")) if w)



def prepare_probe(node, provider, mode, sc):
    text = (MATRIX.get((node, provider)) or {}).get("text")
    if not text:
        return False, "no safe text fixture for provider capability discovery"
    if not text.get("present"):
        ok, detail = deploy(node, provider, text)
        if not ok:
            return False, detail
    item = catalog(node, provider).get(text["name"]) or {}
    wires = (item.get("details") or {}).get("wire_endpoints")
    if mode == "embed" and isinstance(wires, list) and "embeddings" not in wires:
        sc.update(pick=text, expected="not_supported", refusal_status=404, refusal_code="backend_error", expect_error="Not Found", capability_absent=True)
        code, d, _ = chat(f"{text['name']}@{node}", "Say OK", 2)
        sc["prompt"] = f"The ready text fixture {text['name']} on {node} with {provider} explicitly does not advertise embeddings. Probe it once with zz_chat probe=embed and report the exact refusal. Do not launch or change settings." + tail(provider) + " JSON keys: verdict (the exact request-local verdict, null if absent), dimensions (null), status, code."
        return code == 200, f"ready text={code}; advertised embeddings absent"
    pick = sc["pick"]
    ok, detail = deploy(node, provider, pick, ["vision"] if mode == "vision" and pick.get("projector") else None)
    item = catalog(node, provider).get(pick["name"]) or {}
    if not ok or item.get("source_id") != pick["repo"]:
        return False, f"selected physical fixture not ready: {detail}"
    endpoint = "embeddings" if mode == "embed" else "chat"
    output = "verdict, dimensions (positive number)" if mode == "embed" else "verdict"
    sc["prompt"] = f"Start the already downloaded physical model {pick['name']} on node {node} with {provider} and endpoint={endpoint}. Follow its launch job to done before probing only with zz_chat probe={mode}. Require verdict=verified. Stop only the run you started by its run_id." + tail(provider) + f" JSON keys: {output}."
    return True, detail


def scenarios(node, provider):
    cell = MATRIX.get((node, provider)) or {}
    out = [{
        "id": "recent-jobs",
        "prompt": f"List the two most recent zzRouter jobs owned by node {node}, without supplying a job id. "
                  "Report the collection total and whether another page exists. Do not start or cancel anything."
                  + tail(provider) + " JSON keys: total, has_more.",
        "check": check_recent_jobs,
    }, {
        "id": "model-detail",
        "provider": provider,
        "prompt": f"On node {node}, find a small {describe(ROLES['text'])} model supported by {provider}. If it is absent, "
                  "download it through the supported zzRouter tools and follow its job. Query zz_models with the exact "
                  f"catalog name or source_id, provider {provider} and node {node}. Report the source-reported context "
                  "map, template presence, capabilities and whether parameters were reported, using only show.model_info, "
                  "show.template_present, show.capabilities and presence of show.parameters. Never use details.wire_endpoints "
                  "or details.features for these source fields. Use null for missing "
                  "metadata; do not infer it or describe metadata as serving verification. If detail is explicitly "
                  "unsupported, report its code and null metadata. Do not start a run."
                  " Use only inventory, model discovery/catalog/download and job tools. Set has_parameters to false "
                  "when detail exists without parameters, or null when detail is unsupported."
                  + tail(provider) + " JSON keys: model, source_id, context, template_present, capabilities, has_parameters, detail_error.",
        "check": check_model_detail,
    }, {
        "id": "model-delete",
        "provider": provider,
        "prompt": f"On node {node}, choose a tiny disposable physical model supported by {provider}. First query zz_models "
                  f"with its full exact registry model/source_id (include the tag for tagged registries) and node {node}, OMIT provider to check every provider, and prove total=0 and has_more=false. "
                  "Choose another model if anything is present. Never delete a pre-existing model or any pinned model. "
                  f"Download that absent model through zz_model_download with node={node} and provider={provider}, using the same exact full registry identifier, including its tag, follow its node job to done, then query zz_models "
                  "with model equal to the returned physical name or source_id, node and provider selected. Require total=1, has_more=false and the matching physical source_id; do not use a wildcard. Do not start a run. Delete only that new physical name with "
                  "zz_model_delete, its provider/node/source_id selectors and a fresh idempotency key. Finally query "
                  "zz_models with the exact physical name and node, OMIT provider, and prove total=0 and has_more=false. Stop if any precondition fails; "
                  "do not use zz_api, shell, SSH or a native API for deletion."
                  + tail(provider) + " JSON keys: model, source_id, deleted (number), absent (bool).",
        "check": check_model_delete,
    }]
    if cell.get("text"):
        out.append({
            "id": "find-run",
            "prompt": f"On node {node}, find {describe(ROLES['text'])} for the {provider} provider (the model search, unless it is already on the node), pick a build "
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
    for mode, pick in (("embed", cell.get("embed")), ("vision", vision)):
        if pick:
            sc = {"id": f"probe-{mode}", "pick": pick, "provider": provider, "expected": "verified",
                  "check": lambda ctx, mode=mode: check_capability_probe(ctx, mode)}
            sc["setup"] = lambda sc=sc, mode=mode: prepare_probe(node, provider, mode, sc)
            out.append(sc)
    big = cell.get("big")
    if big:
        out.append({
            "id": "tool-calling",
            "prompt": f"On node {node}, make {big['name']} serve tool calls with {provider}: start it, prove tool calling with "
                      "zzRouter's tool-call probe, and if the probe or a refusal says the engine needs tool-calling settings, "
                      "set them for this model only, restart the run and probe again until it is verified. Then stop it."
                      + tail(provider) + " JSON keys: verified (bool), settings_changed (object or null), stopped (bool).",
            "check": check_tool_calling,
            "expect_error": "tool_calling_not_enabled",
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
                      + tail(provider) + " JSON keys: both_served (bool: both held at once; false if one was evicted or refused), refused_model, holder, stopped (bool).",
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
    got = {(p if isinstance(p, str) else p.get("name") or p.get("provider") or "").lower() for p in rj.get("providers") or []}
    return [("RESULT line parsed", bool(rj), rj), ("local providers match", want <= got, f"want {want} got {got}")]


def check_find_run(ctx):
    rj = ctx["rj"] or {}
    # A model already on the node is found in its catalog; searching the registry for it would be wasted work.
    found = tool_calls(ctx, "zz_model_search", True) or (rj.get("downloaded") is False and tool_calls(ctx, "zz_models", True))
    return [("found the model (registry, or the catalog when already local)", bool(found), len(tool_calls(ctx, "zz_model_search"))),
            ("started a run", bool(tool_calls(ctx, "zz_run_start", True)), len(tool_calls(ctx, "zz_run_start"))),
            ("chat answered", bool(tool_calls(ctx, "zz_chat", True)), rj.get("reply")),
            ("stopped", bool(tool_calls(ctx, "zz_run_stop", True)) and rj.get("stopped") is True, rj),
            ("no runs left on node", not ctx["left"], ctx["left"])]


def check_recent_jobs(ctx):
    calls = [c for c in tool_calls(ctx, "zz_job", True) if not (c["args"] or {}).get("job_id")]
    fields = (calls[-1].get("evidence") or {}) if calls else {}
    args = (calls[-1].get("args") or {}) if calls else {}
    rj = ctx["rj"] or {}
    jobs = fields.get("jobs")
    total = fields.get("total")
    page = isinstance(jobs, list) and type(total) is int and total >= 0 and len(jobs) == min(2, total)
    owned = page and all(j.get("node") == ctx["node"] and j.get("job_id") and j.get("kind") and j.get("state") for j in jobs)
    unique = owned and len({j["job_id"] for j in jobs}) == len(jobs)
    verified = unique
    if unique:
        for job in jobs:
            code, current, _ = http("GET", f"/zzrouter/v1/jobs/{job['job_id']}?" + urlencode({"node": ctx["node"]}))
            current = current.get("data") or current
            verified = verified and code == 200 and all(current.get(k) == job.get(k) for k in ("job_id", "node", "kind"))
    return [("complete recent job page", verified and fields.get("has_more") is (total > 2), jobs),
            ("used node-selected collection", bool(calls) and args.get("node") == ctx["node"] and fields.get("node") == ctx["node"] and args.get("limit") == 2 and args.get("offset", 0) == 0, args),
            ("collection metadata present", type(fields.get("total")) is int and isinstance(fields.get("has_more"), bool), fields),
            ("agent claim matches collection", bool(fields) and rj.get("total") == fields.get("total") and rj.get("has_more") == fields.get("has_more"), rj)]


def check_model_detail(ctx):
    rj = ctx["rj"] or {}
    provider, node = ctx["scenario"]["provider"], ctx["node"]
    call = {}
    for candidate in reversed(tool_calls(ctx, "zz_models", True)):
        args, fields = candidate.get("args") or {}, candidate.get("evidence") or {}
        entries = fields.get("models") or []
        selector = str(args.get("model") or "").casefold()
        addressed = selector == str(rj.get("model") or "").casefold() or (selector == str(rj.get("source_id") or "").casefold() and len(entries) <= 1)
        if selector and addressed and args.get("provider") == provider and args.get("node") == node:
            call = candidate
            break
    args, fields = call.get("args") or {}, call.get("evidence") or {}
    entries = fields.get("models") or []
    entry = entries[0] if len(entries) == 1 else {}
    exact = str(args.get("model") or "").casefold() in {str(entry.get(k) or "").casefold() for k in ("name", "source_id")}
    selected = entry.get("provider") == provider and entry.get("node") == node
    show, refusal = entry.get("show"), entry.get("show_unavailable") or {}
    unsupported = refusal.get("status") == 501 and refusal.get("code") == "model_detail_unsupported"
    identity = isinstance(show, dict) and show.get("name") == entry.get("name") and show.get("assigned_app") == provider and show.get("node") == node and show.get("source_id") == entry.get("source_id") and show.get("source_repo") == entry.get("source_repo")
    metadata = show or {}
    claims = all(k in rj and rj[k] == metadata.get(v) for k, v in (("context", "model_info"), ("template_present", "template_present"), ("capabilities", "capabilities"), ("has_parameters", "has_parameters")))
    return [("exact selected catalog entry", bool(args.get("model")) and exact and selected and args.get("provider") == provider and args.get("node") == node and fields.get("total") == 1 and fields.get("has_more") is False, args),
            ("detail carries selected identity or typed unsupported", identity or unsupported, entry),
            ("agent metadata matches source evidence", claims and rj.get("model") == entry.get("name") and rj.get("source_id") == entry.get("source_id") and rj.get("detail_error") == (refusal.get("code") if unsupported else None), rj),
            ("bounded evidence captured", not fields.get("capture_overflow"), fields.get("capture_overflow")),
            ("metadata scenario used no serving or lifecycle calls", not any(c["tool"] in ("zz_run_start", "zz_run_restart", "zz_run_stop", "zz_chat", "zz_api") for c in ctx["calls"]), [c["tool"] for c in ctx["calls"]]),
            ("no owned runs left", not ctx["left"], ctx["left"])]



def delete_ownership(calls, args):
    """Prove a physical identity was absent node-wide and downloaded by this scenario."""
    node, provider, name, source = (args.get(k) for k in ("node", "provider", "model", "source_id"))
    if not all((node, provider, name, source)):
        return False
    for pi in range(len(calls) - 1, -1, -1):
        present = calls[pi]
        a, e = present.get("args") or {}, present.get("evidence") or {}
        entries = e.get("models") or []
        if present["tool"] != "zz_models" or present.get("is_error") is not False or a.get("node") != node or a.get("provider") not in (None, "", provider) or a.get("model") not in (name, source) or e.get("total") != 1 or e.get("has_more") is not False or len(entries) != 1:
            continue
        m = entries[0]
        if any(m.get(k) != v for k, v in (("name", name), ("source_id", source), ("node", node), ("provider", provider))) or m.get("variant_of") or m.get("is_cloud"):
            continue
        for di, download in enumerate(calls[:pi]):
            da, de = download.get("args") or {}, download.get("evidence") or {}
            if download["tool"] != "zz_model_download" or download.get("is_error") is not False or da.get("node") != node or da.get("provider") != provider or da.get("model") != source:
                continue
            if da.get("file") and gguf_name(da["file"]).casefold() != name.casefold():
                continue
            absent = any(c["tool"] == "zz_models" and c.get("is_error") is False and (c.get("args") or {}).get("model") == source and (c.get("args") or {}).get("node") == node and not (c.get("args") or {}).get("provider") and (c.get("evidence") or {}).get("total") == 0 and (c.get("evidence") or {}).get("has_more") is False for c in calls[:di])
            jobs = {r.get("job_id") for r in de.get("nodes") or [] if r.get("node") == node and r.get("job_id")}
            done = any(c["tool"] == "zz_job" and c.get("is_error") is False and (c.get("args") or {}).get("job_id") in jobs and (c.get("args") or {}).get("node") in (None, "", node) and (c.get("evidence") or {}).get("state") == "done" for c in calls[di + 1:pi])
            if absent and jobs and done:
                return True
    return False


def cleanup_disposable(calls, node, provider):
    """Reconcile every captured download, retaining any unproven physical identity."""
    issues = []
    for download in [c for c in calls if c["tool"] == "zz_model_download"]:
        try:
            ok, detail = cleanup_download(download, calls, node, provider)
        except Exception as exc:
            ok, detail = False, f"cleanup interrupted ({type(exc).__name__}); resources retained"
        if not ok:
            issues.append(detail)
    return not issues, "; ".join(issues) if issues else "no disposable resources remain"


def cleanup_download(download, calls, node, provider):
    args, evidence = download.get("args") or {}, download.get("evidence") or {}
    if args.get("node") != node or args.get("provider") != provider:
        return False, "unselected download retained for inspection"
    # The deployment owner's typed400 refusal precedes creation and worker dispatch.
    if (download.get("is_error") is True and evidence.get("status") == 400 and evidence.get("code") == "bad_request"
            and not any(key in evidence for key in ("deployment_id", "accepted", "job_id", "nodes"))
            and not evidence.get("capture_overflow")):
        return True, "request refused before deployment acceptance"
    trace = [c for c in calls if c["tool"] != "zz_model_download" or c is download]
    jobs = [j["job_id"] for j in evidence.get("nodes") or [] if j.get("node") == node and j.get("job_id")]
    if not jobs:
        return False, "download has no owned job; retained for inspection"
    for job in jobs:
        path = f"/zzrouter/v1/jobs/{job}?" + urlencode({"node": node})
        code, state, _ = http("GET", path)
        if code != 200:
            return False, f"owned job unavailable: {job}"
        if job_phase(state) not in JOB_TERMINAL:
            code, _, _ = http("DELETE", f"/zzrouter/v1/jobs/{job}?" + urlencode({"node": node}), {})
            if code != 202:
                return False, f"owned job cancellation refused: {job}"
            end = time.time() + 60
            while time.time() < end and job_phase(state) not in JOB_TERMINAL:
                time.sleep(1); code, state, _ = http("GET", path)
            if code != 200 or job_phase(state) not in JOB_TERMINAL:
                return False, f"owned job did not terminate: {job}"
        if job_phase(state) not in JOB_SUCCESS:
            return False, f"partial/failed download retained for inspection: {job}"
        trace.append({"tool": "zz_job", "is_error": False, "args": {"node": node, "job_id": job}, "evidence": {"state": "done"}})
    if download.get("is_error") is not False:
        return False, "uncertain download retained for inspection after reconciling its jobs"
    source = args.get("model")
    code, inventory, _ = http("GET", "/zzrouter/v1/models?" + urlencode({"node": node, "model": source, "refresh": "true", "limit": 1000}))
    if code != 200 or inventory.get("has_more"):
        return False, "owned source inventory unavailable; retained"
    owned = any(delete_ownership(trace, {"node": node, "provider": provider, "model": m.get("name"), "source_id": source})
                for c in trace if c["tool"] == "zz_models" for m in (c.get("evidence") or {}).get("models") or [])
    for raw in inventory.get("data") or []:
        if raw.get("source_id") != source or raw.get("assigned_app") != provider:
            continue
        m = {**raw, "provider": raw.get("assigned_app")}
        selected = {"node": node, "provider": provider, "model": m["name"], "source_id": source}
        trace.append({"tool": "zz_models", "is_error": False, "args": {"node": node, "provider": provider, "model": m["name"]}, "evidence": {"total": 1, "has_more": False, "models": [m]}})
        if not delete_ownership(trace, selected):
            return False, "physical ownership unproven; retained"
        owned = True
        code, result, _ = http("DELETE", "/zzrouter/v1/models", selected)
        if code != 200 or (result.get("data") or {}).get("errors") or (result.get("data") or {}).get("deleted") != 1:
            return False, f"owned physical cleanup refused: {code}"
    if not owned:
        return False, "successful download has no proven physical identity; retained for inspection"
    code, inventory, _ = http("GET", "/zzrouter/v1/models?" + urlencode({"node": node, "model": source, "refresh": "true", "limit": 1000}))
    if code != 200 or inventory.get("total") != 0:
        return False, "owned physical source remains after cleanup"
    return True, "owned source is absent"


def check_model_delete(ctx):
    calls, rj = ctx["calls"], ctx["rj"] or {}
    attempts = [(i, c) for i, c in enumerate(calls) if c["tool"] == "zz_model_delete"]
    index, deletion = attempts[-1] if attempts else (-1, {})
    args, effects = deletion.get("args") or {}, deletion.get("evidence") or {}
    owned = index >= 0 and delete_ownership(calls[:index], args)
    selected = args.get("node") == ctx["node"] and args.get("provider") == ctx["scenario"]["provider"]
    absent = any(c["tool"] == "zz_models" and c.get("is_error") is False and (c.get("args") or {}).get("model") == args.get("model") and (c.get("args") or {}).get("node") == args.get("node") and not (c.get("args") or {}).get("provider") and (c.get("evidence") or {}).get("total") == 0 and (c.get("evidence") or {}).get("has_more") is False for c in calls[index + 1:]) if index >= 0 else False
    return [("proven new physical identity on selected node", owned and selected, args),
            ("one exact deletion completed", len(attempts) == 1 and deletion.get("is_error") is False and bool(args.get("idempotency_key")) and effects.get("model") == args.get("model") and effects.get("node") == args.get("node") and effects.get("deleted") == 1 and not effects.get("errors"), effects),
            ("node-wide post-delete absence", absent and rj.get("absent") is True, rj),
            ("agent claims match effects", rj.get("model") == args.get("model") and rj.get("source_id") == args.get("source_id") and rj.get("deleted") == effects.get("deleted"), rj),
            ("no escape or owned runs", not any(c["tool"] in (*ESCAPE_HATCH, "zz_run_start", "zz_run_restart", "zz_chat") for c in calls) and not ctx["left"], ctx["left"])]


def check_capability_probe(ctx, mode):
    calls = [c for c in tool_calls(ctx, "zz_chat") if (c["args"] or {}).get("probe") == mode]
    fields = (calls[-1].get("evidence") or {}) if calls else {}
    rj = ctx["rj"] or {}
    verdict = fields.get("verdict")
    pick, node, provider = ctx["scenario"]["pick"]["name"], ctx["node"], ctx["scenario"]["provider"]
    args = (calls[-1].get("args") or {}) if calls else {}
    addressed = ((args.get("model") == pick and args.get("node") == node) or
                 (args.get("model") == f"{pick}@{node}" and not args.get("node")))
    starts = [c for c in tool_calls(ctx, "zz_run_start", True) if calls and ctx["calls"].index(c) < ctx["calls"].index(calls[-1]) and (c["args"] or {}).get("model") == pick
              and (c["args"] or {}).get("node") == node and (c["args"] or {}).get("provider") == provider
              and (c["args"] or {}).get("endpoint", "chat") == ("embeddings" if mode == "embed" else "chat")]
    expected = ctx["scenario"].get("expected", "verified")
    positive = expected == "verified"
    typed = fields.get("status") == ctx["scenario"].get("refusal_status", 400) and fields.get("code") == ctx["scenario"].get("refusal_code", "unsupported_input")
    outcome = calls and ((not calls[-1].get("is_error") and fields.get("probe") == mode and verdict == "verified") if positive else (calls[-1].get("is_error") and typed and verdict in (None, "not_supported")))
    completed = False
    for start in starts:
        start_evidence = start.get("evidence") or {}
        job_id, run_id = start_evidence.get("job_id"), start_evidence.get("run_id")
        ready = not job_id and start_evidence.get("status") in ("ready", "running")
        if job_id:
            ready = any(j["tool"] == "zz_job" and j.get("is_error") is False
                        and (j.get("args") or {}).get("job_id") == job_id
                        and (j.get("args") or {}).get("node") in (None, "", node)
                        and (j.get("evidence") or {}).get("job_id") == job_id
                        and (j.get("evidence") or {}).get("state") == "done"
                        for j in ctx["calls"][ctx["calls"].index(start) + 1:ctx["calls"].index(calls[-1])])
        stopped = run_id and any(stop["tool"] == "zz_run_stop" and stop.get("is_error") is False
                                 and (stop.get("args") or {}).get("run_id") == run_id
                                 for stop in ctx["calls"][ctx["calls"].index(calls[-1]) + 1:])
        if ready and stopped:
            completed = True
            break
    out = [("ready launch followed and owned run stopped", completed if positive else ctx["scenario"].get("capability_absent", False), starts),
           ("used addressed capability probe", bool(calls) and addressed, args),
           ("started with selected provider" if positive else "no incompatible launch", bool(starts) if positive else not tool_calls(ctx, "zz_run_start"), provider),
           ("no owned runs left", not ctx["left"], ctx["left"]),
           ("expected capability evidence returned", bool(outcome), fields),
           ("agent claim matches evidence", bool(fields) and rj.get("verdict") == verdict and (positive or (rj.get("status") == fields.get("status") and rj.get("code") == fields.get("code") and rj.get("dimensions") is None)), rj)]
    if mode == "embed" and verdict == "verified":
        out.append(("dimensions match", type(fields.get("dimensions")) is int and fields["dimensions"] > 0 and rj.get("dimensions") == fields.get("dimensions"), fields))
    return out


def tool_probes(ctx):
    """zz_chat probe=tools calls, and whether one verified tool calling."""
    probes = [c for c in tool_calls(ctx, "zz_chat", True) if (c["args"] or {}).get("probe") == "tools"]
    verified = any((c.get("evidence") or {}).get("tool_call") == "verified" and (c.get("evidence") or {}).get("verdict") == "verified" for c in probes)
    return probes, verified


def check_tool_calling(ctx):
    rj = ctx["rj"] or {}
    probes, verified = tool_probes(ctx)
    return [("probed with tools", bool(probes), len(probes)),
            ("probe verified tool calling", verified, [c["resp"][:160] for c in probes][-1:]),
            ("agent claims match the probe", rj.get("verified") is verified, rj),
            ("no runs left on node", not ctx["left"], ctx["left"])]


def check_tool_endpoint(ctx):
    """Acceptance is the harness's own /v1 tool call to the model the agent left serving, not the agent's word."""
    rj = ctx["rj"] or {}
    probes, verified = tool_probes(ctx)
    model = rj.get("model") or ""
    code, d, ms, tc, ok = openai_tool_call(model) if model else (None, {}, 0, None, False)
    return [("probed with tools", bool(probes), len(probes)),
            ("probe verified tool calling", verified, [c["resp"][:160] for c in probes][-1:]),
            ("agent claims match the probe", rj.get("verified") is verified, rj),
            ("left a run serving", bool(ctx["left"]), ctx["left"]),
            ("coordinator /v1 tool call to the reported model", ok, f"{model!r} {code} {ms}ms {tc or d!s:.200}")]


def check_contention(ctx):
    rj = ctx["rj"] or {}
    starts = tool_calls(ctx, "zz_run_start") + tool_calls(ctx, "zz_job")
    refused = [c for c in starts if "cannot fit" in c["resp"]]
    # With pressure eviction on, the second launch stops the idle first run instead of being refused.
    evicted = [c for c in starts + tool_calls(ctx, "zz_runs") if '"evicted_runs"' in c["resp"]]
    out = [(">= 2 launches", len(tool_calls(ctx, "zz_run_start")) >= 2, len(tool_calls(ctx, "zz_run_start"))),
           ("agent claims match zzRouter", bool(rj) and (rj.get("both_served") is True) == (not refused and not evicted), rj),
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
        stop_runs(node)
        if "setup" in sc:
            ok, detail = sc["setup"]()
            R.add("mcp", node, f"{label}.{agent}", "precondition", ok, detail)
            if not ok:
                continue
        namespace = uuid.uuid4().hex
        with open(os.path.join(wd, "scenario.json"), "w") as context:
            json.dump({"expect_error": sc.get("expect_error"), "idempotency_namespace": namespace}, context)
        prompt = sc["prompt"] + f"\nFor tools whose input schema declares idempotency_key, prefix each new key with {namespace}: and a distinct suffix per operation. Reuse a key only to retry that same operation."
        before = {r["id"] for r in runs(node)}
        drive = run_claude_mcp if agent == "claude" else run_codex_mcp
        try:
            text, meta, secs = drive(sid, prompt, wd)
            time.sleep(2)
            left = [f"{r['model']}({r['id']})" for r in runs(node) if r["id"] not in before]
            calls = calls_for(sid)
            with open(os.path.join(wd, "calls.json"), "w") as stream:
                json.dump(calls, stream, indent=1)
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
        finally:
            stop_runs(node)
            if sc.get("id") == "model-delete":
                ok, detail = cleanup_disposable(calls_for(sid), node, sc["provider"])
                R.add("mcp", node, f"{label}.{agent}", "disposable cleanup", ok, detail)


def track_mcp(R, node, cells, agents, only, out):
    run_scenario(R, node, "inventory", {
        "prompt": f"Report node {node}'s operating system, GPU and free GPU memory, and its local providers ({', '.join(LAB[node]['providers'])}) "
                  "with the local models each can run.\nUse only the zzrouter tools. Finish with exactly one line: RESULT: <compact JSON>. "
                  "JSON keys: node, os, providers (list), model_count.",
        "check": check_inventory}, agents, only, out)
    if LAB[node].get("tool_endpoint") and ROLES.get("big"):
        run_scenario(R, node, "tool-endpoint", {
            "prompt": f"On node {node}, serve {describe(ROLES['big'])} for tool calling. Choose the engine (one of this node's local "
                      f"providers: {', '.join(LAB[node]['providers'])}) and the precision (build) that give the most faithful tool calls "
                      "on this node's GPU, get what it needs onto the node, and start it. Prove tool calling with zzRouter's tool-call probe; "
                      "if the probe or a refusal says the engine needs tool-calling settings, set them for this model only, restart and probe "
                      "again. Leave the verified model serving: a client will call it through the coordinator's OpenAI-compatible /v1 API.\n"
                      "Use only the zzrouter tools and this node's local providers; no cloud providers. Finish with exactly one line: "
                      "RESULT: <compact JSON>. JSON keys: model (the exact model name a /v1 client sends), provider, build, verified (bool), "
                      "why (one sentence on the engine and precision choice).",
            "check": check_tool_endpoint,
            "expect_error": "tool_calling_not_enabled"}, agents, only, out)
    for provider in cells:
        R.add("mcp", node, f"{provider}.probe-embed", "embedding role configured", bool((MATRIX.get((node, provider)) or {}).get("embed")),
              "Configure an embed role in the lab inventory for this node/provider; missing embedding coverage is a failure.")
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
