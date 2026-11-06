#!/usr/bin/env python3
"""gardener-autoblock-audit — flag live gardeners that lack an intelligent self-block.

A gardener (type=trigger bead carrying gardener fields) MUST NOT loop unbounded.
Per advice ka-0000000974, every enabled gardener must declare, in its
gardener.stop spec, at least one *progress-aware or goal-completion* auto-block
so it terminates when it stops making forward progress — not only when a pod
crashes (consecutive_failures) or a multi-week wall_clock elapses.

This is the core logic of the gardener-auto-block *sentinel*: run it each cycle,
it prints one JSON object per offender so the calling gardener can file/refresh a
bug per offender.

Classification — a gardener is an OFFENDER when ALL of the following hold:
  * it is enabled (enabled != "false") and status=open
  * it has NO `run_budget` / `max_runs` ceiling, AND
  * its stop.loop_end.any contains NONE of the "intelligent" stop kinds:
      no_progress_for | bead_query | specific_bead_closed | max_runs
A gardener with a finite run_budget OR any intelligent loop_end kind is OK —
those terminate on their own. wall_clock and consecutive_failures alone do NOT
count as intelligent (they are crash/old-age backstops, not wedge detectors).

A gardener is additionally WEAK (warn, not offend) when it has a run_budget or a
bead_query/specific_bead_closed stop but NO progress-aware wedge detector
(no_progress_for + a progress spec). Weak gardeners terminate eventually but
cannot detect "crew keeps getting blocked, nothing is closing".

TWO SAFETY AXES — this tool scores ONE of them (bug ka-0000000849). The
offender/weak/ok verdict above is the progress-aware SELF-BLOCK axis (advice
ka-0000000974's intelligent loop_end). A SECOND, orthogonal axis is the
engine-enforced BACKSTOP: run_budget>0 AND a hard_stop wall_clock in (0,720h] —
the emergency brake `gb gardener status` reports as `bounded`
(bridge.ValidateGardenerBounds). Neither implies the other, so the two tools can
legitimately disagree word-for-word on the SAME gardener: e.g. ka-0000003772 is
self-block `ok` here (run_budget + no_progress_for + progress spec) yet
`bounded: NO` there (its only hard_stop is consecutive_failures, no wall_clock).
That divergence is not a bug in either tool — they measure different properties —
but it used to be invisible, so a surveyor reading one saw a different offender
set than the other. To make the two legible together, EVERY classified line here
now also carries `has_backstop` (the mirror of the status tool's `bounded`), and
`--summary` reports how many classified gardeners lack the backstop. Fixing the
self-block does NOT add a backstop, and vice versa — a fully safe gardener wants
both.

A gardener trigger with NO gardener block at all (no stop / progress /
run_budget — a legacy cron loop) is reported as UNMANAGED: it fires forever
and can never self-terminate. These are the canonical "gardeners that just keep
running" and are reported before offenders/weak (only PHANTOM, below, outranks
them). Two shapes qualify:

  * a `role~gardener` + `action=spawn_agent` trigger (the classic legacy loop), OR
  * a *gardener-shaped* trigger — one whose title or role says "gardener" — that
    self-fires on a cron (`action` in {spawn_agent, apply_formula}) and carries
    NO whole-trigger gate (no top-level `conditions` pre-flight). This second
    shape catches the `apply_formula` "…CDQR Gardener" epic-drivers whose only
    stop is a cron-*entry* pre-flight (which merely skips a day and closes
    nothing) — invisible to the classic detector because their `role` is empty
    and their action is not `spawn_agent`. An event-only dispatcher (no cron)
    or one carrying a top-level `conditions` gate that bounds *every* fire is
    NOT flagged — those are correctly out of scope.

A gardener explicitly labeled `perpetual:true` is a *reviewed* opt-out: a
fleet-service gardener that drains a continuously-refilled queue (e.g. the ready
queue) and has no terminal "done" state, so a self-stop would be wrong. These
are skipped entirely — the label is what makes that classification stick across
cycles instead of being re-flagged forever as UNMANAGED.

Orthogonal to all of the above is the LIVENESS axis (ka-0000004476): the engine
loads trigger beads BY SOURCE LABEL (`trigger:schedule` / `trigger:multi` / …,
see TriggerSourceLabelForFields in bridge/triggers.go). A bead that is enabled
and carries a fire shape (a `cron` field or a non-empty `triggers[]` array) but
NO `trigger:*` label is PHANTOM: it is never evaluated at all, no matter how
perfect its gardener block. The 2026-07-18 cycle found three of these, including
one that had been auto-block-remediated ten days earlier while nobody noticed it
never fires. PHANTOM is reported before every other verdict, and — unlike the
auto-block verdicts — it applies even to `perpetual:true` gardeners (a reviewed
opt-out of self-stopping is not a claim that the trigger is loaded; the dead
sentinel ka-0000007378 was exactly this shape). A `last_checked` of null is
corroborating signal, not required — fixing the label resets nothing. Disabled
beads are exempt: `enabled=false` is the EXPLICIT way to encode dormancy.
Beads-viewer E2E fixtures (`fixture:e2e:*` labels) are skipped, mirroring the
engine's own isE2EFixture exemption.

Usage:
    gardener-autoblock-audit.py [--json] [--include-weak] [--all-roles]

A `gardener` blob is the strongest possible signal that a bead IS a gardener, so
EVERY enabled blob-carrying trigger is audited regardless of its `role` field
(ka-0000008185). The old default scope dropped any blob-carrier whose role did
not contain the substring "gardener" (e.g. role=mc-resident / auto-inspector /
phase0-haiku / empty), silently capping coverage at a fraction of the live
gardener population while the summary line still read as an all-clear. `role` is
only a heuristic for the blob-LESS legacy-loop detection below. Output is one
JSON object per line (offenders first, then weak if --include-weak). With no
offenders the script prints nothing and exits 0, so it composes cleanly with a
gardener loop that files a bug only when there is output.

A LEASE-WEDGED gardener is a separate, orthogonal failure: its concurrency lease
is full (live agent count >= max_concurrent) and every holder is STALE (agent
`updated_at` older than --lease-stale-hours, default 3h). `max_concurrent` is
enforced as a live lease with NO age bound (TriggerEngine.liveSpawnCount in
bridge/triggers.go), so a single hung agent on a max_concurrent=1 lane defers
every subsequent fire indefinitely — while `last_checked` keeps reading fresh, so
the lane looks healthy to every other liveness/dark-gardener check. This axis is
independent of stop-spec quality: a gardener with a flawless self-block, including
a reviewed `perpetual:true` one, wedges exactly the same way. Hence the verdict
is reported before the perpetual skip and before any auto-block verdict. Found
after two prod outages went unnoticed for 2 days: gardener-delivery lost ~23 fires
over 7h49m and gardener-bugfix ~18 over 9h07m (bug ka-0000001095). The check fails
toward NOT flagging (an unparseable/absent agent timestamp is never an offender).

Requires the `kd` CLI on PATH (uses `kd list --type=trigger --status open
--json` plus `kd list --type=agent` for live lease holders).
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from datetime import datetime, timezone

# Stop kinds that, in loop_end.any, prove the gardener terminates on progress or
# goal completion rather than only on crash / old age.
INTELLIGENT_LOOP_END_KINDS = {
    "no_progress_for",
    "bead_query",
    "specific_bead_closed",
    "max_runs",
}

# Stop kinds that, alone, are NOT sufficient for the progress-aware SELF-BLOCK
# axis — they are backstops, not wedge/goal detectors. NOTE: the wall_clock
# member is exactly what the engine's BACKSTOP axis (has_backstop below) DOES
# credit — the two axes measure different safety properties (bug ka-0000000849),
# so a kind being "backstop-only" here is not a contradiction with it satisfying
# the backstop axis there. Used by has_backstop() and documented for clarity.
BACKSTOP_ONLY_KINDS = {"wall_clock", "consecutive_failures", "manual", "manual_force"}

# The engine's absolute wall-clock ceiling — a hard_stop wall_clock larger than
# this is not a real ceiling, so it does NOT count as a backstop. Mirrors
# gardenerMaxWallClock (720h / 30 days) in
# gardenio/controller/internal/bridge/gardener_bounds_guard.go. Held in seconds so
# the parse below is dependency-free.
GARDENER_MAX_WALL_CLOCK_SECONDS = 720 * 3600


def load_gardeners() -> list[dict]:
    """Return all open type=trigger beads as dicts via the kd CLI.

    `kd list` defaults to a small page size (20), which silently truncates the
    result set — with ~100 open triggers that means the audit would only ever
    see a fraction of the gardeners each cycle and miss most offenders (this is
    how ka-0000006579 went un-reflagged). Pass an explicit high `--limit` so we
    audit every open trigger.
    """
    out = subprocess.run(
        ["kd", "list", "--type=trigger", "--status", "open", "--limit", "1000", "--json"],
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    data = json.loads(out)
    if isinstance(data, list):
        return data
    return data.get("issues", data.get("beads", []))


def parse_gardener(bead: dict) -> dict | None:
    """Extract and JSON-decode the `gardener` field; None if not a gardener."""
    fields = bead.get("fields") or {}
    cap = fields.get("gardener")
    if not cap:
        return None
    if isinstance(cap, str):
        try:
            cap = json.loads(cap)
        except json.JSONDecodeError:
            return None
    return cap if isinstance(cap, dict) else None


def loop_end_kinds(cap: dict) -> list[str]:
    stop = cap.get("stop") or {}
    le = stop.get("loop_end") or {}
    entries = le.get("any") or []
    return [e.get("kind") for e in entries if isinstance(e, dict)]


def run_budget(cap: dict) -> int:
    for key in ("run_budget", "max_runs"):
        v = cap.get(key)
        if isinstance(v, int) and v > 0:
            return v
    return 0


def has_progress_spec(cap: dict) -> bool:
    prog = cap.get("progress")
    if isinstance(prog, dict):
        return bool(prog)
    if isinstance(prog, str):
        return prog.strip() not in ("", "{}")
    return False


def _parse_go_duration_seconds(raw) -> float | None:
    """Parse a Go duration string (e.g. "168h", "30m", "1h30m") to seconds.

    Mirrors time.ParseDuration for the units a gardener wall_clock realistically
    uses (h/m/s and the sub-second units, though those never appear here).
    Returns None for an unparseable/empty value — the fail-closed answer the Go
    guard gives too (an unparseable duration is not a ceiling).
    """
    if not isinstance(raw, str):
        return None
    s = raw.strip()
    if not s:
        return None
    units = {
        "ns": 1e-9, "us": 1e-6, "µs": 1e-6, "ms": 1e-3,
        "s": 1.0, "m": 60.0, "h": 3600.0,
    }
    total = 0.0
    num = ""
    matched = False
    i = 0
    # Optional leading sign — a negative duration is non-positive, so fail it.
    negative = False
    if s and s[0] in "+-":
        negative = s[0] == "-"
        s = s[1:]
    while i < len(s):
        ch = s[i]
        if ch.isdigit() or ch == ".":
            num += ch
            i += 1
            continue
        # Read the (1-2 char) unit.
        unit = s[i:i + 2]
        if unit in units:
            i += 2
        else:
            unit = s[i:i + 1]
            if unit not in units:
                return None  # unknown unit — unparseable
            i += 1
        if num == "":
            return None  # unit with no preceding number
        try:
            total += float(num) * units[unit]
        except ValueError:
            return None
        num = ""
        matched = True
    if num != "" or not matched:
        return None  # trailing number with no unit, or no unit at all
    return -total if negative else total


def has_backstop(cap: dict) -> bool:
    """True when the gardener carries the engine-enforced BACKSTOP (bug ka-0000000849).

    This is the SECOND, orthogonal safety axis: the emergency brake the engine
    and reaper enforce unconditionally. It requires BOTH a positive run_budget
    AND a hard_stop wall_clock that parses to a duration in (0, 720h]. It is the
    EXACT check `gb gardener status` / the engine warn run via
    bridge.ValidateGardenerBounds (gardener_bounds_guard.go) — mirrored here so the
    audit can report the same backstop axis the status tool does. It is NOT the
    progress-aware self-block classify() scores; the two are reported side by
    side precisely because they measure different things.
    """
    if run_budget(cap) <= 0:
        return False
    stop = cap.get("stop") or {}
    hard = stop.get("hard_stop") or {}
    for e in hard.get("any") or []:
        if not isinstance(e, dict) or e.get("kind") != "wall_clock":
            continue
        secs = _parse_go_duration_seconds(e.get("duration"))
        if secs is not None and 0 < secs <= GARDENER_MAX_WALL_CLOCK_SECONDS:
            return True
    return False


def is_enabled(bead: dict) -> bool:
    fields = bead.get("fields") or {}
    return str(fields.get("enabled", "true")).lower() != "false"


def role_of(bead: dict) -> str:
    fields = bead.get("fields") or {}
    return str(fields.get("role", ""))


def is_perpetual_by_design(bead: dict) -> bool:
    """True when the gardener is explicitly labeled perpetual-by-design.

    An operator/agent who has reviewed a fleet-service gardener (one that drains
    a continuously-refilled queue and has no terminal "done" state) marks it
    `perpetual:true`. Honoring the label here is what makes the marking *stick*:
    without it the audit would re-flag the same trigger every cycle. The
    label is a deliberate, reviewed opt-out — not a default escape hatch.
    """
    labels = bead.get("labels") or []
    return any(str(label) == "perpetual:true" for label in labels)


def _decode_field(bead: dict, key: str):
    """Return fields[key] as a Python value, JSON-decoding a string payload.

    `kd --json` sometimes hands back a field pre-decoded (e.g. `triggers` as a
    list) and sometimes as a raw JSON string (e.g. `conditions`). Normalize so
    callers can treat both uniformly.
    """
    v = (bead.get("fields") or {}).get(key)
    if isinstance(v, str):
        s = v.strip()
        if not s:
            return None
        try:
            return json.loads(s)
        except json.JSONDecodeError:
            return v
    return v


def is_cron_driven(bead: dict) -> bool:
    """True when the trigger self-fires on a cron (top-level or in triggers[]).

    A cron-driven trigger wakes itself on a schedule with no external event —
    the runaway shape. An event-only dispatcher (JIRA ticket, bead lifecycle)
    only fires in response to work arriving and cannot loop on its own, so it is
    NOT gardener-shaped for auto-block purposes.
    """
    fields = bead.get("fields") or {}
    if str(fields.get("cron", "")).strip():
        return True
    entries = _decode_field(bead, "triggers")
    if isinstance(entries, list):
        for e in entries:
            if isinstance(e, dict) and (e.get("kind") == "cron" or e.get("cron")):
                return True
    return False


def has_whole_trigger_gate(bead: dict) -> bool:
    """True when a top-level `conditions` pre-flight gates EVERY fire.

    A top-level `conditions` bead_query (e.g. the remediated FLAT gardener
    ka-0000002785) is evaluated on every firing path, so it bounds the whole
    trigger — unlike a cron-*entry* pre-flight, which only gates the cron leg
    and leaves bead_event / decision_resolved / manual entries firing ungated.
    A trigger with such a whole-trigger gate is bounded and NOT unmanaged.
    """
    cond = _decode_field(bead, "conditions")
    if isinstance(cond, (list, dict)):
        return bool(cond)
    return False


def has_trigger_source_label(bead: dict) -> bool:
    """True when a `trigger:*` source label is present — i.e. the engine loads it.

    The engine's per-source load queries filter by label (trigger:schedule,
    trigger:multi, trigger:jira, …; see TriggerSourceLabelForFields in
    bridge/triggers.go). No matching label → never loaded → never evaluated.
    """
    return any(str(label).startswith("trigger:") for label in bead.get("labels") or [])


def is_e2e_fixture(bead: dict) -> bool:
    """True for beads-viewer E2E fixture triggers (`fixture:e2e:*` labels).

    These are seeded enabled into the shared prod daemon so viewer specs have
    live rows; the engine deliberately never fires them (isE2EFixture in
    bridge/triggers.go), so a missing source label is by design, not a defect.
    """
    return any(str(label).startswith("fixture:e2e:") for label in bead.get("labels") or [])


def is_phantom(bead: dict) -> bool:
    """True when the trigger is enabled with a fire shape but never engine-loaded.

    Signature: enabled, a `cron` field or non-empty `triggers[]` present, and no
    `trigger:*` source label. Such a bead reads as alive on every dashboard while
    the engine never evaluates it — the failure is on the liveness axis, fully
    orthogonal to auto-block quality. `last_checked` null corroborates but is not
    required (relabeling resets nothing). E2E fixtures are exempt by design.
    """
    if not is_enabled(bead) or is_e2e_fixture(bead) or has_trigger_source_label(bead):
        return False
    if str((bead.get("fields") or {}).get("cron", "")).strip():
        return True
    entries = _decode_field(bead, "triggers")
    return isinstance(entries, list) and len(entries) > 0


def load_live_agents() -> list[dict]:
    """Return all agent beads the engine still counts as occupying a lease.

    Mirrors the status set in TriggerEngine.liveSpawnCount (bridge/triggers.go):
    open/in_progress/blocked/deferred. An agent in any of these holds its
    trigger's `max_concurrent` slot.
    """
    out = subprocess.run(
        [
            "kd", "list", "--type=agent",
            "--status", "open,in_progress,blocked,deferred",
            "--limit", "1000", "--json",
        ],
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    data = json.loads(out)
    if isinstance(data, list):
        return data
    return data.get("issues", data.get("beads", []))


def _parse_ts(value) -> datetime | None:
    """Parse an RFC3339 timestamp to an aware datetime; None if unparseable."""
    if not isinstance(value, str) or not value.strip():
        return None
    s = value.strip().replace("Z", "+00:00")
    # Trim sub-second precision beyond microseconds (kd emits 6+ digits).
    if "." in s:
        head, _, tail = s.partition(".")
        digits = "".join(ch for ch in tail if ch.isdigit())[:6]
        offset = tail[len(tail) - 6:] if ("+" in tail or "-" in tail) else "+00:00"
        s = f"{head}.{digits or '0'}{offset}"
    try:
        parsed = datetime.fromisoformat(s)
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)


def lease_holders_by_trigger(agents: list[dict]) -> dict[str, list[dict]]:
    """Group live agent beads by the trigger whose lease they occupy."""
    out: dict[str, list[dict]] = {}
    for a in agents:
        spawned_by = str((a.get("fields") or {}).get("spawned_by", ""))
        if not spawned_by.startswith("trigger:"):
            continue
        out.setdefault(spawned_by[len("trigger:"):], []).append(a)
    return out


def max_concurrent_of(bead: dict) -> int:
    try:
        return int(str((bead.get("fields") or {}).get("max_concurrent", "")).strip() or 0)
    except ValueError:
        return 0


def lease_wedge(bead: dict, holders: list[dict], stale_hours: float, now: datetime):
    """Return a detail dict when this trigger's concurrency lease is wedged.

    A trigger is LEASE-WEDGED when its live-agent count has reached
    `max_concurrent` AND every holder is stale (`updated_at` older than
    `stale_hours`). The engine's lease check (bridge/triggers.go liveSpawnCount)
    has no age bound, so such a trigger defers every subsequent fire forever
    while `last_checked` keeps reading fresh — it looks healthy on every
    dashboard and dark-gardener check while spawning nothing (bug ka-0000001095:
    gardener-delivery lost ~23 fires over 7h49m, gardener-bugfix ~18 over 9h07m).

    Orthogonal to auto-block quality: a gardener with a perfect stop spec still
    wedges this way, which is why the verdict applies even to perpetual:true.
    """
    cap_slots = max_concurrent_of(bead)
    if cap_slots <= 0 or len(holders) < cap_slots:
        return None
    ages = []
    for h in holders:
        ts = _parse_ts(h.get("updated_at"))
        if ts is None:
            return None  # cannot prove staleness — fail toward not-flagging
        ages.append(((now - ts).total_seconds() / 3600.0, h))
    if not ages or min(age for age, _ in ages) < stale_hours:
        return None
    oldest = max(ages, key=lambda pair: pair[0])
    return {
        "id": bead.get("id"),
        "title": (bead.get("title") or "")[:80],
        "role": role_of(bead),
        "max_concurrent": cap_slots,
        "live_agents": len(holders),
        "stalest_agent": oldest[1].get("id"),
        "stale_hours": round(oldest[0], 1),
        "reason": (
            f"concurrency lease full ({len(holders)}/{cap_slots}) and every holder "
            f"is stale (oldest updated {round(oldest[0], 1)}h ago) — liveSpawnCount "
            "has no age bound, so every further fire is deferred indefinitely while "
            "last_checked still reads fresh. Close/kill the stale agent bead to "
            "release the lane, then fix the engine-side bound (bug ka-0000001095)."
        ),
    }


def is_gardener_shaped(bead: dict) -> bool:
    """True when the trigger presents as a gardener via its title or role.

    ONLY consulted on the blob-LESS legacy-loop path — a bead carrying a
    `gardener` blob is audited on blob presence alone and never reaches this
    heuristic (ka-0000008185). The classic detector keys on the `role` field,
    but the apply_formula epic-drivers carry an empty role and announce
    themselves only in the title (\"…CDQR Gardener\"). Match either so those
    blob-less loops are no longer invisible.
    """
    role = role_of(bead).lower()
    title = (bead.get("title") or "").lower()
    return "gardener" in role or "gardener" in title


def classify(bead: dict, cap: dict) -> tuple[str, dict]:
    """Return (verdict, detail). verdict in {ok, weak, offender}."""
    kinds = loop_end_kinds(cap)
    budget = run_budget(cap)
    intelligent = [k for k in kinds if k in INTELLIGENT_LOOP_END_KINDS]
    progress_aware = "no_progress_for" in kinds and has_progress_spec(cap)

    detail = {
        "id": bead.get("id"),
        "title": (bead.get("title") or "")[:80],
        "role": role_of(bead),
        "run_budget": budget,
        "loop_end_kinds": kinds,
        "has_progress_spec": has_progress_spec(cap),
        "progress_aware_wedge_detector": progress_aware,
        # The SECOND, orthogonal safety axis (bug ka-0000000849): the
        # engine-enforced backstop `gb gardener status` reports as `bounded`.
        # Carried on EVERY line so a surveyor reading the audit sees the same two
        # axes the status tool does — a gardener can be self-block `ok` here yet
        # `has_backstop: false` (no hard_stop wall_clock), which is exactly the
        # divergence that made the two tools look like they contradicted.
        "has_backstop": has_backstop(cap),
    }

    if budget == 0 and not intelligent:
        detail["reason"] = (
            "no run_budget and no intelligent loop_end kind "
            "(no_progress_for/bead_query/specific_bead_closed/max_runs) — "
            "this gardener can only stop on a pod crash or a multi-week wall_clock"
        )
        return "offender", detail

    if not progress_aware:
        detail["reason"] = (
            "terminates eventually (budget or goal-completion stop) but has no "
            "progress-aware wedge detector (no_progress_for + progress spec) — "
            "cannot detect 'crew keeps getting blocked, nothing is closing'"
        )
        return "weak", detail

    detail["reason"] = "ok: bounded + progress-aware"
    return "ok", detail


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--json", action="store_true", help="JSON output (default)")
    ap.add_argument(
        "--include-weak",
        action="store_true",
        help="also emit weak gardeners (bounded but no wedge detector)",
    )
    ap.add_argument(
        "--all-roles",
        action="store_true",
        help=(
            "retained for back-compat; no longer narrows scope. A `gardener` "
            "blob is now the definitive scope signal, so every enabled "
            "blob-carrying trigger is audited regardless of role (ka-0000008185). "
            "The role/title `is_gardener_shaped` heuristic only ever applied to "
            "the blob-LESS legacy-loop detection, which always runs."
        ),
    )
    ap.add_argument(
        "--summary",
        action="store_true",
        help="print a human-readable summary line to stderr",
    )
    ap.add_argument(
        "--lease-stale-hours",
        type=float,
        default=3.0,
        help=(
            "hours of agent updated_at inactivity before a concurrency-lease "
            "holder counts as stale (default 3.0 — above the observed ~2h "
            "legitimate long-run cluster, below the 7-9h outage outliers)"
        ),
    )
    args = ap.parse_args()

    beads = load_gardeners()
    now = datetime.now(timezone.utc)
    holders_by_trigger = lease_holders_by_trigger(load_live_agents())
    offenders, weak, ok, unmanaged, phantoms, wedged = [], [], [], [], [], []
    # Honest skip accounting (ka-0000008185): count gardener-population beads that
    # are deliberately excluded from the verdict buckets so a run that skips part
    # of the population never reads as full coverage in the summary line.
    perpetual_skipped = 0
    for b in beads:
        if not is_enabled(b):
            continue
        # E2E fixtures are seeded enabled but the engine never fires them
        # (isE2EFixture in bridge/triggers.go) — no verdict applies.
        if is_e2e_fixture(b):
            continue
        # Liveness axis, checked BEFORE the perpetual skip and before any
        # gardener-block quality verdict: a bead the engine never loads has no
        # behavior to audit, and a perpetual:true label on it is a reviewed
        # opt-out of self-stopping, not evidence the trigger fires at all.
        if is_phantom(b):
            phantoms.append(
                {
                    "id": b.get("id"),
                    "title": (b.get("title") or "")[:80],
                    "role": role_of(b),
                    "last_checked": (b.get("fields") or {}).get("last_checked"),
                    "reason": (
                        "enabled with a cron/triggers[] fire shape but NO "
                        "trigger:* source label — the engine loads triggers by "
                        "label, so this bead is never evaluated. Either load it "
                        "(kd label add <id> trigger:multi — or trigger:schedule "
                        "for a bare single-cron bead) or make dormancy explicit "
                        "(kd update <id> -f enabled=false)."
                    ),
                }
            )
            continue
        # Concurrency-lease axis, checked BEFORE the perpetual skip and before
        # any auto-block quality verdict: a gardener with a flawless stop spec
        # (and a reviewed perpetual:true opt-out) wedges this way just the same,
        # because the lease lives on the trigger fire-path, not the stop path.
        if wedge := lease_wedge(b, holders_by_trigger.get(b.get("id"), []),
                                args.lease_stale_hours, now):
            wedged.append(wedge)
            continue
        # A reviewed, explicitly perpetual-by-design gardener is a deliberate
        # opt-out: it drains a continuously-refilled queue (e.g. the ready
        # queue) and has no terminal goal, so a self-stop would be wrong.
        # Skip it entirely so the audit does not re-flag it every cycle — but
        # count it as a gardener-population bead we deliberately excluded, so the
        # summary can say so rather than reading as full coverage.
        if is_perpetual_by_design(b):
            if parse_gardener(b) is not None:
                perpetual_skipped += 1
            continue
        role = role_of(b)
        cap = parse_gardener(b)

        if cap is None:
            # A gardener trigger with no gardener block at all: a legacy cron
            # loop that fires forever with no self-stop. These are the canonical
            # "gardeners that just keep running". Two shapes qualify — see the
            # module docstring.
            fields = b.get("fields") or {}
            action = str(fields.get("action", "spawn_agent"))

            # Shape 1 (classic): role~gardener + action=spawn_agent.
            classic = "gardener" in role and action == "spawn_agent"

            # Shape 2 (apply_formula epic-driver): gardener-shaped (title/role),
            # a spawn_agent OR apply_formula action, self-fires on a cron, and
            # carries NO whole-trigger `conditions` gate. Its only stop is a
            # cron-entry pre-flight, which skips a day and closes nothing — so
            # it re-fires forever. Excludes event-only dispatchers (no cron) and
            # triggers already bounded by a top-level conditions gate.
            formula_loop = (
                is_gardener_shaped(b)
                and action in ("spawn_agent", "apply_formula")
                and is_cron_driven(b)
                and not has_whole_trigger_gate(b)
            )

            if classic or formula_loop:
                if classic:
                    reason = (
                        "gardener-role spawn trigger with NO gardener block "
                        "(no stop/progress/run_budget) — a legacy cron loop "
                        "that fires forever and can never self-terminate. "
                        "Either attach a gardener block (gb gardener start) "
                        "or, if perpetual-by-design, label it explicitly."
                    )
                else:
                    reason = (
                        f"gardener-shaped {action} trigger with NO gardener block "
                        "(no stop/progress/run_budget) — self-fires on a cron "
                        "and its only stop is a cron-entry pre-flight, which "
                        "skips a day but closes nothing, so it re-fires forever. "
                        "Attach a real self-block (kd update <id> -f gardener=@block "
                        "with run_budget + loop_end bead_query, NOT a bare "
                        "top-level run_budget which the engine ignores), add a "
                        "top-level conditions gate, or label perpetual:true if "
                        "reviewed as a standing cadence with no done-state."
                    )
                unmanaged.append(
                    {
                        "id": b.get("id"),
                        "title": (b.get("title") or "")[:80],
                        "role": role,
                        "action": action,
                        "reason": reason,
                    }
                )
            continue

        # The presence of a `gardener` blob is definitive: this bead IS a
        # gardener, so it is audited regardless of role (ka-0000008185). The old
        # `role~gardener` cap here silently dropped every blob-carrier whose role
        # did not contain the substring — mc-resident, auto-inspector,
        # phase0-haiku, empty — capping coverage at a fraction of the live
        # gardener population while the summary still read all-clear.
        verdict, detail = classify(b, cap)
        {"offender": offenders, "weak": weak, "ok": ok}[verdict].append(detail)

    for d in phantoms:
        print(json.dumps({"verdict": "phantom", **d}))
    for d in wedged:
        print(json.dumps({"verdict": "lease-wedged", **d}))
    for d in unmanaged:
        print(json.dumps({"verdict": "unmanaged", **d}))
    for d in offenders:
        print(json.dumps({"verdict": "offender", **d}))
    if args.include_weak:
        for d in weak:
            print(json.dumps({"verdict": "weak", **d}))

    if args.summary:
        audited = (
            len(phantoms) + len(wedged) + len(unmanaged)
            + len(offenders) + len(weak) + len(ok)
        )
        # State the excluded count explicitly so a capped run never reads as full
        # coverage (ka-0000008185): perpetual:true blob-carriers are a reviewed
        # opt-out but still part of the live gardener population.
        skipped = f", {perpetual_skipped} perpetual-skipped" if perpetual_skipped else ""
        # Second axis (bug ka-0000000849): how many CLASSIFIED gardeners (those
        # that reached classify(), i.e. carry a gardener block) lack the
        # engine-enforced backstop `gb gardener status` reports as `bounded`. This
        # is what makes the two tools' verdicts legible together — a gardener can
        # be self-block `ok` yet still lack the backstop, the exact split that
        # made the tools look contradictory.
        classified = offenders + weak + ok
        no_backstop = sum(1 for d in classified if not d.get("has_backstop"))
        print(
            f"gardener-autoblock-audit: {len(phantoms)} phantom, "
            f"{len(wedged)} lease-wedged, "
            f"{len(unmanaged)} unmanaged, "
            f"{len(offenders)} offender(s), {len(weak)} weak, {len(ok)} ok "
            f"(of {audited} audited enabled gardeners{skipped}); "
            f"{no_backstop}/{len(classified)} classified lack the engine backstop "
            "(run_budget+hard_stop wall_clock — orthogonal to self-block; see `gb gardener status`)",
            file=sys.stderr,
        )

    # Exit 0 always — the calling gardener decides what to do with the lines.
    return 0


if __name__ == "__main__":
    sys.exit(main())
