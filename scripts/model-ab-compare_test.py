#!/usr/bin/env python3
"""model-ab-compare_test.py — hermetic unit suite for model-ab-compare.py.

Guards the pure surface of the Haiku vs Opus-5 cost/latency harness
(task ka-0000007402): corpus validation, latency percentiles, per-model
aggregation, the tradeoff ratios, markdown rendering (no key leak), key
resolution, --dry-run, and the LIVE committed corpus. Stdlib only — no network,
no LiteLLM, no `kd`. Imports the harness by path so it runs from anywhere.
"""
import importlib.util
import io
import json
import os
import sys
import tempfile
import unittest
from contextlib import redirect_stdout, redirect_stderr

_HERE = os.path.dirname(os.path.abspath(__file__))
_SPEC = importlib.util.spec_from_file_location(
    "model_ab_compare", os.path.join(_HERE, "model-ab-compare.py"))
mab = importlib.util.module_from_spec(_SPEC)
# Register before exec so dataclass type-hint resolution (KW_ONLY probe) can find
# the module in sys.modules when the harness is loaded by path.
sys.modules[_SPEC.name] = mab
_SPEC.loader.exec_module(mab)

CORPUS_PATH = os.path.join(_HERE, "model-ab-corpus.json")


def _write(tmp, obj):
    p = os.path.join(tmp, "corpus.json")
    with open(p, "w", encoding="utf-8") as fh:
        json.dump(obj, fh)
    return p


class SanitizeAndRedact(unittest.TestCase):
    def test_sanitize_model(self):
        self.assertEqual(mab.sanitize_model("claude-haiku-4-5"), "CLAUDE_HAIKU_4_5")
        self.assertEqual(mab.sanitize_model("claude-opus-5"), "CLAUDE_OPUS_5")
        self.assertEqual(mab.sanitize_model("a..b"), "A_B")

    def test_redact_key_never_shows_full(self):
        self.assertEqual(mab.redact_key(None), "<none>")
        self.assertEqual(mab.redact_key("sk-abcdef123456"), "sk-…123456")
        self.assertNotIn("abcdef", mab.redact_key("sk-abcdef123456"))
        self.assertEqual(mab.redact_key("short"), "sk-****")


class Percentiles(unittest.TestCase):
    def test_percentile_interpolates(self):
        vals = [0.0, 10.0, 20.0, 30.0, 40.0]
        self.assertEqual(mab.percentile(vals, 0), 0.0)
        self.assertEqual(mab.percentile(vals, 100), 40.0)
        self.assertEqual(mab.percentile(vals, 50), 20.0)

    def test_percentile_empty_and_single(self):
        self.assertEqual(mab.percentile([], 50), 0.0)
        self.assertEqual(mab.percentile([7.0], 90), 7.0)

    def test_summarize_latencies(self):
        s = mab.summarize_latencies([30, 10, 20])
        self.assertEqual(s["n"], 3)
        self.assertEqual(s["min_ms"], 10.0)
        self.assertEqual(s["max_ms"], 30.0)
        self.assertEqual(s["p50_ms"], 20.0)

    def test_summarize_empty(self):
        s = mab.summarize_latencies([])
        self.assertEqual(s["n"], 0)
        self.assertEqual(s["mean_ms"], 0.0)


class Aggregate(unittest.TestCase):
    def _recs(self):
        return [
            mab.TurnRecord("t1", "thread", ok=True, latency_ms=100.0,
                           cost_usd=0.001, input_tokens=10, output_tokens=20),
            mab.TurnRecord("t2", "crew", ok=True, latency_ms=300.0,
                           cost_usd=0.003, input_tokens=30, output_tokens=40),
            mab.TurnRecord("t3", "crew", ok=False, latency_ms=50.0,
                           error="HTTP 429: budget"),
        ]

    def test_aggregate_sums_and_counts(self):
        s = mab.aggregate_model_result("claude-haiku-4-5", self._recs())
        self.assertEqual(s["turns_total"], 3)
        self.assertEqual(s["turns_ok"], 2)
        self.assertEqual(s["turns_error"], 1)
        self.assertAlmostEqual(s["cost_usd_total"], 0.004)
        self.assertAlmostEqual(s["cost_usd_per_turn"], 0.002)
        self.assertEqual(s["input_tokens_total"], 40)
        self.assertEqual(s["output_tokens_total"], 60)
        self.assertTrue(s["cost_complete"])
        self.assertEqual(len(s["errors"]), 1)
        self.assertEqual(s["errors"][0]["task_id"], "t3")
        # latency stats computed over OK turns only (50ms error excluded).
        self.assertEqual(s["latency"]["min_ms"], 100.0)

    def test_per_category_breakdown(self):
        s = mab.aggregate_model_result("m", self._recs())
        self.assertIn("thread", s["per_category"])
        self.assertEqual(s["per_category"]["crew"]["turns"], 1)  # only OK crew turn

    def test_cost_incomplete_flagged(self):
        recs = [
            mab.TurnRecord("t1", "thread", ok=True, latency_ms=100.0, cost_usd=0.001),
            mab.TurnRecord("t2", "thread", ok=True, latency_ms=100.0, cost_usd=None),
        ]
        s = mab.aggregate_model_result("m", recs)
        self.assertFalse(s["cost_complete"])

    def test_no_ok_turns(self):
        recs = [mab.TurnRecord("t1", "x", ok=False, latency_ms=1.0, error="boom")]
        s = mab.aggregate_model_result("m", recs)
        self.assertEqual(s["turns_ok"], 0)
        self.assertEqual(s["cost_usd_per_turn"], 0.0)


class Tradeoff(unittest.TestCase):
    def _summaries(self):
        return [
            {"model": "claude-haiku-4-5", "cost_usd_total": 0.01,
             "cost_usd_per_turn": 0.002, "cost_complete": True,
             "latency": {"p50_ms": 200.0, "mean_ms": 250.0}},
            {"model": "claude-opus-5", "cost_usd_total": 0.30,
             "cost_usd_per_turn": 0.06, "cost_complete": True,
             "latency": {"p50_ms": 800.0, "mean_ms": 1000.0}},
        ]

    def test_ratios(self):
        t = mab.build_tradeoff(self._summaries(), "claude-haiku-4-5", "claude-opus-5")
        self.assertEqual(t["cost_ratio"], 30.0)
        self.assertEqual(t["cost_per_turn_ratio"], 30.0)
        self.assertEqual(t["latency_p50_ratio"], 4.0)
        self.assertTrue(t["cost_complete"])

    def test_zero_baseline_ratio_is_none(self):
        s = self._summaries()
        s[0]["cost_usd_total"] = 0.0
        t = mab.build_tradeoff(s, "claude-haiku-4-5", "claude-opus-5")
        self.assertIsNone(t["cost_ratio"])

    def test_missing_model(self):
        t = mab.build_tradeoff(self._summaries()[:1], "claude-haiku-4-5", "claude-opus-5")
        self.assertIn("note", t)


class Markdown(unittest.TestCase):
    def test_render_no_key_leak_and_has_rows(self):
        summaries = [mab.aggregate_model_result("claude-haiku-4-5", [
            mab.TurnRecord("t1", "thread", ok=True, latency_ms=120.0, cost_usd=0.001,
                           input_tokens=5, output_tokens=6)])]
        report = {
            "meta": {"corpus": "c.json", "corpus_tasks": 1, "repeat": 1,
                     "litellm_url": "https://litellm.example", "captured_at": "now"},
            "models": summaries,
            "tradeoff": {"baseline": "claude-haiku-4-5", "comparand": "claude-opus-5",
                         "cost_ratio": 12.0, "latency_p50_ratio": 3.0,
                         "cost_complete": True},
        }
        md = mab.render_markdown(report)
        self.assertIn("claude-haiku-4-5", md)
        self.assertIn("Tradeoff", md)
        self.assertNotIn("sk-", md)

    def test_incomplete_cost_warning(self):
        summaries = [mab.aggregate_model_result("m", [
            mab.TurnRecord("t1", "x", ok=True, latency_ms=1.0, cost_usd=None)])]
        report = {"meta": {}, "models": summaries,
                  "tradeoff": {"baseline": "m", "comparand": "m", "cost_ratio": 1.0,
                               "latency_p50_ratio": 1.0, "cost_complete": False}}
        self.assertIn("⚠", mab.render_markdown(report))


class CorpusLoading(unittest.TestCase):
    def test_valid(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = _write(tmp, {"tasks": [
                {"id": "a", "category": "thread", "user": "hi", "max_tokens": 10},
                {"id": "b", "category": "crew", "user": "yo", "max_tokens": 20,
                 "system": "sys"},
            ]})
            tasks = mab.load_corpus(p)
            self.assertEqual([t.id for t in tasks], ["a", "b"])
            self.assertEqual(tasks[1].system, "sys")

    def test_missing_file(self):
        with self.assertRaises(mab.ConfigError):
            mab.load_corpus("/no/such/corpus.json")

    def test_bad_json(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = os.path.join(tmp, "c.json")
            with open(p, "w") as fh:
                fh.write("{not json")
            with self.assertRaises(mab.ConfigError):
                mab.load_corpus(p)

    def test_empty_tasks(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(mab.ConfigError):
                mab.load_corpus(_write(tmp, {"tasks": []}))

    def test_missing_id(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(mab.ConfigError):
                mab.load_corpus(_write(tmp, {"tasks": [
                    {"category": "x", "user": "u", "max_tokens": 1}]}))

    def test_duplicate_id(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(mab.ConfigError):
                mab.load_corpus(_write(tmp, {"tasks": [
                    {"id": "a", "category": "x", "user": "u", "max_tokens": 1},
                    {"id": "a", "category": "y", "user": "v", "max_tokens": 1}]}))

    def test_bad_max_tokens(self):
        with tempfile.TemporaryDirectory() as tmp:
            for bad in (0, -1, "10", True):
                with self.assertRaises(mab.ConfigError):
                    mab.load_corpus(_write(tmp, {"tasks": [
                        {"id": "a", "category": "x", "user": "u",
                         "max_tokens": bad}]}))

    def test_bad_system_type(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(mab.ConfigError):
                mab.load_corpus(_write(tmp, {"tasks": [
                    {"id": "a", "category": "x", "user": "u", "max_tokens": 1,
                     "system": 5}]}))


class LiveCorpus(unittest.TestCase):
    """The committed corpus must always load, cover the fleet's key categories,
    and stay bounded (a corpus that quietly grows blows the budget)."""

    def test_committed_corpus_loads(self):
        tasks = mab.load_corpus(CORPUS_PATH)
        self.assertGreaterEqual(len(tasks), 3)
        self.assertLessEqual(len(tasks), 20)
        cats = {t.category for t in tasks}
        for expected in ("thread", "crew", "gardener", "refusal"):
            self.assertIn(expected, cats, f"corpus missing category {expected!r}")

    def test_committed_corpus_within_call_cap(self):
        tasks = mab.load_corpus(CORPUS_PATH)
        self.assertLessEqual(len(tasks), mab.MAX_CALLS_PER_MODEL)


class KeyResolution(unittest.TestCase):
    def test_cli_over_env(self):
        keys = mab.resolve_keys(
            ["claude-haiku-4-5"], {"claude-haiku-4-5": "sk-cli"},
            {"LITELLM_KEY_CLAUDE_HAIKU_4_5": "sk-env"})
        self.assertEqual(keys["claude-haiku-4-5"], "sk-cli")

    def test_env_fallback(self):
        keys = mab.resolve_keys(
            ["claude-opus-5"], {}, {"LITELLM_KEY_CLAUDE_OPUS_5": "sk-env"})
        self.assertEqual(keys["claude-opus-5"], "sk-env")

    def test_unresolved_omitted(self):
        keys = mab.resolve_keys(["claude-opus-5"], {}, {})
        self.assertNotIn("claude-opus-5", keys)

    def test_parse_cli_keys_bad(self):
        with self.assertRaises(mab.ConfigError):
            mab._parse_cli_keys(["no-equals"])


class DryRun(unittest.TestCase):
    def test_dry_run_no_network_ok(self):
        out = io.StringIO()
        err = io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            rc = mab.main(["--corpus", CORPUS_PATH, "--dry-run"])
        self.assertEqual(rc, 0)
        doc = json.loads(out.getvalue())
        self.assertTrue(doc["dry_run"])
        plan = doc["plan"]
        self.assertEqual(plan["models"], mab.DEFAULT_MODELS)
        # calls_per_model == tasks * repeat; keys redacted (never raw).
        self.assertEqual(plan["calls_per_model"], plan["corpus_tasks"])
        for shown in plan["keys"].values():
            self.assertNotIn("sk-x", shown)

    def test_dry_run_plan_call_math(self):
        out = io.StringIO()
        with redirect_stdout(out), redirect_stderr(io.StringIO()):
            mab.main(["--corpus", CORPUS_PATH, "--dry-run", "--repeat", "3",
                      "--models", "claude-haiku-4-5,claude-opus-5"])
        plan = json.loads(out.getvalue())["plan"]
        self.assertEqual(plan["calls_total"], plan["corpus_tasks"] * 3 * 2)

    def test_live_run_without_url_errors(self):
        with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
            rc = mab.main(["--corpus", CORPUS_PATH])  # no --url, no LITELLM_URL
        # Guard against a real LITELLM_URL in the ambient env leaking in.
        if not os.environ.get("LITELLM_URL"):
            self.assertEqual(rc, 2)


class RunCap(unittest.TestCase):
    def test_run_rejects_over_cap(self):
        tasks = [mab.Task(id=f"t{i}", category="x", user="u", max_tokens=1)
                 for i in range(mab.MAX_CALLS_PER_MODEL + 1)]
        with self.assertRaises(mab.ConfigError):
            mab.run(["m"], tasks, 1, "https://x", {"m": "sk-x"}, 1.0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
