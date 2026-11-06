#!/usr/bin/env python3
"""Tests for scripts/plan-badge-backfill.py (task ka-0000000777).

Hermetic: drives normalize()/build_html() with a synthetic plan list. No kd
call, no Chrome, no network.

Run: python3 scripts/plan-badge-backfill_test.py
"""
from __future__ import annotations

import importlib.util
import os
import unittest
import xml.dom.minidom as md

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "pbb", os.path.join(_HERE, "plan-badge-backfill.py")
)
assert _spec and _spec.loader
pbb = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(pbb)


_SAMPLE = [
    {
        "id": "gd-n37hX7dFys",
        "title": "[E2E fixture] Plans UX demo plan",
        "labels": ["plan", "project:gardenio"],
        "fields": {"plan_state": "building"},
        "project": "gardenio",
    },
    {
        "id": "gd-Xs3QgeUHCR",
        "title": "[E2E fixture] Plans authoring — ready-to-build plan",
        "labels": ["plan"],
        "fields": {"plan_state": "ready"},
        "project": "gardenio",
    },
    {
        "id": "gd-notaplan",
        "title": "Just an epic, no plan label",
        "labels": ["project:gardenio"],
        "fields": {},
    },
]


class NormalizeTests(unittest.TestCase):
    def test_stable_sort(self):
        norm = pbb.normalize(_SAMPLE)
        titles = [r["title"] for r in norm]
        self.assertEqual(titles, sorted(titles, key=str.lower))

    def test_carries_state_and_project(self):
        norm = pbb.normalize(_SAMPLE)
        by_id = {r["id"]: r for r in norm}
        self.assertEqual(by_id["gd-n37hX7dFys"]["state"], "building")
        self.assertEqual(by_id["gd-n37hX7dFys"]["project"], "gardenio")

    def test_plan_state_top_level_fallback(self):
        norm = pbb.normalize([{"id": "x", "title": "T", "plan_state": "draft"}])
        self.assertEqual(norm[0]["state"], "draft")


class BuildHtmlTests(unittest.TestCase):
    def test_contains_every_plan(self):
        norm = pbb.normalize(_SAMPLE)
        html = pbb.build_html(norm)
        for r in norm:
            self.assertIn(r["id"], html)

    def test_three_sizes_per_plan(self):
        norm = pbb.normalize(_SAMPLE[:1])
        html = pbb.build_html(norm)
        self.assertIn('width="128"', html)
        self.assertIn('width="40"', html)
        self.assertIn('width="24"', html)

    def test_count_in_header(self):
        norm = pbb.normalize(_SAMPLE)
        html = pbb.build_html(norm)
        self.assertIn(f"{len(norm)} live plans", html)

    def test_title_escaped(self):
        norm = pbb.normalize([{"id": "x", "title": 'A & B <x>', "labels": ["plan"]}])
        html = pbb.build_html(norm)
        self.assertIn("A &amp; B", html)
        self.assertNotIn("<x>", html)

    def test_svgs_are_well_formed(self):
        # Every embedded SVG must be valid XML so Chrome rasterizes it.
        norm = pbb.normalize(_SAMPLE)
        for r in norm:
            svg = pbb.pbt0.render_svg(r["id"], r["title"], 128)
            md.parseString(svg)


if __name__ == "__main__":
    unittest.main()
