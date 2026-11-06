#!/usr/bin/env python3
"""Tests for scripts/check-advice-snapshot-freshness.py (ka-0000004917).

Hermetic: builds a temp repo tree holding only the meta sidecar at the real
relative path, then drives the script's evaluate()/run() with a controlled
wall clock and controlled meta contents. No live daemon, no network.

Run: python3 scripts/check-advice-snapshot-freshness_test.py
"""
from __future__ import annotations

import datetime as _dt
import importlib.util
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

_spec = importlib.util.spec_from_file_location(
    "fresh",
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "check-advice-snapshot-freshness.py"),
)
fresh = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(fresh)


def _iso(dt: _dt.datetime) -> str:
    """RFC3339 with a trailing Z, exactly as writeSnapshotMeta emits."""
    return dt.astimezone(_dt.timezone.utc).replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


class EvaluateTest(unittest.TestCase):
    """Unit tests over evaluate() with an injected `now`."""

    def setUp(self):
        self.now = _dt.datetime(2026, 8, 28, 12, 0, 0, tzinfo=_dt.timezone.utc)
        self.soft = fresh.DEFAULT_SOFT_AGE_HOURS
        self.hard = fresh.DEFAULT_HARD_AGE_HOURS

    def _eval(self, captured: _dt.datetime, **extra):
        meta = {"captured_at": _iso(captured)}
        meta.update(extra)
        return fresh.evaluate(meta, self.now, self.soft, self.hard)

    def test_fresh_just_captured(self):
        r = self._eval(self.now - _dt.timedelta(hours=1))
        self.assertEqual(r["verdict"], "FRESH")
        self.assertFalse(r["past_hard_ceiling"])

    def test_fresh_just_under_soft(self):
        # 6d 23h < 7d soft ceiling.
        r = self._eval(self.now - _dt.timedelta(days=6, hours=23))
        self.assertEqual(r["verdict"], "FRESH")

    def test_stale_at_soft_boundary(self):
        # Exactly 7d -> STALE (>= is the trip condition), still under hard.
        r = self._eval(self.now - _dt.timedelta(days=7))
        self.assertEqual(r["verdict"], "STALE")
        self.assertFalse(r["past_hard_ceiling"])

    def test_stale_between_soft_and_hard(self):
        r = self._eval(self.now - _dt.timedelta(days=10))
        self.assertEqual(r["verdict"], "STALE")
        self.assertFalse(r["past_hard_ceiling"])

    def test_stale_past_hard_ceiling(self):
        # 14d exactly -> STALE and past the hard cliff (the fleet-wide P0 case).
        r = self._eval(self.now - _dt.timedelta(days=14))
        self.assertEqual(r["verdict"], "STALE")
        self.assertTrue(r["past_hard_ceiling"])

    def test_future_timestamp_treated_as_fresh(self):
        # Clock skew / hand-edit stamped in the future must not crash.
        r = self._eval(self.now + _dt.timedelta(days=3))
        self.assertEqual(r["verdict"], "FRESH")
        self.assertLessEqual(r["age_hours"], 0)

    def test_counts_passthrough(self):
        r = self._eval(self.now - _dt.timedelta(days=1),
                       advice_bead_count=308, fleet_archetype_count=65)
        self.assertEqual(r["advice_bead_count"], 308)
        self.assertEqual(r["fleet_archetype_count"], 65)


class ParseTest(unittest.TestCase):
    def test_z_suffix(self):
        dt = fresh._parse_captured_at("2026-08-14T17:38:13Z")
        self.assertEqual(dt.tzinfo, _dt.timezone.utc)
        self.assertEqual(dt.year, 2026)

    def test_explicit_offset(self):
        dt = fresh._parse_captured_at("2026-08-14T17:38:13+00:00")
        self.assertEqual(dt.hour, 17)

    def test_empty_is_operational_error(self):
        with self.assertRaises(fresh.OperationalError):
            fresh._parse_captured_at("   ")

    def test_garbage_is_operational_error(self):
        with self.assertRaises(fresh.OperationalError):
            fresh._parse_captured_at("not-a-timestamp")


class RunTest(unittest.TestCase):
    """End-to-end tests over run(), asserting exit codes + JSON on a temp repo."""

    def _write_meta(self, root: str, captured_at, extra=None):
        meta_path = os.path.join(root, fresh.META_REL_PATH)
        os.makedirs(os.path.dirname(meta_path), exist_ok=True)
        body = {"captured_at": captured_at, "advice_bead_count": 308, "fleet_archetype_count": 65}
        if extra is not None:
            body.update(extra)
        with open(meta_path, "w", encoding="utf-8") as fh:
            json.dump(body, fh)
        return meta_path

    def _run_json(self, root, extra_args=None):
        """Run --json, capture stdout, return (exit_code, parsed_or_None)."""
        import io
        import contextlib
        args = ["--repo-root", root, "--json"] + (extra_args or [])
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            code = fresh.run(args)
        out = buf.getvalue().strip()
        parsed = json.loads(out) if out else None
        return code, parsed

    def test_fresh_exit_0(self):
        with tempfile.TemporaryDirectory() as root:
            captured = _iso(_dt.datetime.now(_dt.timezone.utc) - _dt.timedelta(days=1))
            self._write_meta(root, captured)
            code, parsed = self._run_json(root)
            self.assertEqual(code, 0)
            self.assertEqual(parsed["verdict"], "FRESH")

    def test_stale_exit_2(self):
        with tempfile.TemporaryDirectory() as root:
            captured = _iso(_dt.datetime.now(_dt.timezone.utc) - _dt.timedelta(days=8))
            self._write_meta(root, captured)
            code, parsed = self._run_json(root)
            self.assertEqual(code, 2)
            self.assertEqual(parsed["verdict"], "STALE")
            self.assertFalse(parsed["past_hard_ceiling"])

    def test_stale_past_hard_exit_2(self):
        with tempfile.TemporaryDirectory() as root:
            captured = _iso(_dt.datetime.now(_dt.timezone.utc) - _dt.timedelta(days=20))
            self._write_meta(root, captured)
            code, parsed = self._run_json(root)
            self.assertEqual(code, 2)
            self.assertTrue(parsed["past_hard_ceiling"])

    def test_missing_meta_exit_1(self):
        with tempfile.TemporaryDirectory() as root:
            code, parsed = self._run_json(root)
            self.assertEqual(code, 1)
            self.assertEqual(parsed["verdict"], "ERROR")

    def test_bad_json_exit_1(self):
        with tempfile.TemporaryDirectory() as root:
            meta_path = os.path.join(root, fresh.META_REL_PATH)
            os.makedirs(os.path.dirname(meta_path), exist_ok=True)
            with open(meta_path, "w", encoding="utf-8") as fh:
                fh.write("{not json")
            code, parsed = self._run_json(root)
            self.assertEqual(code, 1)

    def test_missing_captured_at_exit_1(self):
        with tempfile.TemporaryDirectory() as root:
            meta_path = os.path.join(root, fresh.META_REL_PATH)
            os.makedirs(os.path.dirname(meta_path), exist_ok=True)
            with open(meta_path, "w", encoding="utf-8") as fh:
                json.dump({"advice_bead_count": 1}, fh)
            code, parsed = self._run_json(root)
            self.assertEqual(code, 1)

    def test_custom_soft_age_flag(self):
        # A 2d-old fixture is fresh at the default 7d, stale at a 1d soft ceiling.
        with tempfile.TemporaryDirectory() as root:
            captured = _iso(_dt.datetime.now(_dt.timezone.utc) - _dt.timedelta(days=2))
            self._write_meta(root, captured)
            code, _ = self._run_json(root)
            self.assertEqual(code, 0)
            code2, parsed2 = self._run_json(root, ["--soft-age-hours", "24"])
            self.assertEqual(code2, 2)
            self.assertEqual(parsed2["verdict"], "STALE")


class DefaultsMirrorGoSourceTest(unittest.TestCase):
    """Guard that the Python default ceilings mirror the Go constants.

    softSnapshotAge / hardSnapshotAge in
    gardenio/controller/cmd/gb/real_catalog_budget_test.go are the authoritative
    values; if someone changes them there without updating this script the alarm
    would fire on a different clock than the CI cliff it fronts. Parse the Go
    source directly so the two cannot silently diverge.
    """

    def _repo_root(self):
        return os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

    def test_defaults_match_go_constants(self):
        go_src = os.path.join(
            self._repo_root(), "gardenio", "controller", "cmd", "gb",
            "real_catalog_budget_test.go",
        )
        if not os.path.exists(go_src):
            self.skipTest(f"go source not materialized: {go_src}")
        with open(go_src, "r", encoding="utf-8") as fh:
            text = fh.read()
        # Expect the literal forms `softSnapshotAge = 7 * 24 * time.Hour` etc.
        import re
        soft = re.search(r"softSnapshotAge\s*=\s*(\d+)\s*\*\s*24\s*\*\s*time\.Hour", text)
        hard = re.search(r"hardSnapshotAge\s*=\s*(\d+)\s*\*\s*24\s*\*\s*time\.Hour", text)
        self.assertIsNotNone(soft, "could not find softSnapshotAge in Go source")
        self.assertIsNotNone(hard, "could not find hardSnapshotAge in Go source")
        self.assertEqual(int(soft.group(1)) * 24, fresh.DEFAULT_SOFT_AGE_HOURS)
        self.assertEqual(int(hard.group(1)) * 24, fresh.DEFAULT_HARD_AGE_HOURS)


if __name__ == "__main__":
    unittest.main()
