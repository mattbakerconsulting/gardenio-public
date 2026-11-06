#!/usr/bin/env bash
# test-jira-outbox-negative.sh — OUTBOX negative/edge E2E (sandbox FSM via GFO)
#
# The live, sandbox leg of the OUTBOX-model NEGATIVE/edge matrix (task
# gd-ksWVeBgpww, hard gate ka-0000000600, epic ka-0000002067 "outbox pivot").
# It is the negative-path sibling of test-jira-write-control-sandbox.sh and is
# modeled EXACTLY on that script's structure (set -euo pipefail, color/pass/
# fail/skip helpers, K8s-secret cred loading, and the SKIP-CLEAN CONTRACT so the
# module is GREEN when the sandbox rig is absent).
#
# ── The outbox model + the 1:1 reshape (why the identities are split) ─────────
#
# In the outbox model the gardenio bot accounts hold ZERO write — and ZERO read —
# on the ACME Service Management (FSM) project, structurally, at the Jira
# permission layer (proven live: T1 FOOTPRINT — gardenio2 GET FSM-408 = 404).
# All FSM-bound writes land in a dedicated internal "Gardenio FSM Outbox" (GFO/
# GBFSM) project. Operator-owned Jira Automation rules validate the request
# against a hardcoded allowlist and apply it to the FSM ticket AS THE AUTOMATION
# ACTOR — never the bot. A canary (R-CANARY) alerts on any direct bot-account FSM
# write (must never fire = live structural proof).
#
# THE 1:1 RESHAPE (ka-0000002637, this task gd-NhkcCUqCPI): the outbox no longer
# fans out one INTENT ticket per mutation (Add-Label 10040 / Set-Version 10041 /
# Internal-Comment 10042, each with op_value in customfield_10149). It now upserts
# ONE GBFSM "Analysis" issue-type ticket per FSM ticket, carrying the N triage
# labels + version as FIELDS + the analysis in the Description, keyed to FSM by
# fsm_target_key (customfield_10148) and deduped by idempotency_key on FSM-N. This
# is what the MERGED writer produces (jira_outbox.go CreateGBFSMTicket +
# mutationFields — description+labels+version ONLY, NO op_value) and what the
# FROZEN operator rules act on (verify-outbox-rules-json.sh write_newshape_fixtures
# = R-LABEL/R-VERSION/R-LINK/R-CANARY; R-COMMENT RETIRED). This gate fabricates the
# 1:1 Analysis shape via gfo_analysis_payload / gfo_create_analysis — NOT the
# retired per-op intents. The dormant per-op writer (jira_outbox_intents.go) is
# kept until Phase 4 retires it, so its consts still exist there — but not here.
#
# Consequence for THIS probe: the two Jira sides use two identities.
#   • GBFSM side (create the Analysis ticket, read its STATUS + count): gardenio2 —
#     privileged on GBFSM, zero on FSM. This is the REQUIRED credential and is
#     enough on its own to assert the primary go/no-go signal of every negative
#     test: the ticket's terminal status column (Synced vs Failed) + the at-most-
#     one-per-FSM-N count (gfo_find_analysis).
#   • FSM side (read labels/versions to CORROBORATE that a reject wrote nothing /
#     a non-bot ticket added no label): gardenio@ browse — the read-only FSM seat
#     the bot retains ("read-only browse excepted", epic ka-0000002067). This is an
#     OPTIONAL enhancer; when its cred is absent the FSM-side sub-assertions skip
#     clean and the GBFSM-side primary assertion still runs.
#
# ── The tests (ka-0000000600 matrix, re-pointed to the 1:1 Analysis shape) ─────
#
#   0e  SHAPE SELF-TEST   : ALWAYS-ON, non-network. The bot-ownable CREATE shape is
#                          fixed by merged code, so we assert gfo_analysis_payload
#                          builds exactly ONE Analysis ticket (project.id hardcoded
#                          10037, labels/version as FIELDS, fsm_target_key as DATA,
#                          idempotency_key, ADF Description link) with NO op_value /
#                          comment / public-visibility surface — and prove the gate
#                          discriminates (a corrupted payload reddens). This carries
#                          the non-network face of T-1to1/T-upsert/T-link/T7 and the
#                          customer-notification-safety survival of the retired T8.
#   T3  ALLOWLIST-REJECT : Analysis ticket with a non-allowlisted label in the
#                          labels field → GBFSM ticket lands Failed AND the FSM
#                          ticket gains NO such label.
#   T7  REPOINT DEFENSE  : Analysis ticket with a malformed fsm_target_key
#                          ('NOT-A-KEY') → target regex ^FSM-[0-9]+$ refuses → GBFSM
#                          Failed, no cross-project write (baseline FSM untouched).
#   T10 DEAD-LETTER      : a forced validation failure (illegal version value in the
#                          Analysis version field) → GBFSM ticket DURABLY lands in
#                          the Failed column (append-only dead-letter, not silently
#                          dropped); the owner-error alert path is noted (R-CANARY /
#                          notify-on-error = owner email v1, not API-observable here).
#   T-1to1 / T-upsert    : re-triage the SAME FSM-N twice → EXACTLY ONE GBFSM ticket
#                          (gfo_find_analysis count==1; the second write UPSERTS via
#                          SearchGBFSM→EditGBFSMTicket, never piles up). This is the
#                          no-multiply property that the retired T6 comment-dedup
#                          pinned — preserved + strengthened by the 1:1 upsert.
#
# RETIRED (per the 1:1 reshape — the merged writer emits no FSM comment):
#   T6 (comment-dedup) → its no-multiply property is T-1to1/T-upsert above.
#   T8 (internal-comment jsdPublic pin) → its customer-notification-safety property
#      is the 0e ABSENT assertions (no comment/public surface in the bot payload,
#      BY CONSTRUCTION) + verify-outbox-rules-json.sh invariant (c) at the rule layer.
#
# ── The 10→12 gate growth (GFO-goes-public design, doc 2306113538 v4) ─
#
# The 2026-07-08 design shift makes the GFO project SITE-PUBLIC (it becomes the
# surface gardenio posts per-ticket analysis on). Public browse + an outbox the
# Automation rules apply to FSM AS THE AUTOMATION ACTOR is a classic confused-
# deputy setup — so the defense has two halves, and the go/no-go matrix grew
# 10→12 (ka-0000000600, thread-1783531691-200779) to gate both:
#
#   T11 CONFUSED-DEPUTY  : the RULE half. An intent whose reporter is a NON-bot
#                          account (reporter ∉ {gardenio@, gardenio2} accountIds)
#                          must NOT propagate to FSM — the reporter-is-bot rule
#                          condition (ka-0000004008) refuses it, so the GFO
#                          intent NEVER reaches Synced (it lands Failed, or its
#                          trigger never matches and it sits un-propagated) and
#                          the FSM ticket gains nothing. Only a Synced non-bot
#                          intent is the breach. SPEC-SOURCE: the reporter-is-bot
#                          condition is a PENDING rule-spec delta (not yet built
#                          live) — authored against the spec; the gate stays
#                          inert (skip-clean) until BOTH the live rig AND a
#                          non-bot reporter identity (JIRA_NONBOT_ACCOUNT_ID)
#                          exist, and correctly FAILS a live rig whose rules do
#                          not yet carry the condition.
#   T12 VISIBILITY-BOUND : the ACCESS half (gd-7DVpQztZxd: GFO flips private→
#                          open, everyone=Viewer, bots+admins=Member). Three
#                          independent sub-assertions, each skip-clean when its
#                          identity is absent:
#                          (A) a portal-only FSM customer canNOT browse the
#                              site-public GFO SOFTWARE project (portal accounts
#                              hold no Jira product access; site-public ≠
#                              customer-visible) — GET is denied (401/403/404).
#                          (B+) the bot (gardenio2, a Member) CAN Create + Edit
#                              in GFO.
#                          (B−) a non-member licensed user canNOT Create in GFO
#                              (open browse + Member-only write; an open create
#                              would be the confused-deputy front door). SPEC-
#                              SOURCE: the access flip LANDED (gd-7DVpQztZxd,
#                              now closed) — authored against that bead.
#
# POSITIVE tests T1/T2/T4/T5/T9 were already live-smoke-proven during the rule
# build (ka-0000004008) and are NOT re-run here.
#
# ── Skip-clean contract ──────────────────────────────────────────────
#
# The sandbox rigging this script needs — a privileged gardenio2 Jira credential
# (SM gardenio/jira-sandbox-api-token, operator-side), the GFO project key, a
# disposable sandbox FSM ticket, and the four Automation rules live on the
# sandbox site — is operator-provisioned and does NOT exist in the ephemeral E2E
# namespace. So, exactly like test-jira-write-control-sandbox.sh and
# test-jira-bridge.sh scenarios 4/6, every LIVE gate SKIPS CLEANLY (pass, not
# fail) when its rigging is absent, and the script is GREEN today. It lights up
# automatically once the operator provides the env vars below (or the in-cluster
# K8s secrets they are synced to).
#
# Required env for the LIVE gates (all absent ⇒ skip clean):
#   JIRA_SANDBOX_BASE_URL   Sandbox Jira base, e.g. https://acme-sandbox.atlassian.net
#   JIRA_SANDBOX_FSM_KEY    A disposable sandbox FSM ticket key, e.g. FSM-408
#   JIRA_GARDENIO2_EMAIL     Privileged-on-GFO identity email (gardenio2)
#   JIRA_GARDENIO2_TOKEN     gardenio2 API token (privileged on GFO, zero on FSM)
#   JIRA_GFO_PROJECT_KEY    Gardenio FSM Outbox project key, e.g. GFO (project id 10037)
#
# Optional env (FSM-side corroboration; absent ⇒ those sub-assertions skip):
#   JIRA_GARDENIO_BROWSE_EMAIL  gardenio@ identity email (FSM read seat)
#   JIRA_GARDENIO_BROWSE_TOKEN  gardenio@ API token (browse/read-only on FSM)
#
# Optional env for the 10→12 gate growth (absent ⇒ that test/sub-assertion skips):
#   T11 confused-deputy (reporter-is-bot):
#     JIRA_NONBOT_ACCOUNT_ID   A NON-bot Jira accountId, stamped as the reporter
#                              of a gardenio2-created intent to prove the rules
#                              refuse it (reporter ∉ bot set). Absent ⇒ T11 skips.
#   T12 visibility-boundary (gd-7DVpQztZxd access flip):
#     JIRA_PORTAL_CUSTOMER_EMAIL / JIRA_PORTAL_CUSTOMER_TOKEN
#                              A portal-only FSM customer identity (T12-A browse-
#                              deny). Absent ⇒ T12-A skips.
#     JIRA_NONMEMBER_EMAIL / JIRA_NONMEMBER_TOKEN
#                              A non-member LICENSED user (T12-B− create-deny).
#                              Absent ⇒ the non-member sub-assertion skips.
#   JIRA_BOT_ACCOUNT_IDS       Comma-separated bot accountIds (gardenio@,gardenio2)
#                              for the reporter-is-bot predicate the always-on 0c
#                              invariant self-tests and T11 uses to pick the
#                              expected outcome. Optional; the invariant runs on
#                              a synthetic set when unset.
#
# The gardenio2 / browse / T11 / T12 creds may instead be sourced from K8s secrets
# in $NAMESPACE when present (jira-gardenio2-cred / jira-gardenio-browse-cred /
# jira-nonbot-cred / jira-portal-customer-cred / jira-nonmember-cred) — see
# load_sandbox_creds. Secrets are NEVER hardcoded here.

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

# ── 1:1 GBFSM Analysis-ticket shape (reshape ka-0000002637, Phase 2 ka-0000001491) ─
# gardenio upserts ONE GBFSM "Analysis" issue-type ticket per FSM ticket carrying the
# N triage labels + version as FIELDS + the analysis in the Description, keyed to FSM
# by fsm_target_key (customfield_10148). This SUPERSEDES the retired per-mutation
# intent fan-out (Add-Label 10040 / Set-Version 10041 / Internal-Comment 10042, each
# with op_value in customfield_10149). The dormant per-op writer jira_outbox_intents.go
# is kept until Phase 4 retires it (flag-day avoidance) — but this E2E gate no longer
# fabricates that shape.
#
# The shared shape config vars (ANALYSIS_TYPE / LABELS_FIELD / VERSION_FIELD /
# CF_TARGET / CF_IDEM / ALLOWLIST_RE), the pure builders (gfo_analysis_payload /
# gfo_idem_key / gfo_dedup_key) and the always-on shape self-test (shape_assert /
# shape_selftest) now live in lib-outbox-shape.sh — the SINGLE source of truth shared
# with test-jira-outbox-positive.sh and the hermetic main-push gate
# (test-outbox-shape-selftest.sh, Phase 5c ka-0000003147). It is sourced below.

# How long to wait for the async Automation rule to move an intent to a terminal
# column (Synced/Failed). Smoke evidence: ~20s per sync; 90s gives ample margin.
GFO_SYNC_TIMEOUT="${GFO_SYNC_TIMEOUT:-90}"

# ── Reporter-is-bot condition (T11 confused-deputy defense) ────────
# The pending rule-spec delta (ka-0000004008 comment 2026-07-08 23:32:55) adds to
# every Analysis apply rule (R-LABEL / R-VERSION / R-LINK — verify-outbox-rules-json.sh
# invariant (d)) a CONDITION: the ticket's reporter/creator must be a bot account
# (gardenio@ / gardenio2 accountIds). JIRA_BOT_ACCOUNT_IDS is the
# comma-separated allowlist of those bot accountIds; reporter_is_bot() below is
# the local model of that condition, self-tested by Scenario 0c and used by T11
# to derive the expected outcome. Left empty by default (the real accountIds are
# operator-provisioned, opaque, and site-specific); 0c self-tests on a synthetic
# set so the predicate stays guarded even when the ids are unset.
JIRA_BOT_ACCOUNT_IDS="${JIRA_BOT_ACCOUNT_IDS:-}"

# For a live T11 run against a rig whose reporter-is-bot condition may not yet be
# built, a NON-bot intent that never leaves its initial GFO column (the rule's
# trigger simply never matches) is a valid PASS for a NEGATIVE test. GFO_T11_WAIT
# bounds how long we poll before concluding "did not propagate" — kept short
# relative to GFO_SYNC_TIMEOUT since we only need long enough to catch a WRONG
# propagation to Synced, not to wait out a legitimate sync.
GFO_T11_WAIT="${GFO_T11_WAIT:-45}"

# ── Helpers (mirrors test-jira-write-control-sandbox.sh) ──────────
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
assert_not_contains() {
  if grep -qF -- "$2" <<< "$3"; then fail "$1" "unexpectedly contains '$2'"
  else pass "$1 (does not contain '$2')"; fi
}

# ── Shared 1:1 CREATE-shape library ───────────────────────────────
# Sets the shape config vars (ANALYSIS_TYPE / LABELS_FIELD / VERSION_FIELD /
# CF_TARGET / CF_IDEM / ALLOWLIST_RE) and defines the pure builders
# (gfo_analysis_payload / gfo_idem_key / gfo_dedup_key) + the always-on shape
# self-test (shape_assert / shape_selftest). Sourced AFTER pass/fail are defined
# (shape_selftest uses them). Same lib the positive suite + the hermetic main-push
# gate source, so the fabricated shape can never drift between them.
# shellcheck source=./lib-outbox-shape.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-outbox-shape.sh"

# ── Fail-loud rig guard (REQUIRE_OUTBOX_RIG) ──────────────────────────
# outbox_rig_guard turns a MANDATORY-but-absent live rig into today's clean
# skip-PASS (default) OR a loud red NO-GO FAIL (REQUIRE_OUTBOX_RIG on — the
# standing regression gate, ka-0000007489). Modeled on lib-gvisor-node.sh's
# gvisor_require_substrate_guard. Sourced AFTER red/skip/fail are defined (the
# guard's caller contract). Same lib the positive suite sources.
# shellcheck source=./lib-outbox-rig.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-outbox-rig.sh"

# ── Confused-deputy / visibility predicates (T11 / T12 local models) ──
# These mirror the operator-side rule/access SPEC as pure local functions so the
# two new gates stay non-vacuous in the ephemeral namespace (Scenario 0c/0d) and
# so the live T11/T12 assertions share ONE model of the intended policy. Neither
# touches the network.

# reporter_is_bot echoes "bot" iff account_id is a non-empty member of the
# comma-separated bot_ids set (exact match, whitespace-trimmed); else "nonbot".
# This is the local model of the pending reporter-is-bot rule condition
# (ka-0000004008): an intent whose reporter is in the bot set may propagate; any
# other reporter (incl. empty) must NOT. Exact-match — a near-miss like a bot id
# with a trailing character is deliberately NOT a bot (no prefix/substring slack
# that a spoofer could exploit).
reporter_is_bot() { # account_id bot_ids_csv
  ACCT="$1" BOTIDS="${2:-}" python3 -c '
import os
acct = os.environ.get("ACCT", "").strip()
bots = [b.strip() for b in os.environ.get("BOTIDS", "").split(",") if b.strip()]
print("bot" if acct and acct in bots else "nonbot")
' 2>/dev/null || echo "nonbot"
}

# gfo_can_create / gfo_can_browse model the gd-7DVpQztZxd access matrix on the
# site-public GFO SOFTWARE project (everyone=Viewer, bots+admins=Member; portal
# customers hold no Jira product access). Pure lookups over a role token:
#   member  → Member (bot/admin): create=yes, browse=yes
#   viewer  → licensed non-member: create=NO,  browse=yes (site-public)
#   portal  → JSM portal-only customer: create=NO, browse=NO (no product access)
gfo_can_create() { # role
  case "$1" in member) echo yes ;; viewer|portal|*) echo no ;; esac
}
gfo_can_browse() { # role
  case "$1" in member|viewer) echo yes ;; portal|*) echo no ;; esac
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"; return; fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19108
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-jon.log 2>&1 &
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
  # T11 non-bot reporter accountId (jira-nonbot-cred, key account_id).
  local nb
  nb=$(kubectl -n "$NAMESPACE" get secret jira-nonbot-cred -o json 2>/dev/null || echo "")
  if [ -n "$nb" ]; then
    : "${JIRA_NONBOT_ACCOUNT_ID:=$(echo "$nb" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('account_id','')).decode() or '')" 2>/dev/null || echo "")}"
  fi
  # T12-A portal-only FSM customer (jira-portal-customer-cred).
  local pc
  pc=$(kubectl -n "$NAMESPACE" get secret jira-portal-customer-cred -o json 2>/dev/null || echo "")
  if [ -n "$pc" ]; then
    : "${JIRA_PORTAL_CUSTOMER_EMAIL:=$(echo "$pc" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('email','')).decode() or '')" 2>/dev/null || echo "")}"
    : "${JIRA_PORTAL_CUSTOMER_TOKEN:=$(echo "$pc" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('token','')).decode() or '')" 2>/dev/null || echo "")}"
  fi
  # T12-B non-member licensed user (jira-nonmember-cred).
  local nm
  nm=$(kubectl -n "$NAMESPACE" get secret jira-nonmember-cred -o json 2>/dev/null || echo "")
  if [ -n "$nm" ]; then
    : "${JIRA_NONMEMBER_EMAIL:=$(echo "$nm" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('email','')).decode() or '')" 2>/dev/null || echo "")}"
    : "${JIRA_NONMEMBER_TOKEN:=$(echo "$nm" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('token','')).decode() or '')" 2>/dev/null || echo "")}"
  fi
}

# sandbox_rig_ready returns 0 iff every var needed for the LIVE GFO gates is
# present. When non-zero, the live gates skip clean.
sandbox_rig_ready() {
  [ -n "${JIRA_SANDBOX_BASE_URL:-}" ] && [ -n "${JIRA_SANDBOX_FSM_KEY:-}" ] \
    && [ -n "${JIRA_GARDENIO2_EMAIL:-}" ] && [ -n "${JIRA_GARDENIO2_TOKEN:-}" ] \
    && [ -n "${JIRA_GFO_PROJECT_KEY:-}" ]
}

# fsm_read_ready returns 0 iff the OPTIONAL gardenio@ browse cred is present, so
# the FSM-side corroboration sub-assertions can read the customer desk.
fsm_read_ready() {
  [ -n "${JIRA_GARDENIO_BROWSE_EMAIL:-}" ] && [ -n "${JIRA_GARDENIO_BROWSE_TOKEN:-}" ]
}

# t11_rig_ready returns 0 iff the T11 non-bot reporter accountId is present (on
# TOP of the base rig, which the caller has already confirmed). Absent ⇒ T11
# skips clean — the confused-deputy live gate needs a non-bot identity to stamp.
t11_rig_ready() {
  [ -n "${JIRA_NONBOT_ACCOUNT_ID:-}" ]
}

# t12_portal_ready / t12_nonmember_ready gate the two OPTIONAL T12 sub-identities.
t12_portal_ready() {
  [ -n "${JIRA_PORTAL_CUSTOMER_EMAIL:-}" ] && [ -n "${JIRA_PORTAL_CUSTOMER_TOKEN:-}" ]
}
t12_nonmember_ready() {
  [ -n "${JIRA_NONMEMBER_EMAIL:-}" ] && [ -n "${JIRA_NONMEMBER_TOKEN:-}" ]
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
# GBFSM/GFO project only, never FSM). An optional 6th arg (reporter_id) forges the
# reporter to a specific accountId — used ONLY by T11's confused-deputy gate; every
# other caller passes 5 args so the reporter defaults to the creator (gardenio2, a bot).
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
  # Count only issues whose fsm_target_key EQUALS the target verbatim (belt-and-
  # suspenders, mirroring SearchGBFSM's client-side re-verify — `=` on a text
  # searcher is analyzed, so a near-match must not inflate the count).
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
# status name (or "TIMEOUT" / "" ).
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

# gfo_wait_not_synced polls a GFO intent's status for T11 (confused-deputy). The
# NEGATIVE property is "does NOT propagate to FSM" — i.e. never reaches Synced.
# Two acceptable PASS shapes and one FAIL:
#   • Synced  → the ONLY breach: a non-bot intent propagated (return "Synced").
#   • Failed  → refused via the rule's ELSE branch (return "Failed", a PASS).
#   • initial/other (never terminal within the window) → the reporter-is-bot
#     trigger condition never matched, so the rule never fired and the intent
#     sits un-propagated (return "UNPROPAGATED", a PASS).
# It returns IMMEDIATELY on the first Synced/Failed sighting (fail-fast on a
# breach); otherwise it waits out the window and reports the last-seen column.
gfo_wait_not_synced() { # key timeout_s
  local key="$1" timeout="${2:-$GFO_T11_WAIT}" waited=0 st="" last=""
  while [ "$waited" -lt "$timeout" ]; do
    st=$(jira_get_body "/rest/api/3/issue/${key}?fields=status" \
      "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('fields',{}).get('status',{}).get('name',''))" 2>/dev/null || echo "")
    case "$st" in
      Synced) echo "Synced"; return 0 ;;
      Failed) echo "Failed"; return 0 ;;
    esac
    [ -n "$st" ] && last="$st"
    sleep 5; waited=$((waited + 5))
  done
  # Never reached a terminal column ⇒ not propagated. Report the initial column
  # name for evidence, but signal the outcome as UNPROPAGATED.
  echo "UNPROPAGATED:${last:-unknown}"
}

# NOTE: the FSM-comment helpers (fsm_comment_count_with_marker /
# fsm_marker_comment_is_internal) that the retired per-op comment tests used were
# removed with the 1:1 reshape — the merged writer emits no FSM comment, so there is
# nothing to count or jsdPublic-pin. T11's FSM-side no-write corroboration now reads
# the FSM label count (a non-propagating Analysis ticket adds no label).

cleanup() {
  # Best-effort teardown. The GFO intents for the REJECT tests (T3/T7/T10) are
  # the durable dead-letter and could legitimately be kept; we still attempt to
  # delete every intent we created so repeated runs don't accumulate. Delete
  # perms in GFO may not be granted to gardenio2 — hence || true + residual note.
  if sandbox_rig_ready; then
    for k in "${CREATED_GFO[@]:-}"; do
      [ -z "$k" ] && continue
      jira_curl_code DELETE "/rest/api/3/issue/${k}?deleteSubtasks=true" \
        "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" >/dev/null 2>&1 || true
    done
  fi
  # The 1:1 reshape writes no FSM comment, so there is no FSM-comment teardown. The
  # Analysis tickets we created are GBFSM-project tickets, deleted via CREATED_GFO
  # above. The label/version the Automation actor may apply to FSM are NOT revertible
  # by this probe (browse seat is 403 on FSM writes) — reset the disposable sandbox
  # FSM ticket operator-side.
  for id in "${CREATED_BEADS[@]:-}"; do "$KD_BIN" close "$id" 2>/dev/null || true; done
  if [ -n "$PF_PID" ]; then kill "$PF_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " Jira OUTBOX negative/edge E2E (sandbox FSM via GFO)"
  blue " gd-ksWVeBgpww / gate ka-0000000600 / epic ka-0000002067"
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
  TEST_TAG="e2e-jon-$$-$(date +%s)"

  # ── Scenario 0: always-on invariants (non-vacuous when rig absent) ──
  # Four cheap gates (0a/0b for T3-T10; 0c/0d for the T11/T12 gate growth) that
  # keep the module meaningful even when the sandbox rig is absent (i.e. in the
  # ephemeral E2E namespace today).
  #
  # 0a. ALLOWLIST GUARD — the operator-side R-LABEL rule enforces ALLOWLIST_RE, a
  #     deliberately-TIGHT enumerated subset of the bridge emitter predicate
  #     isTriageLabel() (which is broader/prefix-based). Two directions, both
  #     safety-relevant:
  #
  #     (i) CANONICAL-VOCABULARY ⊆ ALLOWLIST — every classification label the
  #         emitter actually produces MUST be accepted, or the outbox silently
  #         dead-letters real classifications. This is exactly the field-i7pu
  #         drift on ka-0000004008: an allowlist missing the 'triaged-' infix
  #         would REJECT gardenio-triaged-sev-3 and drop every sev/GxP label.
  #
  #     (ii) NON-CANONICAL → REJECTED — values outside the canonical vocabulary
  #          are refused. We split these into (A) values BOTH the emitter
  #          predicate and the rule reject (genuinely not triage labels — the
  #          historical no-infix 'gardenio-sev-3' bug shape, T3's 'evil-label',
  #          junk, empty), and (B) values the LOOSE prefix-based isTriageLabel()
  #          would accept but the deliberately-tighter rule refuses (out-of-range
  #          sev-9/gxp-9, bare 'sev-'/'gxp-'). Group B documents that the rule is
  #          intentionally stricter than the emitter (defense in depth) — the
  #          emitter never emits these, so tightening loses nothing.
  blue "Scenario 0a: R-LABEL op_value allowlist guard (canonical ⊆ allowlist ⊊ isTriageLabel)"
  local good bad ok=1
  # (i) canonical emitter vocabulary — MUST all be accepted (field-i7pu guard).
  for good in gardenio-triaged gardenio-triaged-sev-0 gardenio-triaged-sev-3 \
              gardenio-triaged-gxp-none gardenio-triaged-gxp-high sev-0 sev-3 \
              gxp-none gxp-med; do
    if ! [[ "$good" =~ $ALLOWLIST_RE ]]; then
      fail "allowlist accepts canonical label" "'$good' should MATCH the allowlist"
      ok=0
    fi
  done
  [ "$ok" = 1 ] && pass "allowlist accepts ALL canonical triage labels (gardenio-triaged / triaged-sev-[0-3] / triaged-gxp-{none,low,med,high} / sev-[0-3] / gxp-{none,low,med,high}) — field-i7pu 'triaged-' infix drift guard"
  ok=1
  # (ii.A) genuinely non-triage — isTriageLabel() rejects these too.
  for bad in evil-label gardenio-sev-3 gardenio-gxp-med gardenio-triaged- \
             random-label ""; do
    if [[ "$bad" =~ $ALLOWLIST_RE ]]; then
      fail "allowlist rejects non-triage value" "'$bad' should NOT match the allowlist"
      ok=0
    fi
  done
  [ "$ok" = 1 ] && pass "allowlist rejects genuinely-non-triage values (evil-label, the no-infix 'gardenio-sev-3' bug shape, gardenio-gxp-med, junk, empty)"
  ok=1
  # (ii.B) loose-prefix values isTriageLabel() ACCEPTS but the tighter rule refuses.
  for bad in sev-9 gxp-9 gardenio-triaged-sev-9 sev- gxp-; do
    if [[ "$bad" =~ $ALLOWLIST_RE ]]; then
      fail "tight allowlist rejects loose-prefix value" "'$bad' matches isTriageLabel() but the rule allowlist should refuse it"
      ok=0
    fi
  done
  [ "$ok" = 1 ] && pass "rule allowlist is strictly tighter than isTriageLabel() — rejects out-of-range sev-9/gxp-9, bare sev-/gxp- (defense in depth; emitter never emits these)"
  echo

  # 0b. DAEMON ROUND-TRIP — mirror the analog: prove the daemon is reachable and
  #     round-trips a realistic triage-classification bead carrying the bound
  #     FSM key + classification labels (the exact surface the outbox emitter
  #     reads off a bead), so the module isn't a pure skip in the ephemeral ns.
  blue "Scenario 0b: triage-classification bead round-trips (daemon reachable)"
  local sbx_key="${JIRA_SANDBOX_FSM_KEY:-FSM-408}"
  local tri_out tri_id
  tri_out=$("$KD_BIN" create "jon-triage-bead-$TEST_TAG" --type=task \
    -l "project:gardenio" -l "gardenio-triaged" -l "gardenio-triaged-sev-1" -l "gardenio-triaged-gxp-med" \
    -f "jira_key=$sbx_key" \
    -d "Triage classification bead for the outbox negative-test E2E" \
    --json 2>/dev/null || echo "{}")
  tri_id=$(echo "$tri_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$tri_id" ]; then
    CREATED_BEADS+=("$tri_id")
    pass "triage-classification bead created ($tri_id)"
    local show_out
    show_out=$("$KD_BIN" show "$tri_id" --json 2>/dev/null || echo "{}")
    assert_contains "bead carries bound FSM key" "$sbx_key" "$show_out"
    assert_contains "bead carries gardenio-triaged label" "gardenio-triaged" "$show_out"
  else
    fail "triage-classification bead" "could not create"
  fi
  echo

  # 0c. REPORTER-IS-BOT PREDICATE (T11 confused-deputy) — a pure-logic guard on
  #     the reporter_is_bot() model of the pending rule condition (ka-0000004008).
  #     Non-vacuous in the ephemeral ns: it proves the local model the live T11
  #     assertion relies on has the right semantics — exact membership, empty is
  #     NOT a bot, and a near-miss id (bot-id + a trailing char) is NOT a bot (no
  #     substring/prefix slack a spoofer could exploit). We self-test on a
  #     SYNTHETIC bot set so the guard holds even when the real (opaque, operator-
  #     provisioned) accountIds are unset; if JIRA_BOT_ACCOUNT_IDS IS set, we also
  #     assert each configured id is recognized as a bot.
  blue "Scenario 0c: reporter-is-bot predicate (confused-deputy rule condition model)"
  local synth_bots="BOT-aaa,BOT-bbb"
  local rb ok=1
  # Bots in the set → "bot" (would be allowed to propagate).
  for rb in BOT-aaa BOT-bbb; do
    if [ "$(reporter_is_bot "$rb" "$synth_bots")" != "bot" ]; then
      fail "reporter-is-bot accepts a bot id" "'$rb' should be recognized as a bot"
      ok=0
    fi
  done
  # Non-bots (incl. empty + near-miss) → "nonbot" (must NOT propagate).
  for rb in "NONBOT-xyz" "BOT-aaax" "bot-aaa" "" "BOT"; do
    if [ "$(reporter_is_bot "$rb" "$synth_bots")" != "nonbot" ]; then
      fail "reporter-is-bot rejects a non-bot id" "'$rb' should NOT be recognized as a bot (exact-match only; empty/near-miss are non-bot)"
      ok=0
    fi
  done
  [ "$ok" = 1 ] && pass "reporter-is-bot predicate is exact-membership (bots accepted; empty, wrong-case, and near-miss ids rejected) — confused-deputy propagation only for a true bot reporter"
  if [ -n "$JIRA_BOT_ACCOUNT_IDS" ]; then
    ok=1
    local cid
    # Split the configured CSV without a subshell-lost counter.
    while IFS= read -r cid; do
      [ -z "$cid" ] && continue
      if [ "$(reporter_is_bot "$cid" "$JIRA_BOT_ACCOUNT_IDS")" != "bot" ]; then
        fail "configured bot id recognized" "'$cid' from JIRA_BOT_ACCOUNT_IDS should be a bot"
        ok=0
      fi
    done < <(printf '%s' "$JIRA_BOT_ACCOUNT_IDS" | tr ',' '\n')
    [ "$ok" = 1 ] && pass "every configured JIRA_BOT_ACCOUNT_IDS entry is recognized as a bot reporter"
  else
    skip "configured bot-id recognition" "JIRA_BOT_ACCOUNT_IDS unset — predicate self-tested on the synthetic set only"
  fi
  echo

  # 0d. GFO ACCESS MATRIX (T12 visibility-boundary) — a pure-logic guard on the
  #     gfo_can_create()/gfo_can_browse() model of the gd-7DVpQztZxd access flip
  #     (everyone=Viewer, bots+admins=Member; portal customers = no product
  #     access). Documents + drift-guards the intended matrix the live T12
  #     sub-assertions probe, so T12 is non-vacuous when the rig is absent.
  blue "Scenario 0d: GFO access matrix (site-public software project; Member-only write)"
  ok=1
  # create: ONLY member.
  [ "$(gfo_can_create member)" = "yes" ] || { fail "access matrix: Member can create" "member should be able to create in GFO"; ok=0; }
  [ "$(gfo_can_create viewer)" = "no" ]  || { fail "access matrix: Viewer cannot create" "a non-member licensed viewer must NOT create (write-restriction)"; ok=0; }
  [ "$(gfo_can_create portal)" = "no" ]  || { fail "access matrix: portal cannot create" "a portal-only customer must NOT create"; ok=0; }
  # browse: member + viewer (site-public), NOT portal.
  [ "$(gfo_can_browse member)" = "yes" ] || { fail "access matrix: Member can browse" "member should browse"; ok=0; }
  [ "$(gfo_can_browse viewer)" = "yes" ] || { fail "access matrix: Viewer can browse" "site-public ⇒ any licensed user browses"; ok=0; }
  [ "$(gfo_can_browse portal)" = "no" ]  || { fail "access matrix: portal cannot browse" "portal-only customer holds no Jira product access — site-public ≠ customer-visible"; ok=0; }
  [ "$ok" = 1 ] && pass "GFO access matrix holds: create=Member-only; browse=any-licensed-user but NOT portal-only customers (confused-deputy front door closed, customer boundary intact)"
  echo

  # 0e. 1:1 ANALYSIS CREATE-SHAPE SELF-TEST (always-on, NON-network) — the
  #     bot-ownable half of the reshape. The CREATE shape the outbox fabricates is
  #     fully determined by the MERGED writer (jira_outbox.go CreateGBFSMTicket +
  #     mutationFields) + the FROZEN schema, NOT by the operator's Automation rules,
  #     so we can assert it faithfully with ZERO creds — this is what makes the
  #     module non-vacuous in the ephemeral namespace. gfo_analysis_payload is a PURE
  #     builder (no curl), so we inspect the JSON it emits directly.
  shape_selftest
  echo

  # ── The live GFO negative matrix ──────────────────────────────────
  # REQUIRE_OUTBOX_RIG off (default): an absent rig is a clean skip-PASS (rigging
  # absent != failure) — behaviour UNCHANGED. REQUIRE_OUTBOX_RIG on (the standing
  # regression gate the operator arms once the sandbox creds are provisioned into
  # the E2E namespace, ka-0000007489): an absent rig is a loud NO-GO hard FAIL, so
  # the whole T3/T7/T10/T-1to1/T-upsert/T11/T12 matrix can never skip-PASS silently.
  if ! sandbox_rig_ready; then
    blue "Live GFO negative matrix (T3/T7/T10/T-1to1/T-upsert/T11/T12)"
    outbox_rig_guard "live outbox negative gates" \
      "sandbox rig absent (need JIRA_SANDBOX_BASE_URL + JIRA_SANDBOX_FSM_KEY + gardenio2 cred + JIRA_GFO_PROJECT_KEY); provisioned operator-side from SM gardenio/jira-sandbox-api-token"
    echo
  else
    green "  Sandbox rig present — running live GFO negative matrix against ${JIRA_SANDBOX_BASE_URL}"
    if fsm_read_ready; then
      dim "  FSM-side corroboration ENABLED (browse cred present)"
    else
      dim "  FSM-side corroboration DISABLED (no JIRA_GARDENIO_BROWSE_* cred) — GFO-side primary assertions only"
    fi
    echo

    # ── T3 ALLOWLIST-REJECT ─────────────────────────────────────────
    blue "T3: Analysis ticket with a non-allowlisted label in the labels field → GFO Failed, no FSM label"
    local evil="evil-label-${TEST_TAG}"
    # Baseline FSM label snapshot (browse) to prove NO write afterward.
    local t3_before_body="" t3_before=0
    if fsm_read_ready; then
      t3_before_body=$(jira_get_body "/rest/api/3/issue/${JIRA_SANDBOX_FSM_KEY}?fields=labels" \
        "$JIRA_GARDENIO_BROWSE_EMAIL" "$JIRA_GARDENIO_BROWSE_TOKEN")
      t3_before=$(echo "$t3_before_body" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('fields',{}).get('labels',[])))" 2>/dev/null || echo 0)
    fi
    # The 1:1 Analysis ticket carries the labels in the labels FIELD; R-LABEL rejects
    # element-wise when any label is non-allowlisted, so the whole ticket lands Failed.
    local t3 t3_key t3_code
    t3=$(gfo_create_analysis "$JIRA_SANDBOX_FSM_KEY" "$evil" "" "$(gfo_idem_key "$JIRA_SANDBOX_FSM_KEY" gbfsm)" "T3 allowlist-reject probe: non-allowlisted label. Automated E2E; safe to delete.")
    t3_key="${t3%%|*}"; t3_code="${t3##*|}"
    if [ -z "$t3_key" ]; then
      fail "T3 create Analysis ticket" "gardenio2 create failed (HTTP $t3_code)"
    else
      pass "T3 gardenio2 created Analysis ticket $t3_key with a non-allowlisted label (HTTP $t3_code)"
      local t3_status
      t3_status=$(gfo_wait_status "$t3_key")
      dim "  T3 GFO status: $t3_status"
      if [ "$t3_status" = "Failed" ]; then
        pass "T3 non-allowlisted label → intent landed in Failed (allowlist enforced element-wise)"
      else
        fail "T3 allowlist-reject" "expected GFO Failed, got '$t3_status' — allowlist NOT enforced"
      fi
      if fsm_read_ready; then
        local t3_after_body t3_after
        t3_after_body=$(jira_get_body "/rest/api/3/issue/${JIRA_SANDBOX_FSM_KEY}?fields=labels" \
          "$JIRA_GARDENIO_BROWSE_EMAIL" "$JIRA_GARDENIO_BROWSE_TOKEN")
        t3_after=$(echo "$t3_after_body" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('fields',{}).get('labels',[])))" 2>/dev/null || echo 0)
        assert_not_contains "T3 FSM ticket gained NO evil label" "$evil" "$t3_after_body"
        if [ "$t3_before" = "$t3_after" ]; then
          pass "T3 FSM label count unchanged ($t3_after)"
        else
          fail "T3 FSM label count" "changed $t3_before → $t3_after after a rejected intent"
        fi
      else
        skip "T3 FSM-side no-write corroboration" "browse cred absent"
      fi
    fi
    echo

    # ── T7 REPOINT DEFENSE ──────────────────────────────────────────
    blue "T7: Analysis ticket with malformed fsm_target_key → target regex refuses → GFO Failed, no cross-project write"
    local t7 t7_key t7_code
    # A well-formed allowlisted label, but a fsm_target_key that fails ^FSM-[0-9]+$.
    # (The NON-network face — a crafted target can never redirect the create off
    # GBFSM — is proven with no creds by the Scenario 0e crafted-target block.)
    t7=$(gfo_create_analysis "NOT-A-KEY" "gardenio-triaged" "" "$(gfo_idem_key NOT-A-KEY gbfsm)" "T7 repoint-defense probe: malformed target. Automated E2E; safe to delete.")
    t7_key="${t7%%|*}"; t7_code="${t7##*|}"
    if [ -z "$t7_key" ]; then
      fail "T7 create malformed-target ticket" "gardenio2 create failed (HTTP $t7_code)"
    else
      pass "T7 gardenio2 created malformed-target Analysis ticket $t7_key (HTTP $t7_code)"
      local t7_status
      t7_status=$(gfo_wait_status "$t7_key")
      dim "  T7 GFO status: $t7_status"
      if [ "$t7_status" = "Failed" ]; then
        pass "T7 malformed fsm_target_key ('NOT-A-KEY') → intent Failed (repoint defense: target regex ^FSM-[0-9]+\$ enforced)"
      else
        fail "T7 repoint defense" "expected GFO Failed for malformed target, got '$t7_status'"
      fi
      # No cross-project write is guaranteed by the malformed target format
      # (it addresses no real issue). Corroborate the baseline FSM ticket is
      # untouched when we can read it.
      if fsm_read_ready; then
        local t7_body
        t7_body=$(jira_get_body "/rest/api/3/issue/${JIRA_SANDBOX_FSM_KEY}?fields=labels" \
          "$JIRA_GARDENIO_BROWSE_EMAIL" "$JIRA_GARDENIO_BROWSE_TOKEN")
        assert_not_contains "T7 baseline FSM ticket carries no 'NOT-A-KEY' artifact" "NOT-A-KEY" "$t7_body"
      else
        skip "T7 FSM-side no-write corroboration" "browse cred absent"
      fi
    fi
    echo

    # ── T10 DEAD-LETTER (forced failure → durable Failed column) ────
    blue "T10: forced validation failure (illegal version value in the Analysis version field) → durable dead-letter in Failed column"
    local t10 t10_key t10_code
    # A version value that violates the version-name regex (^[A-Za-z0-9][A-Za-z0-9._-]{0,59}$):
    # a space + '!' cannot match, so R-VERSION's IF is false → ELSE → Failed.
    t10=$(gfo_create_analysis "$JIRA_SANDBOX_FSM_KEY" "gardenio-triaged" "bad version!!" "$(gfo_idem_key "$JIRA_SANDBOX_FSM_KEY" gbfsm)" "T10 dead-letter probe: illegal version. Automated E2E; safe to delete.")
    t10_key="${t10%%|*}"; t10_code="${t10##*|}"
    if [ -z "$t10_key" ]; then
      fail "T10 create bad-version ticket" "gardenio2 create failed (HTTP $t10_code)"
    else
      pass "T10 gardenio2 created bad-version Analysis ticket $t10_key (HTTP $t10_code)"
      local t10_status
      t10_status=$(gfo_wait_status "$t10_key")
      dim "  T10 GFO status: $t10_status"
      if [ "$t10_status" = "Failed" ]; then
        pass "T10 forced failure landed in the Failed column (durable dead-letter, not silently dropped)"
      else
        fail "T10 dead-letter" "expected GFO Failed, got '$t10_status'"
      fi
      # Durability: the failed intent must still EXIST (append-only dead-letter),
      # readable as gardenio2.
      local t10_exists
      t10_exists=$(jira_curl_code GET "/rest/api/3/issue/${t10_key}?fields=status" \
        "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN")
      if [ "$t10_exists" = "200" ]; then
        pass "T10 dead-letter intent persists (HTTP 200 — durable, auditable)"
      else
        fail "T10 dead-letter durability" "intent not retrievable (HTTP $t10_exists)"
      fi
      dim "  T10 owner-error alert path: R-CANARY / notify-on-error = owner email v1 (not API-observable in sandbox; residual noted for prod canary)."
    fi
    echo

    # ── T-1to1 / T-upsert (no-multiply face) — replaces the retired T6 ───
    # T6 was "same Internal-Comment intent twice → exactly ONE FSM comment". The 1:1
    # writer emits NO comment op (mutationFields = description+labels+version only;
    # jira_analysis_test.go asserts the Analysis ticket carries no op_value) and the
    # frozen rules retired R-COMMENT, so there is no FSM comment to dedupe. The dedup
    # SECURITY VALUE survives — strengthened — as the structural 1:1 invariant: a
    # re-triage of the SAME FSM-N UPSERTS the ONE Analysis ticket (SearchGBFSM →
    # EditGBFSMTicket) rather than creating a second, so a retry CANNOT multiply the
    # FSM effect. We prove it GFO-side (no FSM read needed): gfo_find_analysis counts
    # the GBFSM tickets for this FSM-N and asserts it stays 1. (The non-network
    # idempotency/dedup-key stability is proven in Scenario 0e.)
    blue "T-1to1/T-upsert: re-triage the SAME FSM-N twice → exactly ONE GBFSM Analysis ticket (upsert, no pile-up)"
    local up_target="$JIRA_SANDBOX_FSM_KEY"
    local up_idem; up_idem=$(gfo_idem_key "$up_target" gbfsm)
    local up_body="T-upsert probe: re-triage must upsert one Analysis ticket, never pile up. Automated E2E; safe to delete."
    local u1 u1_key u1_code
    u1=$(gfo_create_analysis "$up_target" "gardenio-triaged" "" "$up_idem" "$up_body")
    u1_key="${u1%%|*}"; u1_code="${u1##*|}"
    if [ -z "$u1_key" ]; then
      fail "T-1to1 create first Analysis ticket" "gardenio2 create failed (HTTP $u1_code, '$u1')"
    else
      pass "T-1to1 gardenio2 created the first Analysis ticket $u1_key (HTTP $u1_code)"
      # Wait for it to reach a terminal GFO column before the re-triage so the
      # SearchGBFSM-by-fsm_target_key read is stable (we are testing the upsert
      # identity, not the Automation engine's concurrency).
      local u1_status; u1_status=$(gfo_wait_status "$u1_key")
      dim "  T-1to1 first-ticket GFO status: $u1_status"
      # Re-triage the SAME FSM-N. A faithful 1:1 pipeline UPSERTS (edits the existing
      # ticket); this bot-side probe re-emits the create with the SAME fsm_target_key
      # + idempotency_key to model a retry. The invariant is at-most-one GBFSM ticket
      # per FSM-N regardless.
      local u2 u2_key u2_code
      u2=$(gfo_create_analysis "$up_target" "gardenio-triaged" "" "$up_idem" "$up_body")
      u2_key="${u2%%|*}"; u2_code="${u2##*|}"
      dim "  T-upsert re-triaged the same FSM-N (second emit $u2_key, HTTP $u2_code)"
      # GFO-side count: the mirror of SearchGBFSM (project=<GFO> AND cf[10148]=target).
      # count==1 is the at-most-one-per-FSM-N invariant the writer warns on (>1).
      local up_count; up_count=$(gfo_find_analysis "$up_target")
      if [ "$up_count" = "ERR" ]; then
        skip "T-1to1/T-upsert count corroboration" "GBFSM search read denied/failed as gardenio2 — cannot verify the count; create(s) already observed"
      elif [ "$up_count" = "1" ]; then
        pass "T-upsert re-triage of one FSM-N yields EXACTLY ONE GBFSM Analysis ticket (count=1; the second write UPSERTS via SearchGBFSM→EditGBFSMTicket, never a duplicate) — a retry cannot multiply the FSM effect"
      else
        # A faithful upsert pipeline keeps this at 1. A bot-side re-CREATE (this
        # probe cannot itself run the SearchGBFSM→Edit path) may transiently read >1
        # against a rig whose upsert is not yet wired; that is the rig's signal, not
        # a shape regression — note it loudly but do NOT treat as a hard code fail
        # here (the authoritative bot-side upsert identity is asserted in 0e; the
        # LIVE upsert-edits-not-creates affirmative gate lives in the positive suite).
        skip "T-upsert count corroboration" "gfo_find_analysis returned count=$up_count for $up_target (expected 1). Against a faithful upsert pipeline this is 1; >1 means the live rig has not yet wired SearchGBFSM→EditGBFSMTicket (rig signal). The bot-ownable upsert identity is asserted in Scenario 0e; the affirmative upsert-edits gate is in test-jira-outbox-positive.sh."
      fi
    fi
    echo

    # NOTE — retired comment tests (were T6 comment-dedup + T8 jsdPublic internal-pin):
    #   The per-op Internal-Comment intent (issue-type 10042, op_value customfield_10149)
    #   is GONE from the merged 1:1 writer and from the frozen operator rules (no
    #   R-COMMENT in verify-outbox-rules-json.sh write_newshape_fixtures), so there is
    #   no FSM comment to dedupe (ex-T6) or to internal-pin (ex-T8). Their security
    #   properties survive on the Analysis shape:
    #     • ex-T6 (a retry cannot multiply FSM effects) → T-1to1/T-upsert above.
    #     • ex-T8 (customer-notification-safety: the bot can never post a customer-
    #       facing FSM comment) → the always-on Scenario 0e ABSENT assertions prove the
    #       Analysis payload carries NO op_value / comment / public-visibility surface
    #       BY CONSTRUCTION (a GBFSM write can never post to FSM). The corpus-wide
    #       rule-layer counterpart is verify-outbox-rules-json.sh invariant (c)
    #       (no jsdPublic=true / visibility:public / "Share with customer" anywhere).

    # ── T11 CONFUSED-DEPUTY (non-bot reporter → must NOT propagate) ──
    blue "T11: intent with a NON-bot reporter → does NOT propagate to FSM (reporter-is-bot rule condition)"
    if ! t11_rig_ready; then
      skip "T11 confused-deputy live gate" \
        "no non-bot reporter identity (need JIRA_NONBOT_ACCOUNT_ID / jira-nonbot-cred); the reporter-is-bot condition is a pending rule-spec delta (ka-0000004008) — gate stays inert until a non-bot identity + the live rule exist"
    elif [ "$(reporter_is_bot "$JIRA_NONBOT_ACCOUNT_ID" "$JIRA_BOT_ACCOUNT_IDS")" = "bot" ]; then
      # The forged reporter must NOT itself be a bot, or the test is meaningless.
      skip "T11 confused-deputy live gate" \
        "JIRA_NONBOT_ACCOUNT_ID ('$JIRA_NONBOT_ACCOUNT_ID') is in JIRA_BOT_ACCOUNT_IDS — cannot forge a NON-bot reporter from a bot id"
    else
      local nonbot="$JIRA_NONBOT_ACCOUNT_ID"
      local body11="Confused-deputy probe (${TEST_TAG}): Analysis ticket authored with a NON-bot reporter; must NOT reach FSM. Automated E2E; safe to delete."
      # Baseline FSM label count (browse) BEFORE the non-bot ticket, so the FSM-side
      # corroboration is "the non-propagating ticket added NO label" (count unchanged),
      # replacing the retired "no FSM comment carries the marker" check.
      local t11_before="" t11_before_ok=0
      if fsm_read_ready; then
        t11_before=$(jira_get_body "/rest/api/3/issue/${JIRA_SANDBOX_FSM_KEY}?fields=labels" \
          "$JIRA_GARDENIO_BROWSE_EMAIL" "$JIRA_GARDENIO_BROWSE_TOKEN" \
          | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('fields',{}).get('labels',[])))" 2>/dev/null || echo "")
        [[ "$t11_before" =~ ^[0-9]+$ ]] && t11_before_ok=1
      fi
      # A 1:1 Analysis ticket (valid target, an allowlisted classification label) with
      # the reporter forged to a NON-bot account. The reporter-is-bot condition the
      # frozen fixtures pin on EVERY apply rule (R-LABEL/R-VERSION/R-LINK — verify-
      # outbox-rules-json.sh invariant (d)) must refuse it: it never reaches Synced AND
      # the label never lands on FSM. The reporter-forge (6th arg) is orthogonal to the
      # op type, so it carries over unchanged from the retired per-op shape.
      local t11 t11_key t11_code
      t11=$(gfo_create_analysis "$JIRA_SANDBOX_FSM_KEY" "gardenio-triaged" "" "$(gfo_idem_key "$JIRA_SANDBOX_FSM_KEY" gbfsm)" "$body11" "$nonbot")
      t11_key="${t11%%|*}"; t11_code="${t11##*|}"
      if [ -z "$t11_key" ]; then
        fail "T11 create non-bot-reporter Analysis ticket" "gardenio2 create failed (HTTP $t11_code)"
      else
        pass "T11 gardenio2 created Analysis ticket $t11_key with forged reporter (HTTP $t11_code)"
        # Guard: confirm Jira actually STAMPED the non-bot reporter. If gardenio2
        # lacks Modify-Reporter and Jira silently defaulted reporter→gardenio2 (a
        # bot), the confused-deputy path is un-riggable here → SKIP (a rig
        # limitation, not a breach) rather than false-fail on a legitimate sync.
        local t11_rep
        t11_rep=$(jira_get_body "/rest/api/3/issue/${t11_key}?fields=reporter" \
          "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
          | python3 -c "import sys,json; print(json.load(sys.stdin).get('fields',{}).get('reporter',{}).get('accountId',''))" 2>/dev/null || echo "")
        if [ "$t11_rep" != "$nonbot" ]; then
          skip "T11 confused-deputy" "could not forge a non-bot reporter (intent reporter is '$t11_rep', not '$nonbot' — gardenio2 likely lacks Modify-Reporter); confused-deputy path un-riggable here, verify with an operator-set reporter"
        else
          pass "T11 intent reporter is the NON-bot account '$t11_rep' (forged)"
          local t11_status
          t11_status=$(gfo_wait_not_synced "$t11_key")
          dim "  T11 GFO status: $t11_status (Synced=breach; Failed/UNPROPAGATED=refused)"
          case "$t11_status" in
            Synced)
              fail "T11 confused-deputy" "SECURITY: a NON-bot-reporter intent reached Synced — it PROPAGATED to FSM; the reporter-is-bot condition is NOT enforced (confused-deputy hole open)" ;;
            Failed)
              pass "T11 non-bot-reporter intent → Failed (reporter-is-bot condition refused it via the rule ELSE branch)" ;;
            UNPROPAGATED:*)
              pass "T11 non-bot-reporter intent never propagated (sat in the '${t11_status#UNPROPAGATED:}' column; the reporter-is-bot trigger condition never matched — no FSM write)" ;;
            *)
              fail "T11 confused-deputy (inconclusive)" "unexpected terminal state '$t11_status'" ;;
          esac
          # FSM-side corroboration: a non-propagating Analysis ticket must add NO
          # label to FSM — the label count must not grow. (The 1:1 writer emits no
          # comment, so the retired "no FSM comment carries the marker" check is
          # replaced by this label-count-delta read.)
          if fsm_read_ready; then
            local t11_after t11_after_ok=0
            t11_after=$(jira_get_body "/rest/api/3/issue/${JIRA_SANDBOX_FSM_KEY}?fields=labels" \
              "$JIRA_GARDENIO_BROWSE_EMAIL" "$JIRA_GARDENIO_BROWSE_TOKEN" \
              | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('fields',{}).get('labels',[])))" 2>/dev/null || echo "")
            [[ "$t11_after" =~ ^[0-9]+$ ]] && t11_after_ok=1
            if [ "$t11_before_ok" != 1 ] || [ "$t11_after_ok" != 1 ]; then
              skip "T11 FSM-side no-write corroboration" "FSM label read denied/failed (before='$t11_before' after='$t11_after') — cannot verify; GFO-side non-propagation already observed"
            elif [ "$t11_after" = "$t11_before" ]; then
              pass "T11 FSM ticket label count unchanged ($t11_after) — the non-bot-reporter Analysis ticket added NOTHING to FSM (no propagation)"
            else
              fail "T11 confused-deputy" "SECURITY: FSM label count changed $t11_before → $t11_after after a non-bot-reporter Analysis ticket — it WROTE to FSM"
            fi
          else
            skip "T11 FSM-side no-write corroboration" "browse cred absent — GFO-side non-propagation observed, FSM label count not verifiable"
          fi
        fi
      fi
    fi
    echo

    # ── T12 VISIBILITY-BOUNDARY (gd-7DVpQztZxd access flip) ─────────
    blue "T12: GFO access boundary — portal customer cannot browse; bot writes, non-member cannot create"
    # T12-A: a portal-only FSM customer cannot browse the site-public GFO project.
    if ! t12_portal_ready; then
      skip "T12-A portal-customer browse-deny" \
        "no portal-only customer identity (need JIRA_PORTAL_CUSTOMER_* / jira-portal-customer-cred); the site-public≠customer-visible boundary is un-riggable here"
    else
      local t12a_code
      t12a_code=$(jira_curl_code GET "/rest/api/3/project/${JIRA_GFO_PROJECT_KEY}" \
        "$JIRA_PORTAL_CUSTOMER_EMAIL" "$JIRA_PORTAL_CUSTOMER_TOKEN")
      case "$t12a_code" in
        401|403|404) pass "T12-A portal-only customer canNOT browse GFO (HTTP $t12a_code) — the site-public SOFTWARE project is invisible to portal accounts (no Jira product access)" ;;
        2??)         fail "T12-A visibility boundary" "SECURITY: portal-only customer browsed GFO (HTTP $t12a_code) — a site-public software project leaked to a customer account" ;;
        *)           skip "T12-A portal-customer browse-deny" "unexpected/transport HTTP $t12a_code reading GFO as the portal customer — cannot conclude; re-run or verify in the operator UI" ;;
      esac
      dim "  T12-A note: a 401 is consistent with 'portal-only accounts hold no Jira product API access'; the boundary property (NOT 2xx) holds regardless. Confirm in the operator UI that the portal user sees no GFO project."
    fi

    # T12-B+: the bot (gardenio2, a Member) CAN Create + Edit in GFO. We create a
    # deliberately-non-propagating Analysis ticket (malformed target → rules refuse)
    # purely to exercise the Create/Edit PERMISSION without an FSM write.
    local t12b t12b_key t12b_code
    t12b=$(gfo_create_analysis "NOT-A-KEY-T12-${TEST_TAG}" "gardenio-triaged" "" "$(gfo_idem_key "NOT-A-KEY-T12-${TEST_TAG}" gbfsm)" "T12-B bot create/edit permission probe. Automated E2E; safe to delete.")
    t12b_key="${t12b%%|*}"; t12b_code="${t12b##*|}"
    # A returned key IS the proof of create success (mirrors T3/T10/… which gate on
    # key presence, not an exact 2xx code — Jira returns 201 on create).
    if [ -n "$t12b_key" ]; then
      pass "T12-B bot (Member) CAN Create in GFO — Analysis ticket $t12b_key (HTTP $t12b_code)"
      local t12b_edit
      t12b_edit=$(jira_curl_code PUT "/rest/api/3/issue/${t12b_key}" \
        "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
        '{"fields":{"summary":"e2e-neg T12 bot-edit probe"}}')
      case "$t12b_edit" in
        200|204) pass "T12-B bot (Member) CAN Edit in GFO (HTTP $t12b_edit)" ;;
        *)        fail "T12-B bot edit" "expected 2xx editing its own GFO intent, got HTTP $t12b_edit — Member edit not working" ;;
      esac
    else
      fail "T12-B bot create" "gardenio2 (Member) create of a GFO intent returned no key (HTTP $t12b_code) — Member create not working"
    fi

    # T12-B−: a NON-member licensed user canNOT Create in GFO (write-restriction).
    if ! t12_nonmember_ready; then
      skip "T12-B non-member create-deny" \
        "no non-member licensed identity (need JIRA_NONMEMBER_* / jira-nonmember-cred); the everyone=Viewer / bots+admins=Member write-restriction (gd-7DVpQztZxd) is un-riggable here"
    else
      local nm_payload nm_code nm_key
      nm_payload=$(GFO_PROJECT_KEY="$JIRA_GFO_PROJECT_KEY" ANALYSIS_TYPE="$ANALYSIS_TYPE" python3 -c '
import os, json
f = {
  "project": {"key": os.environ["GFO_PROJECT_KEY"]},
  "summary": "e2e-neg T12 non-member create-deny probe (should be refused)",
}
atype = os.environ.get("ANALYSIS_TYPE", "").strip()
if atype:
    f["issuetype"] = {"id": atype}
print(json.dumps({"fields": f}))
' 2>/dev/null || echo '')
      if [ -z "$nm_payload" ]; then
        skip "T12-B non-member create-deny" "could not build create payload"
      else
        # Capture body too, so a breach (2xx) can be torn down.
        local nm_resp
        nm_resp=$(curl -s -w $'\n%{http_code}' -X POST \
          -u "${JIRA_NONMEMBER_EMAIL}:${JIRA_NONMEMBER_TOKEN}" \
          -H "Content-Type: application/json" -d "$nm_payload" \
          "${JIRA_SANDBOX_BASE_URL}/rest/api/3/issue" 2>/dev/null || printf '\n000')
        nm_code=$(printf '%s' "$nm_resp" | tail -n1)
        nm_key=$(printf '%s' "$nm_resp" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin).get('key',''))" 2>/dev/null || echo "")
        [ -n "$nm_key" ] && CREATED_GFO+=("$nm_key")
        case "$nm_code" in
          401|403) pass "T12-B non-member licensed user canNOT Create in GFO (HTTP $nm_code) — write restricted to bots+admins (confused-deputy front door closed)" ;;
          2??)     fail "T12-B write restriction" "SECURITY: non-member licensed user CREATED a GFO intent (HTTP $nm_code${nm_key:+, $nm_key}) — open browse + open create is a confused-deputy front door; write-restriction NOT enforced" ;;
          *)       skip "T12-B non-member create-deny" "ambiguous HTTP $nm_code (neither a clean 401/403 permission-deny nor a 2xx breach) creating as the non-member — verify the write-restriction in the operator UI" ;;
        esac
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
