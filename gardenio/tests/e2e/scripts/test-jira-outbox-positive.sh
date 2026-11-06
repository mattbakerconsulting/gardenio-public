#!/usr/bin/env bash
# test-jira-outbox-positive.sh — OUTBOX positive/apply E2E (sandbox FSM via GFO)
#
# The POSITIVE (happy-path) sibling of test-jira-outbox-negative.sh. It closes
# the "Synced != applied" FALSE-GREEN that hid the P0 silent no-op
# (esc gd-l0ffwDFvQ8 / bug gd-d7U7jd6cyH, adversarially reproduced 2026-07-10):
# GBFSM writes were transitioning Pending→Synced while the target FSM ticket was
# NEVER modified (no label / no version). Synced alone proved untrustworthy, so
# this module proves the effect actually LANDED on FSM, read back via the browse
# seat. This module IS that gate.
#
# ── The 1:1 reshape (ka-0000002637, this task gd-NhkcCUqCPI) ──────────
#
# The outbox no longer fans out one INTENT ticket per mutation (Add-Label /
# Set-Version / Internal-Comment, each with op_value). It now upserts ONE GBFSM
# "Analysis" issue-type ticket per FSM ticket, carrying the N triage labels +
# version as FIELDS + the analysis in the Description, keyed to FSM by
# fsm_target_key (customfield_10148). This gate fabricates that 1:1 shape via
# gfo_analysis_payload / gfo_create_analysis — NOT the retired per-op intents.
#
# ── THE CORE INVARIANT ────────────────────────────────────────────────
#
#   For a VALID Analysis ticket (allowlisted labels + a legal version) reaching
#   Synced MUST be CORROBORATED by the effect actually LANDING on the FSM ticket,
#   read back via the gardenio@ browse seat. A Synced-with-no-effect is a HARD FAIL
#   — the exact gd-d7U7jd6cyH signature ("SILENT-NOOP: Synced but the effect never
#   landed on FSM"). Plus: exactly ONE GBFSM ticket per FSM-N (no fan-out).
#
# ── KEY DEVIATION FROM THE NEGATIVE SUITE ─────────────────────────────
#
# In the negative suite the FSM-side browse read is an OPTIONAL enhancer (absent
# ⇒ the GBFSM-side primary assertion still runs). On the POSITIVE path the browse
# read is MANDATORY: the whole point is to prove the apply LANDED, and Synced
# alone is precisely the signal that proved untrustworthy. So when the base rig
# is present but the gardenio@ browse seat (JIRA_GARDENIO_BROWSE_EMAIL/TOKEN, K8s
# secret jira-gardenio-browse-cred) is ABSENT, the positive apply matrix SKIPS
# with a LOUD note ("cannot corroborate apply without the gardenio@ browse seat")
# — it NEVER infers applied from Synced. This is a deliberate guard against any
# future edit that would let the gate degrade back to a Synced-only false-green.
#
# ── The tests (ka-0000000600 matrix, re-pointed to the 1:1 Analysis shape) ────
#
#   0e  SHAPE SELF-TEST : ALWAYS-ON, non-network — the bot-ownable CREATE shape is
#                    fixed by merged code, so we assert gfo_analysis_payload builds
#                    exactly ONE Analysis ticket (labels/version as FIELDS,
#                    fsm_target_key as DATA, idempotency_key, ADF link) with NO
#                    op_value / comment / public-visibility surface, and prove the
#                    gate discriminates. Shared with the negative suite.
#   T-1to1 (was P-LABEL T2 + P-VERSION T4): ONE Analysis ticket carrying the
#                    canonical label + version → MUST reach Synced; exactly ONE
#                    GBFSM ticket exists (gfo_find_analysis count==1, no fan-out);
#                    and BOTH effects LAND on FSM (browse-read). ATTRIBUTION GUARD
#                    (verbatim from the old P-LABEL/P-VERSION): only an effect ABSENT
#                    at this run's baseline and present AFTER Synced is attributable
#                    to THIS ticket (label = additive +1 no-clobber; version = from-
#                    ABSENT). A pre-existing effect ⇒ SKIP LOUDLY, never a green
#                    'applied' off a leftover (reset the disposable ticket). The
#                    version leg needs JIRA_SANDBOX_FSM_VERSION (a pre-existing
#                    project version; absent ⇒ that leg skips, a missing version 400s).
#   T-link         : the GBFSM ticket's fsm_target_key resolves to the FSM-N and its
#                    Description carries the clickable FSM link (the bot-emitted
#                    GBFSM→FSM half). The FSM→GBFSM back-link is the operator R-LINK
#                    rule (pending/operator-owned) — SKIP LOUDLY, never asserted live.
#   T-upsert (was P-IDEMPOTENT): re-triage the SAME FSM-N → still exactly ONE GBFSM
#                    ticket (upsert via SearchGBFSM→EditGBFSMTicket, not a second
#                    create) AND the FSM label count does NOT grow (idempotent apply).
#
# RETIRED (per the 1:1 reshape — the merged writer emits no FSM comment):
#   P-COMMENT (T5/T9 internal-comment landing + jsdPublic pin) → the customer-
#   notification-safety property survives as the 0e ABSENT assertions (no comment/
#   op/public surface in the bot Analysis payload, BY CONSTRUCTION) + verify-outbox-
#   rules-json.sh invariant (c) at the rule layer. There is no FSM comment to land.
#
# Each apply prints "Synced STRICTLY IMPLIES applied" on pass.
#
# ── Skip-clean contract ──────────────────────────────────────────────
#
# Exactly like test-jira-outbox-negative.sh and test-jira-write-control-sandbox.sh:
# the operator-provisioned sandbox rigging (gardenio2 Jira cred, GFO project key,
# a disposable sandbox FSM ticket, the live Automation rules) does NOT exist in
# the ephemeral E2E namespace, so every LIVE gate SKIPS CLEANLY (pass, not fail)
# and the module is GREEN today. It lights up automatically once the operator
# provides the env vars below (or the K8s secrets they sync from). Against the
# currently-broken live rules it will CORRECTLY go RED once creds are provisioned
# — that RED is the intended true-positive signal for gd-d7U7jd6cyH.
#
# Required env for the LIVE apply gates (all absent ⇒ skip clean):
#   JIRA_SANDBOX_BASE_URL   Sandbox Jira base, e.g. https://acme-sandbox.atlassian.net
#   JIRA_SANDBOX_FSM_KEY    A disposable sandbox FSM ticket key, e.g. FSM-408
#   JIRA_GARDENIO2_EMAIL     Privileged-on-GFO identity email (gardenio2)
#   JIRA_GARDENIO2_TOKEN     gardenio2 API token (privileged on GFO, zero on FSM)
#   JIRA_GFO_PROJECT_KEY    Gardenio FSM Outbox project key, e.g. GFO (project id 10037)
#
# MANDATORY on the positive path (base rig present but this absent ⇒ apply matrix
# SKIPS LOUDLY; Synced is NEVER treated as applied):
#   JIRA_GARDENIO_BROWSE_EMAIL  gardenio@ identity email (FSM read seat)
#   JIRA_GARDENIO_BROWSE_TOKEN  gardenio@ API token (browse/read-only on FSM)
#
# Optional env:
#   JIRA_OUTBOX_POSITIVE_FSM_KEY  A disposable FSM ticket for THIS suite,
#                                 defaulting to JIRA_SANDBOX_FSM_KEY. Prefer a
#                                 distinct ticket the operator resets, to avoid
#                                 contention with the write-control / negative
#                                 suites on the shared disposable ticket.
#   JIRA_SANDBOX_FSM_VERSION      A PRE-EXISTING project version name for the T-1to1
#                                 version leg (absent ⇒ that leg skips).
#   FSM_APPLY_WAIT                Bounded window (seconds) to poll the FSM-side
#                                 read for a slow-but-correct async apply before
#                                 declaring the effect absent (default 45).
#
# The gardenio2 / browse creds may instead be sourced from K8s secrets in
# $NAMESPACE (jira-gardenio2-cred / jira-gardenio-browse-cred) — see
# load_sandbox_creds. Secrets are NEVER hardcoded here.
#
# ── Teardown (why the probe cannot revert the FSM effects) ───────────
#
# The FSM label/version are written by the Automation ACTOR, not by any credential
# this probe holds: the gardenio@ browse seat is 403-denied on FSM writes (proven by
# test-jira-write-control-sandbox.sh:296-323) and gardenio2 has ZERO FSM access. So
# the probe CANNOT reliably revert an applied label or version. The GBFSM Analysis
# ticket(s) we create ARE deletable (a GBFSM-project write); we best-effort delete
# them. Prefer a disposable JIRA_OUTBOX_POSITIVE_FSM_KEY the operator resets so the
# from-ABSENT attribution baseline holds. Teardown NEVER hard-fails.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()
CREATED_GFO=()       # GBFSM (GFO) Analysis-ticket keys we created, for best-effort teardown

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# The FSM ticket this suite writes to. Prefer a distinct disposable ticket so the
# positive apply writes don't contend with the write-control / negative suites on
# the shared ticket; default to the base-rig ticket when unset.
FSM_KEY="${JIRA_OUTBOX_POSITIVE_FSM_KEY:-${JIRA_SANDBOX_FSM_KEY:-}}"

# ── 1:1 GBFSM Analysis-ticket shape (reshape ka-0000002637, Phase 2 ka-0000001491) ─
# gardenio upserts ONE GBFSM "Analysis" issue-type ticket per FSM ticket carrying the
# N triage labels + version as FIELDS + the analysis in the Description, keyed to FSM
# by fsm_target_key (customfield_10148). This SUPERSEDES the retired per-mutation
# intent fan-out (Add-Label 10040 / Set-Version 10041 / Internal-Comment 10042, each
# with op_value in customfield_10149).
#
# The shared shape config vars (ANALYSIS_TYPE / LABELS_FIELD / VERSION_FIELD /
# CF_TARGET / CF_IDEM / ALLOWLIST_RE), the pure builders (gfo_analysis_payload /
# gfo_idem_key / gfo_dedup_key) and the always-on shape self-test (shape_assert /
# shape_selftest) now live in lib-outbox-shape.sh — the SINGLE source of truth shared
# with test-jira-outbox-negative.sh and the hermetic main-push gate
# (test-outbox-shape-selftest.sh, Phase 5c ka-0000003147). It is sourced below.

# How long to wait for the async Automation rule to move an intent to a terminal
# column (Synced/Failed). Smoke evidence: ~20s per sync; 90s gives ample margin.
GFO_SYNC_TIMEOUT="${GFO_SYNC_TIMEOUT:-90}"

# After a VALID intent reaches Synced, poll the FSM-side read for this bounded
# window before concluding the effect is absent — a slow-but-correct async apply
# must NOT be misread as a silent no-op (gd-d7U7jd6cyH false-positive guard).
FSM_APPLY_WAIT="${FSM_APPLY_WAIT:-45}"

# The canonical classification label the positive P-LABEL apply uses. It is the
# exact value the bridge emitter produces in practice and MUST match the R-LABEL
# allowlist (ALLOWLIST_RE, in lib-outbox-shape.sh) — the field-i7pu drift (a rule
# that dropped the 'triaged-' infix) would silently dead-letter this exact label.
CANON_LABEL="${JIRA_OUTBOX_CANON_LABEL:-gardenio-triaged-sev-3}"

# ── Helpers (mirrors test-jira-outbox-negative.sh) ────────────────
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
# skip records a cleanly-skipped gate as a PASS (rigging absent ≠ failure).
skip() { dim "  ⤼ SKIP: $1"; [ -n "${2:-}" ] && dim "    $2"; PASS=$((PASS + 1)); }

assert_contains() {
  if grep -qF -- "$2" <<< "$3"; then pass "$1 (contains '$2')"
  else fail "$1" "expected to contain '$2'; got: $(echo "$3" | head -3)"; fi
}

# ── Shared 1:1 CREATE-shape library ───────────────────────────────
# Sets the shape config vars (ANALYSIS_TYPE / LABELS_FIELD / VERSION_FIELD /
# CF_TARGET / CF_IDEM / ALLOWLIST_RE) and defines the pure builders
# (gfo_analysis_payload / gfo_idem_key / gfo_dedup_key) + the always-on shape
# self-test (shape_assert / shape_selftest). Sourced AFTER pass/fail are defined
# (shape_selftest uses them). Same lib the negative suite + the hermetic main-push
# gate source, so the fabricated shape can never drift between them.
# shellcheck source=./lib-outbox-shape.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-outbox-shape.sh"

# ── Fail-loud rig guard (REQUIRE_OUTBOX_RIG) ──────────────────────────
# outbox_rig_guard turns a MANDATORY-but-absent live rig/seat into today's clean
# skip-PASS (default) OR a loud red NO-GO FAIL (REQUIRE_OUTBOX_RIG on — the
# standing regression gate, ka-0000007489). Modeled on lib-gvisor-node.sh's
# gvisor_require_substrate_guard. Sourced AFTER red/skip/fail are defined (the
# guard's caller contract). Same lib the negative suite sources. On the POSITIVE
# path BOTH the sandbox rig AND the gardenio@ browse seat are MANDATORY (the whole
# point is to prove the effect LANDED — Synced alone is the gd-d7U7jd6cyH false
# green), so BOTH collapse points below route through the guard.
# shellcheck source=./lib-outbox-rig.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-outbox-rig.sh"

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"; return; fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19109
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-pos.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  # if...fi (not `[ ... ] && export`) so a MISSING secret — the CI/ephemeral-ns
  # reality the skip-clean contract targets — does NOT make this function return
  # non-zero and abort the whole script under `set -e` before main() runs.
  if [ -n "$secret" ]; then export BEADS_TOKEN="$secret"; fi
}

# load_sandbox_creds fills the JIRA_GARDENIO2_* / JIRA_GARDENIO_BROWSE_* vars from
# K8s secrets when they are not already set in the env. Best-effort and silent:
# any that stay empty gate a clean skip below.
load_sandbox_creds() {
  local g2 gb
  g2=$(kubectl -n "$NAMESPACE" get secret jira-gardenio2-cred -o json 2>/dev/null || echo "")
  if [ -n "$g2" ]; then
    : "${JIRA_GARDENIO2_EMAIL:=$(echo "$g2" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('email','')).decode() or '')" 2>/dev/null || echo "")}"
    : "${JIRA_GARDENIO2_TOKEN:=$(echo "$g2" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('token','')).decode() or '')" 2>/dev/null || echo "")}"
  fi
  gb=$(kubectl -n "$NAMESPACE" get secret jira-gardenio-browse-cred -o json 2>/dev/null || echo "")
  if [ -n "$gb" ]; then
    : "${JIRA_GARDENIO_BROWSE_EMAIL:=$(echo "$gb" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('email','')).decode() or '')" 2>/dev/null || echo "")}"
    : "${JIRA_GARDENIO_BROWSE_TOKEN:=$(echo "$gb" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('token','')).decode() or '')" 2>/dev/null || echo "")}"
  fi
}

# sandbox_rig_ready returns 0 iff every var needed for the LIVE GFO gates is
# present. When non-zero, the live gates skip clean.
sandbox_rig_ready() {
  [ -n "${JIRA_SANDBOX_BASE_URL:-}" ] && [ -n "${JIRA_SANDBOX_FSM_KEY:-}" ] \
    && [ -n "${JIRA_GARDENIO2_EMAIL:-}" ] && [ -n "${JIRA_GARDENIO2_TOKEN:-}" ] \
    && [ -n "${JIRA_GFO_PROJECT_KEY:-}" ] && [ -n "${FSM_KEY:-}" ]
}

# fsm_read_ready returns 0 iff the gardenio@ browse cred is present. On the
# POSITIVE path this is MANDATORY (see header) — absent ⇒ the apply matrix skips
# LOUDLY rather than inferring applied from Synced.
fsm_read_ready() {
  [ -n "${JIRA_GARDENIO_BROWSE_EMAIL:-}" ] && [ -n "${JIRA_GARDENIO_BROWSE_TOKEN:-}" ]
}

# ── Thin Jira REST v3 curl wrappers ───────────────────────────────
jira_curl_code() { # method path email token json
  local method="$1" path="$2" email="$3" token="$4" json="${5:-}"
  local args=(-s -o /dev/null -w '%{http_code}' -X "$method"
    -u "${email}:${token}" -H "Content-Type: application/json"
    "${JIRA_SANDBOX_BASE_URL}${path}")
  [ -n "$json" ] && args+=(-d "$json")
  curl "${args[@]}" 2>/dev/null || echo "000"
}

jira_get_body() { # path email token
  curl -s -u "${2}:${3}" -H "Accept: application/json" \
    "${JIRA_SANDBOX_BASE_URL}${1}" 2>/dev/null || echo "{}"
}

# jira_get_ok echoes the response body ONLY when the HTTP status is 2xx;
# otherwise it echoes "HTTP_ERR:<code>" (or HTTP_ERR:000 on a transport failure).
# This lets FSM-side readers DISTINGUISH a denied/rate-limited read (403/404/429)
# — which the browse seat can hit on the JSM comment endpoint — from a genuinely
# empty result, so a denied read becomes a clean SKIP rather than a false FAIL.
jira_get_ok() { # path email token
  local resp code body
  resp=$(curl -s -w $'\n%{http_code}' -u "${2}:${3}" -H "Accept: application/json" \
    "${JIRA_SANDBOX_BASE_URL}${1}" 2>/dev/null || printf '\n000')
  code=$(printf '%s' "$resp" | tail -n1)
  body=$(printf '%s' "$resp" | sed '$d')
  case "$code" in
    2??) printf '%s' "$body" ;;
    *)   printf 'HTTP_ERR:%s' "$code" ;;
  esac
}

# ── 1:1 GBFSM Analysis-ticket network helpers (create + count) ─────
# The PURE builders (gfo_analysis_payload / gfo_idem_key / gfo_dedup_key) live in
# lib-outbox-shape.sh, sourced above. The two functions below are the LIVE (network)
# create + count path — they need the sandbox rig / gardenio2 creds, so they stay
# script-local. gfo_create_analysis POSTs the ONE upserted GBFSM "Analysis" ticket the
# merged 1:1 writer produces (jira_outbox.go CreateGBFSMTicket), NOT a per-op intent;
# gfo_find_analysis is the shell mirror of the writer's SearchGBFSM exact-match.

# gfo_create_analysis creates the ONE GBFSM Analysis ticket AS GARDENIO2 and echoes
# "KEY|HTTPCODE" (KEY empty on failure). The SINGLE live create path (targets the
# GBFSM/GFO project only, never FSM).
gfo_create_analysis() { # target labels_csv version idem_key body [reporter_id]
  local payload resp code key bodyjson browse=""
  [ -n "${JIRA_SANDBOX_BASE_URL:-}" ] && browse="${JIRA_SANDBOX_BASE_URL}/browse/$1"
  payload=$(gfo_analysis_payload "$1" "$2" "$3" "$4" "$5" "$browse" "${6:-}")
  if [ -z "$payload" ]; then echo "|000"; return; fi
  resp=$(curl -s -w $'\n%{http_code}' -X POST \
    -u "${JIRA_GARDENIO2_EMAIL}:${JIRA_GARDENIO2_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "$payload" "${JIRA_SANDBOX_BASE_URL}/rest/api/3/issue" 2>/dev/null || printf '\n000')
  code=$(printf '%s' "$resp" | tail -n1)
  bodyjson=$(printf '%s' "$resp" | sed '$d')
  key=$(printf '%s' "$bodyjson" | python3 -c "import sys,json; print(json.load(sys.stdin).get('key',''))" 2>/dev/null || echo "")
  [ -n "$key" ] && CREATED_GFO+=("$key")
  echo "${key}|${code}"
}

# gfo_find_analysis counts the GBFSM tickets whose fsm_target_key EQUALS target, read
# as gardenio2 via JQL (project=<GFO> AND cf[<n>]="<target>") — the shell mirror of the
# writer's SearchGBFSM exact-match. Echoes an integer count (the at-most-one-per-FSM-N
# invariant is count==1), or "ERR" on a denied/failed read so the caller can SKIP.
gfo_find_analysis() { # target
  local target="$1" cfnum jql resp code body
  cfnum="${CF_TARGET#customfield_}"
  jql="project = \"${JIRA_GFO_PROJECT_KEY}\" AND cf[${cfnum}] = \"${target}\""
  resp=$(curl -s -w $'\n%{http_code}' -G \
    -u "${JIRA_GARDENIO2_EMAIL}:${JIRA_GARDENIO2_TOKEN}" \
    -H "Accept: application/json" \
    --data-urlencode "jql=${jql}" \
    --data-urlencode "fields=${CF_TARGET}" \
    --data-urlencode "maxResults=50" \
    "${JIRA_SANDBOX_BASE_URL}/rest/api/3/search/jql" 2>/dev/null || printf '\n000')
  code=$(printf '%s' "$resp" | tail -n1)
  body=$(printf '%s' "$resp" | sed '$d')
  case "$code" in 2??) : ;; *) echo "ERR"; return ;; esac
  TARGET="$target" CF_TARGET="$CF_TARGET" python3 -c '
import sys, json, os
target = os.environ["TARGET"]; cf = os.environ["CF_TARGET"]
try:
    d = json.load(sys.stdin)
except Exception:
    print("ERR"); sys.exit(0)
n = 0
for it in d.get("issues", []):
    if (it.get("fields", {}) or {}).get(cf) == target:
        n += 1
print(n)
' <<< "$body" 2>/dev/null || echo "ERR"
}

# gfo_wait_status polls a GFO intent's status name (read as gardenio2, who is
# privileged on GFO) until it reaches a terminal column or times out. Echoes the
# status name (or "TIMEOUT" / "").
gfo_wait_status() { # key timeout_s
  local key="$1" timeout="${2:-$GFO_SYNC_TIMEOUT}" waited=0 st=""
  while [ "$waited" -lt "$timeout" ]; do
    st=$(jira_get_body "/rest/api/3/issue/${key}?fields=status" \
      "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('fields',{}).get('status',{}).get('name',''))" 2>/dev/null || echo "")
    case "$st" in
      Synced|Failed) echo "$st"; return 0 ;;
    esac
    sleep 5; waited=$((waited + 5))
  done
  echo "${st:-TIMEOUT}"
}

# ── FSM-side apply probes (read via the browse identity) ──────────
# Each echoes one of: present | absent | readerr. A readerr (denied/rate-limited
# browse read) is a rigging limitation, distinct from a resolvable absent.

# fsm_labels_body echoes the FSM labels-field body (jira_get_ok semantics).
fsm_labels_body() {
  jira_get_ok "/rest/api/3/issue/${FSM_KEY}?fields=labels" \
    "$JIRA_GARDENIO_BROWSE_EMAIL" "$JIRA_GARDENIO_BROWSE_TOKEN"
}

# fsm_label_present label -> present | absent | readerr
fsm_label_present() { # label
  local label="$1" body
  body=$(fsm_labels_body)
  if [[ "$body" == HTTP_ERR:* ]]; then echo "readerr"; return; fi
  LABEL="$label" python3 -c '
import sys, json, os
label = os.environ["LABEL"]
try:
    d = json.load(sys.stdin)
except Exception:
    print("readerr"); sys.exit(0)
labels = d.get("fields", {}).get("labels", [])
print("present" if label in labels else "absent")
' <<< "$body" 2>/dev/null || echo "readerr"
}

# fsm_version_present version -> present | absent | readerr (checks both the
# affects-versions and fix-versions arrays, by NAME).
fsm_version_present() { # version
  local version="$1" body
  body=$(jira_get_ok "/rest/api/3/issue/${FSM_KEY}?fields=versions,fixVersions" \
    "$JIRA_GARDENIO_BROWSE_EMAIL" "$JIRA_GARDENIO_BROWSE_TOKEN")
  if [[ "$body" == HTTP_ERR:* ]]; then echo "readerr"; return; fi
  VERSION="$version" python3 -c '
import sys, json, os
version = os.environ["VERSION"]
try:
    d = json.load(sys.stdin)
except Exception:
    print("readerr"); sys.exit(0)
f = d.get("fields", {})
names = set()
for key in ("versions", "fixVersions"):
    for v in (f.get(key) or []):
        n = v.get("name")
        if n:
            names.add(n)
print("present" if version in names else "absent")
' <<< "$body" 2>/dev/null || echo "readerr"
}

# fsm_wait_present PROBE ARG TIMEOUT — polls `PROBE ARG` every 5s until it echoes
# "present" (→ present) or "readerr" (→ readerr, bail so the caller can SKIP);
# on timeout echoes "absent". This is the bounded-window guard so a slow-but-
# correct async apply is not misread as a silent no-op.
fsm_wait_present() { # probe_fn arg timeout
  local probe="$1" arg="$2" timeout="${3:-$FSM_APPLY_WAIT}" waited=0 st=""
  while [ "$waited" -lt "$timeout" ]; do
    st=$("$probe" "$arg")
    case "$st" in
      present) echo "present"; return 0 ;;
      readerr) echo "readerr"; return 0 ;;
    esac
    sleep 5; waited=$((waited + 5))
  done
  echo "absent"
}

# labels_from_body json -> space-separated labels (empty on parse error).
labels_from_body() { # json
  BODY="$1" python3 -c '
import os, json
try:
    d = json.loads(os.environ["BODY"])
    print(" ".join(d.get("fields", {}).get("labels", [])))
except Exception:
    print("")
' 2>/dev/null || echo ""
}

# label_count_from_body json -> integer count, or "ERR" on parse failure.
label_count_from_body() { # json
  BODY="$1" python3 -c '
import os, json
try:
    d = json.loads(os.environ["BODY"])
    print(len(d.get("fields", {}).get("labels", [])))
except Exception:
    print("ERR")
' 2>/dev/null || echo "ERR"
}

# labels_subset before_json after_json -> yes (every before-label still present
# in after = no clobber) | no | err.
labels_subset() { # before_json after_json
  BEFORE="$1" AFTER="$2" python3 -c '
import os, json
def labels(s):
    return set(json.loads(s).get("fields", {}).get("labels", []))
try:
    b = labels(os.environ["BEFORE"])
    a = labels(os.environ["AFTER"])
except Exception:
    print("err"); raise SystemExit(0)
print("yes" if b <= a else "no")
' 2>/dev/null || echo "err"
}

# NOTE: the FSM-comment helpers (fsm_comment_marker_present /
# fsm_comment_count_with_marker / fsm_marker_comment_is_internal) that the retired
# P-COMMENT test used were removed with the 1:1 reshape — the merged writer emits no
# FSM comment, so there is nothing to count or jsdPublic-pin. The customer-
# notification-safety property survives as the always-on Scenario 0e ABSENT
# assertions (no comment/op/public surface in the bot Analysis payload).

cleanup() {
  # Best-effort teardown. We delete every GBFSM Analysis ticket we created (a GBFSM-
  # project write, always safe). The label/version the Automation ACTOR may apply to
  # FSM are NOT revertible by this probe — the browse seat is 403-denied on FSM writes
  # and gardenio2 has zero FSM access — so reset the disposable ticket operator-side.
  # The 1:1 reshape writes no FSM comment, so there is no FSM-comment teardown.
  # Teardown NEVER hard-fails.
  if sandbox_rig_ready; then
    for k in "${CREATED_GFO[@]:-}"; do
      [ -z "$k" ] && continue
      jira_curl_code DELETE "/rest/api/3/issue/${k}?deleteSubtasks=true" \
        "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" >/dev/null 2>&1 || true
    done
    dim "  Attempted teardown of the GBFSM Analysis ticket(s) we created; any label/version the Automation actor applied to FSM are NOT revertible by this probe — reset the disposable JIRA_OUTBOX_POSITIVE_FSM_KEY ticket operator-side."
  fi
  for id in "${CREATED_BEADS[@]:-}"; do "$KD_BIN" close "$id" 2>/dev/null || true; done
  if [ -n "$PF_PID" ]; then kill "$PF_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " Jira OUTBOX positive/apply E2E (sandbox FSM via GFO)"
  blue " gd-gYv0BhZIjc — Synced STRICTLY IMPLIES applied-to-FSM"
  blue " closes the false-green of gd-d7U7jd6cyH / gd-l0ffwDFvQ8"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token
  load_sandbox_creds

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"; exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG
  TEST_TAG="e2e-pos-$$-$(date +%s)"

  # ── Scenario 0: always-on invariants (non-vacuous when rig absent) ──
  # Two cheap gates that keep the module meaningful even when the sandbox rig is
  # absent (i.e. in the ephemeral E2E namespace today).

  # 0a. ALLOWLIST ACCEPTS THE CANONICAL LABEL — the positive P-LABEL apply uses
  #     CANON_LABEL (gardenio-triaged-sev-3). The R-LABEL rule must ACCEPT it, or
  #     a valid classification intent would be silently dead-lettered (the exact
  #     field-i7pu 'triaged-' infix drift). We assert both the specific label the
  #     apply uses AND the full canonical emitter vocabulary is accepted.
  blue "Scenario 0a: R-LABEL allowlist ACCEPTS the canonical classification vocabulary (field-i7pu guard)"
  if [[ "$CANON_LABEL" =~ $ALLOWLIST_RE ]]; then
    pass "allowlist accepts the P-LABEL apply value '$CANON_LABEL' (the exact label the positive apply lands)"
  else
    fail "allowlist accepts canonical label" "'$CANON_LABEL' should MATCH the allowlist — a valid classification intent would be dead-lettered"
  fi
  local good ok=1
  for good in gardenio-triaged gardenio-triaged-sev-0 gardenio-triaged-sev-3 \
              gardenio-triaged-gxp-none gardenio-triaged-gxp-high sev-0 sev-3 \
              gxp-none gxp-med; do
    if ! [[ "$good" =~ $ALLOWLIST_RE ]]; then
      fail "allowlist accepts canonical label" "'$good' should MATCH the allowlist"
      ok=0
    fi
  done
  [ "$ok" = 1 ] && pass "allowlist accepts ALL canonical triage labels (gardenio-triaged / triaged-sev-[0-3] / triaged-gxp-{none,low,med,high} / sev-[0-3] / gxp-{none,low,med,high}) — every valid apply value reaches Synced, none silently dead-lettered"
  echo

  # 0b. DAEMON ROUND-TRIP — prove the daemon is reachable and round-trips a
  #     realistic triage-classification bead carrying the bound FSM key + the
  #     canonical classification labels (the exact surface the outbox emitter
  #     reads off a bead), so the module isn't a pure skip in the ephemeral ns.
  blue "Scenario 0b: triage-classification bead round-trips (daemon reachable)"
  local sbx_key="${FSM_KEY:-FSM-408}"
  local tri_out tri_id
  tri_out=$("$KD_BIN" create "pos-triage-bead-$TEST_TAG" --type=task \
    -l "project:gardenio" -l "gardenio-triaged" -l "$CANON_LABEL" -l "gardenio-triaged-gxp-med" \
    -f "jira_key=$sbx_key" \
    -d "Triage classification bead for the outbox positive-apply E2E" \
    --json 2>/dev/null || echo "{}")
  tri_id=$(echo "$tri_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$tri_id" ]; then
    CREATED_BEADS+=("$tri_id")
    pass "triage-classification bead created ($tri_id)"
    local show_out
    show_out=$("$KD_BIN" show "$tri_id" --json 2>/dev/null || echo "{}")
    assert_contains "bead carries bound FSM key" "$sbx_key" "$show_out"
    assert_contains "bead carries the canonical apply label" "$CANON_LABEL" "$show_out"
  else
    fail "triage-classification bead" "could not create"
  fi
  echo

  # 0e. 1:1 ANALYSIS CREATE-SHAPE SELF-TEST (always-on, NON-network) — the
  #     bot-ownable half of the reshape. The CREATE shape the outbox fabricates is
  #     fully determined by the MERGED writer (jira_outbox.go CreateGBFSMTicket +
  #     mutationFields) + the FROZEN schema, NOT by the operator's Automation rules,
  #     so we assert it faithfully with ZERO creds. Shares the shape self-test with
  #     test-jira-outbox-negative.sh; carries the non-network faces of T-1to1/
  #     T-upsert/T-link and the customer-notification-safety survival of P-COMMENT.
  shape_selftest
  echo

  # ── The live GBFSM positive apply matrix (1:1 Analysis shape) ─────
  # REQUIRE_OUTBOX_RIG off (default): an absent rig/seat is a clean skip-PASS —
  # behaviour UNCHANGED. REQUIRE_OUTBOX_RIG on (the standing regression gate the
  # operator arms once the sandbox + browse creds are provisioned, ka-0000007489):
  # an absent rig OR an absent browse seat is a loud NO-GO hard FAIL — on the
  # POSITIVE path the FSM-side read is MANDATORY, so BOTH are rig-mandatory.
  if ! sandbox_rig_ready; then
    blue "Live GBFSM positive apply matrix (T-1to1/T-upsert/T-link)"
    outbox_rig_guard "live outbox positive apply gates" \
      "sandbox rig absent (need JIRA_SANDBOX_BASE_URL + JIRA_SANDBOX_FSM_KEY + gardenio2 cred + JIRA_GFO_PROJECT_KEY); provisioned operator-side from SM gardenio/jira-sandbox-api-token"
    echo
  elif ! fsm_read_ready; then
    # MANDATORY-browse gate. The whole point of the positive path is to prove the
    # effect LANDED — Synced alone is exactly the signal that proved a false
    # green (gd-d7U7jd6cyH). Without the browse seat we CANNOT corroborate, so we
    # SKIP LOUDLY and NEVER infer applied from Synced. This is a deliberate guard
    # against any future edit that degrades the gate back to Synced-only. Under
    # REQUIRE_OUTBOX_RIG the missing MANDATORY seat is a NO-GO hard FAIL, not a skip.
    blue "Live GBFSM positive apply matrix (T-1to1/T-upsert/T-link)"
    red "  ⚠ MANDATORY FSM-side browse read is ABSENT — cannot corroborate apply without the gardenio@ browse seat"
    outbox_rig_guard "live outbox positive apply gates" \
      "base rig present but the gardenio@ browse seat (JIRA_GARDENIO_BROWSE_EMAIL/TOKEN, secret jira-gardenio-browse-cred) is ABSENT — on the POSITIVE path the FSM-side read is MANDATORY; NEVER infer applied from Synced (this is the gd-d7U7jd6cyH false-green guard). Provision the browse cred to light this gate."
    echo
  else
    green "  Sandbox rig present + browse seat present — running live positive apply matrix against ${JIRA_SANDBOX_BASE_URL} (FSM ticket ${FSM_KEY})"
    dim "  FSM-side corroboration is MANDATORY on the positive path — Synced is proven applied ONLY when the effect is read back on the FSM ticket."
    echo

    # ── T-1to1 (was P-LABEL T2 + P-VERSION T4): ONE Analysis ticket carrying
    #    the triage label + version → Synced ⇒ EXACTLY ONE GBFSM ticket AND
    #    both effects LAND on FSM. The 1:1 reshape collapses the former per-op
    #    P-LABEL + P-VERSION into a SINGLE ticket — creating two separate tickets
    #    for one FSM-N would itself violate the invariant this gate asserts. ──
    blue "T-1to1: ONE Analysis ticket (label '$CANON_LABEL' + version) → Synced MUST imply exactly ONE GBFSM ticket AND both effects LAND on FSM"
    local pv_val="${JIRA_SANDBOX_FSM_VERSION:-}"
    # Baselines (browse) for the from-ABSENT attribution guard — the gd-d7U7jd6cyH
    # false-green closer, preserved verbatim: only an effect ABSENT at this run's
    # baseline and present AFTER Synced is attributable to THIS ticket.
    local pl_before_body pl_before_has pl_before_count pv_before_has
    pl_before_body=$(fsm_labels_body)
    if [[ "$pl_before_body" == HTTP_ERR:* ]]; then
      skip "T-1to1 apply corroboration" "FSM labels read denied/failed (${pl_before_body}) establishing the baseline — cannot corroborate; verify the browse seat / rate limits"
    else
      pl_before_count=$(label_count_from_body "$pl_before_body")
      pl_before_has=$(fsm_label_present "$CANON_LABEL")
      [ -n "$pv_val" ] && pv_before_has=$(fsm_version_present "$pv_val")
      dim "  T-1to1 baseline: label ${pl_before_has} (count=${pl_before_count})${pv_val:+, version ${pv_val} ${pv_before_has}}"
      # Build the ONE Analysis ticket: label in the labels field, version in the
      # version field (when JIRA_SANDBOX_FSM_VERSION is set), analysis in the body.
      local t1 t1_key t1_code t1_status
      t1=$(gfo_create_analysis "$FSM_KEY" "$CANON_LABEL" "$pv_val" "$(gfo_idem_key "$FSM_KEY" gbfsm)" "T-1to1 positive-apply probe: one Analysis ticket carrying the triage label + version. Automated E2E; safe to delete.")
      t1_key="${t1%%|*}"; t1_code="${t1##*|}"
      if [ -z "$t1_key" ]; then
        fail "T-1to1 create Analysis ticket" "gardenio2 create failed (HTTP $t1_code)"
      else
        pass "T-1to1 gardenio2 created ONE Analysis ticket $t1_key (HTTP $t1_code)"
        t1_status=$(gfo_wait_status "$t1_key")
        dim "  T-1to1 GBFSM status: $t1_status"
        if [ "$t1_status" != "Synced" ]; then
          fail "T-1to1 reach Synced" "a VALID Analysis ticket (allowlisted label${pv_val:+ + pre-existing version}) must reach Synced; got '$t1_status'"
        else
          pass "T-1to1 valid Analysis ticket reached Synced ($t1_key)"
          # EXACTLY-ONE-GBFSM-TICKET (the count==1 invariant). Mirror SearchGBFSM.
          local t1_count; t1_count=$(gfo_find_analysis "$FSM_KEY")
          if [ "$t1_count" = "ERR" ]; then
            skip "T-1to1 count corroboration" "GBFSM search read denied/failed — cannot verify count==1; the ticket create + Synced are already observed"
          elif [ "$t1_count" = "1" ]; then
            pass "T-1to1 EXACTLY ONE GBFSM ticket exists for ${FSM_KEY} (count=1 — no per-op fan-out; the reshape invariant holds)"
          else
            fail "T-1to1 exactly-one-ticket" "expected exactly 1 GBFSM ticket for ${FSM_KEY}, found $t1_count — a fan-out regression (the 1:1 invariant is violated)"
          fi
          # LABEL LANDED on FSM (from-ABSENT attribution + additive no-clobber). This
          # is the P-LABEL corroboration verbatim, now attributed to the ONE ticket.
          if [ "$pl_before_has" = "readerr" ]; then
            skip "T-1to1 label apply corroboration" "FSM labels baseline read denied/failed — cannot establish an attributable baseline; verify the browse seat / rate limits"
          elif [ "$pl_before_has" = "present" ]; then
            red "  ⚠ T-1to1 cannot attribute label apply: '$CANON_LABEL' ALREADY on FSM ${FSM_KEY} at baseline (leftover from a prior run — the label is not probe-revertible)"
            skip "T-1to1 label apply corroboration" "the canonical label PRE-EXISTED on ${FSM_KEY} before this run's ticket — a plain membership read cannot attribute it to THIS ticket, so a Synced-with-no-write silent no-op is indistinguishable here. NEVER pass 'applied' off a leftover: reset the disposable ticket (prefer JIRA_OUTBOX_POSITIVE_FSM_KEY) so T-1to1 runs from an ABSENT baseline"
          else
            local landed
            landed=$(fsm_wait_present fsm_label_present "$CANON_LABEL" "$FSM_APPLY_WAIT")
            case "$landed" in
              present)
                pass "T-1to1 Synced STRICTLY IMPLIES applied — '$CANON_LABEL' was ABSENT at baseline and LANDED on FSM ticket ${FSM_KEY} (attributable to this ticket)"
                local pl_after_body pl_after_count subset
                pl_after_body=$(fsm_labels_body)
                if [[ "$pl_after_body" == HTTP_ERR:* ]]; then
                  skip "T-1to1 additive-delta corroboration" "FSM labels re-read denied/failed after apply — landing already confirmed; delta not verifiable"
                else
                  pl_after_count=$(label_count_from_body "$pl_after_body")
                  subset=$(labels_subset "$pl_before_body" "$pl_after_body")
                  if [ "$subset" = "no" ]; then
                    fail "T-1to1 no-clobber" "a pre-existing FSM label was DROPPED by the apply — the label apply must be additive, not a clobbering set-write"
                  elif [ "$subset" = "err" ]; then
                    skip "T-1to1 no-clobber corroboration" "could not compare label sets (parse error) — landing already confirmed"
                  elif [ "$pl_before_count" = "ERR" ] || [ "$pl_after_count" = "ERR" ]; then
                    skip "T-1to1 additive-delta corroboration" "label count unreadable (before=$pl_before_count after=$pl_after_count) — landing + no-clobber already confirmed"
                  elif [ "$pl_after_count" -eq "$((pl_before_count + 1))" ]; then
                    pass "T-1to1 additive delta +1 (count $pl_before_count → $pl_after_count; exactly the new label added, no clobber)"
                  else
                    fail "T-1to1 additive delta" "expected +1 (before=$pl_before_count), got after=$pl_after_count — apply is not a clean additive +1"
                  fi
                fi
                ;;
              absent)
                fail "T-1to1 SILENT-NOOP (label)" "ticket Synced but the label was ABSENT at baseline and STILL never landed on FSM ${FSM_KEY} within ${FSM_APPLY_WAIT}s — this is the gd-d7U7jd6cyH silent no-op signature" ;;
              readerr)
                skip "T-1to1 label apply corroboration" "FSM labels read denied/failed while polling for the applied label — cannot corroborate; verify the browse seat / rate limits" ;;
            esac
          fi
          # VERSION LANDED on FSM (from-ABSENT attribution). The P-VERSION
          # corroboration verbatim, now attributed to the SAME ticket. Skips when no
          # pre-existing project version is configured (a missing version 400s).
          if [ -z "$pv_val" ]; then
            skip "T-1to1 version apply corroboration" "no pre-existing project version (need JIRA_SANDBOX_FSM_VERSION); a missing/unknown version legitimately 400s, so the version leg needs a real project version to assert the happy path"
          elif [ "$pv_before_has" = "readerr" ]; then
            skip "T-1to1 version apply corroboration" "FSM versions baseline read denied/failed — cannot establish an attributable baseline; verify the browse seat / rate limits"
          elif [ "$pv_before_has" = "present" ]; then
            red "  ⚠ T-1to1 cannot attribute version apply: version '$pv_val' ALREADY on FSM ${FSM_KEY} at baseline (leftover from a prior run — not probe-revertible)"
            skip "T-1to1 version apply corroboration" "version '$pv_val' PRE-EXISTED on ${FSM_KEY} before this run's ticket — a plain membership read cannot attribute it to THIS ticket. NEVER pass 'applied' off a leftover: reset the disposable ticket (prefer JIRA_OUTBOX_POSITIVE_FSM_KEY) so the version leg runs from an ABSENT baseline"
          else
            local pv_landed
            pv_landed=$(fsm_wait_present fsm_version_present "$pv_val" "$FSM_APPLY_WAIT")
            case "$pv_landed" in
              present)
                pass "T-1to1 Synced STRICTLY IMPLIES applied — version '$pv_val' was ABSENT at baseline and LANDED on FSM ticket ${FSM_KEY} (attributable to this ticket; versions/fixVersions)" ;;
              absent)
                fail "T-1to1 SILENT-NOOP (version)" "ticket Synced but version '$pv_val' was ABSENT at baseline and STILL never landed on FSM ${FSM_KEY} within ${FSM_APPLY_WAIT}s — gd-d7U7jd6cyH silent no-op signature" ;;
              readerr)
                skip "T-1to1 version apply corroboration" "FSM versions read denied/failed while polling — cannot corroborate; verify the browse seat / rate limits" ;;
            esac
          fi
        fi
      fi
    fi
    echo

    # ── T-link (was part of the 1:1 link, ka-0000001070): the GBFSM ticket's
    #    fsm_target_key (customfield_10148) resolves to the real FSM-N, and the
    #    Description carries the clickable FSM link — the bot-emitted GBFSM→FSM half.
    #    The FSM→GBFSM back-link is the operator R-LINK rule (pending/operator-owned),
    #    so it stays behind the guard and degrades to SKIP LOUDLY, never RED. ──
    blue "T-link: the GBFSM ticket's fsm_target_key resolves to ${FSM_KEY} and its Description carries the clickable FSM link (GBFSM→FSM half)"
    if [ -z "${t1_key:-}" ]; then
      skip "T-link resolves" "no GBFSM ticket key from T-1to1 (create failed/skipped) — cannot read back the link"
    else
      local link_body link_target
      link_body=$(jira_get_ok "/rest/api/3/issue/${t1_key}?fields=${CF_TARGET},description" \
        "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN")
      if [[ "$link_body" == HTTP_ERR:* ]]; then
        skip "T-link resolves" "reading the GBFSM ticket ${t1_key} back denied/failed (${link_body}) — cannot corroborate the link"
      else
        link_target=$(CF_TARGET="$CF_TARGET" python3 -c "import sys,json,os; print((json.load(sys.stdin).get('fields',{}) or {}).get(os.environ['CF_TARGET'],''))" <<< "$link_body" 2>/dev/null || echo "")
        if [ "$link_target" = "$FSM_KEY" ]; then
          pass "T-link GBFSM→FSM: ticket ${t1_key} carries fsm_target_key=${FSM_KEY} as authoritative machine-readable link DATA (customfield_10148)"
        else
          fail "T-link GBFSM→FSM" "GBFSM ticket ${t1_key} fsm_target_key is '$link_target', expected '$FSM_KEY'"
        fi
        # Check the "FSM ticket: " link paragraph specifically (matching the 0e AND
        # discipline). Do NOT OR in a bare "$FSM_KEY" match: the read-back includes
        # the customfield_10148 field, so a bare-key grep would be trivially true and
        # could not distinguish "the Description carries the link" from "the payload
        # merely mentions the key" — the machine-readable link is already asserted above.
        if grep -qF "FSM ticket: " <<< "$link_body"; then
          pass "T-link GBFSM→FSM: the ticket Description renders the clickable FSM link (one-click GBFSM→FSM navigation)"
        else
          skip "T-link Description link" "could not confirm the 'FSM ticket:' link paragraph in the read-back Description (ADF render/whitespace may vary) — the machine-readable fsm_target_key link above is authoritative"
        fi
      fi
      dim "  T-link FSM→GBFSM back-link (operator R-LINK rule) is NOT asserted here — it is operator-owned/pending; corroborated by verify-outbox-rules-json.sh invariant (g). Asserting it live would be authoring against a not-yet-built rule."
    fi
    echo

    # ── T-upsert (was P-IDEMPOTENT): re-triage the SAME FSM-N → still EXACTLY ONE
    #    GBFSM ticket (upsert, not a second create) AND the FSM label set does NOT
    #    grow (idempotent apply). Subsumes P-IDEMPOTENT's "count MUST NOT grow". ──
    blue "T-upsert: re-triage the SAME FSM-N → still exactly ONE GBFSM ticket (upsert) AND the FSM label count does NOT grow"
    local idem_before_body idem_before_count idem_before_has
    idem_before_body=$(fsm_labels_body)
    if [[ "$idem_before_body" == HTTP_ERR:* ]]; then
      skip "T-upsert corroboration" "FSM labels read denied/failed establishing the upsert baseline — cannot corroborate"
    else
      idem_before_count=$(label_count_from_body "$idem_before_body")
      idem_before_has=$(fsm_label_present "$CANON_LABEL")
      if [ "$idem_before_has" != "present" ]; then
        # T-1to1 must have landed the label already; if it isn't present the upsert
        # premise doesn't hold — skip rather than a spurious fail (T-1to1 owns the
        # landing assertion).
        skip "T-upsert corroboration" "canonical label not present at the upsert baseline (T-1to1 landing is the authoritative gate for that) — skipping the no-growth re-triage"
      else
        # Re-triage the SAME FSM-N with the SAME classification (same fsm_target_key
        # + idempotency_key). A faithful pipeline UPSERTS via SearchGBFSM→EditGBFSMTicket;
        # this probe re-emits the create to model a retry. The invariant is: still
        # exactly ONE GBFSM ticket, and no label growth on FSM.
        local ti ti_key ti_code ti_status
        ti=$(gfo_create_analysis "$FSM_KEY" "$CANON_LABEL" "$pv_val" "$(gfo_idem_key "$FSM_KEY" gbfsm)" "T-upsert probe: re-triage the same FSM-N; must upsert one ticket, never grow the FSM effect. Automated E2E; safe to delete.")
        ti_key="${ti%%|*}"; ti_code="${ti##*|}"
        if [ -z "$ti_key" ]; then
          fail "T-upsert re-triage create" "gardenio2 create failed (HTTP $ti_code)"
        else
          pass "T-upsert gardenio2 re-triaged the SAME FSM-N (second emit $ti_key, HTTP $ti_code)"
          ti_status=$(gfo_wait_status "$ti_key")
          dim "  T-upsert GBFSM status: $ti_status"
          # Still exactly ONE GBFSM ticket (the upsert invariant).
          local ti_count; ti_count=$(gfo_find_analysis "$FSM_KEY")
          if [ "$ti_count" = "ERR" ]; then
            skip "T-upsert exactly-one-ticket" "GBFSM search read denied/failed — cannot verify count stays 1"
          elif [ "$ti_count" = "1" ]; then
            pass "T-upsert re-triage keeps EXACTLY ONE GBFSM ticket for ${FSM_KEY} (count=1; a re-triage UPSERTS via SearchGBFSM→EditGBFSMTicket, never a second create)"
          else
            # A faithful upsert pipeline keeps this at 1; a bot-side re-CREATE (this
            # probe cannot itself run SearchGBFSM→Edit) may transiently read >1 on a
            # rig whose upsert is not yet wired — a rig signal, noted loudly, not a
            # hard code fail (the bot-side upsert identity is asserted in 0e).
            skip "T-upsert exactly-one-ticket" "gfo_find_analysis returned count=$ti_count for ${FSM_KEY} (expected 1). Against a faithful upsert pipeline this is 1; >1 means the live rig has not yet wired SearchGBFSM→EditGBFSMTicket (rig signal). The bot-ownable upsert identity is asserted in Scenario 0e."
          fi
          # FSM label count MUST NOT grow (P-IDEMPOTENT's exact assertion, verbatim).
          local ti_landed
          ti_landed=$(fsm_wait_present fsm_label_present "$CANON_LABEL" "$FSM_APPLY_WAIT")
          if [ "$ti_landed" = "readerr" ]; then
            skip "T-upsert no-growth corroboration" "FSM labels read denied/failed while polling — cannot corroborate"
          else
            local idem_after_body idem_after_count idem_after_has
            idem_after_body=$(fsm_labels_body)
            if [[ "$idem_after_body" == HTTP_ERR:* ]]; then
              skip "T-upsert no-growth corroboration" "FSM labels re-read denied/failed after the re-triage — cannot verify no-growth"
            else
              idem_after_count=$(label_count_from_body "$idem_after_body")
              idem_after_has=$(fsm_label_present "$CANON_LABEL")
              if [ "$idem_after_has" != "present" ]; then
                fail "T-upsert label retained" "the canonical label is no longer present after re-triaging the same FSM-N — a re-triage must be a no-op, not a removal"
              elif [ "$idem_before_count" = "ERR" ] || [ "$idem_after_count" = "ERR" ]; then
                skip "T-upsert no-growth corroboration" "label count unreadable (before=$idem_before_count after=$idem_after_count) — label still present"
              elif [ "$idem_after_count" = "$idem_before_count" ]; then
                pass "T-upsert re-triaging the SAME FSM-N did NOT grow the FSM label count (stayed $idem_after_count; additive/idempotent apply)"
              else
                fail "T-upsert no-growth" "re-triaging the SAME FSM-N changed the label count $idem_before_count → $idem_after_count — the apply is not idempotent"
              fi
            fi
          fi
        fi
      fi
    fi
    echo
  fi

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do red "   - $err"; done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
