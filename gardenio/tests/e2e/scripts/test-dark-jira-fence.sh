#!/usr/bin/env bash
# test-dark-jira-fence.sh — dark-pod Jira write-token fence, NETWORK leg E2E
# (task ka-0000007263, coverage-backfill epic ka-0000000958; fence bead
# ka-0000005242, R7 epic ka-0000004670).
#
# ── What the fence is ────────────────────────────────────────────────────────
#
# R7 proved the "a dark (parallel-universe) agent cannot touch real upstream
# Jira" guarantee was NOT structural: the privileged Jira WRITE token could
# reach a dark pod through unguarded plaintext-env injection (project/role
# EnvVars + EnvOverrides), which — unlike the prefix-gated secrets path — had no
# name filtering. MR !6594 fenced it with defense in depth, three layers:
#
#   pod layer      scrubDarkForbiddenEnv strips JIRA_API_TOKEN from a dark pod's
#                  Env + SecretEnv, whichever layer set it (podspec.go).
#   network layer  the dark-agent-egress NetworkPolicy denies a dark pod egress
#                  to Atlassian, so even a leaked token has nowhere to go.
#   client layer   GARDENIO_DARK=true makes gb build a READ-ONLY JiraClient whose
#                  writes fail closed with ErrJiraReadOnly.
#
# ── Why this module exists (the gap) ─────────────────────────────────────────
#
# The pod layer has unit coverage (podspec_dark_jira_test.go), the client layer
# has unit coverage (jira_readonly_test.go), and the chart TEMPLATE has render
# coverage (agent_external_keycloak_egress_test.go, landgate_test.go). But NOTHING asserted that the
# DEPLOYED objects actually fence the real Jira address. A render test proves
# the template emits CIDRs; it cannot prove those CIDRs still cover
# acme.atlassian.net, nor that some OTHER policy in the namespace
# re-grants what this one subtracts. That is the gap this module closes, and
# it is the layer a silent regression would hide in.
#
# ── Why containment arithmetic, not a live connect() ─────────────────────────
#
# The obvious test — exec into a dark pod and curl Jira — is not viable as a
# shard gate, and would be WEAKER than what we do here:
#
#   1. The in-shard suite spawns NO real agent pods (the gVisor suite that does
#      runs out-of-band on a dedicated node behind GVISOR_E2E). Spawning a dark
#      pod + a non-dark control pod just to probe egress is minutes of wall
#      clock and two more flake surfaces on every shard.
#   2. A connect() probe is ambiguous in the wrong direction: a TIMEOUT is
#      indistinguishable from Jira being down, a CNI hiccup, or the pod having
#      no route yet — so the "secure" result is exactly the result a broken
#      environment produces. That is a test that passes loudest when it is
#      least trustworthy.
#
# Instead we assert the DEPLOYED policy objects with set arithmetic: resolve the
# real Jira host, then prove every resolved IP falls INSIDE the dark policy's
# ipBlock `except` list (fenced) and OUTSIDE the non-dark policy's (still
# reachable). Same guarantee, deterministic, and it names the exact CIDR that
# covers Jira when it fails.
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ──────────────────
#
#   1  policies-present   Both halves of the post-fence split are deployed:
#                         <release>-agent-internet-egress AND
#                         <release>-dark-agent-egress.
#   2  selector-split     The internet policy selects agent Exists + dark
#                         DoesNotExist; the dark policy selects dark Exists. If
#                         the internet policy ever stops excluding dark pods,
#                         dark pods regain unfenced internet via the union.
#   3  union-exclusivity  THE STRUCTURAL LEG. NetworkPolicy is allow-UNION, so
#                         the fence holds only if dark-agent-egress is the ONLY
#                         egress policy granting a dark pod a broad 0.0.0.0/0
#                         allow. We simulate label-selector matching over EVERY
#                         egress policy in the namespace against a synthetic
#                         dark pod and assert exactly that. A new (or
#                         re-broadened) policy that also selects dark pods
#                         silently re-opens Atlassian while legs 1/2/4 stay
#                         green — this is the only leg that catches it.
#   4  jira-fenced        Every IP acme.atlassian.net resolves to is
#                         CONTAINED in the dark policy's except-list.
#   5  nondark-control    Those same IPs are NOT excepted by the non-dark
#                         policy. Without this control, leg 4 would also pass
#                         under a global egress blackout — i.e. it keeps the
#                         fence assertion from going vacuous.
#
# Leg 5 is what makes this suite honest: legs 1-4 all pass if egress is broken
# for everyone, which is not the property we are claiming.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The fence shipped in 2026.217.x. A namespace deployed from an older chart has
# NEITHER policy, and long-lived ephemeral namespaces can predate it. Absent
# BOTH policies we skip LOUD (exit 0) rather than red the suite — the module
# cannot assert a control the deployment does not have. But if the dark policy
# is present and its CONTENT is wrong, that is a HARD failure: a half-applied
# fence is a regression, not a missing feature.
#
# Usage:
#   ./tests/e2e/scripts/test-dark-jira-fence.sh
#   JIRA_FENCE_HOST=other.atlassian.net ./tests/e2e/scripts/test-dark-jira-fence.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
# Chart resources are named <release>-<component>; release == namespace in the
# per-run e2e namespaces. Never hardcode the namespace into a
# resource name (test-concurrency-isolation.sh invariant 4).
RELEASE="${RELEASE:-${NAMESPACE}}"
# The real upstream Jira the dark universe must never reach. Overridable so the
# module stays useful if the org's Jira host changes.
JIRA_FENCE_HOST="${JIRA_FENCE_HOST:-acme.atlassian.net}"

DARK_NP="${RELEASE}-dark-agent-egress"
INET_NP="${RELEASE}-agent-internet-egress"

PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# skip_all exits 0 with a LOUD banner. A skip must never be silently read as a
# pass for a SECURITY control — the banner spells out what was NOT asserted.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP dark-jira-fence (LOUD): $why"
  red " This run did NOT assert the dark-pod Jira write-token fence's NETWORK"
  red " leg (ka-0000005242): that the deployed dark-agent-egress NetworkPolicy"
  red " denies a dark pod egress to $JIRA_FENCE_HOST, that no other policy"
  red " re-grants it via the allow-union, and that non-dark agents are"
  red " unaffected. A green suite here proves NOTHING about the fence."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

echo "════════════════════════════════════════════════════"
echo " dark-pod Jira write-token fence — network leg E2E (ka-0000007263)"
echo " policies-present -> selector-split -> union-exclusivity -> jira-fenced -> nondark-control"
echo "════════════════════════════════════════════════════"

# ── 0. Preconditions ────────────────────────────────────────────────
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed"
command -v python3 >/dev/null 2>&1 || skip_all "python3 not installed"
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 \
  || skip_all "namespace '$NAMESPACE' not present"

# Fetch every NetworkPolicy in the namespace ONCE; all five legs are projections
# over this single capture (fetch-once/project-many). Retried: a transient API
# blip must not read as an absent fence.
NP_JSON=""
for _try in 1 2 3; do
  NP_JSON=$(kubectl -n "$NAMESPACE" get networkpolicy -o json 2>/dev/null) && \
    printf '%s' "$NP_JSON" | python3 -c "import sys,json; json.load(sys.stdin)['items']" 2>/dev/null && break
  NP_JSON=""
  dim "  kubectl get networkpolicy attempt $_try failed; retrying"
  sleep 2
done
[ -n "$NP_JSON" ] || skip_all "could not list NetworkPolicies in '$NAMESPACE' after 3 attempts"

NP_FILE=$(mktemp "/tmp/dark-jira-fence-np.XXXXXX")
LEG_OUT=$(mktemp "/tmp/dark-jira-fence-legs.XXXXXX")
cleanup() { rm -f "$NP_FILE" "$LEG_OUT" 2>/dev/null || true; }
trap cleanup EXIT
printf '%s' "$NP_JSON" >"$NP_FILE"

# The fence is only DEPLOYED on 2026.217.x+. Neither policy present ⇒ the
# namespace predates it ⇒ skip loud. The dark policy present but malformed is a
# HARD failure, handled by the legs below.
if ! python3 -c "
import json,sys
items=json.load(open('$NP_FILE'))['items']
names={i['metadata']['name'] for i in items}
sys.exit(0 if ('$DARK_NP' in names or '$INET_NP' in names) else 1)
" 2>/dev/null; then
  skip_all "neither '$DARK_NP' nor '$INET_NP' present in '$NAMESPACE' — deployment predates the fence (shipped 2026.217.x)"
fi

# ── 1. Resolve the Jira host ────────────────────────────────────────
# Resolution failure is a SKIP, not a failure: it says nothing about the fence.
blue "[resolve] $JIRA_FENCE_HOST"
JIRA_IPS=$(python3 -c "
import socket
try:
    _,_,ips = socket.gethostbyname_ex('$JIRA_FENCE_HOST')
    print(' '.join(sorted(set(ips))))
except Exception:
    pass
" 2>/dev/null)
[ -n "$JIRA_IPS" ] || skip_all "could not resolve $JIRA_FENCE_HOST (DNS unavailable) — cannot assert containment"
pass "resolved $JIRA_FENCE_HOST -> $JIRA_IPS"
echo

# ── 2. Drive all five legs ──────────────────────────────────────────
# One python3 pass emits "LEG <name>: PASS|FAIL <detail>" lines, mirroring the
# probe-driven precedent in test-gitlab-dark-provenance.sh. Bash maps each line
# to an assertion and REQUIRES every expected leg to be present, so a leg that
# silently stops emitting is a failure rather than an invisible no-op.
blue "[run] asserting the deployed fence"
python3 - "$NP_FILE" "$DARK_NP" "$INET_NP" "$JIRA_IPS" >"$LEG_OUT" 2>&1 <<'PYEOF'
import ipaddress
import json
import sys

np_file, dark_np, inet_np, jira_ips_raw = sys.argv[1:5]
items = json.load(open(np_file))["items"]
by_name = {i["metadata"]["name"]: i for i in items}
jira_ips = [ipaddress.ip_address(x) for x in jira_ips_raw.split()]

results = []


def leg(name, ok, detail):
    results.append((name, ok, detail))


def egress_broad_excepts(policy):
    """Return the `except` lists of every rule granting a broad 0.0.0.0/0 allow.

    A policy can grant broad internet through more than one rule; each is its
    own hole, so callers get one list per broad rule rather than a merge.
    """
    out = []
    for rule in policy.get("spec", {}).get("egress", []) or []:
        for to in rule.get("to", []) or []:
            ib = to.get("ipBlock")
            if ib and ib.get("cidr") == "0.0.0.0/0":
                out.append([ipaddress.ip_network(c) for c in ib.get("except", []) or []])
    return out


def selects(selector, labels):
    """Does a podSelector match a pod carrying `labels`? (K8s semantics.)

    An empty/absent selector selects EVERY pod in the namespace — which is
    exactly how a catch-all policy would silently re-grant dark egress, so it
    must match here rather than be skipped.
    """
    if selector is None:
        return True
    for k, v in (selector.get("matchLabels") or {}).items():
        if labels.get(k) != v:
            return False
    for expr in selector.get("matchExpressions") or []:
        key, op = expr.get("key"), expr.get("operator")
        vals = expr.get("values") or []
        if op == "Exists" and key not in labels:
            return False
        if op == "DoesNotExist" and key in labels:
            return False
        if op == "In" and labels.get(key) not in vals:
            return False
        if op == "NotIn" and labels.get(key) in vals:
            return False
    return True


# ── Leg 1: both halves of the post-fence split are deployed ─────────
missing = [n for n in (dark_np, inet_np) if n not in by_name]
leg(
    "policies-present",
    not missing,
    f"both {dark_np} and {inet_np} deployed"
    if not missing
    else f"missing: {', '.join(missing)} (a half-applied fence is a regression)",
)

# ── Leg 2: the selector split ───────────────────────────────────────
# The dark policy must select ONLY on the dark marker; the internet policy must
# EXCLUDE dark pods. If the internet policy stops excluding them, dark pods get
# unfenced internet back through the allow-union even with the dark policy intact.
if dark_np in by_name and inet_np in by_name:
    dsel = by_name[dark_np]["spec"].get("podSelector") or {}
    isel = by_name[inet_np]["spec"].get("podSelector") or {}
    dexprs = {(e.get("key"), e.get("operator")) for e in dsel.get("matchExpressions") or []}
    iexprs = {(e.get("key"), e.get("operator")) for e in isel.get("matchExpressions") or []}
    dark_ok = ("gardenio.io/dark", "Exists") in dexprs
    inet_ok = ("gardenio.io/dark", "DoesNotExist") in iexprs and (
        "gardenio.io/agent",
        "Exists",
    ) in iexprs
    problems = []
    if not dark_ok:
        problems.append(f"{dark_np} must select gardenio.io/dark Exists (got {sorted(dexprs)})")
    if not inet_ok:
        problems.append(
            f"{inet_np} must select gardenio.io/agent Exists + gardenio.io/dark DoesNotExist "
            f"(got {sorted(iexprs)}) — otherwise dark pods regain unfenced internet"
        )
    leg(
        "selector-split",
        not problems,
        "dark policy selects dark pods; internet policy excludes them"
        if not problems
        else "; ".join(problems),
    )
else:
    leg("selector-split", False, "cannot check: one half of the split is missing")

# ── Leg 3: allow-union exclusivity (the structural leg) ─────────────
# Simulate a dark agent pod and find EVERY egress policy that selects it. Only
# dark-agent-egress may grant it a broad 0.0.0.0/0 allow.
dark_pod_labels = {
    "gardenio.io/agent": "dark-fence-probe",
    "gardenio.io/dark": "true",
    "app.kubernetes.io/name": "gardenio",
}
broad_grantors = []
for name, pol in sorted(by_name.items()):
    spec = pol.get("spec", {})
    if "Egress" not in (spec.get("policyTypes") or []):
        continue
    if not selects(spec.get("podSelector"), dark_pod_labels):
        continue
    if egress_broad_excepts(pol):
        broad_grantors.append(name)

# Both failure directions are real regressions, but they are OPPOSITE faults and
# an operator reading the red needs to know which: extra grantors mean the fence
# is BREACHED (Atlassian reachable again); zero grantors mean dark egress
# COLLAPSED (dark agents lost all internet, so they cannot reach the model API
# or git either). A single "any extra grantor" message would misdiagnose the latter.
if broad_grantors == [dark_np]:
    union_detail = f"{dark_np} is the ONLY egress policy granting a dark pod broad internet"
elif not broad_grantors:
    union_detail = (
        f"NO egress policy grants a dark pod broad internet (expected {dark_np}). "
        "Atlassian is fenced a fortiori, but dark agents have lost ALL internet egress "
        "— check whether the broad-allow rule was dropped from the dark policy"
    )
else:
    extra = [n for n in broad_grantors if n != dark_np]
    union_detail = (
        f"broad-internet grantors for a dark pod = {broad_grantors}, want ['{dark_np}']; "
        f"NetworkPolicy is allow-union, so {extra} re-open(s) Atlassian to dark pods"
    )
leg("union-exclusivity", broad_grantors == [dark_np], union_detail)

# ── Leg 4: the real Jira address is fenced for dark pods ────────────
if dark_np in by_name:
    broads = egress_broad_excepts(by_name[dark_np])
    if not broads:
        # No broad allow at all ⇒ Jira is unreachable a fortiori. Honest pass,
        # but labelled so it is not mistaken for the Atlassian-CIDR fence.
        leg(
            "jira-fenced",
            True,
            f"{dark_np} grants no broad 0.0.0.0/0 egress at all — dark pods cannot reach {jira_ips_raw} a fortiori",
        )
    else:
        unfenced = []
        covering = []
        for excepts in broads:
            for ip in jira_ips:
                hit = next((c for c in excepts if ip in c), None)
                if hit is None:
                    unfenced.append(str(ip))
                else:
                    covering.append(f"{ip}∈{hit}")
        leg(
            "jira-fenced",
            not unfenced,
            f"every {jira_ips_raw} IP is excepted from the dark broad-allow ({', '.join(sorted(set(covering)))})"
            if not unfenced
            else (
                f"NOT fenced: {sorted(set(unfenced))} fall INSIDE {dark_np}'s broad allow — "
                "a dark pod can open a connection to real upstream Jira"
            ),
        )
else:
    leg("jira-fenced", False, f"{dark_np} absent")

# ── Leg 5: the non-dark control (keeps leg 4 from being vacuous) ────
# Non-dark agents must STILL reach Jira. If they don't, egress is broken for
# everyone and leg 4's green says nothing about the dark fence specifically.
if inet_np in by_name:
    broads = egress_broad_excepts(by_name[inet_np])
    if not broads:
        leg(
            "nondark-control",
            False,
            f"{inet_np} grants no broad 0.0.0.0/0 egress — non-dark agents cannot reach "
            "the internet either, so the dark fence assertion above is vacuous",
        )
    else:
        blocked = []
        for excepts in broads:
            for ip in jira_ips:
                hit = next((c for c in excepts if ip in c), None)
                if hit is not None:
                    blocked.append(f"{ip}∈{hit}")
        leg(
            "nondark-control",
            not blocked,
            f"non-dark agents still reach {jira_ips_raw} — the fence is dark-scoped, not a global block"
            if not blocked
            else (
                f"non-dark agents are ALSO blocked from Jira ({sorted(set(blocked))}) — either the "
                "Atlassian deny leaked into the non-dark policy or egress is globally broken; "
                "the dark-fence assertion is vacuous until this is resolved"
            ),
        )
else:
    leg("nondark-control", False, f"{inet_np} absent")

for name, ok, detail in results:
    print(f"LEG {name}: {'PASS' if ok else 'FAIL'} {detail}")
PYEOF
py_rc=$?

EXPECTED_LEGS="policies-present selector-split union-exclusivity jira-fenced nondark-control"
for leg in $EXPECTED_LEGS; do
  line=$(grep -E "^LEG ${leg}: " "$LEG_OUT" 2>/dev/null | head -1 || true)
  if [ -z "$line" ]; then
    fail "leg $leg" "no result line emitted (leg dropped)"
    continue
  fi
  detail="${line#LEG "${leg}": }"
  case "$detail" in
    PASS*) pass "leg $leg — ${detail#PASS }" ;;
    FAIL*) fail "leg $leg" "${detail#FAIL }" ;;
    *)     fail "leg $leg" "unrecognized result: $detail" ;;
  esac
done

# A python-level error (traceback, bad JSON) must never be masked by a clean
# leg parse.
if [ "$py_rc" -ne 0 ] && [ "$FAIL" -eq 0 ]; then
  fail "assertion driver" "python exited $py_rc but all parsed legs passed: $(tail -3 "$LEG_OUT")"
fi
echo

# ── Summary ─────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed"
else
  red " ✗ $FAIL/$total assertions failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  echo
  dim "  Driver output:"
  sed 's/^/    /' "$LEG_OUT" 2>/dev/null || true
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
