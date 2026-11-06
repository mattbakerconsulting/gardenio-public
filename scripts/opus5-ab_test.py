#!/usr/bin/env python3
"""Unit tests for opus5-ab.py — the P4 A-B regression gate (task ka-0000001334).

Guards the two "material, not noise" refinements added to make the gate faithful
to its own docstring contract ("catch a *material* over-constraint regression, not
p-hacked noise") — the exact judgment four A-B re-capture agents (ka-0000007395)
each had to hand-write into operator decision ka-0000007166 instead of the tool
encoding it:

  (a) SAMPLE-SIZE FLOOR — a role with B-side n < MIN_SAMPLE is UNDER-SAMPLED, so
      its turns/spend breaches are advisory, never a hard regression (thread @ n=2).
  (b) COUPLED turns+spend — a lone turns-OR-spend breach in a well-sampled role is
      a WATCH-ITEM; only BOTH breaching together is the over-constraint signature
      (gardener-delivery turns↑/spend↓ = task-mix noise, not constraint).

Success-rate regressions remain a hard fail at any n >= MIN_SAMPLE. These use
synthetic snapshots — no `kd`, network, or live fleet required.
"""
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
MODULE_PATH = os.path.join(HERE, "opus5-ab.py")


def _load_module():
    # opus5-ab.py is hyphenated, so it can't be a normal import — load by path.
    spec = importlib.util.spec_from_file_location("opus5_ab", MODULE_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


ab = _load_module()


def _role(n, *, succ=100.0, turns=10.0, spend=1.0, prime=30000):
    """A per-role metric block shaped like distill()/capture() output."""
    return {
        "n": n,
        "success_rate_pct": succ,
        "turns_median": turns,
        "spend_median": spend,
        "prime_bytes": prime,
    }


class TestSampleSizeFloor(unittest.TestCase):
    def test_undersampled_turns_and_spend_breach_is_advisory_not_regression(self):
        """thread @ n=2 with turns+spend both up must NOT be a hard regression."""
        A = {"thread": _role(200, turns=123.0, spend=13.35)}
        B = {"thread": _role(2, turns=159.0, spend=22.64)}
        v = ab.evaluate_gate(A, B)
        self.assertEqual(v["regressions"], [], "under-sampled role must not hard-fail")
        self.assertEqual(len(v["undersampled"]), 1)
        self.assertIn("thread", v["undersampled"][0])

    def test_undersampled_success_drop_is_advisory_not_regression(self):
        """A success dip in a sub-floor window is noise, surfaced as advisory."""
        A = {"crew": _role(200, succ=100.0)}
        B = {"crew": _role(2, succ=50.0)}       # 1/2 done — meaningless
        v = ab.evaluate_gate(A, B)
        self.assertEqual(v["regressions"], [])
        self.assertTrue(any("success" in u for u in v["undersampled"]))

    def test_at_floor_boundary_is_well_sampled(self):
        """n == MIN_SAMPLE is on the well-sampled side (>= floor)."""
        A = {"crew": _role(200, turns=10.0, spend=1.0)}
        B = {"crew": _role(ab.MIN_SAMPLE, turns=100.0, spend=100.0)}
        v = ab.evaluate_gate(A, B)
        self.assertEqual(len(v["regressions"]), 1, "coupled breach at the floor is a regression")


class TestCoupledMetrics(unittest.TestCase):
    def test_turns_up_spend_down_is_watch_not_regression(self):
        """gardener-delivery: turns 14→19 while spend collapses — task-mix noise."""
        A = {"gardener-delivery": _role(196, turns=14.0, spend=0.74)}
        B = {"gardener-delivery": _role(40, turns=19.0, spend=0.17)}
        v = ab.evaluate_gate(A, B)
        self.assertEqual(v["regressions"], [], "single-metric drift must not hard-fail")
        self.assertEqual(len(v["watch_items"]), 1)
        self.assertIn("turns", v["watch_items"][0])

    def test_spend_up_turns_flat_is_watch_not_regression(self):
        A = {"crew": _role(200, turns=10.0, spend=1.0)}
        B = {"crew": _role(40, turns=10.0, spend=2.0)}   # spend up, turns flat
        v = ab.evaluate_gate(A, B)
        self.assertEqual(v["regressions"], [])
        self.assertEqual(len(v["watch_items"]), 1)
        self.assertIn("spend", v["watch_items"][0])

    def test_both_up_well_sampled_is_regression(self):
        """gardener-bugfix: turns AND spend both up at n=20 — the real signature."""
        A = {"gardener-bugfix": _role(199, turns=9.0, spend=0.625)}
        B = {"gardener-bugfix": _role(20, turns=15.5, spend=1.10)}
        v = ab.evaluate_gate(A, B)
        self.assertEqual(len(v["regressions"]), 1)
        self.assertIn("gardener-bugfix", v["regressions"][0])
        self.assertIn("turns", v["regressions"][0])
        self.assertIn("spend", v["regressions"][0])


class TestSuccessGate(unittest.TestCase):
    def test_success_drop_well_sampled_is_hard_regression(self):
        """Success is the quality signal — a real drop hard-fails regardless of coupling."""
        A = {"crew": _role(200, succ=100.0)}
        B = {"crew": _role(80, succ=90.0)}
        v = ab.evaluate_gate(A, B)
        self.assertTrue(any("success" in r for r in v["regressions"]))

    def test_success_within_tolerance_passes(self):
        A = {"crew": _role(200, succ=100.0)}
        B = {"crew": _role(80, succ=98.5)}    # -1.5pp, within 2.0 tolerance
        v = ab.evaluate_gate(A, B)
        self.assertEqual(v["regressions"], [])

    def test_success_improvement_never_flags(self):
        A = {"plan-gardener": _role(200, succ=94.5, turns=13.0, spend=1.05)}
        B = {"plan-gardener": _role(93, succ=100.0, turns=8.0, spend=0.73)}
        v = ab.evaluate_gate(A, B)
        self.assertEqual(v["regressions"], [])
        self.assertEqual(v["watch_items"], [])


class TestFrozenArtifactEndToEnd(unittest.TestCase):
    """The committed A/B artifact must now read PASS via `compare` (exit 0 vs the
    old exit 2), while the one genuine coupled+well-sampled signal (gardener-bugfix)
    stays a regression — proving the gate discriminates rather than crying wolf."""

    def _snap(self, roles, window=None):
        d = {"roles": roles}
        if window:
            d["window_since"] = window
        return d

    def test_pass_when_only_noise_and_watch_items(self):
        A = {
            "thread": _role(162, turns=123.0, spend=13.35),
            "crew": _role(193, turns=49.0, spend=4.30),
            "gardener-delivery": _role(196, turns=14.0, spend=0.74),
            "plan-gardener": _role(200, succ=94.5, turns=13.0, spend=1.05),
        }
        B = {
            "thread": _role(2, turns=159.0, spend=22.64),            # under-sampled
            "crew": _role(41, turns=20.0, spend=1.77),               # improved
            "gardener-delivery": _role(40, turns=19.0, spend=0.17),   # watch (turns↑/spend↓)
            "plan-gardener": _role(93, succ=100.0, turns=8.0, spend=0.73),  # improved
        }
        with tempfile.TemporaryDirectory() as td:
            ap = os.path.join(td, "a.json")
            bp = os.path.join(td, "b.json")
            with open(ap, "w") as fh:
                json.dump(self._snap(A), fh)
            with open(bp, "w") as fh:
                json.dump(self._snap(B, window="2026-07-25T09:40:00Z"), fh)
            r = subprocess.run([sys.executable, MODULE_PATH, "compare", "--a", ap, "--b", bp],
                               capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, f"expected PASS, got:\n{r.stdout}\n{r.stderr}")
            self.assertIn("A-B PASS", r.stdout)

    def test_regression_when_coupled_well_sampled(self):
        A = {"gardener-bugfix": _role(199, turns=9.0, spend=0.625)}
        B = {"gardener-bugfix": _role(20, turns=15.5, spend=1.10)}
        with tempfile.TemporaryDirectory() as td:
            ap = os.path.join(td, "a.json")
            bp = os.path.join(td, "b.json")
            with open(ap, "w") as fh:
                json.dump(self._snap(A), fh)
            with open(bp, "w") as fh:
                json.dump(self._snap(B, window="2026-07-25T09:40:00Z"), fh)
            r = subprocess.run([sys.executable, MODULE_PATH, "compare", "--a", ap, "--b", bp],
                               capture_output=True, text=True)
            self.assertEqual(r.returncode, 2, f"expected REGRESSION, got:\n{r.stdout}")
            self.assertIn("REGRESSION", r.stdout)


if __name__ == "__main__":
    unittest.main()
