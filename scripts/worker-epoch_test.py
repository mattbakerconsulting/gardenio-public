#!/usr/bin/env python3
"""worker-epoch_test.py — hermetic unit suite for worker-epoch.py.

Guards the pure surface of the worker-epoch qualification suite (W4,
task ka-0000002814): catalog validation (bounded 5-8, unique ids, required
fields, category coverage, well-formed outcome_boundary), config-snapshot
normalization + stable epoch_id, result-skeleton emission, the deterministic
outcome-boundary grading rubric, and cross-epoch comparison + subtraction
candidates. Also loads the LIVE committed catalog and asserts it validates and
stays representative. Stdlib only — no network, no `kd`, no clock, no randomness.
Imports the tool by path so it runs from anywhere.
"""
import copy
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
    "worker_epoch", os.path.join(_HERE, "worker-epoch.py"))
we = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = we
_SPEC.loader.exec_module(we)

CATALOG_PATH = os.path.join(_HERE, "worker-epoch-journeys.json")


def _valid_journey(jid, category, proof_required=False, max_relays=0):
    return {
        "id": jid,
        "category": category,
        "title": f"title {jid}",
        "description": f"description {jid}",
        "outcome_boundary": {
            "job_closed": True,
            "proof_required": proof_required,
            "max_human_relays": max_relays,
            "assertions": ["an assertion"],
        },
        "scaffolding": [f"kd-{jid}"],
    }


def _min_valid_catalog():
    # Exactly the required categories, 5 journeys -> at the lower bound.
    cats = ["thread", "crew", "gardener", "release", "decision"]
    return {
        "version": 1,
        "journeys": [_valid_journey(f"j{i}", c) for i, c in enumerate(cats)],
    }


def _write(tmp, obj, name="catalog.json"):
    p = os.path.join(tmp, name)
    with open(p, "w", encoding="utf-8") as fh:
        json.dump(obj, fh)
    return p


class ValidateCatalog(unittest.TestCase):
    def test_minimal_catalog_valid(self):
        self.assertEqual(we.validate_catalog(_min_valid_catalog()), [])

    def test_no_journeys_array(self):
        errs = we.validate_catalog({"version": 1})
        self.assertTrue(any("no `journeys`" in e for e in errs))

    def test_too_few_journeys(self):
        cat = _min_valid_catalog()
        cat["journeys"] = cat["journeys"][:4]  # only 4 -> below MIN, drops a category
        errs = we.validate_catalog(cat)
        self.assertTrue(any("outside the bounded range" in e for e in errs))

    def test_too_many_journeys(self):
        cat = _min_valid_catalog()
        # pad to 9 with extra valid journeys in an existing category
        for i in range(4):
            cat["journeys"].append(_valid_journey(f"extra{i}", "crew"))
        self.assertEqual(len(cat["journeys"]), 9)
        errs = we.validate_catalog(cat)
        self.assertTrue(any("outside the bounded range" in e for e in errs))

    def test_duplicate_id(self):
        cat = _min_valid_catalog()
        cat["journeys"][1]["id"] = cat["journeys"][0]["id"]
        errs = we.validate_catalog(cat)
        self.assertTrue(any("duplicate id" in e for e in errs))

    def test_missing_required_category(self):
        cat = _min_valid_catalog()
        cat["journeys"][0]["category"] = "crew"  # now no 'thread'
        errs = we.validate_catalog(cat)
        self.assertTrue(any("required categories" in e and "thread" in e for e in errs))

    def test_missing_title(self):
        cat = _min_valid_catalog()
        del cat["journeys"][0]["title"]
        errs = we.validate_catalog(cat)
        self.assertTrue(any("`title`" in e for e in errs))

    def test_bad_outcome_boundary_types(self):
        cat = _min_valid_catalog()
        cat["journeys"][0]["outcome_boundary"]["job_closed"] = "yes"
        cat["journeys"][0]["outcome_boundary"]["max_human_relays"] = -1
        cat["journeys"][1]["outcome_boundary"]["assertions"] = []
        errs = we.validate_catalog(cat)
        self.assertTrue(any("job_closed must be a bool" in e for e in errs))
        self.assertTrue(any("max_human_relays must be a non-negative int" in e for e in errs))
        self.assertTrue(any("assertions must be a non-empty list" in e for e in errs))

    def test_max_human_relays_bool_rejected(self):
        # bool is a subclass of int — must not sneak through as a relay count.
        cat = _min_valid_catalog()
        cat["journeys"][0]["outcome_boundary"]["max_human_relays"] = True
        errs = we.validate_catalog(cat)
        self.assertTrue(any("max_human_relays" in e for e in errs))

    def test_missing_outcome_boundary(self):
        cat = _min_valid_catalog()
        del cat["journeys"][0]["outcome_boundary"]
        errs = we.validate_catalog(cat)
        self.assertTrue(any("missing `outcome_boundary`" in e for e in errs))


class SnapshotAndEpochId(unittest.TestCase):
    def test_normalization_strips_and_lowercases(self):
        snap = we.snapshot_config("  Claude-Opus-4-8 ", "HIGH", "1.2.3", "2026.241.15")
        self.assertEqual(snap["model"], "claude-opus-4-8")
        self.assertEqual(snap["effort"], "high")

    def test_identical_configs_same_epoch_id(self):
        a = we.build_snapshot_record("claude-opus-4-8", "high", "1.2.3", "t1")
        b = we.build_snapshot_record("CLAUDE-OPUS-4-8", " high ", "1.2.3", "t1")
        self.assertEqual(a["epoch_id"], b["epoch_id"])

    def test_flip_changes_epoch_id(self):
        base = we.build_snapshot_record("claude-opus-4-8", "high", "1.2.3", "t1")
        for changed in (
            we.build_snapshot_record("claude-fable-5", "high", "1.2.3", "t1"),
            we.build_snapshot_record("claude-opus-4-8", "xhigh", "1.2.3", "t1"),
            we.build_snapshot_record("claude-opus-4-8", "high", "1.2.4", "t1"),
            we.build_snapshot_record("claude-opus-4-8", "high", "1.2.3", "t2"),
        ):
            self.assertNotEqual(base["epoch_id"], changed["epoch_id"])

    def test_epoch_id_deterministic_and_short(self):
        snap = we.snapshot_config("m", "e", "c", "i")
        self.assertEqual(we.epoch_id(snap), we.epoch_id(snap))
        self.assertEqual(len(we.epoch_id(snap)), 12)

    def test_resolve_dimension_precedence(self):
        os.environ.pop("WE_TEST_DIM", None)
        self.assertEqual(we.resolve_dimension("flag", "WE_TEST_DIM"), "flag")
        os.environ["WE_TEST_DIM"] = "envval"
        try:
            self.assertEqual(we.resolve_dimension(None, "WE_TEST_DIM"), "envval")
            self.assertEqual(we.resolve_dimension("flag", "WE_TEST_DIM"), "flag")
        finally:
            del os.environ["WE_TEST_DIM"]
        self.assertEqual(we.resolve_dimension(None, "WE_TEST_DIM"), "unknown")


class ResultSkeleton(unittest.TestCase):
    def test_skeleton_one_row_per_journey(self):
        cat = _min_valid_catalog()
        rec = we.build_snapshot_record("m", "e", "c", "i")
        skel = we.result_skeleton(cat, rec)
        self.assertEqual(len(skel["results"]), len(cat["journeys"]))
        self.assertEqual(skel["epoch_id"], rec["epoch_id"])
        row = skel["results"][0]
        self.assertIsNone(row["grade"])
        self.assertIsNone(row["observed"]["job_closed"])
        self.assertIn("proof_required", row["expected"])


class GradeJourney(unittest.TestCase):
    def _j(self, proof_required=False, max_relays=0):
        return _valid_journey("x", "crew", proof_required, max_relays)

    def test_not_closed_is_fail(self):
        g = we.grade_journey(self._j(), {"job_closed": False})
        self.assertEqual(g, "fail")

    def test_closed_clean_is_pass(self):
        g = we.grade_journey(
            self._j(proof_required=True, max_relays=1),
            {"job_closed": True, "proof_attached": True, "human_relays": 1},
        )
        self.assertEqual(g, "pass")

    def test_missing_required_proof_is_partial(self):
        g = we.grade_journey(
            self._j(proof_required=True),
            {"job_closed": True, "proof_attached": False, "human_relays": 0},
        )
        self.assertEqual(g, "partial")

    def test_too_many_relays_is_partial(self):
        g = we.grade_journey(
            self._j(max_relays=0),
            {"job_closed": True, "proof_attached": True, "human_relays": 2},
        )
        self.assertEqual(g, "partial")

    def test_proof_not_required_ignored(self):
        g = we.grade_journey(
            self._j(proof_required=False),
            {"job_closed": True, "proof_attached": False, "human_relays": 0},
        )
        self.assertEqual(g, "pass")


class GradeResults(unittest.TestCase):
    def test_tally_and_ungraded(self):
        cat = _min_valid_catalog()
        # make j0 proof-required to exercise partial
        cat["journeys"][0]["outcome_boundary"]["proof_required"] = True
        rec = we.build_snapshot_record("m", "e", "c", "i")
        skel = we.result_skeleton(cat, rec)
        r = skel["results"]
        r[0]["observed"] = {"job_closed": True, "proof_attached": False, "human_relays": 0}  # partial
        r[1]["observed"] = {"job_closed": True, "proof_attached": True, "human_relays": 0}  # pass
        r[2]["observed"] = {"job_closed": False, "proof_attached": False, "human_relays": 0}  # fail
        # r[3], r[4] left null -> ungraded
        graded = we.grade_results(cat, skel)
        self.assertEqual(graded["tally"]["pass"], 1)
        self.assertEqual(graded["tally"]["partial"], 1)
        self.assertEqual(graded["tally"]["fail"], 1)
        self.assertEqual(graded["tally"]["ungraded"], 2)

    def test_grade_results_does_not_mutate_input(self):
        cat = _min_valid_catalog()
        rec = we.build_snapshot_record("m", "e", "c", "i")
        skel = we.result_skeleton(cat, rec)
        skel["results"][0]["observed"] = {"job_closed": True, "proof_attached": True, "human_relays": 0}
        before = copy.deepcopy(skel)
        we.grade_results(cat, skel)
        self.assertEqual(skel, before)


class CompareEpochs(unittest.TestCase):
    def _graded(self, cat, grades, epoch):
        rows = []
        for j, g in zip(cat["journeys"], grades):
            rows.append({"journey_id": j["id"], "grade": g})
        return {"epoch_id": epoch, "results": rows}

    def test_regression_improvement_unchanged(self):
        cat = _min_valid_catalog()
        base = self._graded(cat, ["pass", "fail", "pass", "partial", "pass"], "e-base")
        cand = self._graded(cat, ["partial", "pass", "pass", "partial", "fail"], "e-cand")
        cmp = we.compare_epochs(cat, base, cand)
        reg_ids = {r["journey_id"] for r in cmp["regressions"]}
        imp_ids = {r["journey_id"] for r in cmp["improvements"]}
        unc_ids = {r["journey_id"] for r in cmp["unchanged"]}
        self.assertEqual(reg_ids, {cat["journeys"][0]["id"], cat["journeys"][4]["id"]})
        self.assertEqual(imp_ids, {cat["journeys"][1]["id"]})
        self.assertEqual(unc_ids, {cat["journeys"][2]["id"], cat["journeys"][3]["id"]})

    def test_subtraction_candidates_only_from_pass(self):
        cat = _min_valid_catalog()
        # j0: pass->pass (steady pass), j1: fail->pass (improved to pass),
        # j2: pass->partial (regressed from pass), j3/j4 partial/partial.
        base = self._graded(cat, ["pass", "fail", "pass", "partial", "partial"], "b")
        cand = self._graded(cat, ["pass", "pass", "partial", "partial", "partial"], "c")
        cmp = we.compare_epochs(cat, base, cand)
        subs = cmp["subtraction_candidates"]
        self.assertIn(cat["journeys"][0]["id"], subs)  # steady pass
        self.assertIn(cat["journeys"][1]["id"], subs)  # improved to pass
        self.assertNotIn(cat["journeys"][2]["id"], subs)  # regressed, not a candidate
        self.assertNotIn(cat["journeys"][3]["id"], subs)  # never pass
        # scaffolding for a candidate is carried through
        self.assertEqual(subs[cat["journeys"][0]["id"]], cat["journeys"][0]["scaffolding"])

    def test_ungraded_side_skipped(self):
        cat = _min_valid_catalog()
        base = self._graded(cat, ["pass", None, "pass", "pass", "pass"], "b")
        cand = self._graded(cat, ["pass", "pass", None, "pass", "pass"], "c")
        cmp = we.compare_epochs(cat, base, cand)
        involved = {r["journey_id"] for r in cmp["regressions"] + cmp["improvements"] + cmp["unchanged"]}
        self.assertNotIn(cat["journeys"][1]["id"], involved)
        self.assertNotIn(cat["journeys"][2]["id"], involved)


class LiveCatalog(unittest.TestCase):
    def setUp(self):
        self.catalog = we.load_catalog(CATALOG_PATH)

    def test_live_catalog_valid(self):
        self.assertEqual(we.validate_catalog(self.catalog), [])

    def test_live_catalog_covers_named_journeys(self):
        ids = {j["id"] for j in self.catalog["journeys"]}
        # The five journeys the task explicitly names must be present.
        for jid in ("thread-qa", "mr-from-bug", "gardener-iteration",
                    "release-watch", "decision-roundtrip"):
            self.assertIn(jid, ids)

    def test_live_catalog_every_journey_has_scaffolding(self):
        # The subtraction pass needs a non-empty scaffolding list per journey.
        for j in self.catalog["journeys"]:
            self.assertTrue(j.get("scaffolding"), f"{j['id']} has no scaffolding")


class CliSmoke(unittest.TestCase):
    def _run(self, argv):
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            rc = we.main(argv)
        return rc, out.getvalue(), err.getvalue()

    def test_validate_live_catalog_exit0(self):
        rc, out, _ = self._run(["validate"])
        self.assertEqual(rc, 0)
        self.assertIn("catalog OK", out)

    def test_validate_bad_catalog_exit2(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = _write(tmp, {"version": 1, "journeys": []})
            rc, _, err = self._run(["validate", "--catalog", p])
            self.assertEqual(rc, 2)
            self.assertIn("INVALID", err)

    def test_snapshot_emits_epoch_id(self):
        rc, out, _ = self._run(
            ["snapshot", "--model", "claude-opus-4-8", "--effort", "high",
             "--coop-version", "1.2.3", "--image-tag", "t1"])
        self.assertEqual(rc, 0)
        rec = json.loads(out)
        self.assertEqual(rec["config"]["model"], "claude-opus-4-8")
        self.assertEqual(len(rec["epoch_id"]), 12)

    def test_skeleton_roundtrips_and_grades(self):
        with tempfile.TemporaryDirectory() as tmp:
            rc, out, _ = self._run(
                ["skeleton", "--model", "m", "--effort", "e",
                 "--coop-version", "c", "--image-tag", "i"])
            self.assertEqual(rc, 0)
            skel = json.loads(out)
            for row in skel["results"]:
                row["observed"] = {"job_closed": True, "proof_attached": True,
                                   "human_relays": 0, "notes": ""}
            p = _write(tmp, skel, "filled.json")
            rc, gout, _ = self._run(["grade", p])
            self.assertEqual(rc, 0)
            graded = json.loads(gout)
            self.assertEqual(graded["tally"]["ungraded"], 0)
            self.assertEqual(graded["tally"]["fail"], 0)


if __name__ == "__main__":
    unittest.main()
