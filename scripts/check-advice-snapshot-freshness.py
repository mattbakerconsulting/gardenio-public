#!/usr/bin/env python3
"""check-advice-snapshot-freshness.py — advice-snapshot staleness ALARM.

Root-cause fix for bug ka-0000004917 (child of ka-0000006923 / ka-0000004418).

## The problem this closes

The advice-catalog budget guard lives in
gardenio/controller/cmd/gb/real_catalog_budget_test.go. It audits a FROZEN
fixture (testdata/advice_catalog_snapshot.json) so CI stays hermetic — CI has
no beads daemon — and a sidecar
(testdata/advice_catalog_snapshot_meta.json) records WHEN the fixture was last
synced with the live corpus. TestRealCatalogSnapshotFreshness then bounds how
far the fixture may drift: a SOFT ceiling (7d) that only `t.Logf`s, and a HARD
ceiling (14d) that `t.Errorf`s and reds CI on EVERY gardenio MR fleet-wide.

That soft/hard split is well designed on paper but the SOFT stage has no
reader. `t.Logf` inside a PASSING test is invisible: `go test` prints nothing
for passing packages without -v, and CI shows a green check. So the guard has
no observable early-warning stage at all — it goes 0 -> fleet-wide P0 (every MR
red, and the release lane blocked because release.sh then ran CI preflight
BEFORE tagging) with no intermediate anyone can see. That is exactly how ka-0000004418
fired: the fleet went ~5h with 25 commits queued and unreleasable.

## What this script does

It converts that invisible soft stage into a LOUD, observable signal a full
week BEFORE the hard per-MR cliff. It reads the committed meta sidecar, computes
the fixture age from `captured_at`, and:

  exit 0  age < soft ceiling           -> FRESH, green
  exit 2  age >= soft ceiling (STALE)  -> ALARM: refresh the snapshot now, well
                                          before the hard 14d ceiling reds every
                                          MR fleet-wide
  exit 1  operational failure (missing/unreadable/malformed meta) -> a staleness
                                          alarm must FAIL LOUD, never silently
                                          pass on an error

It is PURELY HERMETIC — committed file + wall clock, no daemon, no creds — so
unlike the sibling drift alarms (gardener-prompt-drift, canonical-prod-drift) it
needs no credentialed pipeline and can run directly in a scheduled job. The
enforcement is a POST-merge scheduled ALARM (allow_failure yellow), NOT a merge
gate: the hard CI cliff in real_catalog_budget_test.go is deliberately left
intact (weakening it is how "green is a lie" came back — ka-0000006923), so this
is the missing OBSERVABLE member of that guard, per the bug's Option (a).

Exit status:
  0  fresh (age < --soft-age-hours).
  2  STALE (age >= --soft-age-hours) — alarm; refresh with -update-advice-snapshot.
  1  operational failure (meta missing / bad JSON / bad timestamp).

Usage:
  ./scripts/check-advice-snapshot-freshness.py [--repo-root .] [--json] \
      [--soft-age-hours 168] [--hard-age-hours 336]

Companion test: scripts/check-advice-snapshot-freshness_test.py
Bug: ka-0000004917. Operator: oidc:matthew.baker@acme.example.
"""
from __future__ import annotations

import argparse
import datetime as _dt
import json
import os
import sys

# Default staleness ceilings, in HOURS. These MIRROR the Go constants
# softSnapshotAge (7d) and hardSnapshotAge (14d) in
# gardenio/controller/cmd/gb/real_catalog_budget_test.go. The alarm reddens at
# the SOFT ceiling so a scheduled pipeline surfaces the staleness a full week
# before the HARD ceiling reds every MR. Keep these in step with the Go source;
# the companion test asserts the defaults so a silent drift of the two is caught.
DEFAULT_SOFT_AGE_HOURS = 7 * 24
DEFAULT_HARD_AGE_HOURS = 14 * 24

# Meta sidecar path, relative to repo root.
META_REL_PATH = os.path.join(
    "gardenio", "controller", "cmd", "gb", "testdata",
    "advice_catalog_snapshot_meta.json",
)


class OperationalError(Exception):
    """Raised for a fail-loud operational problem (exit 1)."""


def _parse_captured_at(raw: str) -> _dt.datetime:
    """Parse an RFC3339 `captured_at` string into an aware UTC datetime.

    The writer (writeSnapshotMeta in real_catalog_budget_test.go) emits
    time.Now().UTC().Format(time.RFC3339), i.e. a `...Z` suffix at second
    precision. Accept the `Z` form (and any explicit offset) robustly.
    """
    s = raw.strip()
    if not s:
        raise OperationalError("meta captured_at is empty")
    # Python's fromisoformat accepts +00:00 but historically not a bare `Z`;
    # normalize `Z` to +00:00 for portability across interpreter versions.
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        parsed = _dt.datetime.fromisoformat(s)
    except ValueError as exc:
        raise OperationalError(
            f"meta captured_at={raw!r} is not RFC3339: {exc}"
        ) from exc
    if parsed.tzinfo is None:
        # No offset at all — treat as UTC but flag it, since the writer always
        # stamps an offset. This keeps the age math well-defined.
        parsed = parsed.replace(tzinfo=_dt.timezone.utc)
    return parsed.astimezone(_dt.timezone.utc)


def load_meta(repo_root: str) -> dict:
    """Read and parse the committed meta sidecar. Fail loud on any problem."""
    path = os.path.join(repo_root, META_REL_PATH)
    try:
        with open(path, "r", encoding="utf-8") as fh:
            raw = fh.read()
    except OSError as exc:
        raise OperationalError(f"cannot read snapshot meta {path}: {exc}") from exc
    try:
        meta = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise OperationalError(f"snapshot meta {path} is not valid JSON: {exc}") from exc
    if not isinstance(meta, dict):
        raise OperationalError(f"snapshot meta {path} is not a JSON object")
    if "captured_at" not in meta:
        raise OperationalError(f"snapshot meta {path} has no captured_at field")
    return meta


def evaluate(
    meta: dict,
    now: _dt.datetime,
    soft_age_hours: float,
    hard_age_hours: float,
) -> dict:
    """Compute the freshness verdict from a parsed meta dict and a wall clock.

    Returns a result dict; raises OperationalError on a fail-loud problem. The
    verdict is FRESH (exit 0) below the soft ceiling and STALE (exit 2) at or
    above it — the alarm deliberately reddens at the SOFT ceiling so the signal
    lands a week before the HARD per-MR cliff.
    """
    captured = _parse_captured_at(str(meta["captured_at"]))
    age = now - captured
    age_hours = age.total_seconds() / 3600.0
    # A negative age means the fixture is stamped in the future — a clock skew or
    # a hand-edited timestamp. Treat it as fresh (0h) rather than crashing, but
    # surface the raw value so it is not silently swallowed.
    effective_hours = max(age_hours, 0.0)
    stale = effective_hours >= soft_age_hours
    past_hard = effective_hours >= hard_age_hours
    return {
        "verdict": "STALE" if stale else "FRESH",
        "captured_at": captured.isoformat(),
        "age_hours": round(age_hours, 2),
        "age_days": round(age_hours / 24.0, 2),
        "soft_age_hours": soft_age_hours,
        "hard_age_hours": hard_age_hours,
        "past_hard_ceiling": past_hard,
        "advice_bead_count": meta.get("advice_bead_count"),
        "fleet_archetype_count": meta.get("fleet_archetype_count"),
    }


def _render_human(result: dict) -> str:
    lines = []
    age_days = result["age_days"]
    soft_days = result["soft_age_hours"] / 24.0
    hard_days = result["hard_age_hours"] / 24.0
    if result["verdict"] == "FRESH":
        lines.append(
            f"advice snapshot FRESH: captured {age_days}d ago "
            f"(< {soft_days:g}d soft ceiling; hard cliff at {hard_days:g}d)."
        )
        return "\n".join(lines)
    # STALE
    if result["past_hard_ceiling"]:
        lines.append(
            f"advice snapshot STALE (PAST HARD CEILING): captured {age_days}d ago "
            f"(>= {hard_days:g}d). Every gardenio MR's CI/Test is ALREADY red on "
            f"TestRealCatalogSnapshotFreshness, and the release lane is blocked."
        )
    else:
        lines.append(
            f"advice snapshot STALE: captured {age_days}d ago "
            f"(>= {soft_days:g}d soft ceiling, still under the {hard_days:g}d hard "
            f"cliff). This is the OBSERVABLE early warning: refresh now, before "
            f"the hard ceiling reds every gardenio MR fleet-wide (ka-0000004418)."
        )
    lines.append("")
    lines.append("Refresh the fixture (needs BEADS_HTTP_ADDR + BEADS_AUTH_TOKEN):")
    lines.append(
        "    cd gardenio/controller && go test ./cmd/gb -run TestRealCatalog "
        "-update-advice-snapshot"
    )
    lines.append(
        "then commit the regenerated testdata/. A refresh that reddens the budget "
        "test is revealing PRE-EXISTING corpus drift (ka-0000006923), not a "
        "regression you introduced — trim the corpus (gb advice audit) or move the "
        "debt register, do not re-freeze a stale fixture to dodge the red."
    )
    return "\n".join(lines)


def run(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description="Advice-snapshot staleness alarm (ka-0000004917).",
    )
    parser.add_argument(
        "--repo-root", default=".",
        help="Repo root the meta sidecar is resolved against (default: .).",
    )
    parser.add_argument(
        "--soft-age-hours", type=float, default=DEFAULT_SOFT_AGE_HOURS,
        help=f"Age at/above which the alarm reddens, in hours (default {DEFAULT_SOFT_AGE_HOURS}).",
    )
    parser.add_argument(
        "--hard-age-hours", type=float, default=DEFAULT_HARD_AGE_HOURS,
        help=f"The per-MR hard-cliff age, in hours, reported for context (default {DEFAULT_HARD_AGE_HOURS}).",
    )
    parser.add_argument(
        "--json", action="store_true",
        help="Emit the verdict as a single JSON object on stdout.",
    )
    args = parser.parse_args(argv)

    try:
        meta = load_meta(args.repo_root)
        result = evaluate(
            meta,
            _dt.datetime.now(_dt.timezone.utc),
            args.soft_age_hours,
            args.hard_age_hours,
        )
    except OperationalError as exc:
        # Fail loud: a staleness alarm never silently passes on an error.
        if args.json:
            print(json.dumps({"verdict": "ERROR", "error": str(exc)}))
        else:
            print(f"advice-snapshot freshness alarm: OPERATIONAL FAILURE: {exc}", file=sys.stderr)
        return 1

    if args.json:
        print(json.dumps(result))
    else:
        out = _render_human(result)
        print(out, file=sys.stderr if result["verdict"] == "STALE" else sys.stdout)

    return 2 if result["verdict"] == "STALE" else 0


def main() -> None:
    sys.exit(run(sys.argv[1:]))


if __name__ == "__main__":
    main()
