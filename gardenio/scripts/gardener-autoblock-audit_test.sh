#!/usr/bin/env bash
# gardener-autoblock-audit_test.sh — unit tests for the auto-block audit
# classifier, with a focus on the apply_formula "…Gardener" blind-spot fix
# (gd-7dCpbNaUI0): a gardener-shaped cron loop with no gardener block must be
# flagged UNMANAGED regardless of action, while event-only dispatchers and
# whole-trigger-gated triggers must NOT be flagged.
#
# The audit module talks to `kd` only through load_gardeners(); the harness below
# imports the module by path and feeds it synthetic beads, so no daemon, no `kd`
# on PATH, and no network are required.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

python3 - "$SCRIPT_DIR/gardener-autoblock-audit.py" <<'PY'
import importlib.util
import json
import sys

# Load the hyphenated module by path.
spec = importlib.util.spec_from_file_location("gardener_autoblock_audit", sys.argv[1])
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)

FAILED = 0


def check(name, cond):
    global FAILED
    if cond:
        print(f"ok   - {name}")
    else:
        print(f"FAIL - {name}")
        FAILED = 1


def bead(id, *, title="", role="", action="spawn_agent", enabled=True,
         cron="", triggers=None, conditions=None, gardener=None, labels=None):
    fields = {"action": action, "enabled": "true" if enabled else "false"}
    if role:
        fields["role"] = role
    if cron:
        fields["cron"] = cron
    if triggers is not None:
        fields["triggers"] = triggers
    if conditions is not None:
        fields["conditions"] = conditions
    if gardener is not None:
        fields["gardener"] = gardener
    return {"id": id, "title": title, "labels": labels or [], "fields": fields}


def classify_fleet(beads):
    """Run main() over `beads` and return the emitted verdict lines as dicts."""
    mod.load_gardeners = lambda: beads
    # No lease holders unless a test installs some (see lease_fleet below).
    mod.load_live_agents = lambda: []
    argv = sys.argv
    sys.argv = ["audit", "--all-roles", "--include-weak"]
    from io import StringIO
    buf, real = StringIO(), sys.stdout
    sys.stdout = buf
    try:
        mod.main()
    finally:
        sys.stdout = real
        sys.argv = argv
    return [json.loads(line) for line in buf.getvalue().splitlines() if line.strip()]


def verdict_for(lines, bead_id):
    for d in lines:
        if d.get("id") == bead_id:
            return d.get("verdict")
    return None


# --- The blind-spot regression: apply_formula "…Gardener" cron loop ----------
cron_entry = [{"kind": "cron", "cron": "37 7 * * *"}]

# --- PHANTOM (liveness axis, ka-0000004476) ---------------------------------
# Enabled + cron but no trigger:* label: the engine never loads it. A perfect
# gardener block does not save it — PHANTOM outranks every auto-block verdict.
phantom_cron = bead(
    "gd-ph-cron", title="C3 bake gardener", role="gardener", cron="11 3 * * *",
    labels=["project:gardenio", "role:gardener"],
    gardener=json.dumps({
        "run_budget": 10,
        "progress": {"events": ["child_closed"]},
        "stop": {"loop_end": {"any": [{"kind": "no_progress_for", "runs": 3}]}},
    }))
# triggers[] fire shape (no top-level cron) is phantom too.
phantom_entries = bead(
    "gd-ph-trig", title="wrapup sentinel", role="crew-ultralite",
    triggers=cron_entry, labels=["trigger-evaluator"])
# perpetual:true does NOT exempt a phantom — the label opts out of self-stop
# review, not of being loaded (the dead-sentinel shape, ka-0000007378).
phantom_perpetual = bead(
    "gd-ph-perp", title="Standing gardener", role="gardener", cron="0 * * * *",
    labels=["perpetual:true"])
# Correctly-labeled trigger is NOT phantom (trigger:multi loads it).
labeled = bead(
    "gd-labeled", title="Live gardener", role="gardener", cron="0 * * * *",
    triggers=cron_entry, labels=["trigger:multi", "perpetual:true"])
# Disabled bead with no label: explicit dormancy, NOT phantom.
dormant = bead(
    "gd-dormant", title="Parked gardener", role="gardener", cron="0 8 * * *",
    enabled=False)
# Event-shape-less bead (no cron, no triggers[]): nothing to fire, not phantom.
shapeless = bead(
    "gd-shapeless", title="Config-only gardener bead", role="gardener")
# E2E fixture: seeded enabled without firing by design — exempt.
fixture = bead(
    "gd-fixture", title="e2e schedule fixture", role="gardener",
    cron="0 9 * * *", labels=["fixture:e2e:schedule"])

people_cdqr = bead(
    "gd-people", title="People-Directory CDQR Gardener (epic gd-epic1)",
    action="apply_formula", role="", cron="37 7 * * *", triggers=cron_entry,
    labels=["trigger:multi"])
gardener_page = bead(
    "gd-cappage", title="Gardener-Page CDQR Gardener (epic gd-epic2)",
    action="apply_formula", role="", cron="47 7 * * *", triggers=cron_entry,
    labels=["trigger:multi"])

# --- Correct exclusions -----------------------------------------------------
# Event-only dispatcher: gardener in title but no cron — fires only on events.
event_only = bead(
    "gd-evt", title="Some Gardener-ish dispatcher", action="apply_formula",
    triggers=[{"kind": "jira", "match": {}}], labels=["trigger:multi"])
# Whole-trigger gate: a top-level conditions pre-flight bounds every fire
# (the remediated FLAT-gardener shape, ka-0000002785). conditions arrives from
# `kd --json` as a RAW STRING, so exercise that encoding too.
gated = bead(
    "gd-gated", title="Users-Page FLAT Gardener", action="apply_formula",
    cron="23 9 * * *", triggers=cron_entry, labels=["trigger:multi"],
    conditions=json.dumps([{"type": "bead_query", "op": ">", "value": "0"}]))
# Agent-free assert action: not a spawner/formula loop.
assert_trig = bead(
    "gd-assert", title="Fleet-liveness assert gardener", action="assert",
    triggers=[{"kind": "bead_event", "match": {}}], labels=["trigger:multi"])
# Classic legacy loop still flagged (role~gardener + spawn_agent, no cron field
# needed — the classic path does not require cron).
classic = bead(
    "gd-classic", title="Legacy loop", role="gardener", action="spawn_agent")
# Perpetual-by-design opt-out is skipped entirely even though gardener-shaped.
perpetual = bead(
    "gd-perp", title="Standing Gardener", action="apply_formula",
    cron="0 * * * *", triggers=cron_entry,
    labels=["perpetual:true", "trigger:multi"])
# A bounded managed gardener (has a real gardener block) stays OK, not unmanaged.
managed = bead(
    "gd-ok", title="Gardener for something", role="gardener",
    action="spawn_agent",
    gardener=json.dumps({
        "run_budget": 10,
        "progress": {"events": ["child_closed"]},
        "stop": {"loop_end": {"any": [
            {"kind": "bead_query", "parent": "gd-e", "op": "==", "value": "0"},
            {"kind": "no_progress_for", "runs": 3},
        ]}},
    }))

lines = classify_fleet([
    people_cdqr, gardener_page, event_only, gated, assert_trig,
    classic, perpetual, managed,
    phantom_cron, phantom_entries, phantom_perpetual,
    labeled, dormant, shapeless, fixture,
])

check("apply_formula CDQR gardener (people) flagged UNMANAGED",
      verdict_for(lines, "gd-people") == "unmanaged")
check("apply_formula CDQR gardener (gardener-page) flagged UNMANAGED",
      verdict_for(lines, "gd-cappage") == "unmanaged")
check("event-only dispatcher NOT flagged",
      verdict_for(lines, "gd-evt") is None)
check("whole-trigger conditions-gated gardener NOT flagged (string-encoded conditions)",
      verdict_for(lines, "gd-gated") is None)
check("assert-action trigger NOT flagged",
      verdict_for(lines, "gd-assert") is None)
check("classic role~gardener spawn loop still flagged UNMANAGED",
      verdict_for(lines, "gd-classic") == "unmanaged")
check("perpetual:true opt-out skipped (no verdict)",
      verdict_for(lines, "gd-perp") is None)
# `ok` gardeners emit no line (the script prints only offenders/unmanaged/weak),
# so a real-block managed gardener must simply be absent from the verdict lines
# — and in particular must NOT be flagged unmanaged.
check("managed gardener with real block NOT flagged (absent = ok)",
      verdict_for(lines, "gd-ok") is None)

# The unmanaged reason for the formula loop should name the correct fix and warn
# against the inert bare top-level run_budget.
people_line = next(d for d in lines if d["id"] == "gd-people")
check("formula-loop reason warns bare top-level run_budget is ignored",
      "NOT a bare top-level run_budget" in people_line["reason"])
check("formula-loop verdict records the action",
      people_line.get("action") == "apply_formula")

# --- PHANTOM verdict (ka-0000004476) ----------------------------------------
check("enabled cron bead without trigger:* label flagged PHANTOM despite full gardener block",
      verdict_for(lines, "gd-ph-cron") == "phantom")
check("triggers[]-shaped bead without trigger:* label flagged PHANTOM",
      verdict_for(lines, "gd-ph-trig") == "phantom")
check("perpetual:true does NOT exempt a phantom",
      verdict_for(lines, "gd-ph-perp") == "phantom")
check("trigger:multi-labeled gardener NOT phantom (perpetual skip applies as before)",
      verdict_for(lines, "gd-labeled") is None)
check("disabled unlabeled bead NOT phantom (explicit dormancy)",
      verdict_for(lines, "gd-dormant") is None)
check("bead with no cron and no triggers[] NOT phantom",
      verdict_for(lines, "gd-shapeless") == "unmanaged")  # classic shape still applies
check("e2e fixture fully exempt — no verdict on any axis (engine never fires it)",
      verdict_for(lines, "gd-fixture") is None)
check("phantom reason names both remedies (label add / enabled=false)",
      "trigger:multi" in next(d for d in lines if d["id"] == "gd-ph-cron")["reason"]
      and "enabled=false" in next(d for d in lines if d["id"] == "gd-ph-cron")["reason"])
# Ordering: phantoms are reported before unmanaged/offender lines.
verdict_sequence = [d["verdict"] for d in lines]
check("phantom lines precede all other verdicts",
      verdict_sequence.index("phantom") == 0
      and verdict_sequence[:verdict_sequence.count("phantom")]
          == ["phantom"] * verdict_sequence.count("phantom"))

# --- LEASE-WEDGED (concurrency axis, bug ka-0000001095) ---------------------
# max_concurrent is a live lease with NO age bound (liveSpawnCount), so one hung
# agent starves a max_concurrent=1 lane forever while last_checked reads fresh.
# Replays the two real outages: gardener-delivery (~23 fires lost over 7h49m) and
# gardener-bugfix (~18 over 9h07m).
from datetime import datetime, timedelta, timezone

NOW = datetime(2026, 7, 24, 18, 40, tzinfo=timezone.utc)


def agent(id, trigger_id, hours_stale):
    ts = NOW - timedelta(hours=hours_stale)
    return {"id": id, "updated_at": ts.strftime("%Y-%m-%dT%H:%M:%S.000000Z"),
            "fields": {"spawned_by": f"trigger:{trigger_id}"}}


def lease_verdict(trig, holders, stale_hours=3.0):
    return mod.lease_wedge(trig, holders, stale_hours, NOW)


# The real delivery gardener, mid-outage: perpetual:true AND a full gardener block,
# so this proves the axis is independent of stop-spec quality and of the opt-out.
delivery = bead(
    "ka-0000001510", title="Gardenio gardener-delivery", role="gardener-delivery",
    cron="*/20 * * * *", labels=["perpetual:true", "trigger:multi"],
    gardener=json.dumps({
        "run_budget": 350,
        "progress": {"events": [{"kind": "mr_merged"}]},
        "stop": {"loop_end": {"any": [{"kind": "max_runs"}]}},
    }))
delivery["fields"]["max_concurrent"] = "1"

wedge = lease_verdict(delivery, [agent("gd-j4dWqjMlkI", "ka-0000001510", 4.0)])
check("hung agent on max_concurrent=1 lane flagged LEASE-WEDGED", wedge is not None)
check("lease-wedged detail names the stalest agent + age",
      wedge and wedge["stalest_agent"] == "gd-j4dWqjMlkI" and wedge["stale_hours"] == 4.0)
check("lease-wedged reason names the remedy (release the lane)",
      wedge and "release the lane" in wedge["reason"])
check("perpetual:true + full gardener block do NOT exempt a lease wedge",
      wedge is not None)

# Controls — each must NOT flag.
check("FRESH lease holder NOT flagged (lease must keep working, ka-0000003120)",
      lease_verdict(delivery, [agent("gd-fresh", "ka-0000001510", 0.1)]) is None)
check("empty lease NOT flagged (nothing holding the slot)",
      lease_verdict(delivery, []) is None)
check("2.1h legitimate long run NOT flagged (below 3h default threshold)",
      lease_verdict(delivery, [agent("gd-longrun", "ka-0000001510", 2.1)]) is None)
check("unparseable agent timestamp fails toward NOT flagging",
      lease_verdict(delivery, [{"id": "gd-bad", "updated_at": "not-a-date",
                                "fields": {"spawned_by": "trigger:ka-0000001510"}}]) is None)
no_limit = bead("gd-nolimit", title="no cap gardener", role="gardener", cron="*/20 * * * *",
                labels=["trigger:multi"])
check("max_concurrent unset -> lease axis does not apply",
      lease_verdict(no_limit, [agent("gd-x", "gd-nolimit", 9.0)]) is None)

# Lease not yet FULL: 1 stale holder on a 2-slot lane still has a free slot.
two_slot = bead("gd-two", title="two slot gardener", role="gardener",
                cron="*/20 * * * *", labels=["trigger:multi"])
two_slot["fields"]["max_concurrent"] = "2"
check("stale holder but lease not full NOT flagged (free slot remains)",
      lease_verdict(two_slot, [agent("gd-s1", "gd-two", 9.0)]) is None)
check("full 2-slot lease with ALL holders stale IS flagged",
      lease_verdict(two_slot, [agent("gd-s1", "gd-two", 9.0),
                               agent("gd-s2", "gd-two", 5.0)]) is not None)
check("full 2-slot lease with ONE fresh holder NOT flagged",
      lease_verdict(two_slot, [agent("gd-s1", "gd-two", 9.0),
                               agent("gd-s2", "gd-two", 0.2)]) is None)

# The second real outage: gardener-bugfix, 9h07m, no perpetual label.
bugfix = bead("ka-0000001007", title="Gardenio gardener-bugfix", role="gardener-bugfix",
              cron="13,43 * * * *", labels=["trigger:schedule"])
bugfix["fields"]["max_concurrent"] = "1"
check("second real outage (gardener-bugfix, 9h stale) flagged LEASE-WEDGED",
      lease_verdict(bugfix, [agent("ka-0000001538", "ka-0000001007", 9.1)]) is not None)

# Grouping: spawned_by must map holders to the right trigger, and non-trigger
# spawns (crew spawned by an agent) must never occupy a trigger lease.
grouped = mod.lease_holders_by_trigger([
    agent("gd-a1", "gd-T1", 4.0),
    agent("gd-a2", "gd-T2", 4.0),
    {"id": "gd-crew", "updated_at": "2026-07-24T18:00:00.000000Z",
     "fields": {"spawned_by": "gardenio-some-agent"}},
])
check("lease holders grouped by trigger id",
      sorted(grouped) == ["gd-T1", "gd-T2"] and len(grouped["gd-T1"]) == 1)
check("agent-spawned crew does NOT occupy a trigger lease",
      not any(h["id"] == "gd-crew" for hs in grouped.values() for h in hs))

# End-to-end through main(): the verdict is emitted, and ordered after phantoms
# but before unmanaged/offender/weak lines.
mod.load_live_agents = lambda: [agent("gd-j4dWqjMlkI", "ka-0000001510", 4.0)]
_real_now = mod.datetime


class _FrozenNow(mod.datetime):
    @classmethod
    def now(cls, tz=None):
        return NOW


mod.datetime = _FrozenNow
try:
    mod.load_gardeners = lambda: [delivery]
    argv = sys.argv
    sys.argv = ["audit", "--all-roles", "--include-weak"]
    from io import StringIO
    buf, real = StringIO(), sys.stdout
    sys.stdout = buf
    try:
        mod.main()
    finally:
        sys.stdout = real
        sys.argv = argv
    e2e = [json.loads(l) for l in buf.getvalue().splitlines() if l.strip()]
finally:
    mod.datetime = _real_now

check("main() emits the lease-wedged verdict end-to-end",
      len(e2e) == 1 and e2e[0]["verdict"] == "lease-wedged"
      and e2e[0]["id"] == "ka-0000001510")


# --- DEFAULT-scope blob coverage (ka-0000008185) ----------------------------
# The sentinel invokes the script WITHOUT --all-roles. A `gardener` blob is now
# the definitive scope signal, so every enabled blob-carrier must be audited
# regardless of its `role` field — the old default dropped any blob-carrier
# whose role did not contain "gardener" (mc-resident / auto-inspector /
# phase0-haiku / empty), silently capping coverage while the summary read
# all-clear.
def run_fleet(beads, argv, agents=None):
    """Run main() over `beads` with the given argv, capturing stdout+stderr."""
    mod.load_gardeners = lambda: beads
    mod.load_live_agents = lambda: (agents or [])
    saved = sys.argv
    sys.argv = argv
    from io import StringIO
    buf, ebuf = StringIO(), StringIO()
    real_out, real_err = sys.stdout, sys.stderr
    sys.stdout, sys.stderr = buf, ebuf
    try:
        mod.main()
    finally:
        sys.stdout, sys.stderr = real_out, real_err
        sys.argv = saved
    lines = [json.loads(l) for l in buf.getvalue().splitlines() if l.strip()]
    return lines, ebuf.getvalue()


# A no-run_budget, no-intelligent-stop blob is an OFFENDER — the strongest
# verdict, so if DEFAULT scope sees the bead at all it must emit this line.
weak_block = json.dumps({"stop": {"loop_end": {"any": [{"kind": "wall_clock"}]}}})

# The four real off-role blob-carriers the bug named (roles that do NOT contain
# "gardener"). Each carries a bare wall_clock stop -> OFFENDER when audited.
mc_resident = bead("gd-sh3TT9XFsr", title="mc-build gardener", role="mc-resident",
                   gardener=weak_block)
inspector = bead("ka-0000004081", title="Real-data session inspector (CED rubric)",
                 role="auto-inspector", gardener=weak_block)
empty_role = bead("ka-0000001426", title="Continual gardener: Grafana GRADE Audit",
                  role="", gardener=weak_block)
haiku = bead("ka-0000001271", title="Meta-Gardener: gardener-effectiveness",
             role="phase0-haiku", gardener=weak_block)
# A role~gardener blob-carrier (seen under the OLD filter too) — still audited.
role_gardener = bead("gd-rolecap", title="Some gardener", role="gardener",
                    gardener=weak_block)

blob_fleet = [mc_resident, inspector, empty_role, haiku, role_gardener]

default_lines, default_err = run_fleet(
    blob_fleet, ["audit", "--include-weak"])  # NO --all-roles: the sentinel path
allroles_lines, _ = run_fleet(
    blob_fleet, ["audit", "--include-weak", "--all-roles"])

for bid, role in [("gd-sh3TT9XFsr", "mc-resident"),
                  ("ka-0000004081", "auto-inspector"),
                  ("ka-0000001426", "(empty)"),
                  ("ka-0000001271", "phase0-haiku")]:
    check(f"DEFAULT scope audits off-role blob-carrier {bid} (role={role})",
          verdict_for(default_lines, bid) == "offender")

check("ka-0000004081 audited despite is_gardener_shaped() false (role+title lack 'gardener')",
      not mod.is_gardener_shaped(inspector)
      and verdict_for(default_lines, "ka-0000004081") == "offender")

check("DEFAULT scope == --all-roles now (no coverage cap)",
      sorted(verdict_for(default_lines, b["id"]) or "" for b in blob_fleet)
      == sorted(verdict_for(allroles_lines, b["id"]) or "" for b in blob_fleet))

# --all-roles must be a pure no-op for scope now.
check("all 5 blob-carriers audited under DEFAULT scope",
      all(verdict_for(default_lines, b["id"]) == "offender" for b in blob_fleet))

# --- Honest skip accounting in the summary (ka-0000008185) ------------------
# A perpetual:true blob-carrier is skipped from verdicts but is still part of
# the gardener population; the summary must disclose it, not read as full
# coverage. (labeled needs a trigger:* label so it is not caught as phantom.)
perp_blob = bead("gd-perpblob", title="Standing gardener", role="gardener",
                 cron="0 * * * *", triggers=cron_entry,
                 labels=["perpetual:true", "trigger:multi"], gardener=weak_block)
_lines, summary_err = run_fleet(
    [mc_resident, perp_blob], ["audit", "--include-weak", "--summary"])
check("summary discloses perpetual-skipped population count",
      "1 perpetual-skipped" in summary_err)
check("summary reports audited count separately from skipped",
      "1 audited enabled gardeners" in summary_err)
# A run with nothing skipped must NOT append the perpetual-skipped clause.
_lines2, summary_err2 = run_fleet(
    [mc_resident], ["audit", "--include-weak", "--summary"])
check("summary omits perpetual-skipped clause when none skipped",
      "perpetual-skipped" not in summary_err2)

# --- Two-axis reconciliation: has_backstop (bug ka-0000000849) ---------------
# The backstop axis is run_budget>0 AND a hard_stop wall_clock in (0,720h],
# mirroring bridge.ValidateGardenerBounds. It is ORTHOGONAL to the self-block
# verdict: a gardener can be self-block ok yet lack the backstop, and vice versa.

# The Go-duration parser must agree with time.ParseDuration on the shapes a
# gardener wall_clock uses.
check("parse 168h", mod._parse_go_duration_seconds("168h") == 168 * 3600)
check("parse composite 1h30m", mod._parse_go_duration_seconds("1h30m") == 5400)
check("parse minutes 30m", mod._parse_go_duration_seconds("30m") == 1800)
check("parse empty -> None", mod._parse_go_duration_seconds("") is None)
check("parse garbage -> None", mod._parse_go_duration_seconds("forever") is None)
check("parse bare number -> None", mod._parse_go_duration_seconds("168") is None)
check("parse negative -> negative", mod._parse_go_duration_seconds("-1h") == -3600)

# has_backstop: the four ValidateGardenerBounds cases.
backstop_full = {
    "run_budget": 20,
    "stop": {"hard_stop": {"any": [{"kind": "wall_clock", "duration": "168h"}]}},
}
check("has_backstop: run_budget + 168h wall_clock -> True",
      mod.has_backstop(backstop_full) is True)
check("has_backstop: missing run_budget -> False",
      mod.has_backstop({"stop": {"hard_stop": {"any": [
          {"kind": "wall_clock", "duration": "168h"}]}}}) is False)
check("has_backstop: run_budget but no wall_clock -> False",
      mod.has_backstop({"run_budget": 20}) is False)
check("has_backstop: wall_clock over 720h ceiling -> False",
      mod.has_backstop({"run_budget": 20, "stop": {"hard_stop": {"any": [
          {"kind": "wall_clock", "duration": "100000h"}]}}}) is False)
check("has_backstop: wall_clock exactly 720h -> True",
      mod.has_backstop({"run_budget": 20, "stop": {"hard_stop": {"any": [
          {"kind": "wall_clock", "duration": "720h"}]}}}) is True)
check("has_backstop: wall_clock in run_end not hard_stop -> False",
      mod.has_backstop({"run_budget": 20, "stop": {"run_end": {"any": [
          {"kind": "wall_clock", "duration": "168h"}]}}}) is False)
check("has_backstop: consecutive_failures-only hard_stop -> False",
      mod.has_backstop({"run_budget": 50, "stop": {"hard_stop": {"any": [
          {"kind": "consecutive_failures", "value": "5"}]}}}) is False)

# THE DIVERGENCE the bug is about: ka-0000003772's real shape — self-block ok
# (run_budget + no_progress_for + progress spec) yet NO backstop (its only
# hard_stop is consecutive_failures). Both verdicts must render, disagreeing.
tree_hug = {
    "run_budget": 50, "max_runs": 50,
    "progress": {"events": [{"kind": "wrapup_field", "field": "progress_score"}]},
    "stop": {
        "loop_end": {"any": [{"kind": "max_runs"}, {"kind": "no_progress_for", "runs": 3}]},
        "hard_stop": {"any": [{"kind": "consecutive_failures", "value": "5"}]},
    },
}
tree_verdict, tree_detail = mod.classify(
    {"id": "ka-0000003772", "title": "tree-hug", "fields": {"role": "gardener"}}, tree_hug)
check("divergent gardener is self-block ok", tree_verdict == "ok")
check("divergent gardener has_backstop False (no wall_clock hard_stop)",
      tree_detail["has_backstop"] is False)
check("every classify detail carries the has_backstop axis",
      "has_backstop" in tree_detail)

# A fully-safe gardener scores yes on BOTH axes.
full_safe = {
    "run_budget": 20, "max_runs": 20,
    "progress": {"events": [{"kind": "wrapup_field", "field": "progress_score"}]},
    "stop": {
        "loop_end": {"any": [{"kind": "no_progress_for", "runs": 3}]},
        "hard_stop": {"any": [{"kind": "wall_clock", "duration": "168h"}]},
    },
}
fs_verdict, fs_detail = mod.classify(
    {"id": "gd-safe", "title": "safe", "fields": {"role": "gardener"}}, full_safe)
check("fully-safe gardener is self-block ok", fs_verdict == "ok")
check("fully-safe gardener has_backstop True", fs_detail["has_backstop"] is True)

# Parity: the INTELLIGENT_LOOP_END_KINDS set is the vocabulary the Go tool must
# match. Pin it so a future edit to one side is caught against the other.
check("INTELLIGENT_LOOP_END_KINDS is the pinned four",
      mod.INTELLIGENT_LOOP_END_KINDS == {
          "no_progress_for", "bead_query", "specific_bead_closed", "max_runs"})

sys.exit(1 if FAILED else 0)
PY
