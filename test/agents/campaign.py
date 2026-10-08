#!/usr/bin/env python3
"""Local-engine test campaign for zzRouter.

Three tracks against a running coordinator:
  direct   the script itself drives /v1 on every local engine (chat, stream,
           OpenAI tools, Anthropic Messages tools, Responses, two models at
           once, an over-budget launch).
  mcp      Claude Code (Haiku) and Codex operate the cluster through the MCP
           server; a logging proxy records every tools/call as ground truth.
  backend  Claude Code and Codex use a local model behind zzRouter as their
           brain and must write and run a file (real tool calling).

Every agent confusion (a schema error, a failed or retried call, a fall
back to the zz_api escape hatch) is a finding: confusion.md lists them.

Usage: ZZR_CAMPAIGN_KEY=<admin key> campaign.py --lab lab.json  (copy lab.example.json)
       [--tracks direct,mcp,backend] [--nodes ...] [--agents claude,codex]
       [--only <scenario-substring>] [--analyze <run dir>]
Needs claude and codex on PATH. Writes report.md and confusion.md under
$TMPDIR/zzr-campaign/<time>/ unless --out says otherwise.
"""
import argparse, base64, json, os, shutil, subprocess, sys, threading, time
import http.server as httpserver, urllib.request, urllib.error
from concurrent.futures import ThreadPoolExecutor

COORD = os.environ.get("ZZR_COORDINATOR", "http://127.0.0.1:9090")
KEY = os.environ.get("ZZR_CAMPAIGN_KEY", "")
PROXY_PORT = 19390
CLAUDE_MODEL = "claude-haiku-5-5"
AGENT_TIMEOUT = 1800  # downloads of several GB happen inside a scenario

# The lab under test (--lab, filled in main()). Per node: "providers" maps
# each local provider to models to exercise; optional scenario blocks say
# what else the node can host: "budget" (a launch-time GPU budget zzRouter
# refuses before start), "vision" (a GGUF model to give a projector),
# "oversize" (a GGUF repo larger than the node's GPU), "fit" (a model to
# judge against free memory) and "cloud" (route cloud media through it).
LAB = {}
BACKEND_MODELS = {}


def providers(node):
    return LAB[node]["providers"]

WEATHER_TOOL = {"type": "function", "function": {"name": "get_weather", "description": "Get the current weather for a city",
                "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}
ANTH_TOOL = {"name": "get_weather", "description": "Get the current weather for a city",
             "input_schema": WEATHER_TOOL["function"]["parameters"]}


# ---------------------------------------------------------------- HTTP helpers
def http(method, path, body=None, headers=None, timeout=600, raw=False):
    h = {"X-API-Key": KEY}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        h["Content-Type"] = "application/json"
    h.update(headers or {})
    req = urllib.request.Request(COORD + path, data=data, method=method, headers=h)
    t = time.time()
    try:
        r = urllib.request.urlopen(req, timeout=timeout)
        code, out = r.status, r.read()
    except urllib.error.HTTPError as e:
        code, out = e.code, e.read()
    except Exception as e:  # timeouts, resets
        return 0, {"error": repr(e)}, int((time.time() - t) * 1000)
    ms = int((time.time() - t) * 1000)
    if raw:
        return code, out, ms
    try:
        return code, json.loads(out), ms
    except ValueError:
        return code, {"_raw": out[:500].decode("utf-8", "replace")}, ms


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


# ---------------------------------------------------------------- direct track
def sse_events(raw):
    for line in raw.decode("utf-8", "replace").splitlines():
        if line.startswith("data:"):
            yield line[5:].strip()


def direct_model(R, node, provider, model):
    name = f"{model}@{node}"
    sc = f"{provider}:{model}"
    # chat (cold start included)
    code, d, ms = http("POST", "/v1/chat/completions", {"model": name, "max_tokens": 32,
                       "messages": [{"role": "user", "content": "Reply with the single word: pong"}]})
    txt = (d.get("choices") or [{}])[0].get("message", {}).get("content") if code == 200 else d
    R.add("direct", node, sc, "chat (cold start)", code == 200 and bool(txt), f"{code} {txt!s:.120}", ms)
    if code != 200:
        return
    served = d.get("zzrouter", {})
    R.add("direct", node, sc, "served by expected node/provider",
          served.get("node") == node and served.get("provider") == provider, served)
    # streaming
    code, raw, ms = http("POST", "/v1/chat/completions", {"model": name, "max_tokens": 32, "stream": True,
                         "messages": [{"role": "user", "content": "Count from 1 to 5."}]}, raw=True)
    ev = list(sse_events(raw)) if code == 200 else []
    R.add("direct", node, sc, "chat stream ends [DONE]", code == 200 and ev[-1:] == ["[DONE]"] and len(ev) > 2,
          f"{code} events={len(ev)}", ms)
    # OpenAI tool calling
    code, d, ms = http("POST", "/v1/chat/completions", {"model": name, "max_tokens": 200, "tools": [WEATHER_TOOL],
                       "messages": [{"role": "user", "content": "What is the weather in Paris right now? Use the tool."}]})
    tc = ((d.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") if code == 200 else None
    if provider == "vllm" and code == 400:
        err = d.get("error") or {}
        R.add("direct", node, sc, "vllm tools refusal is classified", err.get("code") == "tool_calling_not_enabled", f"{err.get('code')} {str(err.get('message'))[:160]}", ms)
        stop_runs(node)
        lc, ld, _ = http("POST", "/zzrouter/v1/runs", {"provider": "vllm", "launch_mode": "native", "node": node, "model": model,
                         "parameters": {"enable-auto-tool-choice": "true", "tool-call-parser": "hermes"}})
        job = (ld.get("data") or {}).get("job_id") if isinstance(ld.get("data"), dict) else None
        if job:
            wait_job(job)
        code, d, ms = http("POST", "/v1/chat/completions", {"model": name, "max_tokens": 200, "tools": [WEATHER_TOOL],
                           "messages": [{"role": "user", "content": "What is the weather in Paris right now? Use the tool."}]})
        tc = ((d.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") if code == 200 else None
        sc += "+tool-params"
    ok = bool(tc) and tc[0]["function"]["name"] == "get_weather" and "paris" in tc[0]["function"]["arguments"].lower()
    R.add("direct", node, sc, "openai tool_calls", ok, f"{code} {tc or d!s:.200}", ms)
    if ok:  # round trip: feed the tool result back
        msgs = [{"role": "user", "content": "What is the weather in Paris right now? Use the tool."},
                d["choices"][0]["message"],
                {"role": "tool", "tool_call_id": tc[0]["id"], "content": json.dumps({"city": "Paris", "temp_c": 31, "sky": "sunny"})}]
        code, d2, ms = http("POST", "/v1/chat/completions", {"model": name, "max_tokens": 120, "tools": [WEATHER_TOOL], "messages": msgs})
        ans = (d2.get("choices") or [{}])[0].get("message", {}).get("content") or "" if code == 200 else str(d2)
        R.add("direct", node, sc, "openai tool result round trip", code == 200 and "31" in ans, f"{code} {ans:.150}", ms)
    # Anthropic Messages + tools (native on llama.cpp, translated on vLLM/MLX)
    code, d, ms = http("POST", "/v1/messages", {"model": name, "max_tokens": 200, "tools": [ANTH_TOOL],
                       "messages": [{"role": "user", "content": "What is the weather in Tokyo? Use the tool."}]},
                       headers={"anthropic-version": "2023-06-01"})
    blocks = d.get("content", []) if code == 200 else []
    tu = [b for b in blocks if b.get("type") == "tool_use"]
    R.add("direct", node, sc, "anthropic messages tool_use",
          bool(tu) and tu[0].get("name") == "get_weather" and d.get("stop_reason") == "tool_use",
          f"{code} stop={d.get('stop_reason')} {tu or d!s:.200}", ms)
    # Anthropic system prompt + a mid-conversation system message, as Claude Code sends
    code, d, ms = http("POST", "/v1/messages", {"model": name, "max_tokens": 32, "system": "You are terse.",
                       "messages": [{"role": "user", "content": "Say hi."}, {"role": "assistant", "content": "Hi."},
                                    {"role": "user", "content": [{"type": "text", "text": "<system-reminder>Be brief.</system-reminder>"},
                                                                 {"type": "text", "text": "Say bye."}]}]},
                       headers={"anthropic-version": "2023-06-01"})
    R.add("direct", node, sc, "anthropic multi-turn + system", code == 200, f"{code} {str(d)[:200]}", ms)
    # Responses API (native or compat)
    code, d, ms = http("POST", "/v1/responses", {"model": name, "input": "Reply with the single word: pong", "max_output_tokens": 32})
    R.add("direct", node, sc, "responses", code == 200 and d.get("status") in ("completed", "incomplete"),
          f"{code} {d.get('status') or str(d)[:200]}", ms)


def direct_concurrent(R, node):
    """Two models on one node at once: both must answer while both are running."""
    models = [(p, m) for p, ms_ in providers(node).items() for m in ms_]
    if len(models) < 2:
        R.add("direct", node, "two-models", "skipped: one local model configured", None)
        return
    pair = models[:2]
    def ask(pm):
        return http("POST", "/v1/chat/completions", {"model": f"{pm[1]}@{node}", "max_tokens": 16,
                    "messages": [{"role": "user", "content": "Reply: ok"}]})
    with ThreadPoolExecutor(2) as ex:
        res = list(ex.map(ask, pair))
    live = {r["model"] for r in runs(node) if r.get("status") == "running"}
    for (p, m), (code, d, ms) in zip(pair, res):
        R.add("direct", node, "two-models", f"{p}:{m} answers concurrently", code == 200, f"{code} {str(d)[:150]}", ms)
    R.add("direct", node, "two-models", "both running at once", all(any(m in l for l in live) for _, m in pair), sorted(live))


def direct_oom(R, node):
    budget = LAB[node].get("budget")
    if budget:
        # A fraction budget is checked against free VRAM before the process starts.
        stop_runs(node)
        code, d, ms = http("POST", "/zzrouter/v1/runs", {"provider": budget["provider"], "launch_mode": "native", "node": node,
                           "model": budget["model"], "parameters": {budget["parameter"]: "0.99"}})
        job = d.get("data", {}).get("job_id") if isinstance(d.get("data"), dict) else None
        if job:  # async launch: read the job outcome
            d = wait_job(job)
            code = 409 if "GPU budget cannot fit" in json.dumps(d) else code
        R.add("direct", node, "oom", "over-budget launch refused before start",
              code == 409 and "GPU budget" in json.dumps(d), f"{code} {json.dumps(d)[:300]}", ms)
        left = [r for r in runs(node) if r.get("provider") == budget["provider"]]
        R.add("direct", node, "oom", "no process left behind", not left, left)
    else:
        R.add("direct", node, "oom", "no launch-time budget on this node; see the mcp oversize/fit scenarios", None)


def wait_job(job, timeout=900):
    end = time.time() + timeout
    while time.time() < end:
        _, d, _ = http("GET", f"/zzrouter/v1/jobs/{job}")
        j = d.get("data") or d
        st = j.get("phase") or j.get("state") or j.get("status")
        if st in ("completed", "failed", "cancelled", "succeeded", "error", "done"):
            return d
        time.sleep(3)
    return {"timeout": job}


def track_direct(R, node):
    stop_runs(node)
    for provider, models in providers(node).items():
        for m in models:
            try:
                direct_model(R, node, provider, m)
            except Exception as e:
                R.add("direct", node, f"{provider}:{m}", "exception", False, repr(e))
    direct_concurrent(R, node)
    direct_oom(R, node)
    direct_catalog(R, node)
    if LAB[node].get("cloud"):
        direct_cloud(R, node)
    stop_runs(node)


def direct_catalog(R, node):
    """A split GGUF is one model in the catalog, not one per shard."""
    _, d, _ = http("GET", f"/zzrouter/v1/models?node={node}")
    names = [m.get("name", "") for m in d.get("data", [])]
    shards = [n for n in names if "-of-0" in n]
    R.add("direct", node, "catalog", "no shard listed as its own model", not shards, shards or f"{len(names)} models")


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


def confusion(calls):
    """Every signal that an agent misread the interface, even when it recovered."""
    out, prev_err = [], None
    for c in calls:
        if c["is_error"]:
            msg = error_text(c)
            kind = "schema" if "validating" in msg and "arguments" in msg else "unknown tool" if "unknown tool" in msg.lower() else "tool error"
            out.append({"kind": kind, "tool": c["tool"], "args": c["args"], "msg": msg})
        if c["tool"] in ESCAPE_HATCH:
            out.append({"kind": "escape hatch", "tool": c["tool"], "args": c["args"], "msg": "task tool missing or unclear"})
        if prev_err and c["tool"] == prev_err:
            out.append({"kind": "retry after error", "tool": c["tool"], "args": c["args"], "msg": ""})
        prev_err = c["tool"] if c["is_error"] else None
    return out


def analyze(out_dir):
    rows = []
    base = os.path.join(out_dir, "mcp")
    for sid in sorted(os.listdir(base)) if os.path.isdir(base) else []:
        p = os.path.join(base, sid, "calls.json")
        if os.path.exists(p):
            for item in confusion(json.load(open(p))):
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


# ---------------------------------------------------------------- MCP scenarios
TAIL = ("\nUse only the zzrouter tools. Local engines only (mlx, vllm, llamacpp); do not use ollama or cloud providers. "
        "Finish with exactly one line: RESULT: <compact JSON>.")


def scenarios(node):
    lab = LAB[node]
    models = [(p, m) for p, ms in providers(node).items() for m in ms]
    out = [{
        "id": "inventory",
        "prompt": f"Report node {node}'s operating system, GPU and free GPU memory, its local providers (mlx, vllm, llamacpp only) "
                  f"and the local models on it those providers can run.{TAIL} JSON keys: node, os, providers (list), model_count.",
        "check": check_inventory,
    }]
    if len(models) >= 2:
        (p1, m1), (p2, m2) = models[0], models[1]
        out.append({
            "id": "two-models",
            "prompt": f"On node {node}, run {m1} ({p1}) and {m2} ({p2}) at the same time. Smoke-test each with one chat, confirm "
                      f"with the runs list that both are running simultaneously, then stop both.{TAIL} JSON keys: run_ids (list), "
                      f"both_running (bool), stopped (bool).",
            "check": lambda ctx: check_lifecycle(ctx, starts=2, chats=2, stops=2),
        })
    else:
        p1, m1 = models[0]
        out.append({
            "id": "lifecycle",
            "prompt": f"On node {node}, start {m1} with {p1}, smoke-test it with one chat, check its logs for errors, then stop it."
                      f"{TAIL} JSON keys: run_id, reply, stopped (bool).",
            "check": lambda ctx: check_lifecycle(ctx, starts=1, chats=1, stops=1),
        })
    if "budget" in lab:
        b = lab["budget"]
        out.append({
            "id": "oom",
            "prompt": f"On node {node}, start {b['model']} with {b['provider']} for this launch only with the launch parameter "
                      f"{b['parameter']} set to 0.99 (do not change any saved provider parameters). If zzRouter refuses, explain "
                      "the refusal from its error details (required vs free memory, who holds the memory). Then launch it again with "
                      "a value that fits, one chat, and stop it." + TAIL + " JSON keys: refused (bool), required_mib, free_mib, "
                      "second_launch_ok (bool), stopped (bool).",
            "check": check_oom_budget,
        })
    if "vision" in lab:
        v = lab["vision"]
        out.append({
            "id": "vision",
            "prompt": f"On node {node}, the model {v['model']} ({v['provider']}) reports no vision. Make it able to read images "
                      "the supported way (its vision projector file from the same HuggingFace repo), then confirm the model catalog "
                      f"on {node} reports vision for it. Do not start any run." + TAIL + " JSON keys: projector_file, vision (bool).",
            "check": check_vision,
        })
    if "oversize" in lab:
        o = lab["oversize"]
        out.append({
            "id": "oom",
            "prompt": f"Node {node} has a {o['gpu_gb']} GB GPU. Make sure a {o['repo']} quantization between {o['min_gb']} and "
                      f"{o['max_gb']} GB is on that node (download it if it is not), then run it with {o['provider']} on that node. "
                      "Report what happens: refusal, a failed launch, or a partial/CPU offload, using the run logs as evidence. Chat "
                      "once if it runs, then stop it. Leave the file downloaded." + TAIL +
                      " JSON keys: file, size_gb, outcome (refused|failed|offloaded|full_gpu), evidence, stopped (bool).",
            "check": check_oom_oversize,
        })
    if "fit" in lab:
        f = lab["fit"]
        out.append({
            "id": "fit",
            "prompt": f"On node {node}, decide whether {f['model']} ({f['provider']}) can be started right now without "
                      "exceeding 80% of the node's currently free memory. Work out its size from the model catalog or files and the "
                      "free memory from the node inventory. Do not start it if it does not fit." + TAIL +
                      " JSON keys: model_gb, free_gb, fits (bool), started (bool).",
            "check": check_fit,
        })
    return out


def tool_calls(ctx, name, ok=None):
    cs = [c for c in ctx["calls"] if c["tool"] == name]
    if ok is not None:
        cs = [c for c in cs if (c["is_error"] is False) == ok]
    return cs


def check_inventory(ctx):
    rj = ctx["rj"] or {}
    want = set(providers(ctx["node"]))
    got = {p.lower() for p in rj.get("providers") or []}
    return [("RESULT line parsed", bool(rj), rj), ("local providers match", want <= got and not ({"ollama"} & got), f"want {want} got {got}")]


def check_lifecycle(ctx, starts, chats, stops):
    s, c, st = tool_calls(ctx, "zz_run_start", True), tool_calls(ctx, "zz_chat", True), tool_calls(ctx, "zz_run_stop", True)
    rj = ctx["rj"] or {}
    return [(f">= {starts} successful zz_run_start", len(s) >= starts, len(s)),
            (f">= {chats} successful zz_chat", len(c) >= chats, len(c)),
            (f">= {stops} successful zz_run_stop", len(st) >= stops, len(st)),
            ("agent claims match", rj.get("stopped") is True and rj.get("both_running", True) is True, rj),
            ("no runs left on node", not ctx["left"], ctx["left"])]


def check_oom_budget(ctx):
    starts = tool_calls(ctx, "zz_run_start") + tool_calls(ctx, "zz_job")
    refused = any("GPU budget cannot fit" in c["resp"] for c in starts)
    rj = ctx["rj"] or {}
    b = LAB[ctx["node"]]["budget"]
    saved = http("GET", f"/zzrouter/v1/providers/{b['provider']}/resolved?node={ctx['node']}")[1]
    return [("zzRouter refused the 0.99 launch (proxy)", refused, len(starts)),
            ("agent reported refusal with numbers", rj.get("refused") is True and bool(rj.get("required_mib")), rj),
            ("second launch + chat ok", bool(tool_calls(ctx, "zz_chat", True)) and rj.get("second_launch_ok") is True, rj.get("second_launch_ok")),
            ("saved params untouched (no zz_params_set)", not tool_calls(ctx, "zz_params_set"), "0.99" not in json.dumps(saved)),
            ("no runs left on node", not ctx["left"], ctx["left"])]


def check_vision(ctx):
    name = LAB[ctx["node"]]["vision"]["model"] + "@" + ctx["node"]
    _, d, _ = http("GET", "/v1/models")
    m = [x for x in d.get("data", []) if x["id"] == name]
    vis = bool(m and m[0].get("capabilities", {}).get("vision"))
    out = [("catalog reports vision", vis, m[0].get("capabilities") if m else None),
           ("agent claim matches catalog", (ctx["rj"] or {}).get("vision") == vis, ctx["rj"])]
    if vis:  # prove it: send an image
        out.append(vision_probe(name))
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


def vision_probe(model):
    img = IMAGE
    b64 = base64.b64encode(open(img, "rb").read()).decode()
    code, d, ms = http("POST", "/v1/chat/completions", {"model": model, "max_tokens": 40, "messages": [{"role": "user", "content": [
        {"type": "text", "text": "What color is this square? One word."},
        {"type": "image_url", "image_url": {"url": "data:image/png;base64," + b64}}]}]})
    ans = (d.get("choices") or [{}])[0].get("message", {}).get("content") or "" if code == 200 else str(d)
    return ("image answered correctly (red)", code == 200 and "red" in ans.lower(), f"{code} {ans[:120]}")


def check_oom_oversize(ctx):
    rj = ctx["rj"] or {}
    dl = tool_calls(ctx, "zz_model_download", True)
    stem = LAB[ctx["node"]]["oversize"]["repo"].split("/")[-1].lower().removesuffix("-gguf")
    present = any(stem in m["id"].lower() and m["id"].endswith("@" + ctx["node"]) for m in http("GET", "/v1/models")[1].get("data", []))
    return [("model present (downloaded or already there)", bool(dl) or present, len(dl)),
            ("run attempted", bool(tool_calls(ctx, "zz_run_start")), len(tool_calls(ctx, "zz_run_start"))),
            ("outcome reported", rj.get("outcome") in ("refused", "failed", "offloaded", "full_gpu"), rj),
            ("logs consulted", bool(tool_calls(ctx, "zz_logs")), len(tool_calls(ctx, "zz_logs"))),
            ("no runs left on node", not ctx["left"], ctx["left"]),
            ("worker still healthy", node_healthy(ctx["node"]), "")]


def check_fit(ctx):
    rj = ctx["rj"] or {}
    started = [c for c in tool_calls(ctx, "zz_run_start") if "27B" in json.dumps(c["args"])]
    consistent = rj.get("fits") is not None and (rj.get("fits") or not started)
    return [("decision reported with numbers", bool(rj.get("model_gb")) and bool(rj.get("free_gb")), rj),
            ("did not start when it does not fit", consistent, f"starts={len(started)}"),
            ("no runs left on node", not ctx["left"], ctx["left"])]


def node_healthy(node):
    _, d, _ = http("GET", "/zzrouter/v1/nodes")
    return any(n["name"] == node and n.get("health_status") == "healthy" for n in d.get("data", []))


def track_mcp(R, node, agents, only, out):
    for sc in scenarios(node):
        for agent in agents:
            sid = f"{node}.{sc['id']}.{agent}"
            if only and only not in sid:
                continue
            wd = os.path.join(out, "mcp", sid); os.makedirs(wd, exist_ok=True)
            stop_runs(node)
            before = {r["id"] for r in runs(node)}
            drive = run_claude_mcp if agent == "claude" else run_codex_mcp
            text, meta, secs = drive(sid, sc["prompt"], wd)
            time.sleep(2)
            left = [f"{r['model']}({r['id']})" for r in runs(node) if r["id"] not in before]
            calls = calls_for(sid)
            json.dump(calls, open(os.path.join(wd, "calls.json"), "w"), indent=1)
            ctx = {"node": node, "calls": calls, "text": text, "rj": result_json(text), "left": left}
            failed = [f"{c['tool']}:{c['status']}" for c in calls if c["is_error"]]
            R.add("mcp", node, f"{sc['id']}.{agent}", "agent finished", bool(text) and not text.startswith(("TIMEOUT", "Error")) and not (meta or {}).get("is_error"),
                  f"{secs}s calls={len(calls)} failed={failed} meta={meta}", secs * 1000)
            conf = confusion(calls)
            R.add("mcp", node, f"{sc['id']}.{agent}", "no agent confusion", not conf,
                  "; ".join(f"{c['kind']}:{c['tool']}:{c['msg'][:80]}" for c in conf))
            for name, ok, detail in sc["check"](ctx):
                R.add("mcp", node, f"{sc['id']}.{agent}", name, ok, detail)
            stop_runs(node)


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
    ap.add_argument("--tracks", default="direct,mcp,backend")
    ap.add_argument("--nodes", default="")
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
    BACKEND_MODELS.update(lab.get("backend_models", {}))
    if not a.nodes:
        a.nodes = ",".join(LAB)
    write_test_image()
    a.out = os.path.abspath(a.out)
    os.makedirs(a.out, exist_ok=True)
    R = Results(a.out)
    tracks, nodes, agents = a.tracks.split(","), a.nodes.split(","), a.agents.split(",")
    if "mcp" in tracks:
        start_proxy()

    def per_node(node):  # nodes in parallel, everything within a node sequential
        if "direct" in tracks:
            track_direct(R, node)
        if "mcp" in tracks:
            track_mcp(R, node, agents, a.only, a.out)
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
