#!/usr/bin/env bash
# test-gb-whoami.sh — E2E gate for `gb whoami`: the read-only capability
# introspection block that answers "what can I actually do?" from any pod or
# laptop (AWS identity + assumable targets, the K8s ServiceAccount + the auth
# can-i probes that matter, the GitLab user + per-repo access level, and which
# Secrets-Manager prefixes are readable).
#
# Bead ka-0000001750 (parent ka-0000000958). Guards the feature merged by
# MR for ka-0000003630 (`feat(gardenio/gb): add gb whoami`, SHA f827c849a):
# 998 lines of net-new AWS/K8s/GitLab/secrets probing runtime code that shipped
# with UNIT tests only.
#
# THE GAP THIS CLOSES: `grep -rI whoami gardenio/tests/` returned ZERO hits
# repo-wide before this module. The unit tests (whoami_test.go) cover the pure
# parsers (parseKubectlWhoami, gitlabAccessLevelName, secretPrefixOf, …) and the
# renderer over hand-built structs — but NOTHING exercised the real `gb whoami`
# binary driving its four external probes and assembling the report end-to-end.
# A regression there is high-blast-radius and quiet: `gb whoami` is the
# agent-facing self-diagnostic the standing advice ka-0000000752 ("attempt the
# privileged action, read the REAL error, then escalate") depends on, and it is
# load-bearing for the boot-time capability block in agent prompts. Wrong output
# here manufactures exactly the false-premise escalations that advice exists to
# prevent — with no CI signal, because every probe section is fail-open and the
# command never exits non-zero for a probe failure.
#
# WHAT THIS PROVES — the REAL `gb whoami` binary, not a reimplementation:
# the four external tools it shells out to (aws / kubectl / glab, plus git for
# repo discovery) are shadowed on PATH by deterministic fixture stubs that emit
# KNOWN identities, and the assertions key on the report the real binary builds
# from them. Legs:
#   1. READ-ONLY DEFAULT (headline): `gb whoami --json` surfaces the exact
#      fixture AWS ARN + account, the parsed K8s ServiceAccount + namespace, the
#      GitLab user, and the aggregated Secrets-Manager prefixes — SHAPE plus
#      specific known-true values, never a brittle full-output golden.
#   2. HUMAN RENDER: the default (non-JSON) block carries every section header
#      and the same derived values, ✓/✗ can-i marks included.
#   3. can-i TWO-PATH truth table: the K8s ServiceAccount auth path is probed
#      INDEPENDENTLY of the AWS identity (yes→allowed, no→denied) — the exact
#      miss (ka-0000003844) that motivated the feature, where an agent probed
#      only AWS and wrongly concluded kubectl was unavailable.
#   4. DERIVATIONS: access_level 30 → "Developer" (via group), and a set of
#      secret names → the correct per-prefix aggregation.
#   5. --jq / --json parity: `gb whoami --jq '.aws.caller_identity.Arn'` returns
#      the single field, matching the JSON body (the machine / prerun path).
#   6. FAIL-OPEN: a DENIED aws + a 401 glab + an absent kubectl each annotate
#      only their own section; `gb whoami` still exits 0 and the healthy
#      sections still render (the independence + never-fail contract).
#   7. OFFLINE / NO-DAEMON: with BEADS_HTTP_ADDR pointed at a dead port and no
#      token, `gb whoami` still runs (it overrides the root PersistentPreRunE so
#      capability introspection works even when the daemon is down — mirrors
#      `gb version`).
#   8. --probe (HERMETIC): with the aws stub implementing sts:AssumeRole
#      deterministically, `--probe` promotes assumable targets to allowed /
#      denied. This exercises verifyAssumableTarget WITHOUT minting any real STS
#      credential — because the real `aws` is shadowed by the stub, the probe
#      never leaves the process. (Task constraint: `--probe` must NOT be a hard
#      gate that assumes a real role on a shard whose IRSA role cannot; shadowing
#      aws satisfies that by construction.)
#   9. NOT-HARDCODED / causation control (AC #2): a SECOND, distinct identity
#      fixture yields a report that tracks IT — proving the output is read live
#      from the tools' responses, not baked in. A constant-returning regression
#      would make run 1 == run 2 and red this leg.
#
# ANTI-VACUITY: every positive leg asserts a SPECIFIC value that would be WRONG
# if the probe/parse broke (the exact ARN; SA name + namespace split out of the
# `system:serviceaccount:<ns>:<name>` username; 30→Developer; the prefix
# aggregation; a denied can-i rendering as ✗) — never merely "some non-empty
# string was printed" — and leg 9 fails if the value is a constant.
#
# Prerequisites: a `gb` binary (GB_BIN, default `gb`) + python3. No kubectl
# context, no namespace, no beads daemon, no AWS credentials, no network egress
# — the four external tools are all local stubs. Fully HERMETIC, so it runs
# unconditionally in every shard and self-SKIPs loud if a prerequisite is
# missing.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-gb-whoami.sh
#   GB_BIN=/path/to/gb ./gardenio/tests/e2e/scripts/test-gb-whoami.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
GB_BIN="${GB_BIN:-gb}"
RUN_ID="${RUN_ID:-$$}"

PASS=0
FAIL=0
ERRORS=()
TMPROOT=""

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
have() { command -v "$1" >/dev/null 2>&1; }

# SKIP loud — a missing prerequisite must never be mistaken for a pass, but must
# also never red the suite (this module is wired into every shard).
skip_all() {
  blue "SKIP gb-whoami: $1"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

assert_eq() {  # assert_eq <name> <got> <want>
  local name="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    pass "$name → $got"
  else
    fail "$name" "got '$got', want '$want'"
  fi
}

assert_contains() {  # assert_contains <name> <haystack-file> <needle>
  local name="$1" file="$2" needle="$3"
  if grep -qF -- "$needle" "$file" 2>/dev/null; then
    pass "$name"
  else
    fail "$name" "'$needle' not found in output; tail: $(tail -3 "$file" 2>/dev/null | tr '\n' '|')"
  fi
}

# shellcheck disable=SC2317  # invoked indirectly via `trap cleanup EXIT`
cleanup() {
  [ -n "$TMPROOT" ] && [ -d "$TMPROOT" ] && rm -rf "$TMPROOT"
  return 0
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gb whoami capability introspection E2E (ka-0000001750)"
echo " the read-only 'what can I actually do?' self-diagnostic"
echo "════════════════════════════════════════════════════"

have python3 || skip_all "python3 not available"
# Resolve gb to an ABSOLUTE path: every invocation runs under `env -i` with a
# curated PATH that deliberately does NOT expose the real aws/kubectl/glab, so
# gb must be reachable without inheriting the ambient PATH.
GB_ABS="$(command -v "$GB_BIN" 2>/dev/null)"
[ -n "$GB_ABS" ] || skip_all "gb binary not available (set GB_BIN)"
# The feature is 2026.212+. On an older gb, `whoami` is an unknown command; SKIP
# loud rather than red so this module is safe to wire in ahead of the rollout.
if ! "$GB_ABS" whoami --help >/dev/null 2>&1; then
  skip_all "this gb binary has no 'whoami' subcommand (pre-2026.212)"
fi

TMPROOT="$(mktemp -d -t gb-whoami-e2e-XXXXXX)"
STUBS="${TMPROOT}/stubs"
TOOLBIN="${TMPROOT}/toolbin"
mkdir -p "$STUBS" "$TOOLBIN"

# Each gb invocation runs under `env -i` with a FULLY HERMETIC PATH of exactly
# two dirs — STUBS then TOOLBIN — so the real aws/kubectl/glab (which live
# elsewhere, e.g. /usr/local/bin) can NEVER leak in. This is load-bearing for the
# kubectl-absent fail-open leg: removing the kubectl stub must make kubectl
# genuinely unreachable, which only holds if no system bin dir is on PATH.
#
# TOOLBIN therefore carries symlinks to the few interpreters the stubs and gb's
# probe children actually need:
#   - bash: the stub shebang is `#!/usr/bin/env bash`, so env resolves bash on
#     the curated PATH.
#   - grep: the aws stub's --probe branch greps its own args.
#   - git:  discoverGitLabProjectPaths runs `git remote get-url origin`. We run
#     gb from a NON-repo cwd (TMPROOT) so that yields nothing and repo discovery
#     is driven deterministically by BOAT_PROJECTS alone — but git must exist for
#     the exec to succeed rather than erroring the section.
for tool in bash grep git; do
  p="$(command -v "$tool" 2>/dev/null || true)"
  [ -n "$p" ] && ln -sf "$p" "${TOOLBIN}/${tool}"
done
have bash || skip_all "bash not available"
have grep || skip_all "grep not available"

# `timeout` is invoked by ABSOLUTE path — it sits ahead of gb in the env line,
# outside the curated PATH — and is simply omitted when the platform lacks it (a
# hung gb is already bounded by the outer shard wall-clock). TIMEOUT_PREFIX
# expands inside the env line just before gb.
TIMEOUT_ABS="$(command -v timeout 2>/dev/null || true)"
if [ -n "$TIMEOUT_ABS" ]; then TIMEOUT_PREFIX=("$TIMEOUT_ABS" 60); else TIMEOUT_PREFIX=(); fi

# ── JSON field extractor (no jq dependency, no eval) ──────────────
# jget <file> <dotted.path[.index]> — walks dicts by key and lists by integer
# index, printing the leaf ('' if any hop is missing). Independent of gb's own
# --jq so the JSON body and the --jq path are asserted by DIFFERENT machinery.
cat > "${TMPROOT}/jget.py" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
for tok in sys.argv[2].split('.'):
    if tok == '':
        continue
    if tok.lstrip('-').isdigit():
        d = d[int(tok)] if isinstance(d, list) and -len(d) <= int(tok) < len(d) else None
    else:
        d = d.get(tok) if isinstance(d, dict) else None
    if d is None:
        break
if isinstance(d, bool):
    print("true" if d else "false")
else:
    print('' if d is None else d)
PYEOF
jget() { python3 "${TMPROOT}/jget.py" "$1" "$2" 2>/dev/null; }

# ── Fixture stubs for the four external tools ─────────────────────
# Each stub is deterministic and reads its emitted identity from the
# environment (STUB_*), so a SINGLE stub set serves multiple identity fixtures
# (the causation control) and multiple failure modes (fail-open leg) without
# rewriting files. gb passes its full os.Environ() to each probe child
# (runProbe: c.Env = os.Environ()), so the STUB_* vars set in the gb env reach
# the stub. Any UNEXPECTED invocation exits non-zero and loud, so a change in
# gb's probe surface surfaces as a red rather than a silent pass.

cat > "${STUBS}/aws" <<'AWSEOF'
#!/usr/bin/env bash
set -u
sub="${1:-} ${2:-}"
case "$sub" in
  "sts get-caller-identity")
    if [ "${STUB_AWS_FAIL:-0}" = "1" ]; then
      echo "An error occurred (AccessDenied) when calling the GetCallerIdentity operation: not authorized" >&2
      exit 254
    fi
    printf '{"UserId":"%s","Account":"%s","Arn":"%s"}\n' \
      "${STUB_AWS_USERID:-AROATEST:sess}" "${STUB_AWS_ACCOUNT:?}" "${STUB_AWS_ARN:?}"
    ;;
  "sts assume-role")
    # --probe path. Allow the target whose ARN contains STUB_ALLOW_MATCH; deny
    # every other candidate. Emits NO real credentials for a denied target and
    # never contacts a real endpoint (this IS the real endpoint, locally).
    if [ -n "${STUB_ALLOW_MATCH:-}" ] && printf '%s ' "$@" | grep -qF "$STUB_ALLOW_MATCH"; then
      echo '{"Credentials":{"AccessKeyId":"AKIAEXAMPLE","SecretAccessKey":"secret","SessionToken":"token","Expiration":"2099-01-01T00:00:00Z"}}'
      exit 0
    fi
    echo "An error occurred (AccessDenied) when calling the AssumeRole operation: User is not authorized to perform: sts:AssumeRole" >&2
    exit 254
    ;;
  "secretsmanager list-secrets")
    if [ "${STUB_SECRETS_FAIL:-0}" = "1" ]; then
      echo "An error occurred (AccessDeniedException) when calling the ListSecrets operation: not authorized" >&2
      exit 254
    fi
    printf '{"SecretList":[%s]}\n' "${STUB_SECRETS_JSON:-}"
    ;;
  *)
    echo "aws-stub: unexpected invocation: $*" >&2
    exit 3
    ;;
esac
AWSEOF

cat > "${STUBS}/kubectl" <<'KEOF'
#!/usr/bin/env bash
set -u
sub="${1:-} ${2:-}"
if [ "$sub" = "auth whoami" ]; then
  if [ "${STUB_K8S_FAIL:-0}" = "1" ]; then
    echo "error: the server could not find the requested resource" >&2
    exit 1
  fi
  printf '{"kind":"SelfSubjectReview","status":{"userInfo":{"username":"%s","uid":"%s","groups":["system:serviceaccounts","system:authenticated"],"extra":{"authentication.kubernetes.io/node-name":["%s"]}}}}\n' \
    "${STUB_K8S_USERNAME:?}" "${STUB_K8S_UID:-uid-test}" "${STUB_K8S_NODE:-ip-10-0-0-1.ec2.internal}"
elif [ "$sub" = "auth can-i" ]; then
  # Truth table the whole feature turns on: the K8s auth path is probed
  # independently of AWS. Allow cluster-wide reads (get pods, incl. the
  # --subresource=log probe), deny writes (create jobs). can-i prints the
  # verdict on stdout and exits non-zero for "no".
  case "$*" in
    *"create jobs"*) echo "no";  exit 1 ;;
    *"get pods"*)    echo "yes"; exit 0 ;;
    *)               echo "no";  exit 1 ;;
  esac
else
  echo "kubectl-stub: unexpected invocation: $*" >&2
  exit 3
fi
KEOF

cat > "${STUBS}/glab" <<'GEOF'
#!/usr/bin/env bash
set -u
# gb calls: `glab api /user` and `glab api projects/<url-encoded-path>`.
if [ "${STUB_GLAB_FAIL:-0}" = "1" ]; then
  echo "GET https://gitlab.com/api/v4/user: 401 {message: 401 Unauthorized}" >&2
  exit 1
fi
case "${2:-}" in
  /user)
    printf '{"id":%s,"username":"%s","name":"gardenio bot","state":"active","bot":true}\n' \
      "${STUB_GLAB_ID:-259}" "${STUB_GLAB_USER:?}"
    ;;
  projects/*)
    printf '{"permissions":{"project_access":null,"group_access":{"access_level":%s}}}\n' \
      "${STUB_GLAB_LEVEL:-30}"
    ;;
  *)
    echo "glab-stub: unexpected invocation: $*" >&2
    exit 3
    ;;
esac
GEOF

chmod +x "${STUBS}/aws" "${STUBS}/kubectl" "${STUBS}/glab"

# ── Identity fixture #1 — the REAL known-true gardenio agent identity ──
# These mirror the actual prod-agent capability surface (acct 123456789012,
# K8s SA gardenio-agent-cluster-admin in gardenio-system, a GitLab bot with
# Developer on the gardenio repo), so the assertions double as a documented
# golden of what an agent genuinely holds. RUN_ID is woven into the values that
# gb echoes verbatim so a stale/cached constant cannot satisfy them.
FX_AGENT="gb-whoami-e2e-${RUN_ID}"
FX_ROLE="crew-e2e"
FX_PROJECT="gardenio"
FX_TASK="ka-0000001750"
FX_NAMESPACE="gardenio-system"
FX_ROLE_ARN="arn:aws:iam::123456789012:role/gardenio-prod-agent"
FX_REGION="us-east-1"
FX_ACCOUNT="123456789012"
FX_ARN="arn:aws:sts::123456789012:assumed-role/gardenio-prod-agent/botocore-session-${RUN_ID}"
FX_K8S_USERNAME="system:serviceaccount:gardenio-system:gardenio-agent-cluster-admin"
FX_NODE="ip-10-50-186-131.ec2.internal"
FX_GLAB_USER="group_14176856_bot_${RUN_ID}"
FX_GLAB_LEVEL="30"
FX_SECRETS_JSON='{"Name":"gardenio/prod/kd-beads-token","LastAccessedDate":"2026-08-01T00:00:00Z"},{"Name":"gardenio/prod/gitlab-token"},{"Name":"gardenio/aws-poc/litellm/master-key"}'
FX_BOAT_PROJECTS="gardenio=https://gitlab.com/Acme/Platform/gardenio.git:kd"
# A discovered assume-role target for the --probe leg (an env var whose name ends
# in _ROLE_ARN, the shape discoverAssumableTargets keys on).
FX_MICROVM_ROLE_ARN="arn:aws:iam::345678901234:role/gardenio-microvm-launcher"

# run_whoami <outfile> [VAR=val ...] [-- gb-args...] — runs the real gb binary
# under a hermetic env with ONLY the stub tools + git reachable, capturing
# stdout to <outfile> and stderr to <outfile>.err, and RETURNING gb's exit code.
# Leading VAR=val tokens (per-leg STUB_*_FAIL / probe / offline overrides) are
# spliced into the env line; everything after them is passed to `gb whoami`. The
# fixture identity (FX_*) is read at call time so legs that reassign FX_* — the
# causation control — take effect without touching this helper.
run_whoami() {
  local out="$1"; shift
  local extra_env=()
  while [ $# -gt 0 ] && [[ "$1" == *=* ]]; do extra_env+=("$1"); shift; done
  ( cd "$TMPROOT" && env -i \
      PATH="${STUBS}:${TOOLBIN}" \
      HOME="$TMPROOT" \
      BOAT_AGENT="$FX_AGENT" BOAT_ROLE="$FX_ROLE" BOAT_PROJECT="$FX_PROJECT" BOAT_TASK_ID="$FX_TASK" \
      BOAT_PROJECTS="$FX_BOAT_PROJECTS" \
      POD_NAMESPACE="$FX_NAMESPACE" \
      AWS_ROLE_ARN="$FX_ROLE_ARN" AWS_REGION="$FX_REGION" \
      AWS_CONFIG_FILE=/nonexistent AWS_EC2_METADATA_DISABLED=true \
      STUB_AWS_ARN="$FX_ARN" STUB_AWS_ACCOUNT="$FX_ACCOUNT" \
      STUB_K8S_USERNAME="$FX_K8S_USERNAME" STUB_K8S_NODE="$FX_NODE" \
      STUB_GLAB_USER="$FX_GLAB_USER" STUB_GLAB_LEVEL="$FX_GLAB_LEVEL" \
      STUB_SECRETS_JSON="$FX_SECRETS_JSON" \
      "${extra_env[@]}" \
      "${TIMEOUT_PREFIX[@]}" "$GB_ABS" whoami "$@" ) >"$out" 2>"${out}.err"
}

# ===========================================================================
# LEG 1 — READ-ONLY DEFAULT (headline): the JSON report is well-formed and
# carries the exact fixture identities.
# ===========================================================================
blue "[read-only] gb whoami --json — shape + known-true identities"
J="${TMPROOT}/whoami.json"
run_whoami "$J" --json
RC=$?
assert_eq "gb whoami --json exits 0" "$RC" "0"

if ! python3 -c "import json,sys; json.load(open('$J'))" 2>/dev/null; then
  fail "gb whoami --json emitted valid JSON" "got: $(head -c 200 "$J"); err: $(tail -2 "${J}.err")"
  # Without parseable JSON the rest of leg 1 is meaningless — but keep going so
  # the human-render + fail-open legs still report.
else
  pass "gb whoami --json emitted valid JSON"

  # Pod context — read straight from the environment (AC #2: not hardcoded).
  assert_eq "pod.agent captured from env"     "$(jget "$J" pod.agent)"     "$FX_AGENT"
  assert_eq "pod.role captured from env"      "$(jget "$J" pod.role)"      "$FX_ROLE"
  assert_eq "pod.project captured from env"   "$(jget "$J" pod.project)"   "$FX_PROJECT"
  assert_eq "pod.task_id captured from env"   "$(jget "$J" pod.task_id)"   "$FX_TASK"

  # AWS — the specific ARN + account, not merely a non-empty string (anti-vacuity).
  assert_eq "aws.caller_identity.Arn is the probed ARN" \
    "$(jget "$J" aws.caller_identity.Arn)" "$FX_ARN"
  assert_eq "aws.caller_identity.Account is the probed account" \
    "$(jget "$J" aws.caller_identity.Account)" "$FX_ACCOUNT"
  assert_eq "aws.region reflects the env" "$(jget "$J" aws.region)" "$FX_REGION"
  assert_eq "aws section has no error on a healthy probe" "$(jget "$J" aws.error)" ""

  # Kubernetes — the SA name + namespace are SPLIT OUT of the
  # system:serviceaccount:<ns>:<name> username (a transform, not an echo).
  assert_eq "k8s.service_account parsed from the SA username" \
    "$(jget "$J" kubernetes.service_account)" "gardenio-agent-cluster-admin"
  assert_eq "k8s.namespace parsed from the SA username" \
    "$(jget "$J" kubernetes.namespace)" "gardenio-system"
  assert_eq "k8s.node parsed from the token extra attrs" \
    "$(jget "$J" kubernetes.node)" "$FX_NODE"
  assert_eq "k8s section has no error on a healthy probe" "$(jget "$J" kubernetes.error)" ""

  # GitLab — the user + the DERIVED access name.
  assert_eq "gitlab.user.username is the probed user" \
    "$(jget "$J" gitlab.user.username)" "$FX_GLAB_USER"
  assert_eq "gitlab.user.bot is true" "$(jget "$J" gitlab.user.bot)" "true"
fi
echo

# ===========================================================================
# LEG 2 — can-i TWO-PATH truth table: the K8s ServiceAccount path is probed
# independently of AWS. yes→allowed, no→denied — the ka-0000003844 miss.
# ===========================================================================
blue "[can-i] K8s auth path probed independently (ka-0000003844 miss)"
if [ -f "$J" ] && python3 -c "import json;json.load(open('$J'))" 2>/dev/null; then
  # Find the two probes by action and assert their allowed flag. The renderer
  # asserts the ✓/✗ marks in leg 3; here we assert the structured booleans.
  get_pods_allowed=$(python3 -c "
import json
d=json.load(open('$J'))
for c in d['kubernetes']['can']:
    if c['action'].startswith('get pods'): print('true' if c['allowed'] else 'false'); break
" 2>/dev/null)
  create_jobs_allowed=$(python3 -c "
import json
d=json.load(open('$J'))
for c in d['kubernetes']['can']:
    if c['action']=='create jobs': print('true' if c['allowed'] else 'false'); break
" 2>/dev/null)
  logs_allowed=$(python3 -c "
import json
d=json.load(open('$J'))
for c in d['kubernetes']['can']:
    if 'log' in c['action']: print('true' if c['allowed'] else 'false'); break
" 2>/dev/null)
  assert_eq "can-i get pods (all namespaces) → allowed" "$get_pods_allowed" "true"
  assert_eq "can-i read pod logs → allowed"             "$logs_allowed"     "true"
  # THE anti-vacuity direction: a denied verb must render as NOT allowed, so a
  # parser that always returns true (the ka-0000003844-shaped bug) reds here.
  assert_eq "can-i create jobs → NOT allowed (denied verb)" "$create_jobs_allowed" "false"
else
  fail "can-i truth table" "no parseable JSON from leg 1"
fi
echo

# ===========================================================================
# LEG 3 — HUMAN RENDER: the default block carries every section + derived vals.
# ===========================================================================
blue "[render] human-readable capability block"
H="${TMPROOT}/whoami.txt"
# No --json: gb renders the human capability block to stdout.
run_whoami "$H"
assert_eq "gb whoami (human render) exits 0" "$?" "0"
assert_contains "render has the AWS section header"        "$H" "AWS"
assert_contains "render has the Kubernetes section header" "$H" "Kubernetes"
assert_contains "render has the GitLab section header"     "$H" "GitLab"
assert_contains "render has the Secrets section header"    "$H" "Secrets"
assert_contains "render shows the parsed ServiceAccount"   "$H" "gardenio-agent-cluster-admin"
assert_contains "render shows the exact AWS identity ARN"  "$H" "$FX_ARN"
# Derived: 30 → Developer.
assert_contains "render shows the DERIVED GitLab access name (30→Developer)" "$H" "Developer"
# can-i marks: an allowed probe renders ✓, a denied one ✗.
assert_contains "render marks an allowed can-i with ✓" "$H" "✓  can-i get pods"
assert_contains "render marks a denied can-i with ✗"   "$H" "✗  can-i create jobs"
echo

# ===========================================================================
# LEG 4 — DERIVATIONS: GitLab access-level name (via group) + secret prefixes.
# ===========================================================================
blue "[derivations] access-level name + secret-prefix aggregation"
if [ -f "$J" ] && python3 -c "import json;json.load(open('$J'))" 2>/dev/null; then
  assert_eq "gitlab repo access_level 30 → 'Developer'" \
    "$(jget "$J" gitlab.repos.0.access_name)" "Developer"
  assert_eq "gitlab repo access is via the group membership" \
    "$(jget "$J" gitlab.repos.0.via)" "group"
  assert_eq "gitlab repo path resolved from BOAT_PROJECTS" \
    "$(jget "$J" gitlab.repos.0.path)" "Acme/Platform/gardenio"

  # Three secret names across two prefixes → total 3, prefixes {gardenio/prod:2,
  # gardenio/aws-poc:1}. Prefixes are sorted, so gardenio/aws-poc is index 0.
  assert_eq "secrets total counts every readable secret" "$(jget "$J" secrets.total)" "3"
  prod_count=$(python3 -c "
import json
d=json.load(open('$J'))
print(next((p['count'] for p in d['secrets']['prefixes'] if p['prefix']=='gardenio/prod'), 'MISSING'))
" 2>/dev/null)
  awspoc_count=$(python3 -c "
import json
d=json.load(open('$J'))
print(next((p['count'] for p in d['secrets']['prefixes'] if p['prefix']=='gardenio/aws-poc'), 'MISSING'))
" 2>/dev/null)
  assert_eq "secrets aggregated: gardenio/prod prefix has 2"    "$prod_count"   "2"
  assert_eq "secrets aggregated: gardenio/aws-poc prefix has 1" "$awspoc_count" "1"
else
  fail "derivations" "no parseable JSON from leg 1"
fi
echo

# ===========================================================================
# LEG 5 — --jq / --json parity: the single-field machine path.
# ===========================================================================
blue "[--jq] single-field extraction matches the JSON body"
JQOUT="${TMPROOT}/jq.out"
run_whoami "$JQOUT" --jq '.aws.caller_identity.Arn'
JQ_RC=$?
assert_eq "gb whoami --jq exits 0" "$JQ_RC" "0"
JQ_VAL="$(tr -d '"' < "$JQOUT" | tr -d '[:space:]')"
assert_eq "gb whoami --jq '.aws.caller_identity.Arn' returns the ARN" "$JQ_VAL" "$FX_ARN"
echo

# ===========================================================================
# LEG 6 — FAIL-OPEN: a denied aws + a 401 glab + an absent kubectl each annotate
# ONLY their own section; gb whoami still exits 0 and healthy sections render.
# ===========================================================================
blue "[fail-open] a failing probe annotates its section, never fails the command"

# 6a. aws denied — the aws section carries an error; k8s + gitlab still resolve.
FO="${TMPROOT}/failopen-aws.json"
run_whoami "$FO" STUB_AWS_FAIL=1 STUB_SECRETS_FAIL=1 --json
assert_eq "aws-denied: gb whoami still exits 0" "$?" "0"
if python3 -c "import json;json.load(open('$FO'))" 2>/dev/null; then
  AWS_ERR="$(jget "$FO" aws.error)"
  if [ -n "$AWS_ERR" ]; then
    pass "aws-denied: aws section records an error ($AWS_ERR)"
  else
    fail "aws-denied: aws section should record an error"
  fi
  # Independence: the OTHER sections are unaffected.
  assert_eq "aws-denied: k8s section still resolves the SA" \
    "$(jget "$FO" kubernetes.service_account)" "gardenio-agent-cluster-admin"
  assert_eq "aws-denied: gitlab section still resolves the user" \
    "$(jget "$FO" gitlab.user.username)" "$FX_GLAB_USER"
  # The healthy AWS FIELDS read from env still render even with the probe denied.
  assert_eq "aws-denied: role_arn from env still present" \
    "$(jget "$FO" aws.role_arn)" "$FX_ROLE_ARN"
else
  fail "aws-denied: output is still valid JSON" "$(head -c 200 "$FO")"
fi

# 6b. kubectl absent — remove ONLY the kubectl stub from a private stub dir, so
# the real kubectl is NOT on PATH either (gb is invoked by absolute path, so it
# never needs the ambient PATH). The k8s section errors; aws + gitlab resolve.
STUBS_NOK8S="${TMPROOT}/stubs-nok8s"
mkdir -p "$STUBS_NOK8S"
cp "${STUBS}/aws" "${STUBS}/glab" "$STUBS_NOK8S"/
NOK8S="${TMPROOT}/failopen-nok8s.json"
( cd "$TMPROOT" && env -i \
    PATH="${STUBS_NOK8S}:${TOOLBIN}" HOME="$TMPROOT" \
    BOAT_AGENT="$FX_AGENT" BOAT_ROLE="$FX_ROLE" BOAT_PROJECT="$FX_PROJECT" BOAT_TASK_ID="$FX_TASK" \
    BOAT_PROJECTS="$FX_BOAT_PROJECTS" POD_NAMESPACE="$FX_NAMESPACE" \
    AWS_ROLE_ARN="$FX_ROLE_ARN" AWS_REGION="$FX_REGION" \
    AWS_CONFIG_FILE=/nonexistent AWS_EC2_METADATA_DISABLED=true \
    STUB_AWS_ARN="$FX_ARN" STUB_AWS_ACCOUNT="$FX_ACCOUNT" \
    STUB_GLAB_USER="$FX_GLAB_USER" STUB_GLAB_LEVEL="$FX_GLAB_LEVEL" \
    STUB_SECRETS_JSON="$FX_SECRETS_JSON" \
    "${TIMEOUT_PREFIX[@]}" "$GB_ABS" whoami --json ) >"$NOK8S" 2>"${NOK8S}.err"
assert_eq "kubectl-absent: gb whoami still exits 0" "$?" "0"
if python3 -c "import json;json.load(open('$NOK8S'))" 2>/dev/null; then
  K8S_ERR="$(jget "$NOK8S" kubernetes.error)"
  if [ -n "$K8S_ERR" ]; then
    pass "kubectl-absent: k8s section records an error ($K8S_ERR)"
  else
    fail "kubectl-absent: k8s section should record an error"
  fi
  assert_eq "kubectl-absent: aws section still resolves the ARN" \
    "$(jget "$NOK8S" aws.caller_identity.Arn)" "$FX_ARN"
else
  fail "kubectl-absent: output is still valid JSON" "$(head -c 200 "$NOK8S")"
fi
echo

# ===========================================================================
# LEG 7 — OFFLINE / NO-DAEMON: capability introspection works with the beads
# daemon down (gb whoami overrides the root PersistentPreRunE — mirrors
# `gb version`).
# ===========================================================================
blue "[offline] runs with the beads daemon unreachable (no daemon dial)"
OFF="${TMPROOT}/offline.json"
# Point BEADS at a closed port and provide a bogus token: if whoami dialed the
# daemon it would hang/fail; it must not.
run_whoami "$OFF" \
  BEADS_HTTP_ADDR="http://127.0.0.1:1" \
  BEADS_HTTP_URL="http://127.0.0.1:1" \
  BEADS_AUTH_TOKEN="bogus-offline-token" \
  --json
assert_eq "offline: gb whoami exits 0 with the daemon down" "$?" "0"
assert_eq "offline: still reports the AWS identity" \
  "$(jget "$OFF" aws.caller_identity.Arn)" "$FX_ARN"
echo

# ===========================================================================
# LEG 8 — --probe (HERMETIC): sts:AssumeRole is exercised against the STUB, so
# assumable targets are promoted to allowed/denied WITHOUT minting a real STS
# credential. Honors the task constraint (never gate on a real assume-role).
# ===========================================================================
blue "[--probe] assume-role verification (hermetic — no real STS mint)"
PROBE="${TMPROOT}/probe.json"
# Two discovered targets via *_ROLE_ARN env vars: the microvm launcher (allowed
# by the stub) and a synthetic forbidden role (denied). STUB_ALLOW_MATCH selects
# which the stub's assume-role accepts.
run_whoami "$PROBE" \
  MICROVM_LAUNCHER_ROLE_ARN="$FX_MICROVM_ROLE_ARN" \
  FORBIDDEN_ROLE_ARN="arn:aws:iam::999999999999:role/forbidden-target" \
  STUB_ALLOW_MATCH="gardenio-microvm-launcher" \
  --probe --json
assert_eq "--probe: gb whoami exits 0" "$?" "0"
if python3 -c "import json;json.load(open('$PROBE'))" 2>/dev/null; then
  allowed_status=$(python3 -c "
import json
d=json.load(open('$PROBE'))
print(next((t['status'] for t in d['aws']['assumable_targets'] if 'microvm-launcher' in t['role_arn']), 'MISSING'))
" 2>/dev/null)
  denied_status=$(python3 -c "
import json
d=json.load(open('$PROBE'))
print(next((t['status'] for t in d['aws']['assumable_targets'] if 'forbidden-target' in t['role_arn']), 'MISSING'))
" 2>/dev/null)
  assert_eq "--probe: an assumable target verifies as 'allowed'" "$allowed_status" "allowed"
  assert_eq "--probe: a forbidden target verifies as 'denied'"   "$denied_status" "denied"
else
  fail "--probe: output is still valid JSON" "$(head -c 200 "$PROBE")"
fi
echo

# ===========================================================================
# LEG 9 — NOT-HARDCODED / causation control (AC #2): a SECOND, distinct identity
# fixture yields a report that tracks IT. A constant-returning regression makes
# run 1 == run 2 and reds this leg.
# ===========================================================================
blue "[causation] a different identity fixture yields a different report"
# Swap in a clearly-synthetic second identity.
FX_ARN="arn:aws:sts::222222222222:assumed-role/other-agent/session-${RUN_ID}"
FX_ACCOUNT="222222222222"
FX_ROLE_ARN="arn:aws:iam::222222222222:role/other-agent"
FX_K8S_USERNAME="system:serviceaccount:other-ns:other-sa"
FX_GLAB_USER="other_bot_${RUN_ID}"
FX_NAMESPACE="other-ns"
J2="${TMPROOT}/whoami2.json"
run_whoami "$J2" --json
assert_eq "causation: second run exits 0" "$?" "0"
if python3 -c "import json;json.load(open('$J2'))" 2>/dev/null; then
  assert_eq "causation: report tracks the SECOND ARN" \
    "$(jget "$J2" aws.caller_identity.Arn)" "$FX_ARN"
  assert_eq "causation: report tracks the SECOND SA" \
    "$(jget "$J2" kubernetes.service_account)" "other-sa"
  assert_eq "causation: report tracks the SECOND GitLab user" \
    "$(jget "$J2" gitlab.user.username)" "$FX_GLAB_USER"
  # The headline proof: the two reports genuinely differ on identity.
  ARN1="$(jget "$J" aws.caller_identity.Arn)"
  ARN2="$(jget "$J2" aws.caller_identity.Arn)"
  if [ -n "$ARN1" ] && [ "$ARN1" != "$ARN2" ]; then
    pass "causation: run-1 ARN ≠ run-2 ARN (output is read live, not hardcoded)"
  else
    fail "causation: the two runs reported the SAME ARN" "arn1='$ARN1' arn2='$ARN2' — output may be hardcoded"
  fi
else
  fail "causation: second run produced valid JSON" "$(head -c 200 "$J2")"
fi
echo

# ── Summary ───────────────────────────────────────────────────────
echo "════════════════════════════════════════════════════"
echo " gb-whoami Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gb-whoami assertions passed — capability introspection is honest end-to-end."
exit 0
