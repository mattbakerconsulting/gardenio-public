#!/usr/bin/env bash
# test-lane-lib.sh — hermetic unit test for lane.sh (the live-namespace e2e
# lane driver, gd-WBz2qt9ADZ). No cluster: the two cluster CLIs lane.sh shells
# out to are replaced by fakes written to a temp dir (KUBECTL= / HELM= seams),
# and the pure helpers are driven directly.
#
# Covers the decisions that would otherwise only be exercised at 09:17 UTC
# with nobody watching:
#   1. lease staleness arithmetic (acquireTime + duration vs now, empty = stale)
#   2. newest-calver selection is numeric per field, not lexical
#   3. SKIP= list is exactly run-shard.sh's HERMETIC_MODULES
#   4. report merge: table rows, counts, exit 1 iff any fail, skips kept visible
#   5. acquire: skips a held namespace, sweeps a stale one, takes the first free
#      one, and the wipe never deletes operator-owned objects
#   6. assert-tenant: passes when the fake API denies every ceiling verb
#      (resourcequotas, limitranges, namespaces, cluster RBAC, escalate/bind
#      on roles) and refuses to install the moment one of them is granted or
#      a needed verb is missing; the positive control (create a Role granting
#      update resourcequotas, server dry run) must come back Forbidden, and a
#      success or any other error is also a refusal
#
# Deliberately never names the real cluster CLI on a code line: this file is
# cluster-free and must not register with test-concurrency-isolation.sh as a
# script that talks to a namespace.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0; FAIL=0
pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; [ -n "${2:-}" ] && echo "      $2"; FAIL=$((FAIL + 1)); }
# check <description> <command...>: pass/fail on the command's exit status.
check() { local d="$1"; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
# check_fails <description> <command...>: pass iff the command, run in a
# subshell of THIS shell (so sourced functions and unexported vars are
# visible -- a child `bash -c` would see neither and pass vacuously), exits
# non-zero.
check_fails() { local d="$1"; shift; if ( "$@" ) >/dev/null 2>&1; then fail "$d" "exited 0, want non-zero"; else pass "$d"; fi; }
# check_eq <description> <got> <want>
check_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "got '$2', want '$3'"; fi; }
# check_grep <description> <pattern> <text>
check_grep() { if grep -qF -- "$2" <<<"$3"; then pass "$1"; else fail "$1" "missing '$2' in: $3"; fi; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── fake cluster CLIs ─────────────────────────────────────────────────────────
# State: $TMP/lease-<ns> holds a JSON lease when one exists. Every invocation
# is appended to $TMP/calls so the tests can assert what was (not) deleted.
cat > "$TMP/kc" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$FAKE_STATE/calls"
ns=""; args=("$@")
for ((i=0; i<${#args[@]}; i++)); do [ "${args[$i]}" = "-n" ] && ns="${args[$((i+1))]}"; done
case "$*" in
  *"get lease "*)
    [ -f "$FAKE_STATE/lease-$ns" ] && { cat "$FAKE_STATE/lease-$ns"; exit 0; }
    echo "Error from server (NotFound)" >&2; exit 1 ;;
  *"create -f -"*)
    body=$(cat); ns=$(sed -n 's/^  namespace: //p' <<<"$body")
    if grep -q '^kind: Role$' <<<"$body"; then
      # The escalation probe. Forbidden unless the test opened the hole
      # ($FAKE_STATE/role-create-ok) or asked for a different failure
      # ($FAKE_STATE/role-create-err holds the message).
      name=$(sed -n 's/^  name: //p' <<<"$body")
      [ -f "$FAKE_STATE/role-create-ok" ] && { echo "role.rbac.authorization.k8s.io/$name created (server dry run)"; exit 0; }
      [ -f "$FAKE_STATE/role-create-err" ] && { cat "$FAKE_STATE/role-create-err" >&2; exit 1; }
      echo "Error from server (Forbidden): error when creating \"STDIN\": roles.rbac.authorization.k8s.io \"$name\" is forbidden: user \"system:serviceaccount:arc-runners:gardenio-e2e-runner\" (groups=[\"system:serviceaccounts\"]) is attempting to grant RBAC permissions not currently held: {APIGroups:[\"\"], Resources:[\"resourcequotas\"], Verbs:[\"update\"]}" >&2
      exit 1
    fi
    [ -f "$FAKE_STATE/lease-$ns" ] && { echo "AlreadyExists" >&2; exit 1; }
    holder=$(sed -n 's/^  holderIdentity: "\(.*\)"/\1/p' <<<"$body")
    printf '{"spec":{"holderIdentity":"%s","leaseDurationSeconds":7200,"acquireTime":"%s"}}' \
      "$holder" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$FAKE_STATE/lease-$ns"; exit 0 ;;
  *"delete lease "*) rm -f "$FAKE_STATE/lease-$ns"; exit 0 ;;
  # A label-selected delete that names leases takes the run lease with it.
  *"delete "*leases*" -l gardenio.io/e2e-run"*) rm -f "$FAKE_STATE/lease-$ns"; exit 0 ;;
  *"auth can-i "*)
    # "<verb> <resource>" lines in $FAKE_STATE/can-i-yes are granted; the
    # ceiling resources are denied unless listed; everything else is granted.
    verb="${args[-2]}"; res="${args[-1]}"
    if [ -f "$FAKE_STATE/can-i-yes" ] && grep -qxF "$verb $res" "$FAKE_STATE/can-i-yes"; then echo yes; exit 0; fi
    if [ -f "$FAKE_STATE/can-i-no" ] && grep -qxF "$verb $res" "$FAKE_STATE/can-i-no"; then echo no; exit 1; fi
    case "$verb" in escalate|bind) echo no; exit 1 ;; esac
    case "$res" in resourcequotas|limitranges|namespaces|cluster*) echo no; exit 1 ;; *) echo yes; exit 0 ;; esac ;;
  *) exit 0 ;;
esac
EOF
cat > "$TMP/hm" <<'EOF'
#!/usr/bin/env bash
echo "helm $*" >> "$FAKE_STATE/calls"
case "$*" in *status*) exit 1 ;; *) exit 0 ;; esac
EOF
# Fake pack-gardenio-chart.sh: logs argv, prints a tgz path on its last line
# like the real one; $FAKE_STATE/pack-fail makes it fail.
cat > "$TMP/pack" <<'EOF'
#!/usr/bin/env bash
echo "pack $*" >> "$FAKE_STATE/calls"
[ -f "$FAKE_STATE/pack-fail" ] && { echo "pack-gardenio-chart.sh: FATAL: render changed" >&2; exit 1; }
: > "$2/gardenio-packed.tgz"
echo "Successfully packaged chart"
echo "$2/gardenio-packed.tgz"
EOF
chmod +x "$TMP/kc" "$TMP/hm" "$TMP/pack"
export FAKE_STATE="$TMP"

export LANE_SOURCE_ONLY=1 KUBECTL="$TMP/kc" HELM="$TMP/hm" LANE_PACK="$TMP/pack"
export LANE_HOLDER="test-run-1" LANE_ACQUIRE_TIMEOUT=0 LANE_ACQUIRE_POLL=0
# shellcheck source=lane.sh
source "$SCRIPT_DIR/lane.sh"

echo "=== lane.sh hermetic tests ==="

# 1. lease staleness
now=$(date -u +%s)
live=$(date -u -d "@$((now - 600))" +%Y-%m-%dT%H:%M:%SZ)
old=$(date -u -d "@$((now - 8000))" +%Y-%m-%dT%H:%M:%SZ)
if ! lane_lease_expired "$live" 7200 "$now"; then pass "10-minute-old lease with 2h duration is live"; else fail "live lease judged stale"; fi
check "8000s-old lease with 2h duration is stale"      lane_lease_expired "$old" 7200 "$now"
check "lease with no acquireTime is stale"             lane_lease_expired "" 7200 "$now"
check "unparseable acquireTime is stale"               lane_lease_expired "not-a-date" 7200 "$now"

# 2. newest calver
got=$(printf '%s\n' 2026.286.9 2026.286.10 2026.285.99 v1.2.3 2026.286.10-rc main | lane_pick_newest_calver)
check_eq "picks 2026.286.10 over 2026.286.9 (numeric, not lexical) and ignores non-calver" "$got" "2026.286.10"
no_calver() { printf 'main\nv1.0\n' | lane_pick_newest_calver; }
check_fails "no calver tag -> non-zero" no_calver

# 3. skip list == HERMETIC_MODULES
want=$(sed -n 's/^HERMETIC_MODULES="\(.*\)"$/\1/p' "$SCRIPT_DIR/run-shard.sh")
got=$(lane_skip_list "$SCRIPT_DIR/run-shard.sh")
check_eq "skip-list is run-shard.sh's HERMETIC_MODULES ($(tr ',' '\n' <<<"$got" | wc -l) modules)" "$got" "$want"
check "skip-list is non-empty" test -n "$got"

# 4. report merge
printf '{"name":"a","status":"pass","rc":0,"duration_s":3,"shard":0}\n{"name":"b","status":"skip","rc":0,"duration_s":0,"shard":0,"skip_reason":"no rig"}\n' > "$TMP/r0.jsonl"
printf '{"name":"c","status":"fail","rc":1,"duration_s":9,"shard":1}\n' > "$TMP/r1.jsonl"
rc=0; out=$(lane_report_merge "$TMP/r0.jsonl" "$TMP/r1.jsonl") || rc=$?
check_eq "report exits 1 when any module failed" "$rc" "1"
check_grep "fail row rendered with shard and time" '| fail | 9s | 1 | c |' "$out"
check_grep "skip row keeps its reason visible"   '| skip | 0s | 0 | b | no rig |' "$out"
check_grep "totals line"                          'pass 1 · fail 1 · skip 1' "$out"
rc=0; out=$(lane_report_merge "$TMP/r0.jsonl") || rc=$?
check_eq "report exits 0 with no fail rows" "$rc" "0"
rc=0; out=$(lane_report_merge "$TMP/missing.jsonl") || rc=$?
check_eq "missing jsonl is tolerated (a shard that never wrote results)" "$rc" "0"

# 5. acquire against the fake cluster
export LANE_POOL="gardenio-e2e-0 gardenio-e2e-1"
printf '{"spec":{"holderIdentity":"other-run","leaseDurationSeconds":7200,"acquireTime":"%s"}}' "$live" > "$TMP/lease-gardenio-e2e-0"
: > "$TMP/calls"
got=$(lane_acquire 2>/dev/null)
check_eq "acquire skips the held namespace and leases the free one" "$got" "gardenio-e2e-1"
check "lease carries our holder identity" grep -q '"holderIdentity":"test-run-1"' "$TMP/lease-gardenio-e2e-1"
wiped_held=$(grep -E 'delete' "$TMP/calls" | grep 'gardenio-e2e-0' || true)
check_eq "held namespace was not wiped" "$wiped_held" ""

# both held -> fails fast (timeout 0)
printf '{"spec":{"holderIdentity":"other-run","leaseDurationSeconds":7200,"acquireTime":"%s"}}' "$live" > "$TMP/lease-gardenio-e2e-1"
check_fails "both held -> acquire fails loudly instead of hanging" lane_acquire

# stale lease is swept and reused
printf '{"spec":{"holderIdentity":"dead-run","leaseDurationSeconds":7200,"acquireTime":"%s"}}' "$old" > "$TMP/lease-gardenio-e2e-0"
: > "$TMP/calls"
got=$(lane_acquire 2>/dev/null)
check_eq "stale lease is swept and the namespace reused" "$got" "gardenio-e2e-0"
check "sweep wiped workloads" grep -q 'delete jobs,cronjobs' "$TMP/calls"
check "sweep deleted only Helm-labelled secrets/SAs/roles" grep -q -- '-l app.kubernetes.io/managed-by=Helm' "$TMP/calls"
# Every delete of secrets/roles/rolebindings must be label-scoped (-l). An
# unlabelled delete would take the operator's Role/RoleBinding/ResourceQuota
# or the `ecr` pull Secret with it.
unscoped=$(grep -E 'delete [a-z,]*(secrets|roles|rolebindings|resourcequotas)' "$TMP/calls" | grep -v -- ' -l ' || true)
check_eq "wipe never deletes operator-owned Role/RoleBinding/ResourceQuota/ecr secret (every such delete is label-scoped)" "$unscoped" ""
check "stale lease deleted before re-lease" grep -q 'delete lease' "$TMP/calls"

# 6. assert-tenant
rm -f "$TMP/can-i-yes" "$TMP/can-i-no"
rc=0; ( lane_assert_tenant gardenio-e2e-0 ) >/dev/null 2>&1 || rc=$?
check_eq "assert-tenant passes when every ceiling verb is denied and the tenant verbs are granted" "$rc" "0"
printf 'update resourcequotas\n' > "$TMP/can-i-yes"
rc=0; out=$(lane_assert_tenant gardenio-e2e-0 2>&1) || rc=$?
check_eq "assert-tenant refuses when the runner can update resourcequotas" "$rc" "1"
check_grep "the refusal names the verb that broke the boundary" 'can-i update resourcequotas' "$out"
rm -f "$TMP/can-i-yes"
printf 'delete limitranges\n' > "$TMP/can-i-yes"
check_fails "assert-tenant refuses when the runner can delete limitranges" lane_assert_tenant gardenio-e2e-0
rm -f "$TMP/can-i-yes"
printf 'patch namespaces\n' > "$TMP/can-i-yes"
check_fails "assert-tenant refuses when the runner can patch namespaces (could drop the PSS label)" lane_assert_tenant gardenio-e2e-0
rm -f "$TMP/can-i-yes"
printf 'create pods\n' > "$TMP/can-i-no"
check_fails "assert-tenant refuses when a verb helm install needs is missing" lane_assert_tenant gardenio-e2e-0
rm -f "$TMP/can-i-no"
# escalate/bind: the blind spot behind verbs ["*"] on roles/rolebindings
# (operator hold on gd-LvbhX557UD). Every direct verb answers no while the
# runner can still write itself a Role that grants them.
printf 'escalate roles\n' > "$TMP/can-i-yes"
rc=0; out=$(lane_assert_tenant gardenio-e2e-0 2>&1) || rc=$?
check_eq "assert-tenant refuses when the runner can escalate roles" "$rc" "1"
check_grep "the refusal names escalate roles" 'can-i escalate roles' "$out"
rm -f "$TMP/can-i-yes"
printf 'bind roles\n' > "$TMP/can-i-yes"
check_fails "assert-tenant refuses when the runner can bind roles" lane_assert_tenant gardenio-e2e-0
rm -f "$TMP/can-i-yes"
printf 'bind clusterroles\n' > "$TMP/can-i-yes"
check_fails "assert-tenant refuses when the runner can bind clusterroles" lane_assert_tenant gardenio-e2e-0
rm -f "$TMP/can-i-yes"
# Positive control: the probe Role must be refused with Forbidden by the
# API's escalation check, as a server-side dry run (never persisted).
rm -f "$TMP/calls"
rc=0; out=$(lane_assert_tenant_positive_control gardenio-e2e-0 2>&1) || rc=$?
check_eq "positive control passes when the probe Role is Forbidden" "$rc" "0"
check "positive control creates the probe as a server-side dry run" grep -q -- 'create -f - --dry-run=server' "$TMP/calls"
: > "$TMP/role-create-ok"
rc=0; out=$(lane_assert_tenant_positive_control gardenio-e2e-0 2>&1) || rc=$?
check_eq "positive control refuses when the API lets the runner create a Role granting update resourcequotas" "$rc" "1"
check_grep "the refusal says the escalation check did not fire" 'escalation check did not refuse' "$out"
check "a probe Role that did get created is deleted again" grep -q -- "delete role $LANE_TENANT_PROBE_ROLE" "$TMP/calls"
check_fails "assert-tenant as a whole refuses when the positive control fails" lane_assert_tenant gardenio-e2e-0
rm -f "$TMP/role-create-ok"
echo 'Error from server: dial tcp 10.0.0.1:443: connect: connection refused' > "$TMP/role-create-err"
rc=0; out=$(lane_assert_tenant_positive_control gardenio-e2e-0 2>&1) || rc=$?
check_eq "positive control refuses when the probe fails for a reason other than Forbidden (proves nothing)" "$rc" "1"
check_grep "the refusal says the boundary is unproven" 'UNPROVEN' "$out"
rm -f "$TMP/role-create-err"
# The deny list must name the two operator-ceiling kinds, the namespace
# relabel path and the escalate/bind path; a future edit that drops one
# silently weakens the guard.
for must in "update resourcequotas" "delete resourcequotas" "update limitranges" "delete limitranges" "patch namespaces" "escalate roles" "bind roles" "bind clusterroles"; do
  if tr ',' '\n' <<<"$LANE_TENANT_DENY" | grep -qxF "$must"; then pass "deny list still covers '$must'"; else fail "deny list still covers '$must'"; fi
done

# The wipe at the start of install must keep the lease it runs under: with it
# gone, the next run's acquire saw the namespace free and wiped this install
# mid-wait (gd-mS5EZeWhZF, PR #801's first live-setup).
rm -f "$TMP"/lease-* "$TMP/calls"
got=$(lane_acquire 2>/dev/null)
lane_wipe "$got" 2>/dev/null
check "wipe keeps the run's own lease" test -f "$TMP/lease-$got"
check_eq "a second acquire skips the namespace the wipe ran in" "$(lane_acquire 2>/dev/null)" "gardenio-e2e-1"
rm -f "$TMP"/lease-*

# Install packs first (gd-mS5EZeWhZF): the raw chart dir overflows the 1 MiB
# release Secret, so helm must get the packed tgz and never $CHART_DIR.
rm -f "$TMP/calls" "$TMP/pack-fail"
rc=0; out=$(lane_helm_install gardenio-e2e-0 2026.294.1 2>&1) || rc=$?
check_eq "helm install succeeds with a packed chart" "$rc" "0"
check "the chart is packed at the deployed tag" grep -q -- "pack $CHART_DIR .* --version 2026.294.1 --app-version 2026.294.1" "$TMP/calls"
check "helm installs the packed tgz" grep -q -- 'helm upgrade --install gardenio-e2e-0 .*/gardenio-packed.tgz -n gardenio-e2e-0' "$TMP/calls"
check_fails "helm is never handed the source chart dir" grep -q -- "helm upgrade --install gardenio-e2e-0 $CHART_DIR " "$TMP/calls"
# A RETURN trap outlives the function that set it; one left behind fires in
# the caller (lane_seed's did, with its local unbound under set -u).
lane_helm_install gardenio-e2e-0 2026.294.1 >/dev/null 2>&1
check_eq "install leaves no RETURN trap behind" "$(trap -p RETURN)" ""
rm -f "$TMP/calls"; : > "$TMP/pack-fail"
rc=0; out=$(lane_helm_install gardenio-e2e-0 2026.294.1 2>&1) || rc=$?
check_eq "a failed pack refuses to install" "$rc" "1"
check_grep "the refusal names the pack" 'chart pack failed' "$out"
check_fails "a failed pack never reaches helm upgrade" grep -q -- 'helm upgrade' "$TMP/calls"
rm -f "$TMP/pack-fail"

echo
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
