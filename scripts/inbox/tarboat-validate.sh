#!/usr/bin/env bash
# tarboat-validate.sh — Tarboat from-scratch, inference-driven validation harness.
#
# Tarboat T2 / G2.3 (ka-0000000094), epic ka-0000007759. Operator: Matt Baker.
#
# THE RELEASE GATE. This wraps the entire Tarboat install procedure
# (gardenio/docs/TARBOAT-SETUP.md) into ONE agent-executable script whose exit
# code answers the operator's question: "given only the tarball and one prompt,
# does a from-scratch box reach a working, self-sufficient gardenio?"
#
#   exit 0  → every gate passed: air-gap staging present, bootstrap succeeded,
#             acceptance smoke green, AND an agent on the box completed one
#             inference round-trip via in-cluster LiteLLM. SHIP-ABLE.
#   exit 1  → at least one gate failed (the failing gate + diagnostics printed).
#   exit 2  → usage / precondition error.
#
# WHY a wrapper and not just acceptance-smoke.sh: acceptance-smoke proves the
# box's runtime POSTURE (cert/route/ports/IMDS). It does NOT prove the
# inference-driven INSTALL succeeded from scratch, nor that the box can think on
# its own. This harness adds those gates and sequences the whole path so its
# single exit code is the thing the operator reads before releasing the tarball.
#
# THE FUNCTIONAL "it actually works" GATES (operator directive, decision
# ka-0000006524): posture-green + Services-exist is NOT proof a freshly-
# provisioned box works. Three gates assert the bars the live-box reconcile
# (ka-0000005410) had to verify by hand and that the posture checks miss:
#   * inference round-trip — with --inference-live, a REAL POST /v1/messages must
#     return an assistant message (catches the coopmux<->agent netpol breakage
#     bug ka-0000004409 that a Service-exists + /health probe stays green through).
#   * login completes — the full oauth2 chain must render a USABLE Keycloak Sign-in
#     page with NO invalid_scope (the headline gasbox breakage, epic ka-0000006506),
#     not merely a route-gating 302.
#   * beads data present — the daemon<->postgres path must be healthy (the beads
#     daemon Ready, i.e. its /v1/health DB-ping passes) and, on a box that runs a
#     bead-seeder (the agent-controller), the `beads` table must have > 0 rows.
#     A seeder-less BYO box (agents.enabled=false) has a legitimately empty table
#     and passes on path-health alone (ka-0000005066).
#
# WHERE THIS RUNS: ON the box (Q1 = the driving agent runs on the box), as root.
# It is what the G2.3 validation wraps around the §2–§4 steps of TARBOAT-SETUP.md.
#
# Usage:
#   sudo scripts/inbox/tarboat-validate.sh [--tarboat DIR] [--namespace NS]
#        [--release NAME] [--skip-bootstrap] [--skip-inference] [--skip-login]
#        [--skip-beads] [--skip-persistence] [--skip-cache] [--skip-snapshot]
#        [--skip-chart-lint] [--skip-helm-upgrade] [--skip-helm-rollback]
#        [--skip-routing] [--skip-lb] [--skip-dns]
#        [--chart-dir DIR] [--inference-live] [--box-shape auto|tarball|byo]
#        [--repo-dir DIR] [--ip ADDR] [--apex HOST]
#        [--results-json PATH] [--calver VER]
#
#   --tarboat DIR      unpacked tarball root (default /opt/gardenio/tarboat)
#   --namespace NS     gardenio namespace (default gardenio)
#   --skip-bootstrap   assume the box is already bootstrapped; validate only
#   --skip-inference   skip gate 4 (use ONLY before in-box LiteLLM lands,
#                      ka-0000007994 — a green run with this flag is NOT a
#                      release-grade pass; the harness says so loudly)
#   --inference-live   inside gate 4, POST a REAL /v1/messages and require an
#                      assistant message back. Needs egress to the LLM provider,
#                      so it is OFF in true air-gap; turn it ON for a live box
#                      (e.g. the standing gasbox) to prove agents can think.
#   --skip-login       skip the login-completes gate (gate 5)
#   --skip-beads       skip the beads-data-present gate (gate 6)
#   --skip-persistence skip the DB-persistence gate (gate 7): sentinel write→read
#                      round-trip through postgres + durable (PVC vs emptyDir)
#                      data-dir backing. Set POSTGRES_EPHEMERAL_OK=1 to accept an
#                      emptyDir data dir (throwaway test namespace) as a pass.
#   --skip-cache       skip the cache-state gate (gate 8): SET/GET round-trip
#                      through the OPTIONAL in-chart LiteLLM Redis. An absent
#                      in-chart Redis passes (cache-state N/A); only a present-
#                      but-not-serving cache fails.
#   --skip-snapshot    skip the snapshot-integrity gate (gate 9): a live
#                      pg_dump -Fc piped into pg_restore -l must enumerate a
#                      non-empty archive TOC (the postgres.backup CronJob's exact
#                      mechanism) — proves a RESTORABLE snapshot, read-only.
#   --skip-chart-lint  skip the chart-lint gate (gate 10): `helm lint` on the
#                      shipped gardenio chart (fully offline, no cluster).
#   --skip-helm-upgrade skip the helm-upgrade gate (gate 11): the live release
#                      must be `deployed` AND a `helm upgrade --dry-run
#                      --reuse-values` must render (non-mutating).
#   --skip-helm-rollback skip the helm-rollback gate (gate 12): `helm history`
#                      must be a coherent revision ledger whose latest revision is
#                      `deployed` (read-only; nothing is rolled back).
#   --chart-dir DIR    force the gardenio chart location for the chart-lint +
#                      helm-upgrade gates (default: $REPO_DIR/gardenio/helm/gardenio).
#   --skip-routing     skip the routing gate (gate 13): every Traefik IngressRoute
#                      service reference on the box must resolve to a Service that
#                      exists in the namespace. A dangling route -> Traefik 404 the
#                      route's Host silently — a "route object exists" check misses
#                      it. Read-only (lists IngressRoutes + Services).
#   --skip-lb          skip the load-balancing gate (gate 14): each critical
#                      Service (beads, beads-viewer, forwardauth, litellm) must
#                      have >=1 READY endpoint. A Service with 0 ready backends
#                      exists + has a ClusterIP but black-holes every request as a
#                      503 — a Service-exists check sails through it. Read-only.
#   --skip-dns         skip the DNS gate (gate 15): in-cluster CoreDNS must resolve
#                      a Service's cluster FQDN to its ClusterIP. Everything in the
#                      box talks by DNS name (daemon<->postgres, viewer<->daemon,
#                      agents<->litellm), so a broken CoreDNS/kube-dns is a
#                      whole-box outage a pods-Running check never surfaces.
#   --box-shape S      tarball | byo | auto (default auto). A TARBALL box has the
#                      air-gap tarball unpacked at $TARBOAT (MANIFEST + repo at
#                      $TARBOAT/repo); a BYO box is an internet-clone with the
#                      repo at /opt/gardenio/repo and NO tarball. Gate 1 (air-gap
#                      staging + integrity) is a tarball-box gate: on a byo box it
#                      SKIPs (run is not release-grade) instead of false-failing
#                      on a missing MANIFEST, and the repo-path gates target the
#                      real repo. auto: tarball if $TARBOAT/MANIFEST exists, else
#                      byo if /opt/gardenio/repo holds the repo. (ka-0000001460)
#   --repo-dir DIR     force the staged-repo location (default: $TARBOAT/repo on
#                      a tarball box, /opt/gardenio/repo on a byo box).
#   --apex HOST        dashed-IP sslip apex for the login probe (else derived
#                      from --ip / self-detected public IP)
#   --ip ADDR          box address for the acceptance smoke (else auto-detect)
#   --results-json P   ALSO write a machine-readable per-gate result document to
#                      P (use "-" for stdout). For CI/cron + the E2 regression-
#                      bug filer (ka-0000003645): one object per gate with its
#                      key, status (pass|fail|skip), duration, and the failing
#                      assertion, plus a top-level release_grade flag. The JSON
#                      is emitted even when a gate fails or the script aborts.
#   --calver VER       box image/calver to stamp into the results (else read
#                      from $TARBOAT/CALVER). Lets E2 record which build regressed.
#
# CI/cron: the script is non-interactive and self-contained. Drive an ephemeral
# box by pointing --tarboat / --namespace / --ip at it (or run ON the box, the
# default). Color is auto-disabled when stdout is not a TTY (set NO_COLOR=1 to
# force-disable). Read --results-json for a stable, parseable verdict; exit code
# still answers ship/no-ship (0 ship, 1 a gate failed, 2 usage/precondition).
#
# RESULTS SCHEMA (tarboat-validate/v1):
#   { schema, tarboat, namespace, release, calver, ip, started_at, finished_at,
#     duration_seconds, gates_run, gates_passed, gates_failed, gates_skipped,
#     overall ("pass"|"fail"), release_grade (bool), exit_code,
#     gates: [ { n, key, name, status, duration_seconds, message } ] }
#   release_grade is true ONLY when overall==pass AND no gate was skipped — a
#   --skip-* run can be green but is NOT release-grade, and the flag says so.

set -uo pipefail

TARBOAT="${TARBOAT:-/opt/gardenio/tarboat}"
NAMESPACE="${NAMESPACE:-gardenio}"
RELEASE="${RELEASE:-gardenio}"
SKIP_BOOTSTRAP=0
SKIP_INFERENCE=0
SKIP_LOGIN=0
SKIP_BEADS=0
SKIP_PERSISTENCE=0
SKIP_CACHE=0
SKIP_SNAPSHOT=0
SKIP_CHART_LINT=0
SKIP_HELM_UPGRADE=0
SKIP_HELM_ROLLBACK=0
CHART_DIR="${CHART_DIR:-}"
SKIP_ROUTING=0
SKIP_LB=0
SKIP_DNS=0
INFERENCE_LIVE=0
APEX=""
IP=""
RESULTS_JSON=""
CALVER="${CALVER:-}"
# Box shape (auto|tarball|byo). Two gates are shape-specific — see the box-shape
# resolution block below. `auto` detects the shape from the box; a --box-shape
# flag forces it. BYO_REPO_DIR is where a BYO internet-clone box stages the repo.
BOX_SHAPE="${BOX_SHAPE:-auto}"
BYO_REPO_DIR="${BYO_REPO_DIR:-/opt/gardenio/repo}"
REPO_DIR_OVERRIDE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --tarboat) TARBOAT="$2"; shift 2;;
    --namespace) NAMESPACE="$2"; shift 2;;
    --release) RELEASE="$2"; shift 2;;
    --skip-bootstrap) SKIP_BOOTSTRAP=1; shift;;
    --skip-inference) SKIP_INFERENCE=1; shift;;
    --skip-login) SKIP_LOGIN=1; shift;;
    --skip-beads) SKIP_BEADS=1; shift;;
    --skip-persistence) SKIP_PERSISTENCE=1; shift;;
    --skip-cache) SKIP_CACHE=1; shift;;
    --skip-snapshot) SKIP_SNAPSHOT=1; shift;;
    --skip-chart-lint) SKIP_CHART_LINT=1; shift;;
    --skip-helm-upgrade) SKIP_HELM_UPGRADE=1; shift;;
    --skip-helm-rollback) SKIP_HELM_ROLLBACK=1; shift;;
    --chart-dir) CHART_DIR="$2"; shift 2;;
    --skip-routing) SKIP_ROUTING=1; shift;;
    --skip-lb) SKIP_LB=1; shift;;
    --skip-dns) SKIP_DNS=1; shift;;
    --inference-live) INFERENCE_LIVE=1; shift;;
    --box-shape) BOX_SHAPE="$2"; shift 2;;
    --repo-dir) REPO_DIR_OVERRIDE="$2"; shift 2;;
    --apex) APEX="$2"; shift 2;;
    --ip) IP="$2"; shift 2;;
    --results-json|--json) RESULTS_JSON="$2"; shift 2;;
    --calver) CALVER="$2"; shift 2;;
    -h|--help) sed -n '2,106p' "$0"; exit 0;;
    *) echo "unknown arg: $1 (try --help)" >&2; exit 2;;
  esac
done

case "$BOX_SHAPE" in
  auto|tarball|byo) :;;
  *) echo "invalid --box-shape: $BOX_SHAPE (want auto|tarball|byo)" >&2; exit 2;;
esac

# Color only when stdout is a TTY and NO_COLOR is unset — keeps CI/cron logs clean.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_GRN=$'\033[0;32m'; C_RED=$'\033[0;31m'; C_CYN=$'\033[1;36m'; C_RST=$'\033[0m'
else
  C_GRN=''; C_RED=''; C_CYN=''; C_RST=''
fi

LAST_DIAG=""   # most recent bad() message — captured per gate for the results doc
ok()   { printf '%s✓ %s%s\n' "$C_GRN" "$*" "$C_RST"; }
bad()  { LAST_DIAG="$*"; printf '%s✗ %s%s\n' "$C_RED" "$*" "$C_RST"; }
info() { printf '%s• %s%s\n' "$C_CYN" "$*" "$C_RST"; }

# Per-gate result accumulation for the machine-readable document. GATE_KEY /
# GATE_SKIPPED are set by the caller before each gate() invocation; gate()
# reads and resets them. Defaults make the unit test's bare `gate` calls work.
GATES_RUN=0
GATES_FAILED=0
GATES_PASSED=0
GATES_SKIPPED=0
GATE_KEY=""
GATE_SKIPPED=0
GATE_JSON=()   # one JSON object per gate, joined at emit time

# json_str <text> — minimal JSON string escaper (dependency-free; no jq/python
# needed on an air-gapped box). Handles the metacharacters our messages can carry.
json_str() {
  local s="$1"
  s=${s//\\/\\\\}; s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}; s=${s//$'\t'/\\t}; s=${s//$'\r'/}
  printf '"%s"' "$s"
}

# ─────────────────────────────────────────────────────────────────────────────
# Pure response classifiers for the FUNCTIONAL "it actually works" gates below.
#
# These exist as standalone, dependency-free functions (no kubectl/curl/jq —
# they classify text they are handed) so the gate logic can be unit-tested
# without a box. The operator's bar (decision ka-0000006524 free-text) is that a
# *freshly-provisioned* gasbox is proven to actually WORK, not merely that its
# posture is safe and its Services exist. Each classifier captures one of the
# three functional bars the live-box reconcile (ka-0000005410) had to verify by
# hand, and which the posture-only acceptance smoke + Service-exists inference
# probe do NOT catch.
# ─────────────────────────────────────────────────────────────────────────────

# classify_login <html-or-redirect-body> — does the public login chain land on a
# real, usable Keycloak Sign-in page? Returns 0 (works) / 1 (broken) and echoes
# a one-word verdict. The headline breakage at gasbox epic ka-0000006506 creation
# was `invalid_scope` (Keycloak 403, login UNUSABLE) — a route-gating 302 check
# (acceptance-smoke) is GREEN through that breakage, so this looks deeper: the
# fetched login page must render the kc-form-login password form AND carry no
# invalid_scope/error marker.
classify_login() {
  local body="$1"
  case "$body" in
    *invalid_scope*|*invalid_client*|*"We are sorry"*|*"error_description"*)
      echo "broken"; return 1;;
  esac
  # A usable Keycloak login renders the username/password form. Accept the
  # canonical markers Keycloak emits (kc-form-login id, a password input, or the
  # login action endpoint) — any one proves the realm served a real Sign-in page.
  case "$body" in
    *kc-form-login*|*'name="password"'*|*'id="password"'*|*login-actions/authenticate*)
      echo "ok"; return 0;;
  esac
  echo "no-form"; return 1
}

# classify_inference <http-status> <response-body> — did a real model call
# succeed? A LiteLLM /v1/messages round-trip "works" only when the HTTP status is
# 200 AND the body is a real assistant message (an `"id":"msg_..."` and a
# `"type":"message"`), NOT a LiteLLM/Anthropic error envelope. This is the bar
# the coopmux<->agent netpol fix (82cd5f8, bug ka-0000004409) was load-bearing
# for: a Service-exists + unauthenticated /health probe stays GREEN even when the
# real authenticated call cannot traverse the netpol.
classify_inference() {
  local status="$1" body="$2"
  if [ "$status" != "200" ]; then echo "http-$status"; return 1; fi
  case "$body" in
    *'"error"'*|*'"detail"'*) echo "error-body"; return 1;;
  esac
  case "$body" in
    *'"type":"message"'*|*'"type": "message"'*|*'"id":"msg_'*|*'"id": "msg_'*)
      echo "ok"; return 0;;
  esac
  # Some LiteLLM passthrough shapes return an OpenAI-style envelope; accept a
  # choices/assistant content as a working round-trip too.
  case "$body" in
    *'"choices"'*|*'"content"'*) echo "ok"; return 0;;
  esac
  echo "no-message"; return 1
}

# classify_beads_count <psql-count-output> — normalize the raw `SELECT count(*)`
# line psql emits into a verdict token: a positive integer is echoed verbatim
# (return 0), a readable empty table is "0" (return 1), and empty/garbage output
# (postgres unreachable, no `beads` relation) is "no-count" (return 1). This is
# a PURE normalizer — whether "0" is acceptable is decided by classify_beads_data
# below, which knows if the box even runs a bead-seeder.
classify_beads_count() {
  local out="$1" n
  n="$(printf '%s' "$out" | tr -dc '0-9' )"
  [ -n "$n" ] || { echo "no-count"; return 1; }
  if [ "$n" -gt 0 ] 2>/dev/null; then echo "$n"; return 0; fi
  echo "0"; return 1
}

# classify_beads_data <count-verdict> <daemon-ready> <seeder-present> — decide
# whether the beads_data bar is met, given (a) the classify_beads_count token,
# (b) whether the beads DAEMON pod is Ready (its readiness probe is GET
# /v1/health, which server-side does store.Ping() — so a Ready daemon PROVES the
# daemon<->postgres path traverses the connstring/secret/NetworkPolicies), and
# (c) whether the box runs a bead-SEEDER (the agent-controller — component=agents
# — whose EnsureConfigs is the ONLY thing that writes the ~176 config beads on
# startup; a Slack-less BYO box sets agents.enabled=false and runs no controller,
# so its `beads` table is legitimately EMPTY, ka-0000005066).
#
# Echoes a one-word verdict + returns 0 (bar met) / 1 (fail):
#   <N>            count>0 → healthy AND populated (return 0)
#   empty-no-seeder count=0 but daemon Ready AND no seeder on the box → the
#                  daemon<->postgres path is proven and an empty table is EXPECTED
#                  (return 0). This is the false-failure this gate used to red.
#   unseeded       count=0, daemon Ready, seeder PRESENT → the seeder ran but
#                  wrote nothing: a real seed regression (return 1).
#   path-broken    count=0/no-count AND daemon NOT Ready → the daemon<->postgres
#                  path is down (connstring/secret/NetworkPolicy) (return 1).
#   no-count       count query itself failed (missing table / psql unreachable)
#                  while daemon Ready → schema/DB anomaly (return 1).
classify_beads_data() {
  local count="$1" ready="$2" seeder="$3"
  # A positive count is unambiguous: the path works and data is present.
  case "$count" in
    ''|*[!0-9]*) : ;;                       # not a bare integer (0 / no-count)
    *) if [ "$count" -gt 0 ] 2>/dev/null; then echo "$count"; return 0; fi;;
  esac
  # Zero / unreadable. If the daemon itself is not Ready, its /v1/health DB ping
  # is failing → the path is genuinely broken (the DOA-era + hypothesis-2 shape).
  if [ "$ready" != "ready" ]; then echo "path-broken"; return 1; fi
  # Daemon Ready but the count query returned nothing parseable → the schema/DB
  # is anomalous even though the daemon can ping it (e.g. no `beads` relation).
  if [ "$count" = "no-count" ]; then echo "no-count"; return 1; fi
  # Daemon Ready, table readable but empty. Acceptable ONLY when nothing on the
  # box was ever going to seed it; a present seeder that produced 0 rows is a bug.
  if [ "$seeder" = "seeder" ]; then echo "unseeded"; return 1; fi
  echo "empty-no-seeder"; return 0
}

# ─────────────────────────────────────────────────────────────────────────────
# PERSISTENCE & STATE classifiers (ka-0000004199) — pure, box-less-testable
# response classifiers for the DB-persistence, cache-state, and snapshot-
# integrity gates below. They mirror classify_beads_data: the KUBECTL/psql/redis
# side effects live in the gate_* wrappers; the pass/fail decision is factored
# into these pure functions so it is locked in tarboat-validate_test.sh WITHOUT a
# live box. Each echoes a one-word verdict and returns 0 (bar met) / 1 (fail).
# ─────────────────────────────────────────────────────────────────────────────

# classify_persistence <sentinel-readback> <backing> — decide whether the beads
# postgres store DURABLY persists a write. Inputs:
#   <sentinel-readback>  the value psql read back for the sentinel row we wrote
#                        (empty ⇒ the write/read round-trip itself failed).
#   <backing>            how the postgres data dir is backed on THIS box, as
#                        derived from the StatefulSet's `data` volume:
#                        "pvc"      → a volumeClaimTemplate/PersistentVolumeClaim
#                                     (postgres.persistence.enabled=true) → the
#                                     data survives a pod restart. RELEASE-GRADE.
#                        "emptydir" → an emptyDir data dir (the ephemeral
#                                     test-namespace shape, ka-0000000711): the
#                                     write round-trips NOW but is LOST on pod
#                                     restart. A functioning-but-not-durable box.
#                        ""/unknown → could not determine the backing.
# Verdicts:
#   ok            round-trip succeeded AND the data dir is PVC-backed → durable.
#   ephemeral     round-trip succeeded but the data dir is an emptyDir → the
#                 store works but will NOT survive a restart. On a box that is
#                 meant to be durable (a real tarboat product box) this is a
#                 regression; on a throwaway ephemeral namespace it is expected.
#                 The gate wrapper decides whether to fail on it (box-shape aware).
#   write-failed  the sentinel did not read back → the write path is broken
#                 (RW mount / disk-full / permissions / connstring) (return 1).
#   unknown-backing round-trip ok but the volume backing could not be classified
#                 → cannot assert durability (return 1; investigate).
classify_persistence() {
  local readback="$1" backing="$2"
  if [ -z "$readback" ]; then echo "write-failed"; return 1; fi
  case "$backing" in
    pvc)      echo "ok"; return 0;;
    emptydir) echo "ephemeral"; return 1;;
    *)        echo "unknown-backing"; return 1;;
  esac
}

# classify_cache <deploy-present> <roundtrip> — decide the LiteLLM Redis cache-
# state bar. The in-chart Redis (component=litellm-redis) is OPTIONAL: it renders
# only when litellm.redis.deploy.enabled, and a box may point LiteLLM at an
# external Redis or run with router coordination off entirely. So an ABSENT
# in-chart Redis is NOT a failure — only a PRESENT-but-broken one is. Inputs:
#   <deploy-present>  "present" if a component=litellm-redis pod exists, else "".
#   <roundtrip>       "ok" if a SET/GET round-trip through the cache returned the
#                     written value, "bad" if it did not, "" if not attempted.
# Verdicts:
#   ok            Redis present AND the SET/GET round-trip returned our value.
#   absent        no in-chart Redis on this box → cache-state N/A, bar met.
#   roundtrip-failed Redis present but SET/GET did not return the value → the
#                 cache is up as a pod but not serving reads/writes (return 1).
classify_cache() {
  local present="$1" roundtrip="$2"
  if [ "$present" != "present" ]; then echo "absent"; return 0; fi
  if [ "$roundtrip" = "ok" ]; then echo "ok"; return 0; fi
  echo "roundtrip-failed"; return 1
}

# classify_snapshot <toc-header-count> — decide the snapshot-integrity bar from a
# live `pg_dump -Fc` piped into `pg_restore -l` (the archive's table-of-contents
# listing). A custom-format dump that pg_restore can enumerate proves the on-box
# backup path (the postgres.backup CronJob's exact mechanism) produces a
# RESTORABLE archive — not merely that a file was written. Input:
#   <toc-header-count>  the number of TOC entry lines pg_restore -l emitted for
#                       the dump (0/empty ⇒ pg_dump or pg_restore -l failed, or
#                       the archive is empty/corrupt).
# Verdicts:
#   <N>           N>0 TOC entries → the dump is a well-formed, restorable
#                 custom-format archive (return 0).
#   empty         0 TOC entries → pg_dump produced nothing enumerable, or
#                 pg_restore could not read the archive → integrity fail (return 1).
classify_snapshot() {
  local n
  n="$(printf '%s' "$1" | tr -dc '0-9')"
  if [ -n "$n" ] && [ "$n" -gt 0 ] 2>/dev/null; then echo "$n"; return 0; fi
  echo "empty"; return 1
}

# ── Helm deployment classifiers (ka-0000003227: chart lint / upgrade / rollback)
# The box installs the platform via a Helm release, so "helm deployment" is a
# first-class regression surface distinct from the state gates: the SHIPPED chart
# must lint, an upgrade of the LIVE release must template cleanly, and the release
# history must carry a coherent revision ledger that a rollback can target. All
# three are release-machinery checks that pods-Running / /health-200 posture and
# the state gates never exercise. Kept pure + box-less so the pass/fail logic is
# unit-testable without a k3s box or a helm binary.

# classify_chart_lint <failed-count> — decide the chart-lint bar from the count of
# charts `helm lint` reported as FAILED (parsed from its "N chart(s) failed"
# summary line). `helm lint` on the shipped chart dir needs no cluster, so this is
# a fully-offline, release-grade check that the chart the box ships is valid.
# Inputs:
#   <failed-count>  the integer from helm lint's "N chart(s) failed" summary.
#                   Empty ⇒ the summary could not be parsed (lint did not run, or
#                   the chart dir was missing) — treated as a failure, not a pass.
# Verdicts:
#   ok            0 charts failed → the shipped chart lints clean (return 0).
#   failed        >0 charts failed → the chart has lint errors (return 1).
#   lint-unknown  no parseable summary → helm lint never produced a verdict; we
#                 cannot assert the chart is clean (return 1 — never a silent pass).
classify_chart_lint() {
  local raw="$1" n
  n="$(printf '%s' "$raw" | tr -dc '0-9')"
  if [ -z "$n" ]; then echo "lint-unknown"; return 1; fi
  if [ "$n" -eq 0 ] 2>/dev/null; then echo "ok"; return 0; fi
  echo "failed"; return 1
}

# classify_helm_upgrade <release-status> <dryrun-result> — decide the upgrade bar.
# The gate renders a `helm upgrade --dry-run --reuse-values` of the LIVE release
# (non-mutating — nothing is applied) and reads the release's current status. A
# green verdict proves an upgrade of the running release would template + validate
# against the live cluster, catching a chart that installed once but can no longer
# be re-templated (drifted CRDs, a values schema break, a removed template dep).
# Inputs:
#   <release-status>  helm's status for the release ("deployed", "failed",
#                     "pending-upgrade", …). Empty ⇒ no such release on the box.
#   <dryrun-result>   "ok" if `helm upgrade --dry-run` exited 0, else "bad"/"".
# Verdicts:
#   ok            release is "deployed" AND the dry-run upgrade rendered cleanly
#                 → the live release is upgradeable (return 0).
#   no-release    empty status → no helm release to upgrade on this box (return 1).
#   not-deployed  release exists but status ≠ deployed (failed / mid-operation) →
#                 not in an upgradeable state (return 1).
#   dryrun-failed release deployed but the dry-run upgrade did NOT render → the
#                 chart can no longer be templated against the box (return 1).
classify_helm_upgrade() {
  local status="$1" dryrun="$2"
  if [ -z "$status" ]; then echo "no-release"; return 1; fi
  if [ "$status" != "deployed" ]; then echo "not-deployed"; return 1; fi
  if [ "$dryrun" = "ok" ]; then echo "ok"; return 0; fi
  echo "dryrun-failed"; return 1
}

# classify_helm_rollback <revision-count> <latest-status> — decide the rollback
# bar from the release history (`helm history`, read-only — nothing is rolled
# back). A rollback is only possible/safe when helm holds a coherent revision
# ledger whose latest revision is `deployed`; a release stuck mid-operation, or
# with no recorded history, cannot be safely rolled back. A freshly-installed box
# with a SINGLE deployed revision has no PRIOR revision to target yet — that is a
# clean state (rollback machinery present, no target), not a failure, mirroring
# how an absent optional component passes elsewhere.
# Inputs:
#   <revision-count>  number of revisions helm history lists. Empty/0 ⇒ no history.
#   <latest-status>   status of the newest revision ("deployed", "superseded",
#                     "failed", "pending-*", …).
# Verdicts:
#   rollback-ready  ≥2 revisions AND latest "deployed" → a prior revision exists
#                   to roll back to and the current one is healthy (return 0).
#   single-revision exactly 1 revision AND latest "deployed" → freshly installed;
#                   rollback machinery present but no prior target yet (return 0).
#   no-history      0/empty revisions → helm has no ledger → cannot roll back (1).
#   not-deployed    latest revision ≠ deployed → release is not in a rollback-safe
#                   terminal state (return 1).
classify_helm_rollback() {
  local n status="$2"
  n="$(printf '%s' "$1" | tr -dc '0-9')"
  if [ -z "$n" ] || [ "$n" -eq 0 ] 2>/dev/null; then echo "no-history"; return 1; fi
  if [ "$status" != "deployed" ]; then echo "not-deployed"; return 1; fi
  if [ "$n" -ge 2 ] 2>/dev/null; then echo "rollback-ready"; return 0; fi
  echo "single-revision"; return 0
}

# ─────────────────────────────────────────────────────────────────────────────
# NETWORKING classifiers (ka-0000006092) — pure, box-less-testable response
# classifiers for the routing, load-balancing, and DNS gates below. They mirror
# classify_beads_data: the KUBECTL side effects live in the gate_* wrappers; the
# pass/fail decision is factored into these pure functions so it is locked in
# tarboat-validate_test.sh WITHOUT a live box. Each echoes a one-word verdict and
# returns 0 (bar met) / 1 (fail). The bar (decision ka-0000006524): a fresh box
# must actually ROUTE — a Service/IngressRoute object existing is not proof a
# request reaches a backend. These catch the three network black-holes a posture-
# only "object exists + pod Running" check sails straight through:
#   * a Traefik IngressRoute pointing at a Service that does not exist -> Traefik
#     serves a 404 for that Host (the route "exists" but goes nowhere);
#   * a Service with a ClusterIP but ZERO ready endpoints -> every request 503s
#     (the Service "exists" but load-balances to nothing);
#   * CoreDNS not resolving cluster FQDNs -> every by-name hop fails cluster-wide
#     (all pods can be Running while nothing can talk to anything).
# ─────────────────────────────────────────────────────────────────────────────

# classify_routing <dangling-count> <routes-checked> — decide whether the box's
# Traefik IngressRoutes all point at Services that exist. A gardenio box routes
# every public host (beads-viewer, keycloak, grafana, …) through a Traefik
# IngressRoute whose `services[].name` must name a Service in the namespace; a
# route referencing a missing Service is a silent 404 for that Host (ka-0000007182
# was this class of "route loaded but goes nowhere" bug). Inputs:
#   <dangling-count>  number of IngressRoute service refs that did NOT resolve to
#                     an existing Service (the gate wrapper counts them).
#   <routes-checked>  total IngressRoute service refs examined. 0 ⇒ nothing to
#                     check — either Traefik IngressRoutes are disabled on this box
#                     (AVA/ALB edge owns routing) or none were found; either way it
#                     is NOT a routing failure, so it passes as "no-routes".
# Verdicts:
#   ok           routes-checked>0 AND dangling-count==0 → every route resolves.
#   no-routes    routes-checked==0 → no in-cluster IngressRoutes to validate
#                (edge-owned routing / IngressRoutes disabled), bar met.
#   dangling-<N> dangling-count>0 → N IngressRoute refs point at a missing
#                Service → those Hosts 404 (return 1).
classify_routing() {
  local dangling="$1" checked="$2"
  dangling="$(printf '%s' "$dangling" | tr -dc '0-9')"; dangling="${dangling:-0}"
  checked="$(printf '%s' "$checked" | tr -dc '0-9')"; checked="${checked:-0}"
  if [ "$checked" -eq 0 ] 2>/dev/null; then echo "no-routes"; return 0; fi
  if [ "$dangling" -eq 0 ] 2>/dev/null; then echo "ok"; return 0; fi
  echo "dangling-$dangling"; return 1
}

# classify_lb <present> <ready-endpoints> — decide whether a Service actually
# load-balances to a live backend. A Service can exist with a ClusterIP and still
# black-hole every request when its Endpoints/EndpointSlice has ZERO ready
# addresses (all backing pods NotReady/absent) — kube-proxy then has nothing to
# forward to and the caller gets a connection refused / 503. A "get svc" check is
# green through this; only counting READY endpoints catches it. Inputs:
#   <present>          "present" if the Service object exists, else "" — an ABSENT
#                      Service is not judged here (it is optional-component or the
#                      wrong box shape); the gate wrapper only feeds present ones.
#   <ready-endpoints>  count of READY endpoint addresses behind the Service.
# Verdicts:
#   ok           present AND ready-endpoints>0 → Service has a live backend.
#   absent       Service not present → not this gate's failure (skipped upstream).
#   no-endpoints present but 0 ready endpoints → Service black-holes traffic (503),
#                the exact failure a Service-exists check misses (return 1).
classify_lb() {
  local present="$1" ready="$2"
  if [ "$present" != "present" ]; then echo "absent"; return 0; fi
  ready="$(printf '%s' "$ready" | tr -dc '0-9')"; ready="${ready:-0}"
  if [ "$ready" -gt 0 ] 2>/dev/null; then echo "ok"; return 0; fi
  echo "no-endpoints"; return 1
}

# classify_dns <resolved-ip> <expected-ip> — decide whether in-cluster DNS
# (CoreDNS/kube-dns) resolves a Service's cluster FQDN to its ClusterIP. Every
# by-name hop in the box (daemon<->postgres, viewer<->daemon, agents<->litellm)
# depends on this; a broken CoreDNS is a cluster-wide outage that leaves every pod
# Running but unable to reach anything. Inputs:
#   <resolved-ip>  the A record CoreDNS returned for the Service FQDN (empty ⇒
#                  resolution failed — NXDOMAIN, SERVFAIL, or CoreDNS unreachable).
#   <expected-ip>  the Service's ClusterIP as read from the API (empty ⇒ the
#                  Service is headless/None or has no ClusterIP, so an exact IP
#                  match is not required — any successful resolution counts).
# Verdicts:
#   ok           resolved-ip is non-empty AND (expected-ip empty OR they match) →
#                CoreDNS resolves the name to the right address.
#   mismatch     both non-empty but resolved-ip != expected-ip → DNS resolves the
#                name to the WRONG address (stale/hijacked record) (return 1).
#   unresolved   resolved-ip empty → CoreDNS did not resolve the name at all →
#                cluster DNS is down (return 1).
classify_dns() {
  local resolved="$1" expected="$2"
  if [ -z "$resolved" ]; then echo "unresolved"; return 1; fi
  if [ -n "$expected" ] && [ "$resolved" != "$expected" ]; then echo "mismatch"; return 1; fi
  echo "ok"; return 0
}

# parse_nslookup_answer — read busybox `nslookup` output on stdin and echo the
# FIRST answer-section address: the "Address" line(s) that FOLLOW the "Name:"
# line, with any trailing :port stripped. The header block ("Server:/Address:" —
# the DNS server's OWN address) and any trailing non-address noise (kubectl run's
# pod-name/status echo) are ignored, and the stream is NOT newline-stripped.
# This replaces the old `awk '/^Address/{a=$NF} END{print a}' | tr -dc '0-9.:'`
# pipeline, whose tr squashed newlines and so concatenated a stray numeric token
# (the probe pod's $$ PID, e.g. "…36" + "45232" -> "…3645232") onto the real
# ClusterIP, producing a phantom DNS "mismatch" on a healthy box (ka-0000000669).
parse_nslookup_answer() {
  awk '
    /^[Nn]ame:/       { ans=1; next }
    ans && /^Address/ { ip=$NF; sub(/:[0-9]+$/, "", ip); print ip; exit }
  '
}

# pick_release <preferred-name> <preferred-namespace> — read helm releases on
# stdin (one per line: "NAME NAMESPACE STATUS CHART", space-delimited — helm
# names/namespaces/statuses/charts never contain spaces) and echo the chosen
# release as "NAME NAMESPACE STATUS" (return 0), or nothing (return 1). This lets
# the helm-upgrade/rollback gates DISCOVER the live release instead of assuming a
# fixed name+namespace: on the first real BYO box the workloads were healthy
# (acceptance PASS) yet `helm status gardenio -n gardenio` came back empty, so the
# gates false-failed "no-release" (ka-0000000669). Selection order:
#   1. an exact NAME==preferred (and, when preferred-ns given, NAMESPACE match) —
#      the assumed pair when helm actually records it (fast path, unchanged);
#   2. else the SINGLE release whose CHART is the gardenio chart (chart gardenio-*),
#      wherever helm recorded it — the box installed under a drifted name/namespace;
#   3. else, if helm holds exactly ONE release total, that one;
#   4. else nothing (ambiguous / none) → the gate reports no-release as before.
# helm_list_lines — read `helm list -A -o json` on stdin and echo one release per
# line as "NAME NAMESPACE STATUS CHART". Dependency-free (no jq — air-gap-safe):
# each JSON object begins with '{' and helm's name/namespace/status/chart values
# never contain '{' or '"', so splitting on '{' isolates one record per line.
helm_list_lines() {
  local obj name ns status chart
  # `|| [ -n "$obj" ]` so the final record (no trailing newline after tr) is read.
  tr '{' '\n' | while IFS= read -r obj || [ -n "$obj" ]; do
    case "$obj" in *'"name"'*) ;; *) continue;; esac
    name="$(printf '%s' "$obj" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p')"
    ns="$(printf '%s' "$obj" | sed -n 's/.*"namespace":"\([^"]*\)".*/\1/p')"
    status="$(printf '%s' "$obj" | sed -n 's/.*"status":"\([^"]*\)".*/\1/p')"
    chart="$(printf '%s' "$obj" | sed -n 's/.*"chart":"\([^"]*\)".*/\1/p')"
    [ -n "$name" ] && printf '%s %s %s %s\n' "$name" "$ns" "$status" "$chart"
  done
}

pick_release() {
  local want="$1" wantns="$2"
  local exact="" gardenio_line="" gardenio_n=0 only_line="" total=0
  local name ns status chart
  while read -r name ns status chart; do
    [ -n "$name" ] || continue
    total=$((total + 1)); only_line="$name $ns $status"
    if [ "$name" = "$want" ] && { [ -z "$wantns" ] || [ "$ns" = "$wantns" ]; }; then
      exact="$name $ns $status"
    fi
    case "$chart" in gardenio-*) gardenio_n=$((gardenio_n + 1)); gardenio_line="$name $ns $status";; esac
  done
  if [ -n "$exact" ]; then printf '%s\n' "$exact"; return 0; fi
  if [ "$gardenio_n" -eq 1 ]; then printf '%s\n' "$gardenio_line"; return 0; fi
  if [ "$total" -eq 1 ]; then printf '%s\n' "$only_line"; return 0; fi
  return 1
}

gate() {  # gate <name> <command...> — run, record pass/fail/skip, never abort early
  local name="$1"; shift
  local key="${GATE_KEY:-}"
  GATES_RUN=$((GATES_RUN + 1))
  LAST_DIAG=""; GATE_SKIPPED=0
  info "GATE ${GATES_RUN}: ${name}"
  local t0 t1 dur rc status msg
  t0=$(date +%s 2>/dev/null || echo 0)
  if "$@"; then rc=0; else rc=1; fi
  t1=$(date +%s 2>/dev/null || echo 0)
  dur=$((t1 - t0)); [ "$dur" -ge 0 ] 2>/dev/null || dur=0
  if [ "${GATE_SKIPPED:-0}" = 1 ]; then
    status="skip"; msg="$LAST_DIAG"; GATES_SKIPPED=$((GATES_SKIPPED + 1))
    info "GATE ${GATES_RUN} SKIP: ${name}"
  elif [ "$rc" = 0 ]; then
    status="pass"; msg=""; GATES_PASSED=$((GATES_PASSED + 1))
    ok "GATE ${GATES_RUN} PASS: ${name}"
  else
    status="fail"; msg="${LAST_DIAG:-gate command exited non-zero (see log above)}"
    GATES_FAILED=$((GATES_FAILED + 1))
    bad "GATE ${GATES_RUN} FAIL: ${name}"
  fi
  GATE_JSON+=("$(printf '{"n":%d,"key":%s,"name":%s,"status":%s,"duration_seconds":%d,"message":%s}' \
    "$GATES_RUN" "$(json_str "$key")" "$(json_str "$name")" "$(json_str "$status")" "$dur" "$(json_str "$msg")")")
  GATE_KEY=""
  [ "$status" = fail ] && return 1
  return 0
}

# ── Box-shape resolution ─────────────────────────────────────────────────────
# The suite runs against two DIFFERENT box shapes and two gates are shape-
# specific; conflating them is what mis-filed the tarboat-2026.200.6 regressions
# ka-0000001460 (staging) + ka-0000002999 (acceptance):
#
#   * TARBALL box — the true air-gap artifact: the tarball is unpacked at
#     $TARBOAT (default /opt/gardenio/tarboat), so $TARBOAT/MANIFEST exists and
#     the repo tree lives at $TARBOAT/repo. Gate 1 (air-gap staging + integrity)
#     is a MEANINGFUL, release-grade check here.
#
#   * BYO box — a bring-your-own internet-clone box (scripts/render-user-data.sh
#     clones the repo to /opt/gardenio/repo in cloud-init; there is NO unpacked
#     tarball, so $TARBOAT/MANIFEST never exists). Gate 1 is INAPPLICABLE — the
#     box was never built from a tarball — and probing $TARBOAT/repo for
#     acceptance-smoke.sh finds nothing because the repo is at /opt/gardenio/repo.
#
# Detect the shape (unless forced): a readable $TARBOAT/MANIFEST ⇒ tarball;
# otherwise, if $BYO_REPO_DIR holds the repo ⇒ byo. Gate 1 then SKIPs on a BYO
# box (recorded as a skip → the run is correctly NOT release-grade) instead of
# false-failing, and REPO_DIR points at wherever the repo actually is.
if [ "$BOX_SHAPE" = auto ]; then
  if [ -s "$TARBOAT/MANIFEST" ]; then
    BOX_SHAPE=tarball
  elif [ -x "$BYO_REPO_DIR/scripts/bootstrap-ec2.sh" ]; then
    BOX_SHAPE=byo
  else
    # Neither shape resolvable — default to tarball so Gate 1 fails LOUDLY with
    # its real diagnostic (a genuinely-missing tarball) rather than silently
    # skipping. A misconfigured box should not masquerade as a BYO skip.
    BOX_SHAPE=tarball
  fi
fi

if [ -n "$REPO_DIR_OVERRIDE" ]; then
  REPO_DIR="$REPO_DIR_OVERRIDE"
elif [ "$BOX_SHAPE" = byo ]; then
  REPO_DIR="$BYO_REPO_DIR"
else
  REPO_DIR="$TARBOAT/repo"
fi

# ── Gate 1: air-gap staging present + tarball integrity ──────────────────────
# Release-grade ONLY on a tarball box. On a BYO internet-clone box there is no
# unpacked tarball to check, so the gate SKIPs (→ not release-grade) rather than
# false-failing on a missing MANIFEST (regression ka-0000001460).
gate_staging() {
  if [ "$BOX_SHAPE" = byo ]; then
    GATE_SKIPPED=1
    LAST_DIAG="box-shape=byo: no unpacked tarball to stage-check (air-gap integrity is a tarball-box gate; run against a tarball box for a release-grade pass)"
    info "  box-shape=byo: air-gap staging gate N/A (no tarball on a BYO clone box)"
    return 0
  fi
  [ -s "$TARBOAT/MANIFEST" ] || { bad "no MANIFEST under $TARBOAT"; return 1; }
  if [ -s "$TARBOAT/SHA256SUMS" ]; then
    ( cd "$TARBOAT" && sha256sum -c SHA256SUMS >/dev/null 2>&1 ) \
      || { bad "SHA256SUMS verification failed"; return 1; }
  else
    bad "no SHA256SUMS (G1.3 not staged?)"; return 1
  fi
  local missing=0 f
  for f in bin/k3s bin/k3s-install.sh bin/helm bin/crane bin/zot \
           manifests/cert-manager.yaml manifests/servicemonitor-crd.yaml \
           charts/keycloak-operator-1.32.0.tgz; do
    [ -s "$TARBOAT/$f" ] || { bad "missing offline asset: $f"; missing=1; }
  done
  # The zot crane OCI-layout bundle --airgap/seed-bundle.sh seeds the in-box
  # registry from (Q3=zot; TARBOAT-SETUP.md §3/§4/§6). This is the same filename
  # the runbook's §3 preconditions require, so a tarball that passes this gate is
  # a tarball whose from-scratch runbook does not FATAL at §3 (ka-0000004567).
  ls "$TARBOAT"/images/gardenio-bundle-*.oci.tar.zst >/dev/null 2>&1 \
    || { bad "missing zot image bundle (images/gardenio-bundle-*.oci.tar.zst)"; missing=1; }
  [ -x "$REPO_DIR/scripts/bootstrap-ec2.sh" ] \
    || { bad "repo tree not staged at $REPO_DIR (G1.2)"; missing=1; }
  [ "$missing" = 0 ]
}

# ── Gate 2: bootstrap succeeded (true air-gap) ───────────────────────────────
gate_bootstrap() {
  if [ "$SKIP_BOOTSTRAP" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-bootstrap: assuming box already up (gate not validated)"
    info "  --skip-bootstrap: assuming box already up"; return 0
  fi
  local appver="latest"
  [ -s "$TARBOAT/CALVER" ] && appver="$(cat "$TARBOAT/CALVER")"
  ( cd "$REPO_DIR" && OFFLINE_ASSETS="$TARBOAT" scripts/bootstrap-ec2.sh \
      --scenario A --harden --airgap-strict --offline-assets "$TARBOAT" \
      --app-version "$appver" \
      --extra-values gardenio/helm/gardenio/values-inbox.yaml \
      --extra-values deploy-values/gardenio-inbox.yaml ) \
    || return 1
}

# ── Gate 3: acceptance smoke (posture) ───────────────────────────────────────
gate_acceptance() {
  [ -n "$IP" ] || IP="$(curl -s -m 3 http://169.254.169.254/latest/meta-data/public-ipv4 2>/dev/null \
                        || hostname -I 2>/dev/null | awk '{print $1}')"
  [ -x "$REPO_DIR/scripts/inbox/acceptance-smoke.sh" ] \
    || { bad "acceptance-smoke.sh not found under $REPO_DIR"; return 1; }
  # Bounded: a hung smoke (the --on-box arg-parse spin ate a sweep's whole 60m
  # CI task budget, ka-0000002999) must fail THIS gate loudly instead.
  local rc=0
  ( cd "$REPO_DIR" && timeout -k 30 "${ACCEPTANCE_TIMEOUT:-600}" \
      scripts/inbox/acceptance-smoke.sh --on-box \
      --namespace "$NAMESPACE" --release "$RELEASE" --ip "$IP" ) || rc=$?
  if [ "$rc" = 124 ] || [ "$rc" = 137 ]; then
    bad "acceptance smoke TIMED OUT after ${ACCEPTANCE_TIMEOUT:-600}s (hung probe — see log above)"
    return 1
  fi
  return "$rc"
}

# ── Gate 4: inference round-trip via in-cluster LiteLLM (the headline) ───────
# In-box LiteLLM (ka-0000007994) has LANDED, so this gate is real: it asserts the
# in-cluster LiteLLM Service exists, its proxy answers an in-cluster probe, and
# agent egress is default-denied (so inference is forced through LiteLLM, not
# direct-to-internet). --skip-inference still bypasses it, but that is explicitly
# NOT a release-grade pass and the harness says so.
gate_inference() {
  if [ "$SKIP_INFERENCE" = 1 ]; then
    GATE_SKIPPED=1
    LAST_DIAG="--skip-inference set: in-box inference unverified (NOT a release-grade pass)"
    bad "  --skip-inference set: NOT a release-grade pass (in-box inference unverified)"
    info "  (drop --skip-inference now that in-box LiteLLM has landed for a real pass)"
    return 0   # not counted as a failure, but recorded as a skip → not release-grade
  fi
  # In-box inference (ka-0000007994) has landed: validate the real round-trip.
  #   (a) the in-cluster LiteLLM Service exists,
  #   (b) its proxy answers (an in-cluster /health or /v1/models probe), and
  #   (c) agent pods have a default-deny egress NetworkPolicy (so inference is
  #       forced through LiteLLM, not direct-to-internet).
  local svc="${LITELLM_SVC:-gardenio-litellm}" port="${LITELLM_PORT:-4000}"
  if ! kubectl -n "$NAMESPACE" get svc "$svc" >/dev/null 2>&1; then
    bad "  no in-cluster LiteLLM Service ($svc) in ns/$NAMESPACE — in-box inference not deployed"
    info "  (pass --skip-inference to validate the rest, but that is NOT release-grade)"
    return 1
  fi
  info "  LiteLLM Service $svc:$port present"

  # (b) Probe the proxy from inside the cluster (a throwaway pod on the cluster
  # network). LiteLLM exposes an unauthenticated /health/liveliness; fall back
  # to the TCP port if the path differs across versions.
  local probe_url="http://${svc}.${NAMESPACE}.svc:${port}"
  if kubectl -n "$NAMESPACE" run tarboat-litellm-probe-$$ --rm -i --restart=Never \
       --image=curlimages/curl:8.10.1 --command -- \
       sh -c "curl -fsS -m 8 ${probe_url}/health/liveliness || curl -fsS -m 8 ${probe_url}/v1/models -H 'Authorization: Bearer sk-noop' -o /dev/null" \
       >/dev/null 2>&1; then
    ok "  LiteLLM proxy answered an in-cluster probe"
  else
    # curlimages may be unavailable air-gapped; fall back to a readiness check.
    if kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm \
         -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True; then
      info "  probe image unavailable; LiteLLM pod reports Ready (acceptable air-gapped)"
    else
      bad "  LiteLLM proxy did not answer and no Ready LiteLLM pod found"
      return 1
    fi
  fi

  # (c) Agent egress must be locked down so inference can ONLY go via LiteLLM.
  if kubectl -n "$NAMESPACE" get networkpolicy 2>/dev/null | grep -qiE "deny.*egress|egress.*deny|default-deny"; then
    ok "  default-deny egress NetworkPolicy present (agents cannot bypass LiteLLM to the internet)"
  else
    bad "  no default-deny egress NetworkPolicy — agents could reach the internet directly, bypassing in-box inference"
    return 1
  fi

  # (d) THE REAL ROUND-TRIP (--inference-live). The probe above proves LiteLLM is
  # up and reachable, but NOT that a model call actually completes — and the
  # coopmux<->agent netpol breakage (bug ka-0000004409) sits exactly in that gap.
  # On a TRUE air-gapped box there is no upstream Anthropic to reach, so this is
  # opt-in: only when --inference-live is set (a live box with egress to the LLM
  # provider) do we POST a real /v1/messages and require classify_inference()==ok.
  if [ "$INFERENCE_LIVE" = 1 ]; then
    local model="${LITELLM_MODEL:-claude-haiku-4-5}"
    local key_secret="${LITELLM_KEY_SECRET:-${svc}-master-key}"
    local master_key
    master_key="$(kubectl -n "$NAMESPACE" get secret "$key_secret" \
                    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null)"
    if [ -z "$master_key" ]; then
      bad "  --inference-live: could not read LiteLLM master key from secret $key_secret (key 'token')"
      return 1
    fi
    # POST a minimal message from inside the cluster and classify the response.
    # We capture HTTP status and body separately (status appended on its own line)
    # so classify_inference can judge both. --max-time keeps a wedged call bounded.
    local raw status body
    raw="$(kubectl -n "$NAMESPACE" run tarboat-infer-rt-$$ --rm -i --restart=Never \
             --image=curlimages/curl:8.10.1 --command -- \
             sh -c "curl -sS -m 45 -w '\n%{http_code}' \
               -X POST '${probe_url}/v1/messages' \
               -H 'content-type: application/json' \
               -H 'x-litellm-api-key: ${master_key}' \
               -H 'anthropic-version: 2023-06-01' \
               -d '{\"model\":\"${model}\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with the single word PONG.\"}]}'" \
             2>/dev/null)"
    status="$(printf '%s' "$raw" | tail -n1 | tr -dc '0-9')"
    body="$(printf '%s' "$raw" | sed '$d')"
    local verdict
    verdict="$(classify_inference "${status:-000}" "$body")"
    if [ "$verdict" = ok ]; then
      ok "  REAL round-trip: POST /v1/messages -> HTTP $status, assistant message returned (model $model)"
    else
      bad "  REAL round-trip FAILED (verdict=$verdict, http=${status:-000}) — LiteLLM is up but a model call does not complete (coopmux<->agent netpol / OAuth-forward / model alias?)"
      return 1
    fi
  else
    info "  (real /v1/messages round-trip skipped — pass --inference-live on a box with LLM-provider egress to exercise it)"
  fi
  return 0
}

# ── Gate 5: login actually completes (the availability bar) ──────────────────
# acceptance-smoke proves routes are auth-GATED (302 -> Keycloak). It does NOT
# prove login is USABLE: the gasbox headline breakage (epic ka-0000006506) was
# `invalid_scope` -> Keycloak 403, a box that is reachable but cannot be logged
# into. This gate follows the full oauth2 redirect chain to the rendered Sign-in
# page and asserts classify_login()==ok (kc-form-login present, no invalid_scope).
gate_login() {
  if [ "$SKIP_LOGIN" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-login set: login completion unverified"
    info "  --skip-login: login completion not validated"; return 0
  fi
  # Resolve the apex (dashed-IP sslip host) so we can hit the beads-viewer route,
  # which forwardAuth-redirects through Keycloak. Prefer --apex; else derive from
  # --ip; else from the box's own public IP.
  local apex="$APEX"
  if [ -z "$apex" ]; then
    local ip4="$IP"
    [ -n "$ip4" ] || ip4="$(curl -s -m 3 http://169.254.169.254/latest/meta-data/public-ipv4 2>/dev/null \
                            || hostname -I 2>/dev/null | awk '{print $1}')"
    [ -n "$ip4" ] || { bad "  no --apex/--ip and could not self-detect IP for the login probe"; return 1; }
    apex="${ip4//./-}.sslip.io"
  fi
  local host="beads-viewer.${apex}"
  info "  following the oauth2 login chain via https://${host}/"
  # -L follows the forwardAuth -> Keycloak redirects to the rendered login page.
  # --resolve pins SNI->box so this works before/without public DNS; the box's
  # own IP is reachable from on-box. ssl trust is asserted by acceptance-smoke;
  # here we only need the final page body.
  local body
  body="$(curl -sSL -k -m 20 --resolve "${host}:443:${IP:-127.0.0.1}" "https://${host}/" 2>/dev/null)" \
    || body="$(curl -sSL -k -m 20 "https://${host}/" 2>/dev/null)"
  local verdict
  verdict="$(classify_login "$body")"
  if [ "$verdict" = ok ]; then
    ok "  login chain renders a usable Keycloak Sign-in page (no invalid_scope)"
    return 0
  fi
  bad "  login chain did NOT reach a usable Sign-in page (verdict=$verdict) — box reachable but login unusable (invalid_scope / realm misconfig?)"
  # ── failure-only diagnostic (unblocks H2 login root-cause, bug ka-0000003256) ──
  # We already returned above on a GREEN gate, so everything from here fires ONLY
  # on a red login gate — a strict no-op on a healthy box. classify_login() is
  # untouched, so tarboat-validate_test.sh stays green. Best-effort, on-box, and
  # dependency-free (curl/grep/sed/head/tr only). WHY it must live here: the
  # ephemeral box is torn down on sweep exit and agent kubectl is RBAC-denied
  # (ka-0000006777), so this on-box dump to the sweep log is the ONLY window the
  # next re-sweep crew gets into WHERE the oauth2-proxy->Keycloak chain lands.
  info "  ── login-failure diagnostic: oauth2 redirect chain + final-body markers ──"
  # (1) Full redirect chain. -D - dumps EVERY hop's response headers (status line
  # + Location) as -L follows them; -w appends the final http_code/url/hop-count;
  # -o /dev/null drops the body from this pass (snapshotted in (2)). Same
  # --resolve/fallback pair as the probe above so it works before/without DNS.
  local hdrs
  hdrs="$(curl -sSL -k -m 20 -o /dev/null -D - \
            -w 'FINAL http_code=%{http_code} url=%{url_effective} num_redirects=%{num_redirects}\n' \
            --resolve "${host}:443:${IP:-127.0.0.1}" "https://${host}/" 2>/dev/null)" \
    || hdrs="$(curl -sSL -k -m 20 -o /dev/null -D - \
            -w 'FINAL http_code=%{http_code} url=%{url_effective} num_redirects=%{num_redirects}\n' \
            "https://${host}/" 2>/dev/null)"
  # Keep only the hop path — each hop's status line + Location, plus the FINAL
  # summary — not the full header soup.
  printf '%s\n' "$hdrs" | grep -iE '^HTTP/|^location:|^FINAL ' | sed 's/^/      chain: /'
  # (2) Bounded snippet of the final rendered body (whitespace-squeezed, first
  # ~1200 bytes) so a Keycloak/oauth2-proxy error page is visible in the log
  # without dumping an unbounded HTML blob.
  info "  final-body snippet (bounded ~1200B, whitespace-squeezed):"
  printf '%s' "$body" | tr -s ' \t\r\n' ' ' | head -c 1200 | sed 's/^/      body: /'
  printf '\n'
  # (3) Grep the body for the KNOWN login-failure markers so the root cause is
  # greppable even if the snippet truncates before it: the Keycloak breakages
  # (invalid_scope/invalid_client/error_description/Realm does not exist/We are
  # sorry) AND the oauth2-proxy error.html message markers (#msgs / messages).
  local marker hit=0
  for marker in \
      'invalid_scope' 'invalid_client' 'error_description' \
      'Realm does not exist' 'We are sorry' \
      'id="msgs"' 'class="messages"' 'Authorization failed'; do
    if grep -qiF "$marker" <<<"$body"; then
      info "  marker HIT: $marker"; hit=1
    fi
  done
  [ "$hit" = 1 ] || info "  no known error marker matched (empty body / non-marker failure — see chain + snippet above)"
  return 1
}

# ── Gate 6: beads data present (the daemon-actually-works bar) ───────────────
# Pods Running is not enough — the beads-viewer is empty/broken if the
# daemon<->postgres path is down. This gate proves that path THREE ways, in the
# order that isolates the real failure mode (ka-0000005066):
#
#   1. The beads DAEMON pod (component=beads) is Ready. Its readiness probe is
#      GET /v1/health, which server-side runs store.Ping() (kbeads http.go
#      handleHealth) — so a Ready daemon proves the daemon actually reaches
#      postgres through the connstring/secret/NetworkPolicies. This is what a
#      raw `psql` in the postgres pod could NEVER show: it tests postgres, not
#      the daemon->postgres hop the viewer depends on.
#   2. The `beads` table is readable (SELECT count(*) succeeds).
#   3. Row-count policy is SEEDER-AWARE. The ~176 baseline beads on a live box
#      are written by the agent-controller's EnsureConfigs at startup — nothing
#      else seeds them. A Slack-less BYO box sets agents.enabled=false and runs
#      NO controller (component=agents), so its table is legitimately empty even
#      when the path is perfectly healthy. So: require >0 rows only when a seeder
#      is present; on a seeder-less box a readable-but-empty table PASSES.
gate_beads_data() {
  if [ "$SKIP_BEADS" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-beads set: beads data presence unverified"
    info "  --skip-beads: beads data presence not validated"; return 0
  fi
  local db="${BEADS_DB:-beads}"
  local pgpod
  pgpod="$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=postgres \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  if [ -z "$pgpod" ]; then
    bad "  no postgres pod (component=postgres) in ns/$NAMESPACE — cannot count beads"
    return 1
  fi

  # (1) Is the beads daemon Ready? Its /v1/health readiness probe pings the DB,
  # so Ready ⇒ the daemon<->postgres path (secret + connstring + NetworkPolicy)
  # is proven up. Absent/NotReady ⇒ that path is the suspect, not seeding.
  local ready="not-ready"
  local ready_col
  ready_col="$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=beads \
                -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)"
  [ "$ready_col" = "True" ] && ready="ready"

  # (3) Does the box run a bead-SEEDER (the agent-controller)? Its Deployment
  # renders only when agents.enabled=true; on a Slack-less box it is absent and
  # an empty `beads` table is expected, not a regression.
  local seeder="no-seeder"
  local seeder_pods
  seeder_pods="$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=agents \
                  -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)"
  [ -n "$seeder_pods" ] && seeder="seeder"

  # (2) Count rows in the (readable?) beads table, via psql in the postgres pod.
  local out count
  out="$(kubectl -n "$NAMESPACE" exec "$pgpod" -c postgres -- \
          psql -U postgres -d "$db" -tAc 'SELECT count(*) FROM beads;' 2>/dev/null)"
  count="$(classify_beads_count "$out")"

  local verdict
  verdict="$(classify_beads_data "$count" "$ready" "$seeder")"
  case "$verdict" in
    empty-no-seeder)
      ok "  beads table readable but empty — daemon<->postgres path healthy (daemon Ready); no controller (component=agents) on this box to seed beads, so an empty table is EXPECTED"
      return 0;;
    path-broken)
      bad "  beads table empty/unreadable AND beads daemon not Ready — daemon<->postgres path down (connstring/secret gardenio-postgres-password/NetworkPolicy?)"
      return 1;;
    unseeded)
      bad "  beads table empty (verdict=$count) but a seeder (component=agents) IS present — controller ran but wrote no beads (EnsureConfigs regression?)"
      return 1;;
    no-count)
      bad "  beads count query returned nothing (daemon Ready) — schema/DB anomaly (no 'beads' relation in db/$db?)"
      return 1;;
    *)
      ok "  beads table has data ($verdict rows) — daemon<->postgres path healthy"
      return 0;;
  esac
}

# ─────────────────────────────────────────────────────────────────────────────
# PERSISTENCE & STATE gates (ka-0000004199). Posture-green + a Ready daemon prove
# the daemon<->postgres PATH is up (gate_beads_data). These three additionally
# prove the DB actually PERSISTS a write, that the optional LiteLLM Redis cache
# serves state, and that the on-box backup path yields a RESTORABLE snapshot —
# the "does the state layer actually work and survive?" bars a posture check
# (pods Running, /health 200) sails straight through. Each writes into the LIVE
# box via kubectl exec (no external deps) and factors its verdict into the pure
# classify_* functions above so the pass/fail logic is unit-tested without a box.
# ─────────────────────────────────────────────────────────────────────────────

# gate_db_persistence — write a sentinel row into the beads postgres, read it
# back (proves the RW path end-to-end), AND classify how the data dir is backed:
# a PVC-backed StatefulSet survives a pod restart (durable, release-grade); an
# emptyDir-backed one round-trips now but loses data on restart (the ephemeral
# test-namespace shape, ka-0000000711). We DERIVE the backing from the rendered
# StatefulSet's `data` volume rather than restart-bouncing postgres — a real
# restart would blow the whole validation run's DB out from under the other
# gates. The sentinel lives in its own table and is dropped after read-back, so
# it never pollutes the `beads` count gate_beads_data asserts on.
gate_db_persistence() {
  if [ "$SKIP_PERSISTENCE" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-persistence set: DB persistence unverified"
    info "  --skip-persistence: DB persistence not validated"; return 0
  fi
  local db="${BEADS_DB:-beads}"
  local pgpod
  pgpod="$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=postgres \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  if [ -z "$pgpod" ]; then
    bad "  no postgres pod (component=postgres) in ns/$NAMESPACE — cannot validate persistence"
    return 1
  fi

  # (1) Write→read a sentinel through the daemon's DB. A unique token proves the
  # value came from OUR write, not a stale row. Kept in its own table; dropped
  # after read so gate_beads_data's `beads` count is untouched.
  local token="tarboat-persist-${STARTED_AT}-$$"
  kubectl -n "$NAMESPACE" exec "$pgpod" -c postgres -- \
    psql -U postgres -d "$db" -tAc \
    "CREATE TABLE IF NOT EXISTS tarboat_persist_probe (token text); DELETE FROM tarboat_persist_probe; INSERT INTO tarboat_persist_probe VALUES ('$token');" \
    >/dev/null 2>&1
  local readback
  readback="$(kubectl -n "$NAMESPACE" exec "$pgpod" -c postgres -- \
    psql -U postgres -d "$db" -tAc \
    "SELECT token FROM tarboat_persist_probe WHERE token = '$token';" 2>/dev/null | tr -d '[:space:]')"
  kubectl -n "$NAMESPACE" exec "$pgpod" -c postgres -- \
    psql -U postgres -d "$db" -tAc 'DROP TABLE IF EXISTS tarboat_persist_probe;' >/dev/null 2>&1
  [ "$readback" = "$token" ] || readback=""   # only OUR exact token counts

  # (2) How is the data dir backed? A `persistentVolumeClaim` on the pod's `data`
  # volume (or a bound PVC from the volumeClaimTemplate) ⇒ durable; an `emptyDir`
  # data volume ⇒ ephemeral. Read the live pod spec, not the chart, so we assert
  # on what is actually RUNNING on the box.
  local voljson backing="unknown"
  voljson="$(kubectl -n "$NAMESPACE" get pod "$pgpod" \
    -o jsonpath='{range .spec.volumes[?(@.name=="data")]}{.persistentVolumeClaim.claimName}|{.emptyDir}{end}' 2>/dev/null)"
  case "$voljson" in
    "|"|"") backing="unknown";;                       # neither field present
    "|"*)   backing="emptydir";;                      # emptyDir set, no PVC
    *"|")   backing="pvc";;                           # PVC claimName set
    *"|"*)  backing="pvc";;
  esac

  local verdict; verdict="$(classify_persistence "$readback" "$backing")"
  case "$verdict" in
    ok)
      ok "  sentinel round-tripped through postgres AND data dir is PVC-backed (durable across restart)"
      return 0;;
    write-failed)
      bad "  sentinel write did NOT read back from postgres — DB write path broken (RW mount / disk / connstring?)"
      return 1;;
    ephemeral)
      # emptyDir postgres survives the round-trip but not a pod restart. That is
      # EXPECTED on a throwaway ephemeral test namespace, but a real regression on
      # a durable product box. Box-shape decides: a byo/product box must be PVC-
      # backed; only treat emptyDir as acceptable when persistence is knowingly off.
      if [ "${POSTGRES_EPHEMERAL_OK:-0}" = 1 ]; then
        info "  postgres data dir is an emptyDir (POSTGRES_EPHEMERAL_OK=1) — round-trip OK, durability NOT asserted"
        return 0
      fi
      bad "  sentinel round-tripped but postgres data dir is an emptyDir — NOT durable across pod restart (persistence.enabled=false?). Set POSTGRES_EPHEMERAL_OK=1 only for a throwaway test namespace."
      return 1;;
    *)
      bad "  could not classify postgres data-dir backing (verdict=$verdict) — cannot assert durability"
      return 1;;
  esac
}

# gate_cache_state — SET/GET round-trip through the OPTIONAL in-chart LiteLLM
# Redis (component=litellm-redis). It renders only when litellm.redis.deploy.
# enabled; a box may use an external Redis or none at all, so an ABSENT in-chart
# Redis is a clean PASS (cache-state N/A). Only a PRESENT-but-not-serving cache
# fails. Proves router coordination state actually reads/writes, which a pod-
# Running + TCP-open check would miss.
gate_cache_state() {
  if [ "$SKIP_CACHE" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-cache set: cache state unverified"
    info "  --skip-cache: cache state not validated"; return 0
  fi
  local rpod present="" roundtrip=""
  rpod="$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=litellm-redis \
           -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  if [ -n "$rpod" ]; then
    present="present"
    local token="tarboat-cache-$$"
    local out
    out="$(kubectl -n "$NAMESPACE" exec "$rpod" -- \
      sh -c "redis-cli SET tarboat_cache_probe '$token' >/dev/null 2>&1; redis-cli GET tarboat_cache_probe; redis-cli DEL tarboat_cache_probe >/dev/null 2>&1" 2>/dev/null | tr -d '[:space:]')"
    [ "$out" = "$token" ] && roundtrip="ok" || roundtrip="bad"
  fi

  local verdict; verdict="$(classify_cache "$present" "$roundtrip")"
  case "$verdict" in
    ok)
      ok "  LiteLLM Redis SET/GET round-trip returned the written value — cache serving state"
      return 0;;
    absent)
      ok "  no in-chart LiteLLM Redis (component=litellm-redis) on this box — cache-state N/A (external/none), bar met"
      return 0;;
    roundtrip-failed)
      bad "  LiteLLM Redis pod present but SET/GET did NOT return the written value — cache up but not serving reads/writes"
      return 1;;
  esac
}

# gate_snapshot_integrity — pipe a LIVE `pg_dump -Fc` of the beads DB into
# `pg_restore -l` and require a non-empty archive table-of-contents. This is the
# exact mechanism the postgres.backup CronJob uses (custom-format dump), so a
# green gate proves the on-box backup path yields a RESTORABLE archive — not
# merely that a dump file was written. Nothing is restored (read-only listing),
# so the live DB is untouched.
gate_snapshot_integrity() {
  if [ "$SKIP_SNAPSHOT" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-snapshot set: snapshot integrity unverified"
    info "  --skip-snapshot: snapshot integrity not validated"; return 0
  fi
  local db="${BEADS_DB:-beads}"
  local pgpod
  pgpod="$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=postgres \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  if [ -z "$pgpod" ]; then
    bad "  no postgres pod (component=postgres) in ns/$NAMESPACE — cannot validate snapshot integrity"
    return 1
  fi
  # pg_dump -Fc to a stream, pg_restore -l reads it back as a TOC listing. Both
  # binaries ship in the postgres image (the backup CronJob relies on the same).
  # Count TOC entry lines: pg_restore -l prefixes real entries with a number.
  local toc
  toc="$(kubectl -n "$NAMESPACE" exec "$pgpod" -c postgres -- \
    sh -c "pg_dump -U postgres -Fc -d '$db' 2>/dev/null | pg_restore -l 2>/dev/null | grep -c '^[0-9;]'" 2>/dev/null | tr -dc '0-9')"
  local verdict; verdict="$(classify_snapshot "$toc")"
  case "$verdict" in
    empty)
      bad "  pg_dump -Fc | pg_restore -l produced an empty/unreadable TOC — snapshot path not producing a restorable archive"
      return 1;;
    *)
      ok "  pg_dump -Fc archive enumerates $verdict TOC entries via pg_restore -l — restorable snapshot"
      return 0;;
  esac
}

# ── Helm deployment gates (ka-0000003227) ────────────────────────────────────
# The box installs the platform as a Helm release; these three exercise the helm
# release machinery the state/posture gates never touch: the shipped chart lints,
# the live release is upgradeable, and its history supports a rollback. All three
# are read-only / non-mutating (helm lint, --dry-run, helm history — nothing is
# applied or rolled back) so they never disturb the running box the other gates
# depend on.

# gate_chart_lint — `helm lint` the SHIPPED chart on the box. Fully offline (no
# cluster needed): proves the chart the box would (re)deploy from is structurally
# valid. Locates the chart under the staged repo, falling back to $CHART_DIR.
gate_chart_lint() {
  if [ "$SKIP_CHART_LINT" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-chart-lint set: chart lint unverified"
    info "  --skip-chart-lint: chart lint not validated"; return 0
  fi
  local chart="${CHART_DIR:-}"
  if [ -z "$chart" ]; then
    for c in "$REPO_DIR/gardenio/helm/gardenio" "$TARBOAT/repo/gardenio/helm/gardenio"; do
      [ -f "$c/Chart.yaml" ] && { chart="$c"; break; }
    done
  fi
  if [ -z "$chart" ] || [ ! -f "$chart/Chart.yaml" ]; then
    bad "  no gardenio chart (Chart.yaml) found under $REPO_DIR/gardenio/helm/gardenio — cannot lint (set CHART_DIR)"
    return 1
  fi
  if ! command -v helm >/dev/null 2>&1; then
    bad "  helm binary not on PATH — cannot lint the shipped chart"
    return 1
  fi
  # helm lint prints a "N chart(s) failed" summary line; parse the count out of it.
  local out failed
  out="$(helm lint "$chart" 2>&1)"
  failed="$(printf '%s\n' "$out" | sed -n 's/.*, \([0-9][0-9]*\) chart(s) failed.*/\1/p' | tail -n1)"
  local verdict; verdict="$(classify_chart_lint "$failed")"
  case "$verdict" in
    ok)
      ok "  helm lint clean on shipped chart $chart (0 charts failed)"
      return 0;;
    failed)
      bad "  helm lint reported $failed failed chart(s) on $chart: $(printf '%s' "$out" | grep -i '\[ERROR\]' | head -n3 | tr '\n' ';')"
      return 1;;
    *)
      bad "  helm lint produced no parseable 'N chart(s) failed' summary for $chart — lint did not yield a verdict"
      return 1;;
  esac
}

# gate_helm_upgrade — the LIVE release must be "deployed" AND a
# `helm upgrade --dry-run --reuse-values` must render against the live cluster.
# NON-MUTATING: --dry-run applies nothing. Proves the running release can still be
# re-templated (a chart that installed once but drifted out of upgradeability is a
# real regression the pods-Running posture never catches).
gate_helm_upgrade() {
  if [ "$SKIP_HELM_UPGRADE" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-helm-upgrade set: release upgradeability unverified"
    info "  --skip-helm-upgrade: helm upgrade not validated"; return 0
  fi
  if ! command -v helm >/dev/null 2>&1; then
    bad "  helm binary not on PATH — cannot validate release upgradeability"
    return 1
  fi
  # DISCOVER the live release rather than assume RELEASE@NAMESPACE: on a real BYO
  # box the workloads were healthy but `helm status gardenio -n gardenio` was empty
  # because helm recorded the release under a different name/namespace, so the
  # gate false-failed no-release (ka-0000000669). pick_release scans `helm list -A`
  # and prefers the assumed pair, else the sole gardenio-chart release, else the
  # sole release; it feeds the SAME name/namespace to status + dry-run below.
  local rel="$RELEASE" relns="$NAMESPACE" chosen
  chosen="$(helm list -A -o json 2>/dev/null | helm_list_lines | pick_release "$RELEASE" "$NAMESPACE")" \
    && { rel="${chosen%% *}"; relns="$(printf '%s' "$chosen" | awk '{print $2}')"; }
  local status
  status="$(helm -n "$relns" status "$rel" -o json 2>/dev/null \
              | sed -n 's/.*"status"[[:space:]]*:[[:space:]]*"\([a-z-]*\)".*/\1/p' | head -n1)"
  [ -n "$status" ] || status="$(helm -n "$relns" list --filter "^${rel}\$" 2>/dev/null | awk 'NR==2{print $8}')"
  local dryrun=""
  if [ "$status" = "deployed" ]; then
    if helm -n "$relns" upgrade "$rel" "${CHART_DIR:-$REPO_DIR/gardenio/helm/gardenio}" \
         --reuse-values --dry-run >/dev/null 2>&1; then
      dryrun="ok"
    else
      dryrun="bad"
    fi
  fi
  local verdict; verdict="$(classify_helm_upgrade "$status" "$dryrun")"
  case "$verdict" in
    ok)
      ok "  release $rel (ns/$relns) is deployed AND helm upgrade --dry-run --reuse-values rendered — upgradeable"
      return 0;;
    no-release)
      bad "  no gardenio helm release discovered (looked for '$RELEASE' in ns/$NAMESPACE, then any gardenio-chart release via helm list -A) — nothing to upgrade (chart never installed?)"
      return 1;;
    not-deployed)
      bad "  helm release '$rel' (ns/$relns) status is '$status' (not 'deployed') — release not in an upgradeable state"
      return 1;;
    dryrun-failed)
      bad "  helm release '$rel' (ns/$relns) is deployed but a --dry-run upgrade did NOT render — chart no longer templates against the box"
      return 1;;
  esac
}

# ─────────────────────────────────────────────────────────────────────────────
# NETWORKING gates (ka-0000006092). Posture-green (routes/Services/pods exist)
# does NOT prove a request actually reaches a backend. These three assert the
# data path itself: every IngressRoute resolves to a real Service, each critical
# Service load-balances to a READY endpoint, and in-cluster CoreDNS resolves a
# Service FQDN. Each reads the LIVE box via kubectl (read-only) and factors its
# verdict into the pure classify_* functions above so the pass/fail logic is
# unit-tested without a box.
# ─────────────────────────────────────────────────────────────────────────────

# gate_routing — every Traefik IngressRoute service reference on the box must
# name a Service that exists in the namespace. A dangling ref means Traefik loaded
# the route but it black-holes to a 404 for that Host. Read-only: lists
# IngressRoutes + Services and cross-checks in shell (no jq — air-gap-safe). When
# the box runs no Traefik IngressRoutes (AVA/ALB edge owns routing, or the CRD is
# absent) there is nothing to validate → classify_routing returns "no-routes" and
# the gate passes rather than false-failing.
gate_routing() {
  if [ "$SKIP_ROUTING" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-routing set: IngressRoute→Service resolution unverified"
    info "  --skip-routing: routing not validated"; return 0
  fi
  # IngressRoute is a Traefik CRD; a box without Traefik has no such resource.
  if ! kubectl get crd ingressroutes.traefik.io >/dev/null 2>&1 \
     && ! kubectl get crd ingressroutes.traefik.containo.us >/dev/null 2>&1; then
    info "  no Traefik IngressRoute CRD on this box — routing is edge-owned (AVA/ALB); gate N/A"
    return 0   # classify_routing no-routes shape; nothing in-cluster to validate
  fi
  # Set of Services that exist in the namespace (one name per line).
  local svcs
  svcs="$(kubectl -n "$NAMESPACE" get svc -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)"
  # Every IngressRoute's service refs: "<route-name> <service-name>" per line.
  # spec.routes[].services[].name is the target Service each route forwards to.
  local refs
  refs="$(kubectl -n "$NAMESPACE" get ingressroute -o jsonpath='{range .items[*]}{range .spec.routes[*]}{range .services[*]}{$.metadata.name}{" "}{.name}{"\n"}{end}{end}{end}' 2>/dev/null)"
  local checked=0 dangling=0 rname sname line
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    rname="${line%% *}"; sname="${line#* }"
    [ -n "$sname" ] || continue
    checked=$((checked + 1))
    if ! grep -qxF "$sname" <<<"$svcs"; then
      dangling=$((dangling + 1))
      bad "  IngressRoute $rname references Service '$sname' which does not exist in ns/$NAMESPACE (Host 404s)"
    fi
  done <<EOF
$refs
EOF
  local verdict; verdict="$(classify_routing "$dangling" "$checked")"
  case "$verdict" in
    ok)
      ok "  all $checked IngressRoute service reference(s) resolve to existing Services — routing wired"
      return 0;;
    no-routes)
      ok "  no IngressRoute service references to validate (edge-owned routing / IngressRoutes disabled) — routing gate N/A"
      return 0;;
    *)
      bad "  routing FAILED (verdict=$verdict): $dangling of $checked IngressRoute ref(s) point at a missing Service — those Hosts 404"
      return 1;;
  esac
}

# gate_helm_rollback — the release history must be a coherent, rollback-safe
# ledger. READ-ONLY: `helm history` lists revisions; nothing is rolled back (a
# real rollback would disrupt the box the other gates run against). A single
# deployed revision (freshly installed) passes — machinery present, no prior
# target yet; a stuck/mid-operation latest revision fails.
gate_helm_rollback() {
  if [ "$SKIP_HELM_ROLLBACK" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-helm-rollback set: rollback readiness unverified"
    info "  --skip-helm-rollback: helm rollback readiness not validated"; return 0
  fi
  if ! command -v helm >/dev/null 2>&1; then
    bad "  helm binary not on PATH — cannot validate rollback readiness"
    return 1
  fi
  # DISCOVER the live release the same way gate_helm_upgrade does — the assumed
  # RELEASE@NAMESPACE pair was empty on a real BYO box whose workloads were
  # healthy, false-failing no-history (ka-0000000669).
  local rel="$RELEASE" relns="$NAMESPACE" chosen
  chosen="$(helm list -A -o json 2>/dev/null | helm_list_lines | pick_release "$RELEASE" "$NAMESPACE")" \
    && { rel="${chosen%% *}"; relns="$(printf '%s' "$chosen" | awk '{print $2}')"; }
  # Count revisions and read the latest revision's status from helm history.
  # `helm history -o json` yields one object per revision; parse without jq.
  local hist n status
  hist="$(helm -n "$relns" history "$rel" -o json 2>/dev/null)"
  n="$(printf '%s' "$hist" | grep -o '"revision"' | wc -l | tr -dc '0-9')"
  # The latest revision's status is the last "status" field in the ordered array.
  status="$(printf '%s' "$hist" | grep -o '"status"[[:space:]]*:[[:space:]]*"[a-z-]*"' \
             | sed -n 's/.*"\([a-z-]*\)"$/\1/p' | tail -n1)"
  local verdict; verdict="$(classify_helm_rollback "$n" "$status")"
  case "$verdict" in
    rollback-ready)
      ok "  release $rel (ns/$relns) has $n revisions, latest deployed — a prior revision exists to roll back to"
      return 0;;
    single-revision)
      ok "  release $rel (ns/$relns) has 1 deployed revision (freshly installed) — rollback machinery present, no prior target yet"
      return 0;;
    no-history)
      bad "  no revision ledger for a gardenio release (looked for '$RELEASE' in ns/$NAMESPACE, then any gardenio-chart release via helm list -A) — cannot roll back"
      return 1;;
    not-deployed)
      bad "  helm release '$rel' (ns/$relns) latest revision status is '$status' (not 'deployed') — not in a rollback-safe state"
      return 1;;
  esac
}

# gate_lb — each critical in-box Service must have >=1 READY endpoint. A Service
# with a ClusterIP but zero ready backing pods exists + resolves + accepts a
# connection to kube-proxy, then 503s because there is nothing to forward to — a
# `get svc`/Service-exists check is green straight through it. We read the
# EndpointSlices (or legacy Endpoints) for each Service and count READY addresses.
# ABSENT Services are skipped (optional component / different box shape) — only a
# present-but-endpointless Service fails.
gate_lb() {
  if [ "$SKIP_LB" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-lb set: Service load-balancing (ready endpoints) unverified"
    info "  --skip-lb: load-balancing not validated"; return 0
  fi
  local fullname="${RELEASE}"
  # The core request-path Services a working box must load-balance to. Named by
  # the chart's fullname-prefixed convention; absent ones are skipped, so a box
  # that legitimately omits a component (e.g. no in-box litellm) is not failed.
  local critical="${LB_SERVICES:-${fullname}-beads ${fullname}-beads-viewer ${fullname}-forwardauth ${fullname}-litellm}"
  local checked=0 failed=0 svc present ready verdict
  for svc in $critical; do
    if ! kubectl -n "$NAMESPACE" get svc "$svc" >/dev/null 2>&1; then
      info "  Service $svc absent — skipped (optional component / box shape)"
      continue
    fi
    present="present"
    # Count READY endpoint addresses. EndpointSlices are the modern source; a
    # slice lists endpoints[].conditions.ready + endpoints[].addresses. Sum the
    # ready addresses across all slices for this Service (label kubernetes.io/
    # service-name). Fall back to legacy Endpoints .subsets[].addresses if no
    # slice is found (older clusters).
    ready="$(kubectl -n "$NAMESPACE" get endpointslice -l "kubernetes.io/service-name=$svc" \
      -o jsonpath='{range .items[*]}{range .endpoints[?(@.conditions.ready==true)]}{.addresses[0]}{"\n"}{end}{end}' 2>/dev/null | grep -c .)"
    if [ "${ready:-0}" -eq 0 ] 2>/dev/null; then
      ready="$(kubectl -n "$NAMESPACE" get endpoints "$svc" \
        -o jsonpath='{range .subsets[*]}{range .addresses[*]}{.ip}{"\n"}{end}{end}' 2>/dev/null | grep -c .)"
    fi
    checked=$((checked + 1))
    verdict="$(classify_lb "$present" "$ready")"
    if [ "$verdict" = ok ]; then
      ok "  Service $svc → $ready ready endpoint(s) — load-balancing to a live backend"
    else
      failed=$((failed + 1))
      bad "  Service $svc present but has 0 ready endpoints (verdict=$verdict) — black-holes requests (503); backing pods NotReady/absent"
    fi
  done
  if [ "$checked" -eq 0 ]; then
    bad "  none of the critical Services ($critical) exist in ns/$NAMESPACE — box has no request-path Services"
    return 1
  fi
  [ "$failed" -eq 0 ] || return 1
  return 0
}

# gate_dns — in-cluster CoreDNS must resolve a Service's cluster FQDN to its
# ClusterIP. Everything in the box talks by DNS name, so a broken CoreDNS is a
# whole-box outage that a pods-Running check never surfaces. We read a target
# Service's ClusterIP from the API, then resolve <svc>.<ns>.svc.cluster.local
# from a throwaway in-cluster pod (getent/nslookup) and compare. A headless
# Service (ClusterIP None/empty) still validates resolution — any answer counts.
gate_dns() {
  if [ "$SKIP_DNS" = 1 ]; then
    GATE_SKIPPED=1; LAST_DIAG="--skip-dns set: in-cluster DNS resolution unverified"
    info "  --skip-dns: DNS not validated"; return 0
  fi
  # Pick a Service that always exists on a working box: the beads daemon. Fall
  # back to kubernetes.default if the chart Service name differs.
  local target="${DNS_SERVICE:-${RELEASE}-beads}" ns="$NAMESPACE"
  if ! kubectl -n "$ns" get svc "$target" >/dev/null 2>&1; then
    target="kubernetes"; ns="default"
  fi
  local expected
  expected="$(kubectl -n "$ns" get svc "$target" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
  case "$expected" in None|"") expected="";; esac   # headless → any answer counts
  local fqdn="${target}.${ns}.svc.cluster.local"
  info "  resolving $fqdn from inside the cluster (expected ClusterIP: ${expected:-<headless/any>})"
  # Resolve from a throwaway pod on the cluster network. busybox nslookup prints
  # a header block (Server:/Address: — the DNS server's own address) then the
  # answer section ("Name:" followed by one or more "Address" lines). We run the
  # raw nslookup in-pod and parse the ANSWER address host-side with
  # parse_nslookup_answer — NOT a `tr -dc '0-9.:'` squash, which strips newlines
  # and concatenates any stray numeric token on stdout (e.g. the probe pod name's
  # $$ PID, or kubectl run's status echo) onto the IP, manufacturing a phantom
  # "mismatch" on a healthy box (ka-0000000669).
  local resolved
  resolved="$(kubectl -n "$ns" run tarboat-dns-probe-$$ --rm -i --restart=Never \
    --image=busybox:1.36 --command -- \
    nslookup "$fqdn" 2>/dev/null \
    | parse_nslookup_answer )"
  local verdict; verdict="$(classify_dns "$resolved" "$expected")"
  case "$verdict" in
    ok)
      ok "  CoreDNS resolved $fqdn → ${resolved} (matches ClusterIP) — in-cluster DNS healthy"
      return 0;;
    mismatch)
      bad "  CoreDNS resolved $fqdn → ${resolved} but the Service ClusterIP is ${expected} — DNS returns the WRONG address (stale/hijacked record)"
      return 1;;
    unresolved)
      bad "  CoreDNS did NOT resolve $fqdn (NXDOMAIN/SERVFAIL/CoreDNS unreachable) — in-cluster DNS is down, every by-name hop fails"
      return 1;;
  esac
}

# emit_results <exit_code> — write the machine-readable per-gate document to
# --results-json (file or "-" for stdout). Dependency-free: no jq/python needed
# on an air-gapped box. Called from a trap so the doc lands even on early exit.
RESULTS_EMITTED=0
STARTED_AT="$(date -u +%FT%TZ 2>/dev/null || echo unknown)"
emit_results() {
  [ -n "$RESULTS_JSON" ] || return 0
  [ "$RESULTS_EMITTED" = 0 ] || return 0
  RESULTS_EMITTED=1
  local code="${1:-1}"
  local finished_at; finished_at="$(date -u +%FT%TZ 2>/dev/null || echo unknown)"
  # overall=pass only when the run completed cleanly (exit 0), every gate ran,
  # and none failed — a precondition abort (exit 2) or early death is NOT a pass.
  local overall="fail"
  if [ "$code" = 0 ] && [ "$GATES_FAILED" -eq 0 ] && [ "$GATES_RUN" -gt 0 ]; then overall="pass"; fi
  # release-grade ONLY when overall pass AND nothing was skipped.
  local release_grade="false"
  [ "$overall" = pass ] && [ "$GATES_SKIPPED" -eq 0 ] && release_grade="true"
  local calver="$CALVER"
  [ -n "$calver" ] || { [ -s "$TARBOAT/CALVER" ] && calver="$(cat "$TARBOAT/CALVER" 2>/dev/null)"; }
  local gates_joined=""; local g
  for g in "${GATE_JSON[@]:-}"; do
    [ -n "$g" ] || continue
    [ -n "$gates_joined" ] && gates_joined="$gates_joined,"
    gates_joined="$gates_joined$g"
  done
  local doc
  doc="$(printf '{"schema":"tarboat-validate/v1","tarboat":%s,"namespace":%s,"release":%s,"calver":%s,"ip":%s,"started_at":%s,"finished_at":%s,"gates_run":%d,"gates_passed":%d,"gates_failed":%d,"gates_skipped":%d,"overall":%s,"release_grade":%s,"exit_code":%d,"gates":[%s]}\n' \
    "$(json_str "$TARBOAT")" "$(json_str "$NAMESPACE")" "$(json_str "$RELEASE")" \
    "$(json_str "$calver")" "$(json_str "$IP")" "$(json_str "$STARTED_AT")" "$(json_str "$finished_at")" \
    "$GATES_RUN" "$GATES_PASSED" "$GATES_FAILED" "$GATES_SKIPPED" \
    "$(json_str "$overall")" "$release_grade" "$code" "$gates_joined")"
  if [ "$RESULTS_JSON" = "-" ]; then
    printf '%s' "$doc"
  else
    if printf '%s' "$doc" > "$RESULTS_JSON" 2>/dev/null; then
      info "results written to $RESULTS_JSON"
    else
      bad "could not write results to $RESULTS_JSON (printing to stderr)"; printf '%s' "$doc" >&2
    fi
  fi
}
# Emit the results doc no matter how we leave (a precondition exit 2, a failed
# gate, or success) so CI/cron + the E2 bug filer always get a verdict.
trap 'emit_results "$?"' EXIT

[ "$(id -u)" = 0 ] || { echo "must run as root (sudo) — bootstrap + on-box probes need it" >&2; exit 2; }

echo "=== Tarboat from-scratch validation — box-shape=$BOX_SHAPE tarboat=$TARBOAT repo=$REPO_DIR ns=$NAMESPACE ==="
GATE_KEY="staging";    gate "air-gap staging + integrity"   gate_staging
GATE_KEY="bootstrap";  gate "bootstrap (true air-gap)"      gate_bootstrap
GATE_KEY="acceptance"; gate "acceptance smoke (posture)"    gate_acceptance
GATE_KEY="inference";  gate "inference round-trip (in-box)" gate_inference
GATE_KEY="login";      gate "login completes (usable)"      gate_login
GATE_KEY="beads_data"; gate "beads data present"            gate_beads_data
GATE_KEY="db_persistence"; gate "DB persistence (write→read + durable backing)" gate_db_persistence
GATE_KEY="cache_state";    gate "cache state (LiteLLM Redis round-trip)"        gate_cache_state
GATE_KEY="snapshot_integrity"; gate "snapshot integrity (pg_dump→pg_restore -l)" gate_snapshot_integrity
GATE_KEY="chart_lint";     gate "chart lint (shipped gardenio chart)"            gate_chart_lint
GATE_KEY="helm_upgrade";   gate "helm upgrade (live release --dry-run)"         gate_helm_upgrade
GATE_KEY="helm_rollback";  gate "helm rollback (revision ledger readiness)"     gate_helm_rollback
GATE_KEY="routing";      gate "routing (IngressRoute→Service resolves)"       gate_routing
GATE_KEY="load_balancing"; gate "load balancing (Service ready endpoints)"   gate_lb
GATE_KEY="dns";          gate "DNS (in-cluster CoreDNS resolves Service FQDN)" gate_dns

echo "----"
if [ "$GATES_FAILED" -eq 0 ]; then
  ok "ALL ${GATES_RUN} GATES PASSED — from-scratch inference-driven install validated"
  if [ "$GATES_SKIPPED" -gt 0 ]; then
    info "NOTE: ${GATES_SKIPPED} gate(s) SKIPPED (--skip-*) — green but NOT release-grade. Re-run without --skip-* (and with --inference-live on a box with LLM egress) for a release-grade pass."
  fi
  exit 0
fi
bad "${GATES_FAILED}/${GATES_RUN} GATES FAILED — NOT ship-able"
exit 1
