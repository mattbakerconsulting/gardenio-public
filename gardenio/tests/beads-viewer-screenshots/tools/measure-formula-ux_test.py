#!/usr/bin/env python3
"""Unit tests for measure-formula-ux.py's execution-honesty frontier truth.

Regression guard for ka-0000004230: the harness used to compute frontier
truth as in_progress-only children, while the molecule view
(molecule_view.go:274-291) defines the frontier as "the in_progress rows,
else the dependency-aware READY rows". A live molecule paused between agent
runs (0 in_progress, some closed + some open) therefore reported
frontier_truth=0 and a nonzero frontier_delta, false-flagging the headline
Execution dimension. The fix landed in MR 5691; these tests pin the extracted
molecule_frontier_ids() rule so it can't silently regress to in_progress-only.
"""
import os
import unittest
from importlib import util as _imputil

HERE = os.path.dirname(os.path.abspath(__file__))
TOOL_PATH = os.path.join(HERE, "measure-formula-ux.py")

_spec = _imputil.spec_from_file_location("measure_formula_ux", TOOL_PATH)
_mod = _imputil.module_from_spec(_spec)
_spec.loader.exec_module(_mod)  # type: ignore[union-attr]


def _linear_recipe(ids):
    """Build a linear-chain recipe: step[i] depends_on step[i-1]."""
    steps = []
    for i, sid in enumerate(ids):
        steps.append({"id": sid,
                      "depends_on": [ids[i - 1]] if i > 0 else None})
    return steps


def _children(recipe, status_by_step):
    """One child bead per step, status from status_by_step (default open)."""
    sc = {}
    for s in recipe:
        sid = s["id"]
        sc[sid] = [{"status": status_by_step.get(sid, "open")}]
    return sc


# The Golden bug-fix-gates recipe (ka-0000001189) — a linear 11-step chain.
GOLDEN = _linear_recipe([
    "check-existing-work", "investigate-fix", "e2e-test", "draft-mr",
    "before-screenshot", "after-screenshot", "visual-diff-review",
    "mark-mr-ready", "tag-reviewer", "iteration-cleanup",
    "conditional-reinvoke",
])


class FrontierIdsTest(unittest.TestCase):
    def test_paused_molecule_uses_dependency_ready_rows(self):
        # ka-0000004230 repro (iteration 2026-07-18T18:23:00Z): molecule
        # ka-0000001566 had check-existing-work + tag-reviewer closed, the
        # rest open, 0 in_progress. The view highlighted the two
        # dependency-ready steps; the old in_progress-only count reported 0.
        sc = _children(GOLDEN, {"check-existing-work": "closed",
                                "tag-reviewer": "closed"})
        frontier = _mod.molecule_frontier_ids(GOLDEN, sc, {})
        self.assertEqual(frontier, {"investigate-fix", "iteration-cleanup"})

    def test_fresh_molecule_frontier_is_first_step(self):
        # Nothing done yet: only the dep-free first step is ready.
        sc = _children(GOLDEN, {})
        frontier = _mod.molecule_frontier_ids(GOLDEN, sc, {})
        self.assertEqual(frontier, {"check-existing-work"})

    def test_mid_run_molecule_uses_in_progress_rows_only(self):
        # An in_progress step suppresses the ready-row fallback entirely.
        sc = _children(GOLDEN, {"check-existing-work": "closed",
                                "investigate-fix": "in_progress"})
        frontier = _mod.molecule_frontier_ids(GOLDEN, sc, {})
        self.assertEqual(frontier, {"investigate-fix"})

    def test_completed_molecule_has_empty_frontier(self):
        sc = _children(GOLDEN, {s["id"]: "closed" for s in GOLDEN})
        frontier = _mod.molecule_frontier_ids(GOLDEN, sc, {})
        self.assertEqual(frontier, set())

    def test_ready_row_demoted_when_dep_not_terminal(self):
        # investigate-fix's dep (check-existing-work) is still open, so
        # investigate-fix is pending, not ready — only the first step fronts.
        sc = _children(GOLDEN, {})
        frontier = _mod.molecule_frontier_ids(GOLDEN, sc, {})
        self.assertNotIn("investigate-fix", frontier)

    def test_missing_dep_row_stays_optimistic(self):
        # A dep that isn't a recipe row (inherited/external) can't be verified,
        # so the view keeps the step ready rather than demoting it.
        steps = [{"id": "only-step", "depends_on": ["external-thing"]}]
        sc = {"only-step": [{"status": "open"}]}
        frontier = _mod.molecule_frontier_ids(steps, sc, {})
        self.assertEqual(frontier, {"only-step"})

    def test_not_poured_step_never_fronts(self):
        # A step with no child beads is not_poured — never a frontier row, even
        # when its deps are done (the view only fronts in_progress/open rows).
        steps = _linear_recipe(["a", "b"])
        sc = {"a": [{"status": "closed"}]}  # b never poured
        frontier = _mod.molecule_frontier_ids(steps, sc, {})
        self.assertEqual(frontier, set())

    def test_step_id_fallback_key(self):
        # Recipe steps may key off step_id instead of id.
        steps = [{"step_id": "s1", "depends_on": None},
                 {"step_id": "s2", "depends_on": ["s1"]}]
        sc = {"s1": [{"status": "closed"}], "s2": [{"status": "open"}]}
        frontier = _mod.molecule_frontier_ids(steps, sc, {})
        self.assertEqual(frontier, {"s2"})

    def test_frontier_delta_zero_for_truthful_paused_view(self):
        # The whole point: frontier_truth (dep-aware) matches the number of
        # frontier rows the view renders, so frontier_delta == 0.
        sc = _children(GOLDEN, {"check-existing-work": "closed",
                                "tag-reviewer": "closed"})
        frontier_truth = len(_mod.molecule_frontier_ids(GOLDEN, sc, {}))
        view_frontier_rows = 2  # the two rows the view stamps .Frontier on
        self.assertEqual(frontier_truth - view_frontier_rows, 0)

    def test_applied_vars_substituted_in_dep_lookup(self):
        # depends_on can carry a {{VAR}} the pour substituted into row ids; the
        # frontier walk substitutes applied vars before the dep lookup.
        steps = [
            {"id": "seed-PE-1", "depends_on": None},
            {"id": "build-PE-1", "depends_on": ["seed-{{TICKET}}"]},
        ]
        sc = {"seed-PE-1": [{"status": "closed"}],
              "build-PE-1": [{"status": "open"}]}
        frontier = _mod.molecule_frontier_ids(steps, sc, {"TICKET": "PE-1"})
        self.assertEqual(frontier, {"build-PE-1"})


class MapStepChildrenTest(unittest.TestCase):
    """Guard the child->step join (iteration 2026-07-21T07:23:00Z): the harness
    used to fetch children via `kd dep children --json` — a THIN projection with
    no .fields — so every child's source_step_id read as null and the join
    silently degraded to title-matching. On E2E-Spec-Fixer (ka-0000001506) the
    poured child 'push-and-verify' had a title that drifted from its recipe
    template, so title-matching dropped it and undercounted the frontier
    (frontier_truth 4 vs an honest view's 5), false-flagging Execution. The fix
    fetches full rows via `kd list --parent` and joins on source_step_id."""

    def test_source_step_id_wins_over_drifted_title(self):
        # The exact repro: recipe title carries a {{SPEC}} var + an extra
        # clause the poured child dropped; only source_step_id joins it.
        recipe = [
            {"id": "push-and-verify",
             "title": "{{SPEC}}: Push, open the MR, verify spec runs in CI"},
        ]
        children = [
            {"id": "gd-child", "status": "open",
             "title": "spec.ts: Push, verify spec runs in CI shards",
             "fields": {"source_step_id": "push-and-verify"}},
        ]
        sc = _mod.map_step_children(children, recipe, {"SPEC": "spec.ts"})
        self.assertIn("push-and-verify", sc)
        self.assertEqual(len(sc["push-and-verify"]), 1)

    def test_title_fallback_when_source_step_id_absent(self):
        # Back-compat: an id-less child still joins by substituted title.
        recipe = [{"id": "s1", "title": "{{SPEC}}: run"}]
        children = [{"id": "c1", "status": "open", "title": "x.ts: run",
                     "fields": {}}]
        sc = _mod.map_step_children(children, recipe, {"SPEC": "x.ts"})
        self.assertEqual(list(sc), ["s1"])

    def test_drifted_title_without_id_is_dropped(self):
        # Demonstrates WHY the id join matters: no id + drifted title => the
        # child cannot be mapped at all (the old undercount path).
        recipe = [{"id": "s1", "title": "{{SPEC}}: Push, open the MR, verify"}]
        children = [{"id": "c1", "status": "open",
                     "title": "x.ts: Push, verify in CI shards", "fields": {}}]
        sc = _mod.map_step_children(children, recipe, {"SPEC": "x.ts"})
        self.assertEqual(sc, {})

    def test_edgeless_recipe_frontier_counts_all_open(self):
        # The E2E-Spec-Fixer shape end-to-end: an edge-less 7-step recipe with
        # 2 closed + 5 open children (all id-joined) fronts all 5 open rows,
        # matching the view — frontier_delta 0.
        ids = ["understand-spec", "run-and-capture", "flake-analysis",
               "fix-and-stabilize", "push-and-verify", "observe-ci-report",
               "mr-cleanup"]
        recipe = [{"id": i, "title": f"{{{{SPEC}}}}: {i}", "depends_on": None}
                  for i in ids]
        closed = {"understand-spec", "run-and-capture"}
        children = [
            {"id": f"kd-{i}", "status": "closed" if i in closed else "open",
             "title": f"drifted {i}",  # titles drift; id must win
             "fields": {"source_step_id": i}}
            for i in ids]
        sc = _mod.map_step_children(children, recipe, {"SPEC": "spec.ts"})
        frontier = _mod.molecule_frontier_ids(recipe, sc, {"SPEC": "spec.ts"})
        self.assertEqual(len(frontier), 5)


class LeakSignatureTest(unittest.TestCase):
    """Guard the LEAK_RE signature (ka-0000003537): a real Go-template ACTION
    is a leak; a formula's own unbound {{VAR}} placeholder is legitimate recipe
    content and must NOT be counted, regardless of var-name casing."""

    def _leaks(self, chrome_text):
        return len(_mod.LEAK_RE.findall(chrome_text))

    def test_unbound_var_placeholders_are_not_leaks(self):
        # Both casings the recipe surface legitimately renders.
        self.assertEqual(self._leaks("Tag release {{version}}"), 0)
        self.assertEqual(self._leaks("{{TICKET}}: Gather ticket data"), 0)
        self.assertEqual(self._leaks("push {{MR_IID}} for {{prev_version}}"), 0)

    def test_real_template_actions_are_leaks(self):
        self.assertEqual(self._leaks("{{.Title}}"), 1)
        self.assertEqual(self._leaks("{{ .CompletedSteps }} / 9"), 1)
        self.assertEqual(self._leaks("{{if .Steps}}x{{end}}"), 2)
        self.assertEqual(self._leaks("{{range .Steps}}"), 1)
        self.assertEqual(self._leaks("owner {{$.BasePath}}"), 1)
        self.assertEqual(self._leaks("<no value>"), 1)

    def test_leak_count_is_var_casing_independent(self):
        # The old [.a-z] signature made this asymmetric; both must be 0 now.
        lower = self._leaks("A {{version}} B")
        upper = self._leaks("A {{VERSION}} B")
        self.assertEqual(lower, upper)
        self.assertEqual(lower, 0)


if __name__ == "__main__":
    unittest.main()
