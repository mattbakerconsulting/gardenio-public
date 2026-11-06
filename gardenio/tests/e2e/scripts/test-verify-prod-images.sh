#!/usr/bin/env bash
# test-verify-prod-images.sh — `gb verify-prod-images` operator-drill E2E
# (task ka-0000003773, coverage-backfill epic ka-0000000958, related ka-0000007820).
#
# Closes a WHOLE-ORCHESTRATOR coverage gap. `gb verify-prod-images` is an
# operator-driven line of defense against a coverage-instrumented (`-cover`) image
# reaching prod, alongside the Kyverno disallow-cover-images ClusterPolicy. It
# is wired into the release runbook
# (gardenio/docs/release-pipeline.md) as a post-deploy step whose doc states the
# runbook "can branch on `$?` alone". Before this module it had ZERO e2e:
#   grep -rl 'verify-prod-images'        gardenio/tests/  -> 0 hits
#   grep -rl 'verifyProdImages'          gardenio/tests/  -> 0 hits
#   grep -rl 'scanClusterForCoverImages' gardenio/tests/  -> 0 hits
#
# ── What is (and isn't) worth exercising here ─────────────────────────────────
#
# The PURE helpers (classifyImageRef, imageTag, scanValuesForCoverImages,
# splitYAMLKeyVal, stripYAMLComment, resolveProdValuesPath, sortOffenders) are
# already unit-tested (verify_prod_images_test.go, 10 tests). Re-testing those
# here would be duplication.
#
# The UNTESTED surface is the TOP-LEVEL orchestrator runVerifyProdImages
# (verify_prod_images.go:325) and the process contract it wires together — and
# that is exactly what a CLI-level e2e reaches that a pure unit test cannot:
#   • the EXIT CODE: non-zero iff any offender was found (errCoverImagesFound,
#     :360-362) — the single thing the release-runbook `$?` branch keys on;
#   • the JSON EMISSION on stdout: the verifyImagesResult contract
#     (:144-152, marked "STABLE CONTRACT") is actually marshalled and printed;
#   • the compose-order: file surface scanned, --skip-cluster reported as a
#     skipped cluster probe that does NOT itself fail the command (:346-348).
#
# ── Why --skip-cluster (and why this gap is NON-VACUOUS) ──────────────────────
#
# --skip-cluster (:346-348) removes the live-cluster branch entirely, leaving a
# PURE file scan over a --values fixture — no cluster, no kubeconfig, no feature
# flag, no dark code path. The module runs IDENTICALLY in any environment (laptop,
# fork CI, ephemeral namespace, prod), so it is a HARD gate everywhere, never an
# always-SKIP. Checked explicitly against the ka-0000007798 false-premise
# retirement: that gap died because the branch was dark everywhere e2e runs; this
# one has no such problem.
#
# ── ANTI-VACUITY MANDATE (task ka-0000003773) ─────────────────────────────────
#
# If `return errCoverImagesFound` (verify_prod_images.go:361) were changed to
# `return nil`, the OFFENDER legs (leg cover-repo + leg cover-tag) would FAIL:
# they assert the process exits NON-ZERO on a fixture that pins a -cover image.
# Change the return to nil and the exit code becomes 0 while offenders is still
# non-empty and pass=false in the JSON — the exit-code assertion catches exactly
# that false-PASS-in-the-dangerous-direction regression. The two offender legs
# are therefore load-bearing, not decorative.
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   cover-repo   a deploy-values pinning a `controller-cover` repo (the repo-
#                suffix signal the promote gate + Kyverno key on) → exit NON-ZERO,
#                JSON pass=false, an offender naming controller-cover with
#                reason "…-cover suffix", surface=file, source=<file>:<line>.
#   cover-tag    a deploy-values pinning a PLAIN repo with a `cover-main` TAG (the
#                tag-scheme signal, coverTagRe) → exit NON-ZERO, JSON pass=false,
#                an offender whose reason cites the tag scheme. Covers the SECOND
#                detection signal the promote gate keys on (task requirement 4).
#   clean        a representative clean prod deploy-values → exit 0, pass=true,
#                offenders == [].
#   clean-near   a clean deploy-values carrying near-miss tokens (coverage-patch
#                repo, `recover`/`discovery-1` tags — 'cover' mid-word) → exit 0,
#                pass=true, offenders == []. Anti-false-positive: proves a happy
#                verdict is a REAL clean scan, not a matcher that never fires.
#   contract     (folded into every leg) the STABLE JSON contract: top-level keys
#                pass, offenders, file_checked, cluster_probe, cluster_detail all
#                present; every offender carries surface, source, image, reason;
#                cluster_probe == "skipped" (the --skip-cluster compose-order).
#
# ── Skip-clean contract ───────────────────────────────────────────────────────
#
# The `gb` binary + python3 are present in every e2e shard (the shard `use:` list
# includes build-gb; e2e-system-deps.lock ships python3). If either is somehow
# absent the module SKIPS CLEANLY (exit 0) with a LOUD banner — an absent binary
# must never red the suite — mirroring the release-status / grafana-render-cli
# skip_all("loud") contract. When present, every leg is a HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-verify-prod-images.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
WORKDIR="$(mktemp -d "/tmp/verify-prod-images.${RUN_ID}.XXXXXX")"
GB_BIN=""
GB_TEMP=""          # non-empty only when we built a throwaway binary to clean up

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

# skip_all exits 0 (a missing binary must NOT red the suite) with a LOUD banner
# so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP verify-prod-images (LOUD): $why"
  red " This run did NOT assert the 'gb verify-prod-images' operator drill (the"
  red " exit-code contract the release runbook branches on, the STABLE JSON"
  red " verdict, and the --skip-cluster file-scan compose-order). A green suite"
  red " here proves NOTHING about that command. The e2e shard is built with the"
  red " gb binary (build-gb task) + python3 (e2e-system-deps.lock) — if this"
  red " fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$why" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

cleanup() {
  # Only remove a gb we built ourselves; never a PATH-installed one.
  [ -n "$GB_TEMP" ] && rm -f "$GB_TEMP" 2>/dev/null || true
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

# jval <json-file> <key> — read a top-level JSON key as a bare scalar (or the
# literal "null"/"__MISSING__"/"__PARSE_ERR__" sentinels), via python3 so we
# never shell out to jq (advice ka-0000006874: a jq `.k // default` would fold
# null/false/"" together — a bool `pass:false` is exactly what we must read
# faithfully here).
jval() {
  python3 - "$1" "$2" <<'PY'
import json, sys
try:
    obj = json.load(open(sys.argv[1]))
except Exception as e:
    print("__PARSE_ERR__:%s" % e); sys.exit(0)
k = sys.argv[2]
if k not in obj:
    print("__MISSING__")
else:
    v = obj[k]
    print("null" if v is None else v)
PY
}

# has_key <json-file> <key> — echo "yes"/"no" for KEY PRESENCE (not truthiness).
# The verifyImagesResult field set is marked a STABLE CONTRACT; a rename breaks
# the release runbook + any gardener parser, and this is what catches it.
has_key() {
  python3 - "$1" "$2" <<'PY'
import json, sys
try:
    obj = json.load(open(sys.argv[1]))
except Exception:
    print("no"); sys.exit(0)
print("yes" if sys.argv[2] in obj else "no")
PY
}

# off_len <json-file> — length of offenders (-1 on parse error / missing).
off_len() {
  python3 - "$1" <<'PY'
import json, sys
try:
    v = json.load(open(sys.argv[1])).get("offenders")
    print(len(v) if isinstance(v, list) else -1)
except Exception:
    print(-1)
PY
}

# off_type <json-file> — the JSON type of offenders (list|null|__MISSING__|…).
# offenders must ALWAYS be a JSON list (runVerifyProdImages seeds it to []), so a
# consumer ranging over it never trips on null.
off_type() {
  python3 - "$1" <<'PY'
import json, sys
try:
    obj = json.load(open(sys.argv[1]))
except Exception:
    print("__PARSE_ERR__"); sys.exit(0)
if "offenders" not in obj:
    print("__MISSING__"); sys.exit(0)
v = obj["offenders"]
print({list:"list", dict:"dict", str:"str", bool:"bool",
       int:"int", float:"float", type(None):"null"}.get(type(v), type(v).__name__))
PY
}

# off_field <json-file> <index> <key> — read offenders[i][key] ("" on any miss).
off_field() {
  python3 - "$1" "$2" "$3" <<'PY'
import json, sys
try:
    arr = json.load(open(sys.argv[1])).get("offenders", [])
    print(arr[int(sys.argv[2])].get(sys.argv[3], ""))
except Exception:
    print("")
PY
}

# off_any_image_contains <json-file> <substr> — "yes" if ANY offender's image
# contains <substr> (order-independent; sortOffenders orders by surface/source/
# image so we don't hard-code an index).
off_any_image_contains() {
  python3 - "$1" "$2" <<'PY'
import json, sys
try:
    arr = json.load(open(sys.argv[1])).get("offenders", [])
except Exception:
    print("no"); sys.exit(0)
sub = sys.argv[2]
print("yes" if any(sub in o.get("image", "") for o in arr) else "no")
PY
}

# The five top-level keys the verifyImagesResult doc comment names as the stable
# contract, and the four offender keys.
CONTRACT_KEYS=(pass offenders file_checked cluster_probe cluster_detail)
OFFENDER_KEYS=(surface source image reason)

# assert_contract <label> <json-file> — the shared shape gate applied to EVERY
# leg's JSON: parseable, every top-level contract key present, offenders a list,
# cluster_probe == skipped (the --skip-cluster compose-order), and — when
# offenders is non-empty — offenders[0] carries all four offender keys. Returns
# non-zero on any violation (the caller has already emitted the failure lines).
assert_contract() {
  local label="$1" jf="$2" ok=1
  if [ ! -s "$jf" ]; then
    fail "$label contract" "no JSON emitted on stdout"; return 1
  fi
  local parse; parse="$(jval "$jf" pass)"
  case "$parse" in
    __PARSE_ERR__*) fail "$label contract" "unparseable JSON: $parse"; return 1 ;;
  esac
  local k
  for k in "${CONTRACT_KEYS[@]}"; do
    if [ "$(has_key "$jf" "$k")" != "yes" ]; then
      fail "$label contract key" "documented key '$k' missing from JSON (STABLE CONTRACT broken)"; ok=0
    fi
  done
  local ot; ot="$(off_type "$jf")"
  if [ "$ot" != "list" ]; then
    fail "$label contract array" "offenders is '$ot', want a JSON list (never null — consumers range over it)"; ok=0
  fi
  local cp; cp="$(jval "$jf" cluster_probe)"
  if [ "$cp" != "skipped" ]; then
    fail "$label contract cluster" "cluster_probe='$cp', want 'skipped' (--skip-cluster compose-order)"; ok=0
  fi
  # When there is at least one offender, its object must carry the full key set.
  if [ "$(off_len "$jf")" -gt 0 ]; then
    for k in "${OFFENDER_KEYS[@]}"; do
      if [ -z "$(off_field "$jf" 0 "$k")" ]; then
        fail "$label offender key" "offender[0] missing/empty key '$k' (offender contract broken)"; ok=0
      fi
    done
  fi
  [ "$ok" = 1 ]
}

echo "════════════════════════════════════════════════════"
echo " gb verify-prod-images operator-drill E2E (ka-0000003773)"
echo " cover-repo · cover-tag · clean · clean-near · contract"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the gb binary ────────────────────────────────────────
# Prefer a binary pre-built + installed onto PATH.
# Fall back to an in-shell workspace build for local dev. Only when BOTH the
# pre-built binary is absent AND no Go toolchain/source is available do we skip.
if command -v gb >/dev/null 2>&1; then
  GB_BIN="$(command -v gb)"
  pass "using pre-built gb ($GB_BIN)"
else
  command -v go >/dev/null 2>&1 \
    || skip_all "no gb on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/gb" ] \
    || skip_all "no gb on PATH and cmd/gb missing under $CONTROLLER_DIR"
  blue "[build] cmd/gb (from source, workspace mode)"
  GB_TEMP="$(mktemp "/tmp/gb.${RUN_ID}.XXXXXX")"
  GB_BIN="$GB_TEMP"
  BUILD_LOG="/tmp/gb-build-vpi-${RUN_ID}.log"
  # cmd/gb imports the local clicreds module via go.work (replace => ../clicreds),
  # so it MUST build in workspace mode (GOWORK on). Pin GOWORK to the repo-root
  # go.work explicitly (a GOWORK=off build FATALs on the missing replacement).
  if ( GOWORK="$(pwd)/go.work" CGO_ENABLED=0 go build -o "$GB_BIN" ./gardenio/controller/cmd/gb/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/gb built from source"
  else
    fail "build gb" "go build failed: $(tail -8 "$BUILD_LOG")"
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (gb build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi

command -v python3 >/dev/null 2>&1 \
  || skip_all "python3 absent — cannot parse the verify-prod-images JSON verdict"
echo

# ── 2. Write the deploy-values fixtures ──────────────────────────────
# Each fixture is a small prod-shaped deploy-values snippet. The offender
# fixtures pin a -cover image via the TWO independent signals the promote gate +
# Kyverno key on; the clean fixtures must produce a zero-offender pass, including
# the near-miss one whose 'cover' substrings are mid-word (coverage/recover/
# discovery) and MUST NOT trip the classifier.
F_COVER_REPO="$WORKDIR/cover-repo.yaml"
F_COVER_TAG="$WORKDIR/cover-tag.yaml"
F_CLEAN="$WORKDIR/clean.yaml"
F_CLEAN_NEAR="$WORKDIR/clean-near.yaml"

cat > "$F_COVER_REPO" <<'YAML'
# Repo-suffix signal: image.repository ends with -cover.
controller:
  image:
    repository: registry.gitlab.com/acme/platform/gardenio/controller-cover
    tag: "main-abc1234"
beads:
  image:
    repository: kbeads
YAML

cat > "$F_COVER_TAG" <<'YAML'
# Tag-scheme signal: a PLAIN repo pinned with a cover TAG (defensive / hand-edited).
controller:
  image:
    repository: controller
    tag: "cover-main"
YAML

cat > "$F_CLEAN" <<'YAML'
controller:
  image:
    repository: controller
    tag: "2026.213.1"
beads:
  image:
    repository: kbeads
    tag: "latest"
postgres:
  image:
    tag: "pg17"
YAML

cat > "$F_CLEAN_NEAR" <<'YAML'
# Clean, but seeded with near-miss tokens a naive substring matcher would flag:
#   coverage-patch -> 'cover' mid-word in a repository name (NOT a -cover suffix)
#   recover        -> 'cover' mid-word in a tag
#   discovery-1    -> 'cover' mid-word in an inline image tag
controller:
  image:
    repository: controller
    tag: "2026.213.1"
coverage-note:
  image:
    repository: coverage-patch
    tag: "recover"
grafana:
  image: registry.gitlab.com/acme/platform/gardenio/controller:discovery-1
YAML

# run_drill <fixture> — drive `gb verify-prod-images --skip-cluster` against one
# fixture, capturing stdout JSON and the exit code. Echoes the exit code; writes
# JSON to $1.json and stderr to $1.err.
run_drill() {
  local fixture="$1"
  "$GB_BIN" verify-prod-images --skip-cluster --values "$fixture" \
    >"${fixture}.json" 2>"${fixture}.err"
  echo $?
}

# ── Leg cover-repo (repo-suffix signal → non-zero exit, pass=false) ──
blue "[leg cover-repo] a -cover repository → exit NON-ZERO, pass=false, offender names it"
rc="$(run_drill "$F_COVER_REPO")"
J="${F_COVER_REPO}.json"
if [ "$rc" = "0" ]; then
  # THE anti-vacuity assertion: if errCoverImagesFound (verify_prod_images.go:361)
  # were `return nil`, exit would be 0 here while pass=false in the JSON.
  fail "leg cover-repo exit" "exit=0 on a -cover fixture; want NON-ZERO (the runbook \$? branch — errCoverImagesFound regressed to nil?)"
elif assert_contract "cover-repo" "$J"; then
  p="$(jval "$J" pass)"; n="$(off_len "$J")"
  if [ "$p" != "False" ] && [ "$p" != "false" ]; then
    fail "leg cover-repo pass" "pass=$p want false"
  elif [ "$n" -lt 1 ]; then
    fail "leg cover-repo offenders" "offenders len=$n want >=1"
  elif [ "$(off_any_image_contains "$J" "controller-cover")" != "yes" ]; then
    fail "leg cover-repo image" "no offender image contained 'controller-cover'"
  else
    # Confirm the reason cites the repo-suffix signal, and surface/source shape.
    surf="$(off_field "$J" 0 surface)"; src="$(off_field "$J" 0 source)"
    reason_hit="$(python3 - "$J" <<'PY'
import json, sys
arr = json.load(open(sys.argv[1])).get("offenders", [])
print("yes" if any("-cover suffix" in o.get("reason","") for o in arr) else "no")
PY
)"
    if [ "$surf" != "file" ]; then
      fail "leg cover-repo surface" "offender surface=$surf want file"
    elif ! grep -q ':[0-9][0-9]*$' <<<"$src"; then
      fail "leg cover-repo source" "offender source='$src' does not carry a <file>:<line> locator"
    elif [ "$reason_hit" != "yes" ]; then
      fail "leg cover-repo reason" "no offender reason cited the '-cover suffix' signal"
    else
      pass "leg cover-repo — exit=$rc (non-zero), pass=false, offender[controller-cover] surface=file with file:line + -cover-suffix reason"
    fi
  fi
fi
echo

# ── Leg cover-tag (tag-scheme signal → non-zero exit, pass=false) ────
blue "[leg cover-tag] a plain repo with a cover TAG → exit NON-ZERO, pass=false, tag-scheme reason"
rc="$(run_drill "$F_COVER_TAG")"
J="${F_COVER_TAG}.json"
if [ "$rc" = "0" ]; then
  fail "leg cover-tag exit" "exit=0 on a cover-tag fixture; want NON-ZERO (the SECOND detection signal must also fail the drill)"
elif assert_contract "cover-tag" "$J"; then
  p="$(jval "$J" pass)"; n="$(off_len "$J")"
  reason_hit="$(python3 - "$J" <<'PY'
import json, sys
arr = json.load(open(sys.argv[1])).get("offenders", [])
print("yes" if any("cover / release-candidate scheme" in o.get("reason","") for o in arr) else "no")
PY
)"
  if [ "$p" != "False" ] && [ "$p" != "false" ]; then
    fail "leg cover-tag pass" "pass=$p want false"
  elif [ "$n" -lt 1 ]; then
    fail "leg cover-tag offenders" "offenders len=$n want >=1"
  elif [ "$reason_hit" != "yes" ]; then
    fail "leg cover-tag reason" "no offender reason cited the cover TAG scheme (coverTagRe signal not exercised)"
  else
    pass "leg cover-tag — exit=$rc (non-zero), pass=false, tag-scheme offender present (both detection signals covered)"
  fi
fi
echo

# ── Leg clean (representative clean values → exit 0, pass=true, []) ──
blue "[leg clean] a clean prod deploy-values → exit 0, pass=true, offenders=[]"
rc="$(run_drill "$F_CLEAN")"
J="${F_CLEAN}.json"
if [ "$rc" != "0" ]; then
  fail "leg clean exit" "exit=$rc on a clean fixture; want 0 (err: $(tail -2 "${F_CLEAN}.err" 2>/dev/null))"
elif assert_contract "clean" "$J"; then
  p="$(jval "$J" pass)"; n="$(off_len "$J")"
  if [ "$p" != "True" ] && [ "$p" != "true" ]; then
    fail "leg clean pass" "pass=$p want true"
  elif [ "$n" != "0" ]; then
    fail "leg clean offenders" "offenders len=$n want 0 (clean values must not false-positive)"
  else
    pass "leg clean — exit=0, pass=true, offenders=[]"
  fi
fi
echo

# ── Leg clean-near (near-miss tokens → still exit 0, pass=true, []) ──
# Anti-false-positive: proves a happy verdict is a REAL clean scan, not a matcher
# that never fires. If the classifier degraded to a naive substring match on
# 'cover', coverage-patch / recover / discovery-1 would produce phantom offenders.
blue "[leg clean-near] near-miss 'cover' substrings (coverage/recover/discovery) → still clean"
rc="$(run_drill "$F_CLEAN_NEAR")"
J="${F_CLEAN_NEAR}.json"
if [ "$rc" != "0" ]; then
  fail "leg clean-near exit" "exit=$rc on a near-miss-but-clean fixture; want 0 — the classifier false-positived on a mid-word 'cover' (err: $(tail -2 "${F_CLEAN_NEAR}.err" 2>/dev/null))"
elif assert_contract "clean-near" "$J"; then
  p="$(jval "$J" pass)"; n="$(off_len "$J")"
  if [ "$p" != "True" ] && [ "$p" != "true" ]; then
    fail "leg clean-near pass" "pass=$p want true (near-miss 'cover' substrings must NOT match)"
  elif [ "$n" != "0" ]; then
    fail "leg clean-near offenders" "offenders len=$n want 0 — a mid-word 'cover' (coverage-patch/recover/discovery-1) was wrongly flagged: $(off_field "$J" 0 image)"
  else
    pass "leg clean-near — exit=0, pass=true, offenders=[] (mid-word 'cover' correctly ignored)"
  fi
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
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
