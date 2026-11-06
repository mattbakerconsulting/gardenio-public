#!/usr/bin/env python3
"""Unit tests for measure-plans.py's dark-skin scoring (ka-0000000260).

Regression guard for the Skin-dimension blind spot: before this iteration the
capture driver hardcoded the light skin, so measure-plans.py could not see a
dark-skin regression at all — the exact debt iteration-16 (ka-0000007937) had
to pay off retroactively when the whole /plans board rendered as white #fff
cards on the black dark canvas. The driver now renders BOTH skins and
measure_skin scores the dark capture. These tests pin the two behaviors that
matter so they can't silently regress:

  1. A HEALTHY dark render (dark cards, near-zero near-white) => the white-island
     flag stays False and the dark block is recorded (dark_captured True).
  2. The iteration-16 REGRESSION (white card islands, high near-white) =>
     dark_white_island_flag flips True — the harness now SCORES what used to be
     invisible.

It also pins the legacy-capture fallback (an old flat-layout capture dir with
no dark axis still measures, dark block null) and that the light half of the
skin block is untouched by whatever the dark half does (additive, not a rewrite).
"""
import os
import unittest
from importlib import util as _imputil

HERE = os.path.dirname(os.path.abspath(__file__))
TOOL_PATH = os.path.join(HERE, "measure-plans.py")

_spec = _imputil.spec_from_file_location("measure_plans", TOOL_PATH)
_mod = _imputil.module_from_spec(_spec)
_spec.loader.exec_module(_mod)  # type: ignore[union-attr]


# A byte-identical light half, reused across cases so a change in the dark half
# provably cannot move the pre-existing light metrics.
LIGHT_RENDER = {
    "plans-index__desktop": {"overflow": 0, "grid_tracks": 3, "skin": "baseline"},
    "plans-index__narrow": {"overflow": 0, "grid_tracks": 1, "skin": "baseline"},
    "plan-detail__desktop": {"overflow": 0, "grid_tracks": 2, "skin": "baseline"},
    "plan-detail__narrow": {"overflow": 0, "grid_tracks": 1, "skin": "baseline"},
}
LIGHT_DESKTOP_PNGS = {
    "plans-index": {"near_white_ratio": 0.51, "mean_luma": 0.86},
    "plan-detail": {"near_white_ratio": 0.90, "mean_luma": 0.92},
}


def _dark_render(skin="dark-terminal"):
    return {
        "plans-index__desktop__dark": {"overflow": 0, "grid_tracks": 3, "skin": skin},
        "plans-index__narrow__dark": {"overflow": 0, "grid_tracks": 1, "skin": skin},
        "plan-detail__desktop__dark": {"overflow": 0, "grid_tracks": 2, "skin": skin},
        "plan-detail__narrow__dark": {"overflow": 0, "grid_tracks": 1, "skin": skin},
    }


class DarkSkinScoringTest(unittest.TestCase):
    def test_healthy_dark_render_does_not_trip_flag(self):
        # Dark canvas + dark cards => near-white ~ 0, low luma. The flag must
        # stay False and the dark capture must be recorded so the judge sees it.
        dark_pngs = {
            "plans-index": {"near_white_ratio": 0.0, "mean_luma": 0.10},
            "plan-detail": {"near_white_ratio": 0.004, "mean_luma": 0.14},
        }
        s = _mod.measure_skin(LIGHT_RENDER, LIGHT_DESKTOP_PNGS, _dark_render(), dark_pngs)
        self.assertTrue(s["dark_captured"])
        self.assertIs(s["dark_white_island_flag"], False)
        self.assertEqual(s["dark_data_skin_index"], "dark-terminal")
        self.assertEqual(s["dark_data_skin_detail"], "dark-terminal")
        self.assertLess(s["dark_desktop_index_mean_luma"], 0.5)

    def test_white_island_regression_trips_flag(self):
        # The iteration-16 regression: white #fff card islands on the dark page
        # push near-white into the double digits. Either surface tripping is
        # enough — the flag is an OR across the captured dark desktop pages.
        dark_pngs = {
            "plans-index": {"near_white_ratio": 0.0, "mean_luma": 0.10},
            "plan-detail": {"near_white_ratio": 0.207, "mean_luma": 0.29},
        }
        s = _mod.measure_skin(LIGHT_RENDER, LIGHT_DESKTOP_PNGS, _dark_render(), dark_pngs)
        self.assertIs(s["dark_white_island_flag"], True)
        self.assertGreater(s["dark_desktop_detail_near_white_ratio"],
                           s["dark_white_island_threshold"])

    def test_boundary_at_threshold_is_not_a_regression(self):
        # Exactly at the threshold is healthy; strictly above trips. Guards the
        # comparison operator against an off-by-one flip.
        thr = _mod.DARK_WHITE_ISLAND_THRESHOLD
        at = {"plans-index": {"near_white_ratio": thr, "mean_luma": 0.12},
              "plan-detail": {"near_white_ratio": thr, "mean_luma": 0.12}}
        s = _mod.measure_skin(LIGHT_RENDER, LIGHT_DESKTOP_PNGS, _dark_render(), at)
        self.assertIs(s["dark_white_island_flag"], False)

    def test_dark_half_is_additive_light_metrics_unchanged(self):
        # Whatever the dark half reports, the light half of the skin block must
        # be byte-identical — the dark pass is additive, never a rewrite.
        healthy = {"plans-index": {"near_white_ratio": 0.0, "mean_luma": 0.10},
                   "plan-detail": {"near_white_ratio": 0.0, "mean_luma": 0.10}}
        regressed = {"plans-index": {"near_white_ratio": 0.6, "mean_luma": 0.7},
                     "plan-detail": {"near_white_ratio": 0.6, "mean_luma": 0.7}}
        a = _mod.measure_skin(LIGHT_RENDER, LIGHT_DESKTOP_PNGS, _dark_render(), healthy)
        b = _mod.measure_skin(LIGHT_RENDER, LIGHT_DESKTOP_PNGS, _dark_render(), regressed)
        for k in ("overflow_index_narrow", "overflow_detail_narrow",
                  "grid_tracks_narrow", "grid_tracks_desktop",
                  "desktop_index_near_white_ratio", "desktop_detail_near_white_ratio"):
            self.assertEqual(a[k], b[k], f"light metric {k} moved with the dark half")

    def test_absent_dark_capture_scores_null_not_false(self):
        # A legacy/flat capture dir has no dark render — dark_captured False and
        # the flag is None (unknown), never a spurious True or a hard fail.
        empty_pngs = {"plans-index": None, "plan-detail": None}
        s = _mod.measure_skin(LIGHT_RENDER, LIGHT_DESKTOP_PNGS, {}, empty_pngs)
        self.assertIs(s["dark_captured"], False)
        self.assertIsNone(s["dark_white_island_flag"])
        # Light half still fully populated.
        self.assertEqual(s["overflow_index_narrow"], 0)
        self.assertEqual(s["grid_tracks_desktop"], 2)


class ScreenshotPathFallbackTest(unittest.TestCase):
    def test_light_falls_back_to_flat_layout_when_no_skin_axis(self):
        # An old capture dir wrote screenshots/desktop/plans-index.png (no skin
        # axis). For the light skin, screenshot_path must fall back to it so the
        # tool can still measure a pre-ka-0000000260 capture.
        import tempfile
        from pathlib import Path
        with tempfile.TemporaryDirectory() as d:
            cap = Path(d)
            flat = cap / "screenshots" / "desktop"
            flat.mkdir(parents=True)
            (flat / "plans-index.png").write_bytes(b"x")
            p = _mod.screenshot_path(cap, "light", "desktop", "plans-index")
            self.assertEqual(p, flat / "plans-index.png")

    def test_light_prefers_skin_axis_when_present(self):
        import tempfile
        from pathlib import Path
        with tempfile.TemporaryDirectory() as d:
            cap = Path(d)
            skinned = cap / "screenshots" / "light" / "desktop"
            skinned.mkdir(parents=True)
            (skinned / "plans-index.png").write_bytes(b"x")
            # Also create the flat one to prove the skin-axis path wins.
            flat = cap / "screenshots" / "desktop"
            flat.mkdir(parents=True)
            (flat / "plans-index.png").write_bytes(b"x")
            p = _mod.screenshot_path(cap, "light", "desktop", "plans-index")
            self.assertEqual(p, skinned / "plans-index.png")

    def test_dark_never_falls_back_to_flat_layout(self):
        # The flat layout was light-only; a dark path must never resolve to it
        # (that would misread a light PNG as the dark capture).
        import tempfile
        from pathlib import Path
        with tempfile.TemporaryDirectory() as d:
            cap = Path(d)
            p = _mod.screenshot_path(cap, "dark", "desktop", "plans-index")
            self.assertEqual(p, cap / "screenshots" / "dark" / "desktop" / "plans-index.png")


if __name__ == "__main__":
    unittest.main()
