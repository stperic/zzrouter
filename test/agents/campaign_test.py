import copy
import unittest
import campaign as c


def call(tool, args, evidence, error=False):
    return {"tool": tool, "args": args, "evidence": evidence, "is_error": error, "resp": ""}


def passed(rows):
    return all(row[1] for row in rows)


class CampaignEvidenceTest(unittest.TestCase):
    def test_recent_jobs_requires_real_node_owned_page(self):
        fields = {"node": "n", "total": 2, "has_more": False, "jobs": [
            {"job_id": "j1", "node": "n", "kind": "run", "state": "done"},
            {"job_id": "j2", "node": "n", "kind": "run", "state": "running"}]}
        def ctx(f):
            return {"node": "n", "rj": {"total": f["total"], "has_more": f["has_more"]},
                    "calls": [call("zz_job", {"node": "n", "limit": 2}, c.call_evidence(f))]}
        from unittest.mock import patch
        def source(method, path):
            job = next((j for j in fields["jobs"] if f"/{j['job_id']}?" in path), {})
            return 200, {"data": job}, 0
        with patch.object(c, "http", side_effect=source):
            self.assertTrue(passed(c.check_recent_jobs(ctx(fields))))
            for mutation in (lambda f: f.pop("jobs"), lambda f: f["jobs"].pop(),
                             lambda f: f["jobs"][0].update(node="other"),
                             lambda f: f["jobs"][1].update(job_id="j1")):
                bad = copy.deepcopy(fields); mutation(bad)
                self.assertFalse(passed(c.check_recent_jobs(ctx(bad))))
            self.assertTrue(passed(c.check_recent_jobs(ctx({"node": "n", "total": 0, "has_more": False, "jobs": []}))))

    def test_recent_jobs_verifies_independent_public_source(self):
        from unittest.mock import patch
        job = {"job_id": "j", "node": "n", "kind": "run", "state": "done"}
        ctx = {"node": "n", "rj": {"total": 1, "has_more": False},
               "calls": [call("zz_job", {"node": "n", "limit": 2}, {"node": "n", "total": 1, "has_more": False, "jobs": [job]})]}
        for code, raw in [(404, {}), (200, {**job, "job_id": "wrong"}), (200, {**job, "node": "wrong"}), (200, {**job, "kind": "download"})]:
            with patch.object(c, "http", return_value=(code, {"data": raw}, 0)) as native:
                self.assertFalse(passed(c.check_recent_jobs(ctx)))
                native.assert_called_once_with("GET", "/zzrouter/v1/jobs/j?node=n")

    def test_positive_probe_requires_verified_endpoint_and_dimensions(self):
        starts = call("zz_run_start", {"model": "m", "provider": "p", "node": "n", "endpoint": "embeddings"}, {"run_id": "r", "job_id": "j"})
        probe = call("zz_chat", {"model": "m", "node": "n", "probe": "embed"}, {"probe": "embed", "verdict": "verified", "dimensions": 8})
        ctx = {"node": "n", "scenario": {"pick": {"name": "m"}, "provider": "p", "expected": "verified"},
               "rj": {"probe": "embed", "verdict": "verified", "dimensions": 8}, "left": [], "calls": [starts, call("zz_job", {"node": "n", "job_id": "j"}, {"state": "done"}), probe, call("zz_run_stop", {"run_id": "r"}, {})]}
        self.assertTrue(passed(c.check_capability_probe(ctx, "embed")))
        for verdict in ("inconclusive", "not_supported"):
            bad = copy.deepcopy(ctx); bad["calls"][-2]["evidence"]["verdict"] = verdict; bad["rj"]["verdict"] = verdict
            self.assertFalse(passed(c.check_capability_probe(bad, "embed")))
        bad = copy.deepcopy(ctx); bad["calls"][0]["args"].pop("endpoint")
        self.assertFalse(passed(c.check_capability_probe(bad, "embed")))
        bad = copy.deepcopy(ctx); bad["calls"][-2]["evidence"]["dimensions"] = 0; bad["rj"]["dimensions"] = 0
        self.assertFalse(passed(c.check_capability_probe(bad, "embed")))
        vision = copy.deepcopy(ctx); vision["calls"][-2]["args"]["probe"] = "vision"; vision["calls"][-2]["evidence"]["verdict"] = "inconclusive"; vision["rj"]["verdict"] = "inconclusive"
        self.assertFalse(passed(c.check_capability_probe(vision, "vision")))

    def test_negative_probe_requires_typed_refusal(self):
        ctx = {"node": "n", "scenario": {"pick": {"name": "m"}, "provider": "p", "expected": "not_supported", "capability_absent": True},
               "rj": {"verdict": "not_supported", "dimensions": None, "status": 400, "code": "unsupported_input"}, "left": [],
               "calls": [call("zz_chat", {"model": "m", "node": "n", "probe": "embed"},
                              c.call_evidence({"verdict": "not_supported", "status": 400, "code": "unsupported_input"}), True)]}
        self.assertTrue(passed(c.check_capability_probe(ctx, "embed")))
        absent = copy.deepcopy(ctx); absent["scenario"].update(refusal_status=404, refusal_code="backend_error")
        absent["calls"][-1]["evidence"] = {"status": 404, "code": "backend_error"}
        absent["rj"] = {"verdict": None, "dimensions": None, "status": 404, "code": "backend_error"}
        self.assertTrue(passed(c.check_capability_probe(absent, "embed")))
        inconclusive = copy.deepcopy(absent); inconclusive["calls"][-1]["evidence"]["verdict"] = "inconclusive"; inconclusive["rj"]["verdict"] = "inconclusive"
        self.assertFalse(passed(c.check_capability_probe(inconclusive, "embed")))
        bad = copy.deepcopy(ctx); bad["calls"][-1]["evidence"]["code"] = "model_load_failed"
        self.assertFalse(passed(c.check_capability_probe(bad, "embed")))

    def test_full_model_picker_excludes_draft_weights(self):
        files = [{"file": name, "size_bytes": size * c.GiB, "quantization": "Q8_0"}
                 for name, size in [("dflash-Qwen3-27B-Q8_0.gguf", 2), ("Qwen3-27B-Q8_0.gguf", 30)]]
        self.assertIsNone(c.gguf_choice(files, 4, True))
        self.assertEqual(c.gguf_choice(files, 40, True)["file"], "Qwen3-27B-Q8_0.gguf")

class ModelDetailEvidenceTest(unittest.TestCase):
    def setUp(self):
        # Synthetic physical detail and a later repository page with two builds.
        entry = {"name": "tiny-q4", "source_id": "repo", "source_repo": "huggingface", "node": "n", "provider": "p",
                 "show": {"name": "tiny-q4", "source_id": "repo", "source_repo": "huggingface", "node": "n", "assigned_app": "p",
                          "model_info": {"context": 2048}, "template_present": True, "capabilities": ["completion"], "has_parameters": False}}
        exact = call("zz_models", {"model": "tiny-q4", "node": "n", "provider": "p"},
                     {"total": 1, "has_more": False, "models": [entry]})
        broad = call("zz_models", {"model": "repo", "node": "n", "provider": "p"},
                     {"total": 2, "has_more": False, "models": [entry, {**entry, "name": "tiny-fp16"}]})
        self.ctx = {"node": "n", "scenario": {"provider": "p"}, "left": [], "calls": [exact, broad],
                    "rj": {"model": "tiny-q4", "source_id": "repo", "context": {"context": 2048}, "template_present": True,
                           "capabilities": ["completion"], "has_parameters": False, "detail_error": None}}

    def test_exact_detail_survives_later_repository_page(self):
        self.assertTrue(passed(c.check_model_detail(self.ctx)))
        source = copy.deepcopy(self.ctx); source["calls"][0]["args"]["model"] = "repo"
        self.assertTrue(passed(c.check_model_detail(source)))
        broad_only = copy.deepcopy(self.ctx); broad_only["calls"].pop(0)
        self.assertFalse(passed(c.check_model_detail(broad_only)))

    def test_selected_detail_stays_strict_and_uses_latest_exact_evidence(self):
        mutations = [lambda x: x["args"].update(model="*tiny*"), lambda x: x["args"].update(node="other"),
                     lambda x: x["args"].update(provider="other"), lambda x: x["evidence"].update(total=2),
                     lambda x: x["evidence"].update(has_more=True), lambda x: x["evidence"].update(capture_overflow=True),
                     lambda x: x["evidence"]["models"][0].update(source_id="other"),
                     lambda x: x["evidence"]["models"][0]["show"].update(node="other"),
                     lambda x: x["evidence"]["models"][0]["show"].update(assigned_app="other"),
                     lambda x: x["evidence"]["models"][0]["show"].update(source_repo="other"),
                     lambda x: x["evidence"]["models"][0]["show"].update(model_info={"context": 4096}),
                     lambda x: x["evidence"]["models"][0].pop("show")]
        for mutate in mutations:
            bad = copy.deepcopy(self.ctx); mutate(bad["calls"][0])
            self.assertFalse(passed(c.check_model_detail(bad)))
        latest = copy.deepcopy(self.ctx); latest["calls"].append(copy.deepcopy(latest["calls"][0]))
        latest["calls"][-1]["evidence"]["models"][0]["show"]["model_info"] = {"context": 4096}
        self.assertFalse(passed(c.check_model_detail(latest)))
        for mutate in (lambda x: x["evidence"]["models"][0].update(name="other"),
                       lambda x: x["evidence"].update(models=[]),
                       lambda x: x["evidence"].update(models=[], capture_overflow=True)):
            malformed = copy.deepcopy(self.ctx); malformed["calls"].append(copy.deepcopy(malformed["calls"][0]))
            mutate(malformed["calls"][-1])
            self.assertFalse(passed(c.check_model_detail(malformed)))
        forged = copy.deepcopy(self.ctx); forged["rj"]["capabilities"] = ["vision"]
        self.assertFalse(passed(c.check_model_detail(forged)))


class DisposableOwnershipTest(unittest.TestCase):
    def setUp(self):
        self.args = {"model": "tiny", "node": "n", "provider": "p", "source_id": "repo", "idempotency_key": "unique"}
        self.trace = [
            call("zz_models", {"node": "n", "model": "repo"}, {"total": 0, "has_more": False}),
            call("zz_model_download", {"node": "n", "provider": "p", "model": "repo", "file": "tiny.gguf"}, {"nodes": [{"node": "n", "job_id": "j"}]}),
            call("zz_job", {"node": "n", "job_id": "j"}, {"state": "done"}),
            call("zz_models", {"node": "n", "provider": "p", "model": "tiny"}, {"total": 1, "has_more": False, "models": [{"name": "tiny", "node": "n", "provider": "p", "source_id": "repo", "is_cloud": False}]})]

    def test_ownership_fails_closed(self):
        self.assertTrue(c.delete_ownership(self.trace, self.args))
        automatic = copy.deepcopy(self.trace); automatic[2]["args"].pop("node"); automatic[3]["args"].pop("provider")
        self.assertTrue(c.delete_ownership(automatic, self.args))
        mutations = [lambda x: x[0]["args"].update(provider="p"), lambda x: x[0]["evidence"].update(total=1),
                     lambda x: x[2]["args"].update(node="other"), lambda x: x[2]["args"].update(job_id="wrong"),
                     lambda x: x[2]["evidence"].update(state="failed"), lambda x: x[3]["args"].update(model="unrelated"),
                     lambda x: x[3]["args"].update(model="*tiny*"),
                     lambda x: x[3]["evidence"]["models"][0].update(variant_of="base"),
                     lambda x: x[3]["evidence"]["models"][0].update(is_cloud=True),
                     lambda x: x[1]["args"].update(file="other.gguf")]
        for mutate in mutations:
            trace = copy.deepcopy(self.trace); mutate(trace)
            self.assertFalse(c.delete_ownership(trace, self.args))

    def test_proxy_never_forwards_unproven_deletion(self):
        import io, json
        from unittest.mock import patch
        class Exchange:
            path, command = "/zzrouter/v1/mcp", "POST"
            def send_response(self, code): self.code = code
            def send_header(self, *args): pass
            def end_headers(self): pass
        req = {"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "zz_model_delete", "arguments": self.args}}
        from types import SimpleNamespace
        reply = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {"isError": False, "structuredContent": {"deleted": 1}}}).encode()
        cases = [(None, None, None), ("2025-06-18", None, None), ("2026-07-28", "2026-07-28", "complete"),
                 (None, "2026-07-28", "complete")]  # Meta-only case covers formatting, not modern HTTP transport validity.
        for header_version, meta_version, expected in cases:
            with self.subTest(header=header_version, meta=meta_version):
                request = copy.deepcopy(req)
                if meta_version:
                    request["params"]["_meta"] = {"io.modelcontextprotocol/protocolVersion": meta_version}
                e = Exchange(); body = json.dumps(request).encode(); e.rfile = io.BytesIO(body); e.wfile = io.BytesIO()
                e.headers = {"Content-Length": str(len(body)), "X-Campaign-Scenario": "guard-unit"}
                if header_version:
                    e.headers["MCP-Protocol-Version"] = header_version
                with patch.object(c.urllib.request, "urlopen", return_value=SimpleNamespace(status=200, read=lambda: reply, headers={})) as upstream:
                    c.Proxy._go(e)
                upstream.assert_not_called()
                result = json.loads(e.wfile.getvalue())["result"]
                self.assertTrue(result["isError"])
                self.assertEqual(result["structuredContent"]["code"], "campaign_ownership_guard")
                self.assertEqual(result.get("resultType"), expected)

    def test_partial_cleanup_never_deletes(self):
        from unittest.mock import patch
        with patch.object(c, "http", return_value=(200, {"phase": "failed"}, 0)) as http:
            ok, detail = c.cleanup_disposable(self.trace, "n", "p")
        self.assertFalse(ok)
        self.assertIn("retained", detail)
        self.assertTrue(all(x.args[0] != "DELETE" for x in http.call_args_list))

    def test_successful_cleanup_reads_public_envelope(self):
        from unittest.mock import patch
        raw = {"name": "tiny", "node": "n", "assigned_app": "p", "source_id": "repo"}
        replies = [(200, {"phase": "done"}, 0), (200, {"data": [raw], "total": 1, "has_more": False}, 0),
                   (200, {"success": True, "data": {"deleted": 1}}, 0), (200, {"data": [], "total": 0}, 0)]
        with patch.object(c, "http", side_effect=replies) as http:
            ok, detail = c.cleanup_disposable(self.trace, "n", "p")
        self.assertTrue(ok, detail)
        self.assertEqual(http.call_args_list[2].args[:2], ("DELETE", "/zzrouter/v1/models"))

    def test_empty_inventory_requires_actual_physical_proof(self):
        from unittest.mock import patch
        alias = copy.deepcopy(self.trace)
        alias[0]["args"]["model"] = "repo-alias"
        alias[1]["args"]["model"] = "repo-alias"
        unrelated = self.trace + [call("zz_model_download", {"node": "n", "provider": "p", "model": "repo", "file": "other.gguf"},
                                       {"nodes": [{"node": "n", "job_id": "j2"}]}),
                                  call("zz_job", {"node": "n", "job_id": "j2"}, {"state": "done"})]
        for trace in (self.trace[:3], alias, unrelated):
            replies = [(200, {"phase": "done"}, 0), (200, {"data": [], "total": 0, "has_more": False}, 0),
                       (200, {"data": [], "total": 0, "has_more": False}, 0)]
            with patch.object(c, "http", side_effect=replies) as http:
                ok, detail = c.cleanup_download(next(x for x in reversed(trace) if x["tool"] == "zz_model_download"), trace, "n", "p")
            self.assertFalse(ok)
            self.assertIn("retained", detail)
            self.assertTrue(all(x.args[0] != "DELETE" for x in http.call_args_list))

    def test_owned_identity_already_absent_still_cleans_up(self):
        from unittest.mock import patch
        replies = [(200, {"phase": "done"}, 0), (200, {"data": [], "total": 0, "has_more": False}, 0),
                   (200, {"data": [], "total": 0, "has_more": False}, 0)]
        with patch.object(c, "http", side_effect=replies) as http:
            ok, detail = c.cleanup_disposable(self.trace, "n", "p")
        self.assertTrue(ok, detail)
        self.assertTrue(all(x.args[0] != "DELETE" for x in http.call_args_list))

    def test_pending_cleanup_uses_public_cancel_route(self):
        from unittest.mock import patch
        replies = [(200, {"phase": "running"}, 0), (202, {"accepted": True}, 0), (200, {"phase": "cancelled"}, 0)]
        with patch.object(c, "http", side_effect=replies) as http, patch.object(c.time, "sleep"):
            ok, detail = c.cleanup_disposable(self.trace, "n", "p")
        self.assertFalse(ok)
        self.assertIn("retained", detail)
        self.assertEqual(http.call_args_list[1].args[:2], ("DELETE", "/zzrouter/v1/jobs/j?node=n"))

    def test_errored_download_still_reconciles_captured_job(self):
        from unittest.mock import patch
        trace = copy.deepcopy(self.trace); trace[1]["is_error"] = True
        with patch.object(c, "http", return_value=(200, {"phase": "done"}, 0)) as http:
            ok, detail = c.cleanup_disposable(trace, "n", "p")
        self.assertFalse(ok)
        self.assertIn("uncertain", detail)
        self.assertEqual(http.call_args_list[0].args[:2], ("GET", "/zzrouter/v1/jobs/j?node=n"))
        self.assertTrue(all(x.args[0] != "DELETE" for x in http.call_args_list))

    def test_cleanup_continues_after_first_failed_download(self):
        from unittest.mock import patch
        second = call("zz_model_download", {"node": "n", "provider": "p", "model": "repo2"}, {"nodes": [{"node": "n", "job_id": "j2"}]})
        replies = [(200, {"phase": "failed"}, 0), (200, {"phase": "running"}, 0),
                   (202, {"accepted": True}, 0), (200, {"phase": "cancelled"}, 0)]
        with patch.object(c, "http", side_effect=replies) as http, patch.object(c.time, "sleep"):
            ok, detail = c.cleanup_disposable([self.trace[1], second], "n", "p")
        self.assertFalse(ok)
        self.assertEqual(http.call_args_list[2].args[:2], ("DELETE", "/zzrouter/v1/jobs/j2?node=n"))
        self.assertIn("j2", detail)


if __name__ == "__main__":
    unittest.main()
