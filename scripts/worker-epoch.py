#!/usr/bin/env python3
"""worker-epoch.py — worker-epoch qualification suite: catalog validation,
config snapshot, result-skeleton emission, deterministic outcome grading, and
cross-epoch comparison.

Workstream W4 (task ka-0000002814, epic ka-0000007152 "Gardenio harness
improvements"). Framework citation: lopopolo/harness-engineering "fixed worker /
epochs" thesis — hold the worker constant, qualify every change to it.

WHAT PROBLEM THIS CLOSES
  Gardenio flips the worker live (model/effort/coop/image) with no standing suite
  proving the flip helped. The fable+xhigh -> opus-4-8+high revert (ka-0000001865)
  was a manual, deterministic trigger with no before/after outcome measurement at
  all. This tool is the fixed CONDITION side of a fixed-worker experiment: a
  bounded catalog of representative WHOLE-JOB journeys (scripts/worker-epoch-
  journeys.json) that every worker epoch runs through, graded at the OUTCOME
  BOUNDARY (job closed? claim-matched proof attached? how many human relays?) and
  explicitly NOT on tokens burned or turns taken.

WHY THIS IS A DISTINCT TOOL (not scripts/model-ab-compare.py / opus5-ab.py)
  model-ab-compare drives a fixed corpus through two models and measures per-TURN
  cost/latency. opus5-ab compares fleet context behaviour before/after a trim.
  Neither measures whether a WHOLE JOB closes. W4 is the whole-job outcome
  comparator that gap leaves open: same journeys, different epoch, graded on
  accepted outcomes.

HERMETIC BY CONSTRUCTION
  Pure stdlib. Every subcommand here is offline: it reads the committed catalog,
  hashes a config tuple, emits JSON. It runs no journeys and touches no daemon,
  no network, no kd/gb, no clock, no randomness — so epoch_id is reproducible and
  the whole surface is unit-testable. Actually RUNNING a journey (spawning a pod,
  driving the job) and observing its outcome-boundary measurements is done out of
  band; this tool defines, snapshots, grades, and compares.

SUBCOMMANDS
  validate   Validate the journeys catalog (bounded 5-8, unique ids, required
             fields, categories, well-formed outcome_boundary). exit 0 ok / 2 bad.
  snapshot   Emit the normalized worker-epoch config snapshot + stable epoch_id.
  skeleton   Emit a result skeleton: the epoch snapshot + one un-graded row per
             journey, ready to fill in observed outcome-boundary measurements and
             store as beads for cross-epoch comparison.
  grade      Grade a filled result file (observed measurements -> pass/partial/
             fail per journey) and print the epoch's pass tally.
  compare    Diff two graded result files journey-by-journey: regressions,
             improvements, unchanged, and subtraction candidates (scaffolding a
             journey stresses that the newer epoch passed clean).

CONFIG SOURCES (snapshot/skeleton)
  Pass the four dimensions explicitly (preferred, reproducible):
      --model --effort --coop-version --image-tag
  Any omitted flag falls back to an env var, then to "unknown":
      GARDENIO_WORKER_MODEL / GARDENIO_WORKER_EFFORT / COOP_VERSION /
      GARDENIO_AGENT_IMAGE_TAG
  These env names are a documented convenience, not load-bearing; qualification
  runs should pass the tuple explicitly so the epoch_id is auditable.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
from typing import Any

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_CATALOG = os.path.join(HERE, "worker-epoch-journeys.json")

# The dimensions that define a worker epoch. Order is fixed so the normalized
# snapshot (and therefore epoch_id) is stable across callers.
EPOCH_DIMENSIONS = ("model", "effort", "coop_version", "image_tag")

# Categories the catalog must cover for the suite to be representative of the
# fleet's dominant whole-job shapes. A catalog missing one of these is not a
# representative suite, so validate() rejects it.
REQUIRED_CATEGORIES = frozenset(
    {"thread", "crew", "gardener", "release", "decision"}
)

MIN_JOURNEYS = 5
MAX_JOURNEYS = 8

GRADES = ("pass", "partial", "fail")


# ─────────────────────────── catalog loading + validation ──────────────────


def load_catalog(path: str = DEFAULT_CATALOG) -> dict[str, Any]:
    """Load and JSON-parse the journeys catalog. Raises on unreadable/malformed
    input — a qualification suite must fail loud, never silently run an empty
    catalog."""
    with open(path, "r", encoding="utf-8") as fh:
        catalog = json.load(fh)
    if not isinstance(catalog, dict):
        raise ValueError("catalog root must be a JSON object")
    return catalog


def validate_catalog(catalog: dict[str, Any]) -> list[str]:
    """Return a list of human-readable problems with the catalog. Empty list ==
    valid. Pure — no I/O."""
    errors: list[str] = []

    journeys = catalog.get("journeys")
    if not isinstance(journeys, list):
        return ["catalog has no `journeys` array"]

    n = len(journeys)
    if n < MIN_JOURNEYS or n > MAX_JOURNEYS:
        errors.append(
            f"journey count {n} outside the bounded range "
            f"[{MIN_JOURNEYS}, {MAX_JOURNEYS}] — keep the suite small"
        )

    seen_ids: set[str] = set()
    categories: set[str] = set()
    for i, j in enumerate(journeys):
        where = f"journey[{i}]"
        if not isinstance(j, dict):
            errors.append(f"{where} is not an object")
            continue
        jid = j.get("id")
        if not isinstance(jid, str) or not jid:
            errors.append(f"{where} missing non-empty `id`")
        else:
            where = f"journey[{i}] {jid}"
            if jid in seen_ids:
                errors.append(f"{where}: duplicate id")
            seen_ids.add(jid)

        for field in ("category", "title", "description"):
            if not isinstance(j.get(field), str) or not j.get(field):
                errors.append(f"{where}: missing non-empty `{field}`")
        cat = j.get("category")
        if isinstance(cat, str) and cat:
            categories.add(cat)

        errors.extend(_validate_outcome_boundary(j.get("outcome_boundary"), where))

    missing = REQUIRED_CATEGORIES - categories
    if missing:
        errors.append(
            "catalog does not cover required categories: "
            + ", ".join(sorted(missing))
        )
    return errors


def _validate_outcome_boundary(ob: Any, where: str) -> list[str]:
    errors: list[str] = []
    if not isinstance(ob, dict):
        return [f"{where}: missing `outcome_boundary` object"]
    if not isinstance(ob.get("job_closed"), bool):
        errors.append(f"{where}: outcome_boundary.job_closed must be a bool")
    if not isinstance(ob.get("proof_required"), bool):
        errors.append(f"{where}: outcome_boundary.proof_required must be a bool")
    mhr = ob.get("max_human_relays")
    if not isinstance(mhr, int) or isinstance(mhr, bool) or mhr < 0:
        errors.append(
            f"{where}: outcome_boundary.max_human_relays must be a non-negative int"
        )
    assertions = ob.get("assertions")
    if not isinstance(assertions, list) or not assertions:
        errors.append(f"{where}: outcome_boundary.assertions must be a non-empty list")
    return errors


# ──────────────────────────── config snapshot + epoch id ───────────────────


def resolve_dimension(flag_value: str | None, env_name: str) -> str:
    """Flag wins, then env var, then the literal 'unknown'. Never guesses."""
    if flag_value:
        return flag_value
    return os.environ.get(env_name) or "unknown"


def snapshot_config(
    model: str, effort: str, coop_version: str, image_tag: str
) -> dict[str, str]:
    """Build the normalized worker-epoch config snapshot. Normalization is what
    makes two identical configs collide on the same epoch_id: values are stripped
    and lowercased so incidental case/whitespace never forks an epoch."""
    raw = {
        "model": model,
        "effort": effort,
        "coop_version": coop_version,
        "image_tag": image_tag,
    }
    return {k: str(raw[k]).strip().lower() for k in EPOCH_DIMENSIONS}


def epoch_id(snapshot: dict[str, str]) -> str:
    """Stable short content-hash of a normalized snapshot. Deterministic: same
    tuple -> same id (no clock, no randomness), a flip in any dimension -> new
    id. 12 hex chars is ample for the handful of live epochs."""
    canonical = json.dumps(
        {k: snapshot.get(k, "unknown") for k in EPOCH_DIMENSIONS},
        sort_keys=True,
        separators=(",", ":"),
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()[:12]


def build_snapshot_record(
    model: str, effort: str, coop_version: str, image_tag: str
) -> dict[str, Any]:
    snap = snapshot_config(model, effort, coop_version, image_tag)
    return {"epoch_id": epoch_id(snap), "config": snap}


# ────────────────────────────── result skeleton ────────────────────────────


def result_skeleton(
    catalog: dict[str, Any], snapshot_record: dict[str, Any]
) -> dict[str, Any]:
    """Emit a fill-in-the-blanks result record: the epoch plus one un-graded row
    per journey. `observed` is null-filled for a grader to complete at the
    outcome boundary; `grade` is left null and computed by `grade` later."""
    rows = []
    for j in catalog.get("journeys", []):
        ob = j.get("outcome_boundary", {})
        rows.append(
            {
                "journey_id": j.get("id"),
                "category": j.get("category"),
                "expected": {
                    "proof_required": ob.get("proof_required"),
                    "max_human_relays": ob.get("max_human_relays"),
                },
                "observed": {
                    "job_closed": None,
                    "proof_attached": None,
                    "human_relays": None,
                    "notes": "",
                },
                "grade": None,
            }
        )
    return {
        "suite": "worker-epoch-qualification",
        "catalog_version": catalog.get("version"),
        "epoch_id": snapshot_record["epoch_id"],
        "config": snapshot_record["config"],
        "results": rows,
    }


# ─────────────────────────── deterministic grading ─────────────────────────


def grade_journey(journey: dict[str, Any], observed: dict[str, Any]) -> str:
    """Map observed outcome-boundary measurements to pass/partial/fail per the
    catalog rubric. Pure and deterministic — grades OUTCOMES, never tokens.

      fail    : the job did not close under the worker's own power.
      partial : closed, but a required proof was missing OR relays exceeded max.
      pass    : closed, proof satisfied, relays within max.
    """
    ob = journey.get("outcome_boundary", {})
    if not observed.get("job_closed"):
        return "fail"

    deficient = False
    if ob.get("proof_required") and not observed.get("proof_attached"):
        deficient = True

    relays = observed.get("human_relays")
    max_relays = ob.get("max_human_relays", 0)
    if isinstance(relays, int) and relays > max_relays:
        deficient = True

    return "partial" if deficient else "pass"


def grade_results(
    catalog: dict[str, Any], results: dict[str, Any]
) -> dict[str, Any]:
    """Grade every row of a filled result file in place-of-copy and return it
    with a tally. Rows whose `observed.job_closed` is still null are left ungraded
    (grade stays None) and counted separately — an unrun journey is not a fail."""
    by_id = {j.get("id"): j for j in catalog.get("journeys", [])}
    tally = {g: 0 for g in GRADES}
    ungraded = 0
    graded_rows = []
    for row in results.get("results", []):
        journey = by_id.get(row.get("journey_id"))
        observed = row.get("observed", {})
        new_row = dict(row)
        if journey is None or observed.get("job_closed") is None:
            new_row["grade"] = None
            ungraded += 1
        else:
            g = grade_journey(journey, observed)
            new_row["grade"] = g
            tally[g] += 1
        graded_rows.append(new_row)
    out = dict(results)
    out["results"] = graded_rows
    out["tally"] = {**tally, "ungraded": ungraded}
    return out


# ───────────────────────────── epoch comparison ────────────────────────────

_GRADE_RANK = {"fail": 0, "partial": 1, "pass": 2}


def compare_epochs(
    catalog: dict[str, Any],
    baseline: dict[str, Any],
    candidate: dict[str, Any],
) -> dict[str, Any]:
    """Diff two graded epoch result files journey-by-journey.

    Returns regressions (grade dropped), improvements (grade rose), unchanged,
    and subtraction_candidates: the scaffolding a journey stresses that the
    CANDIDATE epoch passed cleanly — the input to a subtraction pass asking which
    advice/hook a stronger worker makes redundant. A journey only contributes
    subtraction candidates when it went from not-pass on baseline to pass on
    candidate (the flip is what makes the scaffolding a real candidate), or was
    already pass on both (steady-state redundancy signal), never on a regression.
    """
    scaffolding_by_id = {
        j.get("id"): j.get("scaffolding", []) for j in catalog.get("journeys", [])
    }
    base_grades = {r.get("journey_id"): r.get("grade") for r in baseline.get("results", [])}
    cand_grades = {r.get("journey_id"): r.get("grade") for r in candidate.get("results", [])}

    regressions, improvements, unchanged = [], [], []
    subtraction: dict[str, list[str]] = {}

    for jid in sorted(set(base_grades) | set(cand_grades)):
        b, c = base_grades.get(jid), cand_grades.get(jid)
        if b is None or c is None:
            continue  # ungraded on one side — not comparable
        entry = {"journey_id": jid, "baseline": b, "candidate": c}
        br, cr = _GRADE_RANK[b], _GRADE_RANK[c]
        if cr < br:
            regressions.append(entry)
        elif cr > br:
            improvements.append(entry)
            if c == "pass":
                subtraction[jid] = scaffolding_by_id.get(jid, [])
        else:
            unchanged.append(entry)
            if c == "pass":
                subtraction[jid] = scaffolding_by_id.get(jid, [])

    return {
        "baseline_epoch": baseline.get("epoch_id"),
        "candidate_epoch": candidate.get("epoch_id"),
        "regressions": regressions,
        "improvements": improvements,
        "unchanged": unchanged,
        "subtraction_candidates": subtraction,
    }


# ─────────────────────────────────── CLI ───────────────────────────────────


def _add_config_flags(p: argparse.ArgumentParser) -> None:
    p.add_argument("--catalog", default=DEFAULT_CATALOG, help="path to journeys catalog")
    p.add_argument("--model", default=None)
    p.add_argument("--effort", default=None)
    p.add_argument("--coop-version", default=None, dest="coop_version")
    p.add_argument("--image-tag", default=None, dest="image_tag")


def _resolved_config(args: argparse.Namespace) -> tuple[str, str, str, str]:
    return (
        resolve_dimension(args.model, "GARDENIO_WORKER_MODEL"),
        resolve_dimension(args.effort, "GARDENIO_WORKER_EFFORT"),
        resolve_dimension(args.coop_version, "COOP_VERSION"),
        resolve_dimension(args.image_tag, "GARDENIO_AGENT_IMAGE_TAG"),
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="cmd", required=True)

    p_val = sub.add_parser("validate", help="validate the journeys catalog")
    p_val.add_argument("--catalog", default=DEFAULT_CATALOG)

    p_snap = sub.add_parser("snapshot", help="emit epoch config snapshot + id")
    _add_config_flags(p_snap)

    p_skel = sub.add_parser("skeleton", help="emit a result skeleton for an epoch")
    _add_config_flags(p_skel)

    p_grade = sub.add_parser("grade", help="grade a filled result file")
    p_grade.add_argument("--catalog", default=DEFAULT_CATALOG)
    p_grade.add_argument("results", help="path to a filled result JSON file")

    p_cmp = sub.add_parser("compare", help="diff two graded epoch result files")
    p_cmp.add_argument("--catalog", default=DEFAULT_CATALOG)
    p_cmp.add_argument("baseline", help="graded baseline result JSON")
    p_cmp.add_argument("candidate", help="graded candidate result JSON")

    args = parser.parse_args(argv)

    if args.cmd == "validate":
        catalog = load_catalog(args.catalog)
        errors = validate_catalog(catalog)
        if errors:
            print("catalog INVALID:", file=sys.stderr)
            for e in errors:
                print(f"  - {e}", file=sys.stderr)
            return 2
        print(
            f"catalog OK: {len(catalog['journeys'])} journeys, "
            f"version {catalog.get('version')}"
        )
        return 0

    if args.cmd == "snapshot":
        rec = build_snapshot_record(*_resolved_config(args))
        print(json.dumps(rec, indent=2))
        return 0

    if args.cmd == "skeleton":
        catalog = load_catalog(args.catalog)
        errors = validate_catalog(catalog)
        if errors:
            print("refusing to build skeleton from INVALID catalog:", file=sys.stderr)
            for e in errors:
                print(f"  - {e}", file=sys.stderr)
            return 2
        rec = build_snapshot_record(*_resolved_config(args))
        print(json.dumps(result_skeleton(catalog, rec), indent=2))
        return 0

    if args.cmd == "grade":
        catalog = load_catalog(args.catalog)
        with open(args.results, "r", encoding="utf-8") as fh:
            results = json.load(fh)
        graded = grade_results(catalog, results)
        print(json.dumps(graded, indent=2))
        return 0

    if args.cmd == "compare":
        catalog = load_catalog(args.catalog)
        with open(args.baseline, "r", encoding="utf-8") as fh:
            baseline = json.load(fh)
        with open(args.candidate, "r", encoding="utf-8") as fh:
            candidate = json.load(fh)
        print(json.dumps(compare_epochs(catalog, baseline, candidate), indent=2))
        return 0

    return 1  # unreachable: subparser is required


if __name__ == "__main__":
    sys.exit(main())
