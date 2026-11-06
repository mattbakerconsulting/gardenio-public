#!/usr/bin/env python3
"""Legibility & confusability test for the Tier-0 plan badge (task ka-0000001220).

This is the acceptance gate the plan narrative (ka-0000005497) named directly:

    "Those two failures are the actual design brief. 24px legibility and
     cross-plan distinctiveness are the acceptance criteria, and they are
     testable."

The two measured Phase-0 prototype failures this suite locks out forever:

  1. LEGIBILITY AT 24px. The Tier-2 "metric triad" glyph collapsed into a
     featureless ring at 24px. Tier-0 avoids fine interior detail by design —
     one bold shape + a high-contrast 1-2 letter monogram — but "by design"
     is not "by test". Here we assert the two things that actually make a
     monogram readable at 24px: enough foreground/background CONTRAST (every
     palette pair clears WCAG's 3.0:1 large-text / non-text floor), and enough
     GLYPH SIZE (the monogram font-size at a 24px render stays above a legible
     minimum).

  2. NO TWO PLANS CONFUSABLE. The Tier-0 badge distinguishes plans on four
     derived axes — monogram, primary hue, container shape, accent ring — but
     only THREE of those survive a shrink to 24px: the subtle secondary-hue
     gradient and the thin accent ring blur out at cmdk-row scale. So the
     confusability bar is deliberately conservative: two plans must differ on
     the 24px-SURVIVABLE fingerprint (monogram, primary palette slot, shape),
     never leaning on a detail that a human cannot resolve at 24px. We assert
     this over the REAL live-plan corpus (the ~22 plans the operator watches on
     /plans) AND under synthetic scaling, so the property holds as the board
     grows, not just for today's snapshot.

Hermetic: pure functions in / SVG string out. Stdlib only — no `kd`, no Chrome,
no network. Imports the renderer under test by path (same pattern as the
determinism suite plan-badge-tier0_test.py), so both scripts stage together.

Run: python3 scripts/plan-badge-legibility_test.py
"""
from __future__ import annotations

import importlib.util
import os
import re
import unittest
from collections import Counter

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "pbt0", os.path.join(_HERE, "plan-badge-tier0.py")
)
assert _spec and _spec.loader
pbt0 = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(pbt0)


# ---------------------------------------------------------------------------
# A snapshot of the REAL live gardenio plan corpus (label=plan, status=open),
# captured 2026-08-14 for task ka-0000001220. The confusability property is a
# claim about THESE badges as an operator sees them on /plans, so the test
# exercises the real (id, title) pairs — not synthetic ones. Two entries share
# the "[E2E fixture]" prefix on purpose: they are the exact monogram-collapse
# case the plan narrative measured ("[E2E fixture]" both rendering "EF"), and
# their presence is what makes the confusability assertion meaningful.
#
# This list is a fixture, not a live query, so the test stays hermetic and
# stable in CI. It does not need to track the live board perfectly — it needs
# to be a realistic, adversarial sample (including the known collision case).
# ---------------------------------------------------------------------------
_LIVE_PLANS = [
    ("gd-Xs3QgeUHCR", "[E2E fixture] Plans authoring — ready-to-build plan"),
    ("gd-n37hX7dFys", "[E2E fixture] Plans UX demo plan"),
    ("ka-0000001199", "Agent metric triad — context, cost, turns on card + every view"),
    ("ka-0000007337", "Audit gardenio agent context for Opus/Fable 5"),
    ("gd-e83z5PcFwt", "Gardener auto-block sentinel revival"),
    ("ka-0000005334", "Cheap deep clones for all gardenio agents"),
    ("ka-0000003733", "Context crisis: consolidate advice + context observability"),
    ("ka-0000005352", "Continual data-density improvement of the /plans index cards"),
    ("ka-0000000093", "Drain the 88-ticket pre-go-live FSM triage backlog"),
    ("ka-0000005891", "Epic: CI/CD minutes -> gardenio Grafana / VictoriaMetrics"),
    ("ka-0000005980", "Every gardenio plan gets its own generated icon/badge"),
    ("ka-0000000137", "FSM Jira Triage Automation: Plan -> Prod v1"),
    ("ka-0000007152", "Gardenio harness improvements — graduation, proof packets"),
    ("ka-0000003709", "gardenio in group DMs: participate, start, invite"),
    ("ka-0000005240", "gVisor canary #16: fat-xlarge-node-first freeze->thaw"),
    ("ka-0000005916", "Never silently drop a GitLab request"),
    ("ka-0000000949", "Opus 5 cutover + model-switch/refusal observability"),
    ("gd-Uj6YcuReql", "Preserve prior_wrapup across resume-in-place"),
    ("ka-0000000572", "Scanner response: block + alert policy"),
    ("gd-HE8w8hlfyz", "Slack link unfurling for gardenio"),
    ("ka-0000004429", "Stop agent pods being interrupted mid-turn — durable fix"),
    ("ka-0000007580", "Trim per-session harness dead-weight"),
]

# The three cmdk-row px targets the acceptance criteria are judged at.
_SMALL = 24


# --- contrast helpers (WCAG 2.x relative luminance) -------------------------
def _srgb_to_lin(channel: int) -> float:
    c = channel / 255.0
    return c / 12.92 if c <= 0.03928 else ((c + 0.055) / 1.055) ** 2.4


def _rel_luminance(hex_color: str) -> float:
    h = hex_color.lstrip("#")
    r, g, b = int(h[0:2], 16), int(h[2:4], 16), int(h[4:6], 16)
    return 0.2126 * _srgb_to_lin(r) + 0.7152 * _srgb_to_lin(g) + 0.0722 * _srgb_to_lin(b)


def _contrast_ratio(a: str, b: str) -> float:
    la, lb = _rel_luminance(a), _rel_luminance(b)
    hi, lo = max(la, lb), min(la, lb)
    return (hi + 0.05) / (lo + 0.05)


def _fingerprint_24px(bead_id: str, title: str) -> tuple:
    """The subset of a badge's distinguishing traits that a human can actually
    resolve at 24px: the monogram, the PRIMARY palette slot, and the container
    shape. Deliberately EXCLUDES the accent ring and the secondary-hue gradient,
    both of which blur out at cmdk-row scale — so a passing corpus is legibly
    distinct, not distinct-only-under-a-magnifier."""
    d = pbt0._digest(bead_id)
    return (
        pbt0._monogram(title).upper(),
        d[0] % len(pbt0._PALETTE),  # primary background hue slot
        d[1] % len(pbt0._SHAPES),   # container shape
    )


class ContrastLegibilityTests(unittest.TestCase):
    """Every palette pair must be readable at 24px: >= WCAG 3.0:1."""

    # WCAG 2.x floor for large-scale text and non-text/UI graphics. A bold
    # monogram at 24px is "large text"; 3.0:1 is the right, defensible bar.
    MIN_CONTRAST = 3.0

    def test_every_palette_pair_clears_floor(self):
        worst = None
        for bg, ink in pbt0._PALETTE:
            ratio = _contrast_ratio(bg, ink)
            if worst is None or ratio < worst[0]:
                worst = (ratio, bg, ink)
            self.assertGreaterEqual(
                ratio,
                self.MIN_CONTRAST,
                f"palette pair {bg}/{ink} contrast {ratio:.2f} < {self.MIN_CONTRAST}:1 "
                "— monogram would be unreadable at 24px",
            )
        # Sanity: the worst pair is a real number in a plausible band, so a
        # future palette edit that silently zeroes a color trips this too.
        self.assertGreater(worst[0], 1.0)

    def test_monogram_uses_a_palette_ink(self):
        # The rendered monogram fill must be one of the palette inks (i.e. the
        # contrast we just proved actually reaches the glyph, not some other
        # hardcoded color). Check across the real corpus.
        inks = {ink for _, ink in pbt0._PALETTE}
        for pid, title in _LIVE_PLANS:
            svg = pbt0.render_svg(pid, title, _SMALL)
            fill = re.search(r'<text[^>]*\bfill="([^"]+)"', svg)
            self.assertIsNotNone(fill, f"{pid}: no monogram <text> fill found")
            self.assertIn(
                fill.group(1),
                inks,
                f"{pid}: monogram fill {fill.group(1)} is not a palette ink",
            )


class GlyphSizeLegibilityTests(unittest.TestCase):
    """The monogram must stay a readable size when the badge renders at 24px."""

    # At a 24px badge the 2-char monogram is the smaller of the two (font-size
    # scales at 0.46 vs 0.56 of the square). 0.46*24 ~= 11px. Anything below
    # ~9px stops being legible in a cmdk row; assert a firm floor so a future
    # font-size tweak that shrinks the glyph gets caught.
    MIN_FONT_PX = 9.0

    def _font_px(self, svg: str) -> float:
        m = re.search(r'<text[^>]*font-size="([0-9.]+)"', svg)
        self.assertIsNotNone(m, "no monogram <text> font-size found")
        return float(m.group(1))

    def test_two_char_monogram_legible_at_24px(self):
        # A two-word title -> 2-char monogram, the worst (smallest) case.
        svg = pbt0.render_svg("gd-g5uahb729t", "Two words here", _SMALL)
        self.assertGreaterEqual(self._font_px(svg), self.MIN_FONT_PX)

    def test_one_char_monogram_legible_at_24px(self):
        svg = pbt0.render_svg("gd-g5uahb729t", "Solo", _SMALL)
        self.assertGreaterEqual(self._font_px(svg), self.MIN_FONT_PX)

    def test_real_corpus_all_legible_at_24px(self):
        for pid, title in _LIVE_PLANS:
            svg = pbt0.render_svg(pid, title, _SMALL)
            self.assertGreaterEqual(
                self._font_px(svg),
                self.MIN_FONT_PX,
                f"{pid} ({title!r}) monogram too small at 24px",
            )

    def test_font_grows_with_badge(self):
        # Glyph size must scale with the badge, so the same asset that is legible
        # at 24px is not comically small on the 128px detail header.
        small = self._font_px(pbt0.render_svg("gd-x", "Two words", _SMALL))
        big = self._font_px(pbt0.render_svg("gd-x", "Two words", 128))
        self.assertGreater(big, small)


class NoFineDetailTests(unittest.TestCase):
    """Guard against re-introducing the metric-triad failure: a Tier-0 badge is
    one bold container shape + a monogram, with NO fine interior detail that
    dissolves to mush at 24px."""

    def test_bold_shape_plus_monogram_only(self):
        # A legible-at-24px badge has exactly one text node (the monogram) and a
        # small, bounded number of drawn primitives. A glyph made of many tiny
        # nested paths (the metric-triad trap) would blow past this.
        for pid, title in _LIVE_PLANS:
            svg = pbt0.render_svg(pid, title, _SMALL)
            self.assertEqual(
                svg.count("<text"), 1, f"{pid}: expected exactly one monogram text node"
            )
            primitives = (
                svg.count("<rect")
                + svg.count("<circle")
                + svg.count("<polygon")
                + svg.count("<path")
            )
            # container shape (1) + up to a 2-ring accent (2) = 3 max.
            self.assertLessEqual(
                primitives,
                3,
                f"{pid}: {primitives} drawn primitives — too much interior detail "
                "to survive 24px",
            )

    def test_no_external_or_raster_refs(self):
        # An <image> or web-font ref would render differently (or not at all) at
        # small sizes and break the "reads at 24px" guarantee.
        for pid, title in _LIVE_PLANS:
            svg = pbt0.render_svg(pid, title, _SMALL)
            self.assertNotIn("<image", svg)
            self.assertNotIn("@font-face", svg)
            self.assertNotIn("url(http", svg)


class ConfusabilityTests(unittest.TestCase):
    """No two plans confusable — on the 24px-survivable fingerprint."""

    def test_real_corpus_all_distinct_at_24px(self):
        seen: dict[tuple, str] = {}
        for pid, title in _LIVE_PLANS:
            fp = _fingerprint_24px(pid, title)
            self.assertNotIn(
                fp,
                seen,
                f"{pid} ({title!r}) shares the 24px fingerprint {fp} with "
                f"{seen.get(fp)} — the two badges are confusable at cmdk scale",
            )
            seen[fp] = pid

    def test_e2e_fixture_pair_distinct(self):
        # The measured collapse: both "[E2E fixture]" plans rendered "EF". After
        # the bracket-tag skip they draw different monograms; even if they still
        # collided on the monogram, the id-derived hue+shape must separate them.
        a = _fingerprint_24px("gd-Xs3QgeUHCR", "[E2E fixture] Plans authoring — ready")
        b = _fingerprint_24px("gd-n37hX7dFys", "[E2E fixture] Plans UX demo plan")
        self.assertNotEqual(a, b)

    def test_same_monogram_different_id_distinct(self):
        # Worst case for confusability: identical title (hence identical
        # monogram), different bead id. The id-derived hue+shape must still make
        # the two 24px fingerprints differ.
        a = _fingerprint_24px("gd-n37hX7dFys", "[E2E fixture] demo")
        b = _fingerprint_24px("gd-Xs3QgeUHCR", "[E2E fixture] demo")
        self.assertNotEqual(a, b)

    def test_confusability_holds_under_scaling(self):
        # The board grows. A conservative distinctiveness bar: across a large
        # synthetic set of plans whose TITLES vary the way real plan titles do
        # (so the monogram axis is genuinely exercised, not pinned to one value),
        # the fraction that collide on the 24px fingerprint stays small. This is
        # a probabilistic property of the hash-derived + monogram traits, not an
        # exact count, so we assert a generous ceiling that a real regression
        # (e.g. dropping the shape or hue axis) would breach.
        heads = [
            "Agent", "Gardener", "Context", "Deploy", "Metric", "Scanner",
            "Release", "Triage", "Harness", "Badge", "Slack", "Vision",
            "Freeze", "Backup", "Density", "Cutover", "Canary", "Runbook",
            "Cache", "Proof",
        ]
        tails = ["plan", "revival", "audit", "cutover", "backlog", "policy"]
        n = 120
        fps = []
        for i in range(n):
            title = f"{heads[i % len(heads)]} {tails[(i // len(heads)) % len(tails)]}"
            fps.append(_fingerprint_24px(f"gd-scale{i:05d}", title))
        counts = Counter(fps)
        collided = sum(c - 1 for c in counts.values() if c > 1)
        # Monogram (varied) x 12 hues x 6 shapes is a large space; expect the
        # overwhelming majority distinct. Allow a small tail, catch a collapse.
        self.assertLessEqual(
            collided,
            len(fps) // 10,
            f"{collided}/{len(fps)} synthetic plans collide on the 24px "
            "fingerprint — distinctiveness has regressed",
        )

    def test_shape_and_hue_axes_are_both_exercised(self):
        # Distinctiveness must not silently collapse onto a single axis (e.g. a
        # bug that pins every badge to one shape and leans entirely on hue). Over
        # the real corpus, both the shape axis and the hue axis must show real
        # spread.
        shapes = {_fingerprint_24px(p, t)[2] for p, t in _LIVE_PLANS}
        hues = {_fingerprint_24px(p, t)[1] for p, t in _LIVE_PLANS}
        self.assertGreaterEqual(len(shapes), 3, "shape axis barely used")
        self.assertGreaterEqual(len(hues), 5, "hue axis barely used")


if __name__ == "__main__":
    unittest.main()
