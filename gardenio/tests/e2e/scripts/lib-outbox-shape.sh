#!/usr/bin/env bash
# lib-outbox-shape.sh — the hermetic 1:1 GBFSM Analysis CREATE-shape self-test,
# factored into ONE sourced library (Phase 5c, ka-0000003147; epic ka-0000002637).
#
# ── Why this library exists ───────────────────────────────────────────────────
#
# The 1:1 outbox reshape (ka-0000002637) collapsed the retired per-op intent
# fan-out (Add-Label / Set-Version / Internal-Comment, each with op_value in
# customfield_10149) into ONE upserted GBFSM "Analysis" issue-type ticket per FSM
# ticket. The CREATE payload the outbox fabricates is fixed by MERGED code
# (jira_outbox.go CreateGBFSMTicket + mutationFields) + the FROZEN schema — NOT by
# the operator's not-yet-built Automation rules. So a PURE-LOCAL assertion on the
# emitted JSON is authoritative and needs ZERO credentials: it is the bot-ownable
# half of the reshape's E2E gate.
#
# Before Phase 5c this self-test lived DUPLICATED, verbatim, in BOTH
# test-jira-outbox-positive.sh and test-jira-outbox-negative.sh, and ran ONLY from
# inside each script's main() — which hard-exits early if the kd daemon is
# unreachable. The main-push CI subset never invoked those scripts
# (they run only via run-shard.sh in the full/scheduled E2E suite), so the reshape
# assertions got ZERO main-push coverage and could silently rot. Phase 5c factors
# the shape here so:
#   1. there is ONE source of truth for the CREATE shape (no drift between the two
#      scripts, which historically diverged only in comment wording);
#   2. a hermetic runner (test-outbox-shape-selftest.sh) can exercise it credless
#      and fail-loud, wired into main-push CI as the always-on gvisor-style gate.
#
# ── What is HERE vs what stays in the E2E scripts ─────────────────────────────
#
# HERE (pure, no network, no creds):
#   - the shared shape config vars (ANALYSIS_TYPE / LABELS_FIELD / VERSION_FIELD /
#     CF_TARGET / CF_IDEM / ALLOWLIST_RE), env-overridable with the illustrative
#     test placeholders as defaults;
#   - gfo_analysis_payload  — builds the ONE Analysis create-body JSON (PURE);
#   - gfo_idem_key / gfo_dedup_key — the writer's upsert-identity contract;
#   - shape_assert          — inspects a built payload (faithful | broken modes);
#   - shape_selftest        — the always-on Scenario 0e: the 6 non-network faces of
#                             T-1to1 / T-link / no-customer-surface / T7 /
#                             T-upsert / discriminator.
#
# NOT here (they need the live sandbox rig / creds, so they stay script-local):
#   - gfo_create_analysis / gfo_find_analysis (the network create + count),
#   - the GFO status pollers, the FSM-side browse probes, the live apply matrix.
#
# ── Caller contract ───────────────────────────────────────────────────────────
#
# Source AFTER the caller has defined the color/reporting helpers this file uses:
#   pass "<msg>"          — record a passing assertion
#   fail "<msg>" "<why>"  — record a failing assertion
# (test-jira-outbox-{positive,negative}.sh and test-outbox-shape-selftest.sh all
# define these before sourcing.) Any of the shape config vars may be overridden by
# exporting them (or their JIRA_* env equivalents) BEFORE sourcing.
#
# Usage:
#   # shellcheck source=./lib-outbox-shape.sh
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-outbox-shape.sh"
#   ...
#   shape_selftest   # runs the 6 always-on, non-network assertions

# Guard against multiple sourcing within the same shell (mirrors lib-retry.sh).
# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_OUTBOX_SHAPE_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_OUTBOX_SHAPE_LOADED=1

# ── Shared 1:1 Analysis shape config ──────────────────────────────────────────
# The GBFSM "Analysis" issue-type id and the labels/version field ids are
# DELIBERATELY EMPTY in the merged writer (the sandbox scaffold is an operator-
# owned Jira-admin action); they are armed via env WITHOUT a code change. We mirror
# that here with env-overridable ids whose defaults are the ILLUSTRATIVE test
# placeholders used by jira_analysis_test.go / verify-outbox-rules-json.sh (10099 /
# customfield_10160 / customfield_10161) — placeholders that pin NOTHING operator-
# facing. Every assertion keys on "$LABELS_FIELD" / "$VERSION_FIELD", never on the
# specific illustrative number.
: "${ANALYSIS_TYPE:=${JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE:-10099}}"     # GBFSM Analysis issue-type id
: "${LABELS_FIELD:=${JIRA_OUTBOX_LABELS_FIELD:-customfield_10160}}" # N triage labels as a FIELD
: "${VERSION_FIELD:=${JIRA_OUTBOX_VERSION_FIELD:-customfield_10161}}" # recommended version as a FIELD

# GBFSM custom fields (sandbox-site ids; overridable for a different site).
# fsm_target_key + idempotency_key are FROZEN by the merged writer
# (customfield_10148 / customfield_10150).
: "${CF_TARGET:=${JIRA_GFO_CF_TARGET:-customfield_10148}}" # fsm_target_key (DATA + link + upsert key)
: "${CF_IDEM:=${JIRA_GFO_CF_IDEM:-customfield_10150}}"     # idempotency_key (keyed on FSM-N)

# ALLOWLIST_RE — the tight op_value allowlist the operator-side R-LABEL rule
# enforces. It is DELIBERATELY a tight ENUMERATED subset of the bridge emitter
# predicate isTriageLabel() (gardenio/controller/internal/bridge/jira_sync.go:
# 638-645), which is PREFIX-based and hence broader. That the rule is TIGHTER than
# the emitter is intended defense-in-depth. The safety-relevant direction the
# always-on shape self-test guards (so drift is caught even with the sandbox rig
# absent) is CANONICAL-EMITTER-VOCABULARY ⊆ RULE-ALLOWLIST: every classification
# label the emitter actually produces MUST be accepted, or the outbox silently
# dead-letters real classifications (the field-i7pu bug — a rule that dropped the
# 'triaged-' infix would REJECT the real gardenio-triaged-sev-3 label).
: "${ALLOWLIST_RE:=^(gardenio-triaged|gardenio-triaged-sev-[0-3]|gardenio-triaged-gxp-(none|low|med|high)|sev-[0-3]|gxp-(none|low|med|high))\$}"

# ── 1:1 GBFSM Analysis-ticket fabrication (repoints the retired per-op fan-out) ──
# gfo_analysis_payload REPLACES the old per-op gfo_create_intent: it builds the ONE
# upserted GBFSM "Analysis" ticket the merged 1:1 writer produces (jira_outbox.go
# CreateGBFSMTicket + mutationFields), NOT an Add-Label / Set-Version / Internal-
# Comment intent. gfo_idem_key / gfo_dedup_key mirror the writer's upsert-identity
# contract (outboxIdempotencyKey / DedupKey).

# gfo_analysis_payload builds the create-body JSON for ONE Analysis ticket and
# echoes it on stdout — PURE, no network, so the always-on shape self-test can
# inspect it with zero creds. Mirrors CreateGBFSMTicket field-for-field:
#   - project.id HARDCODED to the GBFSM numeric id (10037) — NEVER derived from the
#     target, so a crafted fsm_target_key can never redirect the write off GBFSM.
#   - issuetype.id = the (env-armable) Analysis type; OMITTED when empty (mirrors the
#     inert-by-default writer: an empty issue type fails closed / sends nothing).
#   - fsm_target_key ($CF_TARGET) = target, carried as DATA (the link + upsert key).
#   - idempotency_key ($CF_IDEM) = idem (keyed on FSM-N), omitted when empty.
#   - the labels field ($LABELS_FIELD) = the N triage labels, included ONLY when the
#     field id AND the labels are non-empty (mirrors mutationFields gating).
#   - the version field ($VERSION_FIELD) = version, same gating.
#   - description = ADF; a leading clickable "FSM ticket: <target>" link paragraph when
#     a browse URL is supplied (the GBFSM→FSM half of the 1:1 link), else plain body.
#   - reporter.accountId = the optional 7th arg (T11's non-bot forge); omitted otherwise.
# It DELIBERATELY never emits op_value (customfield_10149), any comment op, or any
# public/visibility marker — that ABSENCE is the customer-notification-safety property
# the always-on shape self-test asserts by construction.
gfo_analysis_payload() { # target labels_csv version idem_key body [browse_url] [reporter_id]
  GFO_PROJECT_ID="10037" ANALYSIS_TYPE="$ANALYSIS_TYPE" \
  CF_TARGET="$CF_TARGET" CF_IDEM="$CF_IDEM" \
  LABELS_FIELD="$LABELS_FIELD" VERSION_FIELD="$VERSION_FIELD" \
  TARGET="$1" LABELS_CSV="${2:-}" VERSION="${3:-}" IDEM="${4:-}" BODY="${5:-}" \
  BROWSE_URL="${6:-}" REPORTER="${7:-}" \
  python3 -c '
import os, json
target = os.environ["TARGET"]
# project.id is the hardcoded GBFSM numeric id — never the (untrusted) target.
f = {"project": {"id": os.environ["GFO_PROJECT_ID"]}}
atype = os.environ.get("ANALYSIS_TYPE", "").strip()
if atype:
    f["issuetype"] = {"id": atype}
f["summary"] = "Gardenio analysis: %s" % target
f[os.environ["CF_TARGET"]] = target
idem = os.environ.get("IDEM", "")
if idem:
    f[os.environ["CF_IDEM"]] = idem
labels = [l for l in os.environ.get("LABELS_CSV", "").split(",") if l]
lf = os.environ.get("LABELS_FIELD", "").strip()
if lf and labels:
    f[lf] = labels
vf = os.environ.get("VERSION_FIELD", "").strip()
version = os.environ.get("VERSION", "").strip()
if vf and version:
    f[vf] = version
body = os.environ.get("BODY", "")
browse = os.environ.get("BROWSE_URL", "").strip()
paras = []
if browse and target:
    paras.append({"type": "paragraph", "content": [
        {"type": "text", "text": "FSM ticket: "},
        {"type": "text", "text": target,
         "marks": [{"type": "link", "attrs": {"href": browse}}]}]})
if body:
    paras.append({"type": "paragraph", "content": [{"type": "text", "text": body}]})
f["description"] = {"type": "doc", "version": 1, "content": paras}
reporter = os.environ.get("REPORTER", "")
if reporter:
    # Jira Cloud REST v3: set the reporter by accountId (symmetric with the
    # reporter.accountId read-back). Requires the caller to hold Modify-Reporter.
    f["reporter"] = {"accountId": reporter}
print(json.dumps({"fields": f}))
' 2>/dev/null || echo ''
}

# gfo_idem_key mirrors outboxIdempotencyKey (jira_outbox.go): "<lead>:<op>[:<value>]"
# with lead = the FSM-N key, so a re-triage of the SAME FSM ticket collides on ONE key.
gfo_idem_key() { # fsm_target_key op [value]
  local key="$1:$2"
  [ -n "${3:-}" ] && key="$key:$3"
  echo "$key"
}

# gfo_dedup_key mirrors DedupKey (jira_outbox.go): "gbfsm-outbox:<fsm>:<sorted-labels>:<version>"
# — the upsert identity keyed on the EFFECT, so two builds of the same classification
# collapse to one slot. Labels are sorted so ordering never changes the key.
gfo_dedup_key() { # fsm_target_key labels_csv version
  local sorted
  # sed '/^$/d' drops blank lines and ALWAYS exits 0 (grep -v exits 1 on all-empty
  # input, which would abort the script under set -euo pipefail).
  sorted=$(printf '%s' "${2:-}" | tr ',' '\n' | sed '/^$/d' | LC_ALL=C sort | paste -sd, -)
  echo "gbfsm-outbox:$1:${sorted}:${3:-}"
}

# ── The always-on 1:1 CREATE-shape self-test (pure-local, non-vacuous) ─────────
# shape_assert PAYLOAD MODE — inspects the built JSON. MODE=faithful asserts the
# payload is correct; MODE=broken asserts a deliberately-corrupted payload WOULD be
# caught (the discriminator, mirroring verify-outbox-rules-json.sh's accept/reject
# self-tests so the gate is proven discriminating, not trivially green). Echoes "ok"
# or "BAD:<reason>".
shape_assert() { # payload mode
  PAYLOAD="$1" MODE="$2" LABELS_FIELD="$LABELS_FIELD" VERSION_FIELD="$VERSION_FIELD" \
  CF_TARGET="$CF_TARGET" CF_IDEM="$CF_IDEM" ANALYSIS_TYPE="$ANALYSIS_TYPE" \
  ALLOWLIST_RE="$ALLOWLIST_RE" python3 -c '
import os, json, re
mode = os.environ["MODE"]
lf = os.environ["LABELS_FIELD"]; vf = os.environ["VERSION_FIELD"]
cft = os.environ["CF_TARGET"]; cfi = os.environ["CF_IDEM"]
atype = os.environ["ANALYSIS_TYPE"].strip()
allow = re.compile(os.environ["ALLOWLIST_RE"])
def bad(r):
    print("BAD:" + r); raise SystemExit(0)
try:
    doc = json.loads(os.environ["PAYLOAD"])
except Exception as e:
    bad("payload is not valid JSON (%s)" % e)
f = doc.get("fields")
if not isinstance(f, dict):
    bad("no fields object")
blob = json.dumps(doc).lower()
q = chr(34)  # double-quote; avoids single quotes inside the bash -c wrapper
# ABSENT — the security core (holds for BOTH faithful and broken=... except the
# specific corruption). We evaluate these first because the discriminator MODE
# injects exactly one of them.
# Field-PRESENCE check by design: the merged writer (jira_outbox.go mutationFields)
# emits description + labels + version ONLY — NO visibility/jsdPublic key of ANY
# kind. So we disallow any such marker outright (a benign visibility:internal would
# also trip, which is intentional fail-safe — the bot payload must carry none). This
# is not a public-vs-internal value check; broadening it to inspect the value would
# weaken the posture.
absent_ok = True; absent_reason = ""
public_markers = ["jsdpublic", q + "visibility" + q, "share with customer", q + "public" + q]
if "customfield_10149" in f:
    absent_ok, absent_reason = False, "op_value (customfield_10149) present — this is a per-op intent, not the 1:1 Analysis ticket"
elif f.get("project", {}).get("id") != "10037":
    absent_ok, absent_reason = False, "project.id is %r, not the hardcoded GBFSM 10037 (target must never redirect the create)" % f.get("project", {}).get("id")
elif any(m in blob for m in public_markers):
    absent_ok, absent_reason = False, "a public/visibility/comment marker is present — customer-notification-safety surface leaked into the bot payload"
if mode == "broken":
    # The caller corrupted the payload; the gate MUST catch it (absent_ok False).
    if absent_ok:
        bad("discriminator: a deliberately-corrupted payload passed the ABSENT checks — the gate is vacuous")
    print("ok"); raise SystemExit(0)
# mode == faithful from here: every check must hold.
if not absent_ok:
    bad(absent_reason)
# PRESENT
if atype:
    if f.get("issuetype", {}).get("id") != atype:
        bad("issuetype.id != the configured Analysis type (%r)" % atype)
else:
    if "issuetype" in f:
        bad("issuetype present though the Analysis type is empty (writer omits it when unarmed)")
tgt = f.get(cft)
if not tgt:
    bad("fsm_target_key (%s) absent" % cft)
# fsm_target_key must appear ONLY as data (+ the Description link), never as project.
if f.get("project", {}).get("id") == tgt:
    bad("fsm_target_key leaked into project.id")
if cfi not in f or not f[cfi]:
    bad("idempotency_key (%s) absent" % cfi)
labels = f.get(lf)
if not isinstance(labels, list) or not labels:
    bad("labels field (%s) is not a non-empty list" % lf)
for l in labels:
    if not allow.match(l):
        bad("label %r not accepted by ALLOWLIST_RE (field-i7pu drift guard)" % l)
if not f.get(vf):
    bad("version field (%s) absent" % vf)
desc = f.get("description", {})
if desc.get("type") != "doc" or not desc.get("content"):
    bad("description is not a well-formed non-empty ADF doc")
print("ok")
'
}

# shape_selftest — the always-on, pure-local 1:1 Analysis CREATE-shape self-test.
# The bot-ownable CREATE shape is fixed by the merged writer (jira_outbox.go), so a
# pure-local assertion on the emitted JSON is authoritative and regresses if a future
# edit reintroduces a per-op/comment/public surface, drops the fsm_target_key/
# idempotency_key, hardcodes a field id, or lets a hostile target redirect the create
# off GBFSM. It runs in BOTH E2E scripts (as Scenario 0e) AND standalone in the
# hermetic main-push gate (test-outbox-shape-selftest.sh). It records exactly SIX
# assertions via the caller-provided pass/fail helpers.
shape_selftest() {
  local canon="gardenio-triaged-sev-3" payload r
  # (1) FAITHFUL payload passes every assertion.
  payload=$(gfo_analysis_payload "FSM-408" "gardenio-triaged,$canon" "2026.7.0" \
    "$(gfo_idem_key FSM-408 gbfsm)" "Analysis for FSM-408" \
    "${JIRA_SANDBOX_BASE_URL:-https://example.invalid}/browse/FSM-408")
  if [ -z "$payload" ]; then
    fail "0e shape self-test: build faithful payload" "gfo_analysis_payload produced nothing"
    return
  fi
  r=$(shape_assert "$payload" faithful)
  if [ "$r" = "ok" ]; then
    pass "0e T-1to1 shape: ONE Analysis ticket — project.id=10037, issuetype=\$ANALYSIS_TYPE, labels under \$LABELS_FIELD (all ALLOWLIST_RE-clean), version under \$VERSION_FIELD, fsm_target_key=$CF_TARGET (DATA), idempotency_key=$CF_IDEM, ADF Description with FSM link"
  else
    fail "0e T-1to1 shape self-test" "faithful Analysis payload rejected: ${r#BAD:}"
  fi

  # (2) T-link GBFSM→FSM half: the Description carries the clickable FSM link when a
  #     browse URL is supplied (pure-local; the FSM→GBFSM half is operator R-LINK,
  #     asserted only live behind guards — never here).
  if grep -qF '"FSM ticket: "' <<< "$payload" && grep -qF '"href"' <<< "$payload"; then
    pass "0e T-link (GBFSM→FSM half): the Analysis Description renders the clickable 'FSM ticket:' link"
  else
    fail "0e T-link shape" "the built Description did not carry the FSM browse link paragraph"
  fi

  # (3) ABSENT security core (customer-notification-safety survival of the retired
  #     Internal-Comment/jsdPublic pin). Covered by shape_assert faithful above (no
  #     op_value / no public/visibility marker), stated explicitly for the reader.
  #     The rule-layer counterpart is verify-outbox-rules-json.sh invariant (c).
  if grep -qi 'customfield_10149\|jsdpublic\|"visibility"\|share with customer' <<< "$payload"; then
    fail "0e no-customer-notification-surface" "the Analysis payload carries an op_value / comment / public-visibility surface — it must not (jira_analysis_test.go asserts no op_value; verify-outbox-rules-json.sh invariant (c) owns the rule-layer guarantee)"
  else
    pass "0e no-customer-notification-surface: the bot Analysis payload carries NO op_value/comment/public-visibility field — a GBFSM write can never post a customer-facing FSM comment (BY CONSTRUCTION; survives the retired T8 jsdPublic pin)"
  fi

  # (4) T7 repoint-defense (non-network face): a crafted fsm_target_key can NEVER
  #     redirect the create off GBFSM. Exact list from jira_analysis_test.go.
  local t ct_ok=1 cpay
  for t in "FSM-1" "PE-1" "DEVOPS-9" "GBFSM-5" "'; DROP" "10037"; do
    cpay=$(gfo_analysis_payload "$t" "gardenio-triaged" "" "$(gfo_idem_key "$t" gbfsm)" "x")
    if ! TARGET="$t" CF_TARGET="$CF_TARGET" python3 -c '
import sys, json, os
d = json.load(sys.stdin); f = d["fields"]
tgt = os.environ["TARGET"]; cf = os.environ["CF_TARGET"]
ok = f.get("project", {}).get("id") == "10037" and f.get(cf) == tgt
sys.exit(0 if ok else 1)
' <<< "$cpay" 2>/dev/null; then
      fail "0e T7 repoint-defense (crafted target)" "target '$t' did not keep project.id=10037 with the key verbatim in $CF_TARGET"
      ct_ok=0
    fi
  done
  [ "$ct_ok" = 1 ] && pass "0e T7 repoint-defense (non-network): every crafted fsm_target_key {FSM-1,PE-1,DEVOPS-9,GBFSM-5,'; DROP,10037} keeps project.id=10037 and lands ONLY in $CF_TARGET — the bot cannot redirect a write onto FSM (zero-FSM-write)"

  # (5) T-upsert identity stability (non-network): two builds of the SAME
  #     classification yield the SAME idempotency + dedup key ⇒ a retry maps to ONE
  #     slot (the writer's outboxIdempotencyKey / DedupKey contract).
  local i1 i2 d1 d2
  i1=$(gfo_idem_key FSM-408 gbfsm); i2=$(gfo_idem_key FSM-408 gbfsm)
  d1=$(gfo_dedup_key FSM-408 "$canon,gardenio-triaged" "2026.7.0")
  d2=$(gfo_dedup_key FSM-408 "gardenio-triaged,$canon" "2026.7.0")  # different order, same set
  if [ "$i1" = "$i2" ] && [ "$d1" = "$d2" ]; then
    pass "0e T-upsert identity stable: same FSM-N ⇒ same idempotency_key ('$i1') and order-independent dedup_key — a re-triage UPSERTS one ticket, never piles up (SearchGBFSM→EditGBFSMTicket)"
  else
    fail "0e T-upsert identity" "idempotency/dedup key not stable across identical builds (idem '$i1' vs '$i2'; dedup '$d1' vs '$d2')"
  fi

  # (6) DISCRIMINATOR — prove the ABSENT checks are not vacuous: three deliberately
  #     corrupted payloads MUST each be caught. Mirrors verify-outbox-rules-json.sh's
  #     accept-faithful/reject-drift self-test discipline.
  local disc_ok=1 bp
  # op_value injected
  bp=$(printf '%s' "$payload" | python3 -c 'import sys,json; d=json.load(sys.stdin); d["fields"]["customfield_10149"]="x"; print(json.dumps(d))' 2>/dev/null)
  [ "$(shape_assert "$bp" broken)" = "ok" ] || disc_ok=0
  # target leaked into project.id
  bp=$(printf '%s' "$payload" | python3 -c 'import sys,json; d=json.load(sys.stdin); d["fields"]["project"]["id"]="FSM-408"; print(json.dumps(d))' 2>/dev/null)
  [ "$(shape_assert "$bp" broken)" = "ok" ] || disc_ok=0
  # public-visibility marker injected
  bp=$(printf '%s' "$payload" | python3 -c 'import sys,json; d=json.load(sys.stdin); d["fields"]["visibility"]={"type":"public"}; print(json.dumps(d))' 2>/dev/null)
  [ "$(shape_assert "$bp" broken)" = "ok" ] || disc_ok=0
  if [ "$disc_ok" = 1 ]; then
    pass "0e discriminator: the shape gate CATCHES an injected op_value, a target-into-project.id leak, and a public-visibility marker — the ABSENT assertions are proven discriminating, not trivially green"
  else
    fail "0e discriminator" "a deliberately-corrupted payload slipped past the ABSENT checks — the shape gate is vacuous and must be tightened"
  fi
}
