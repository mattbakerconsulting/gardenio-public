#!/usr/bin/env python3
"""Tests for scripts/plan-badge-tier0.py (task ka-0000000777).

Hermetic: pure functions in / SVG string out, no network, no filesystem beyond
importing the module under test. Pins the two acceptance criteria the plan
narrative (ka-0000005497) named:
  1. determinism — identical (id, title) -> byte-identical SVG;
  2. cross-plan distinctiveness for same-monogram plans (the measured
     "[E2E fixture]" -> "EF" collapse).

Run: python3 scripts/plan-badge-tier0_test.py
"""
from __future__ import annotations

import importlib.util
import os
import re
import unittest

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "pbt0", os.path.join(_HERE, "plan-badge-tier0.py")
)
assert _spec and _spec.loader
pbt0 = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(pbt0)


class MonogramTests(unittest.TestCase):
    def test_two_words(self):
        self.assertEqual(pbt0._monogram("Cheap deep clones"), "CD")

    def test_single_word(self):
        self.assertEqual(pbt0._monogram("DAILBERT"), "DA")

    def test_single_char_word(self):
        self.assertEqual(pbt0._monogram("X"), "X")

    def test_skips_leading_bracket_tag(self):
        # The measured failure: both E2E-fixture plans collapsed to "EF".
        # After skipping the "[E2E fixture]" tag they draw from the real title.
        self.assertEqual(pbt0._monogram("[E2E fixture] Plans UX demo plan"), "PU")
        self.assertEqual(
            pbt0._monogram("[E2E fixture] Plans authoring — ready-to-build plan"),
            "PA",
        )

    def test_skips_leading_paren_tag(self):
        self.assertEqual(pbt0._monogram("(Preview) Migrate UAT environments"), "MU")

    def test_bracket_only_title_uses_tag(self):
        # No real text after the tag -> fall back to the tag words.
        self.assertEqual(pbt0._monogram("[E2E fixture]"), "EF")

    def test_empty_title(self):
        self.assertEqual(pbt0._monogram(""), "?")

    def test_leading_punctuation(self):
        self.assertEqual(pbt0._monogram("  ...Fix both gates"), "FB")


class DeterminismTests(unittest.TestCase):
    def test_byte_identical_repeat(self):
        a = pbt0.render_svg("ka-0000005980", "Every plan gets a badge")
        b = pbt0.render_svg("ka-0000005980", "Every plan gets a badge")
        self.assertEqual(a, b)

    def test_retitle_changes_output(self):
        # Content-addressing on (id, title): a retitle must change the SVG so a
        # stale-glyph cache invalidates naturally.
        a = pbt0.render_svg("ka-0000005980", "Old title")
        b = pbt0.render_svg("ka-0000005980", "New different title")
        self.assertNotEqual(a, b)


class DistinctivenessTests(unittest.TestCase):
    def test_same_monogram_distinct_by_id(self):
        # Two plans with the SAME monogram but different bead ids must produce
        # visibly different badges (different hue/shape/ring/gradient), even if
        # the monogram happened to still collide.
        a = pbt0.render_svg("gd-n37hX7dFys", "[E2E fixture] demo")
        b = pbt0.render_svg("gd-Xs3QgeUHCR", "[E2E fixture] demo")
        self.assertNotEqual(a, b)

    def test_traits_spread_across_corpus(self):
        # Across a realistic set of ids, the derived (hue, shape) pairs should
        # not all collapse to one value.
        ids = [
            "ka-0000005980",
            "gd-n37hX7dFys",
            "gd-Xs3QgeUHCR",
            "ka-0000003709",
            "ka-0000001199",
            "ka-0000000949",
            "ka-0000004429",
            "ka-0000005334",
        ]
        pairs = set()
        for i in ids:
            d = pbt0._digest(i)
            pairs.add((d[0] % len(pbt0._PALETTE), d[1] % len(pbt0._SHAPES)))
        # Expect a healthy spread, not a single collapsed bucket.
        self.assertGreaterEqual(len(pairs), len(ids) - 2)


class SvgShapeTests(unittest.TestCase):
    def test_well_formed_svg(self):
        svg = pbt0.render_svg("gd-abc", "Test plan title")
        self.assertTrue(svg.startswith("<svg"))
        self.assertTrue(svg.rstrip().endswith("</svg>"))
        # No external references that would break inlining / rasterization.
        self.assertNotIn("<image", svg)
        self.assertNotIn("http://", svg.replace('xmlns="http://www.w3.org/2000/svg"', ""))
        # Parses as XML.
        import xml.dom.minidom as md

        md.parseString(svg)  # raises on malformed

    def test_size_honored(self):
        svg = pbt0.render_svg("gd-abc", "Title", size=40)
        self.assertIn('width="40"', svg)
        self.assertIn('height="40"', svg)

    def test_xml_escape_in_title(self):
        svg = pbt0.render_svg("gd-abc", 'A & B <plan> "q"')
        self.assertNotIn("A & B", svg)  # raw ampersand must be escaped
        self.assertIn("A &amp; B", svg)
        import xml.dom.minidom as md

        md.parseString(svg)

    def test_aria_label_present(self):
        svg = pbt0.render_svg("gd-abc", "Accessible plan")
        self.assertIn('role="img"', svg)
        self.assertRegex(svg, r'aria-label="Plan badge for Accessible plan"')


if __name__ == "__main__":
    unittest.main()
