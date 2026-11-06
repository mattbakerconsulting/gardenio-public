# release-deploy_test_harness.sh -- shared harness for release-deploy_test.sh
# and release-deploy-record_test.sh: the stubs, per-test setup/teardown,
# run_under_test and the asserts. Sourced, never run; the sourcing test sets
# SCRIPT_DIR first. See release-deploy_test.sh for how the stubs and the
# baseline fixtures work.
# shellcheck shell=bash

PASS=0
FAIL=0

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# ── stub scaffolding ────────────────────────────────────────────────────

# REAL_TOOLS -- the non-builtin binaries release-deploy.sh (and
# verify-image-tags.sh, which it shells out to) need, that are safe to
# delegate to the real system copy: they do generic text/VCS work the tests
# don't need to fake.
REAL_TOOLS=(git jq sort diff grep sed cut paste tail mktemp dirname cat rm bash tee basename sleep)

make_min_path() {
  local bin="$1"
  mkdir -p "$bin"
  local t
  for t in "${REAL_TOOLS[@]}"; do
    ln -sf "$(command -v "$t")" "${bin}/${t}"
  done
}

# make_helm_stub -- logs every invocation's argv to $HELM_CALL_LOG. Behavior
# per subcommand is driven entirely by env vars / fixture files, set per
# test before calling run_under_test.
make_helm_stub() {
  cat > "$1/helm" <<'EOF'
#!/usr/bin/env bash
printf 'helm %s\n' "$*" >> "$HELM_CALL_LOG"
# HELM_STDERR_WARN=1: every call also writes a warning to stderr, as helm v4
# does for deprecated flags. Must never corrupt JSON parsing.
if [ "${HELM_STDERR_WARN:-0}" = "1" ]; then
  echo "WARNING: Flag --atomic has been deprecated, use --rollback-on-failure instead" >&2
fi
case "$1" in
  status)
    # After an upgrade has been attempted, serve HELM_STATUS_POST_FILE (if
    # set) -- lets a test control what --atomic left behind.
    if [ -n "${HELM_STATUS_POST_FILE:-}" ] && grep -q '^helm upgrade' "$HELM_CALL_LOG"; then
      if [ "${HELM_STATUS_POST_FAIL:-0}" = "1" ]; then
        echo "Error: Kubernetes cluster unreachable" >&2
        exit 1
      fi
      cat "$HELM_STATUS_POST_FILE"
      exit 0
    fi
    if [ "${HELM_STATUS_FAIL:-0}" = "1" ]; then
      printf '%s\n' "${HELM_STATUS_ERR:-Error: release: not found}" >&2
      exit 1
    fi
    cat "$HELM_STATUS_FILE"
    exit 0
    ;;
  get)
    if [ "$2" = "values" ]; then
      if [ "${HELM_GET_VALUES_FAIL:-0}" = "1" ]; then
        echo "Error: get values failed" >&2
        exit 1
      fi
      cat "$HELM_GET_VALUES_FILE"
      exit 0
    fi
    echo "stub helm: unsupported get subcommand: $*" >&2
    exit 1
    ;;
  upgrade)
    printf '%s\n' "${HELM_UPGRADE_OUTPUT:-upgraded}"
    exit "${HELM_UPGRADE_EXIT:-0}"
    ;;
  history)
    if [ "${HELM_HISTORY_FAIL:-0}" = "1" ]; then
      echo "Error: history failed" >&2
      exit 1
    fi
    cat "${HELM_HISTORY_FILE:-/dev/null}"
    exit 0
    ;;
  rollback)
    printf '%s\n' "${HELM_ROLLBACK_OUTPUT:-rolled back}"
    exit "${HELM_ROLLBACK_EXIT:-0}"
    ;;
  *)
    echo "stub helm: unsupported invocation: $*" >&2
    exit 1
    ;;
esac
EOF
  chmod +x "$1/helm"
}

# make_pack_stub -- stands in for scripts/pack-gardenio-chart.sh: logs argv
# to $PACK_CALL_LOG, writes <dest>/gardenio-<version>.tgz and prints its path
# last, like the real one. PACK_FAIL=1 fails; PACK_NO_FILE=1 prints a path
# but writes nothing.
make_pack_stub() {
  cat > "$1" <<'EOF'
#!/usr/bin/env bash
printf 'pack %s\n' "$*" >> "$PACK_CALL_LOG"
if [ "${PACK_FAIL:-0}" = "1" ]; then
  echo "pack-gardenio-chart.sh: FATAL: comment strip changed helm template render" >&2
  exit 1
fi
version=""; prev=""
for a in "$@"; do [ "$prev" = "--version" ] && version="$a"; prev="$a"; done
tgz="$2/gardenio-${version}.tgz"
[ "${PACK_NO_FILE:-0}" = "1" ] || : > "$tgz"
echo "Successfully packaged chart"
echo "$tgz"
EOF
  chmod +x "$1"
}

# make_theme_stub -- stands in for gardenio/scripts/keycloak-theme-configmap.py
# (rule 4b): logs argv to $THEME_CALL_LOG and prints a one-line ConfigMap
# JSON like the real one. THEME_FAIL=1 fails the way a bad theme tree does.
make_theme_stub() {
  cat > "$1" <<'EOF'
#!/usr/bin/env bash
printf 'theme %s\n' "$*" >> "$THEME_CALL_LOG"
if [ "${THEME_FAIL:-0}" = "1" ]; then
  echo "keycloak-theme-configmap.py: theme path 'login/bad__name.ftl' contains '__'" >&2
  exit 1
fi
echo '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"stub-theme"}}'
EOF
  chmod +x "$1"
}

# make_kubectl_stub -- serves release-deploy.sh's own pause-configmap probe
# and theme ConfigMap apply (stdin captured to $KUBECTL_APPLY_LOG;
# KUBECTL_APPLY_FAIL=1 fails it), and verify-image-tags.sh's pod-listing call.
#
# Rule 4c (edge manifests) against a fake cluster under $EDGE_FIX:
#   live/<ns>_<name>.json  the live ConfigMaps; `get configmap` reads them and
#                          an applied ConfigMap (a List item) overwrites them,
#                          so a pod's mounted file is current at once
#   pods-<ns>.txt          the Running pods `get pods -l` names
#   exec cat <mount>/<key> that key of a live ConfigMap in <ns>
#                          (EDGE_EXEC_STALE=1: never the new content)
# EDGE_NGINX_T_FAIL=<ns> fails `nginx -t` there, EDGE_RELOAD_FAIL=<ns> the
# reload, EDGE_CM_GET_FAIL=1 the live read, and KUBECTL_APPLY_FAIL_MATCH fails
# an apply whose stdin contains it. Every apply also logs the kinds it got.
make_kubectl_stub() {
  cat > "$1/kubectl" <<'EOF'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >> "$KUBECTL_CALL_LOG"
if [ "$1" = "apply" ]; then
  stdin="$(cat)"
  printf '%s\n' "$stdin" >> "$KUBECTL_APPLY_LOG"
  printf 'kubectl applied %s\n' "$(jq -r '[.items[]?.kind // .kind] | join(",")' <<<"$stdin" 2>/dev/null)" >> "$KUBECTL_CALL_LOG"
  if [ "${KUBECTL_APPLY_FAIL:-0}" = "1" ]; then
    echo "error: configmaps \"stub-theme\" is forbidden" >&2
    exit 1
  fi
  if [ -n "${KUBECTL_APPLY_FAIL_MATCH:-}" ] && grep -qF -- "$KUBECTL_APPLY_FAIL_MATCH" <<<"$stdin"; then
    echo "error: simulated apply failure" >&2
    exit 1
  fi
  case " $* " in
    *" --dry-run=server "*) ;;
    *)
      if [ -n "${EDGE_FIX:-}" ]; then
        jq -c '.items[]? | select(.kind == "ConfigMap")' <<<"$stdin" | while IFS= read -r cm; do
          f="$(jq -r '"\(.metadata.namespace)_\(.metadata.name).json"' <<<"$cm")"
          printf '%s\n' "$cm" > "${EDGE_FIX}/live/${f}"
        done
      fi
      ;;
  esac
  echo "configmap/stub-theme serverside-applied"
  exit 0
fi
if [ "$1" = "exec" ]; then
  ns="$3" pod="$4"
  shift 7  # exec -n <ns> <pod> -c nginx --
  case "$*" in
    "cat "*)
      [ "${EDGE_EXEC_STALE:-0}" = "1" ] && { echo stale; exit 0; }
      key="$(basename "$2")"
      for f in "${EDGE_FIX}/live/${ns}"_*.json; do
        jq -e --arg k "$key" '.data | has($k)' "$f" >/dev/null 2>&1 && { jq -r --arg k "$key" '.data[$k]' "$f"; exit 0; }
      done
      echo "cat: can't open '$2': No such file or directory" >&2
      exit 1
      ;;
    "nginx -s reload")
      [ "${EDGE_RELOAD_FAIL:-}" = "$ns" ] && { echo "nginx: [error] kill(1, 1) failed" >&2; exit 1; }
      exit 0
      ;;
    *"nginx -t"*)
      if [ "${EDGE_NGINX_T_FAIL:-}" = "$ns" ]; then
        echo "nginx: [emerg] unknown directive \"bogus\" in /etc/nginx/conf.d/default.conf:3" >&2
        exit 1
      fi
      echo "nginx: configuration file /etc/nginx/nginx.conf test is successful" >&2
      exit 0
      ;;
    "sh -c "*) exit 0 ;;  # the conf.d restore
  esac
  echo "stub kubectl: unsupported exec in ${pod}: $*" >&2
  exit 1
fi
if [ "$1" = "get" ] && [ "$2" = "configmap" ] && [ "$3" != "release-deployer-pause" ]; then
  if [ "${EDGE_CM_GET_FAIL:-0}" = "1" ]; then
    echo "Error from server (Forbidden): configmaps \"$3\" is forbidden" >&2
    exit 1
  fi
  f="${EDGE_FIX}/live/${5}_${3}.json"
  [ -f "$f" ] && { cat "$f"; exit 0; }
  echo "Error from server (NotFound): configmaps \"$3\" not found" >&2
  exit 1
fi
if [ "$1" = "get" ] && [ "$2" = "pods" ] && [ "$3" = "-n" ] && [ "$5" = "-l" ]; then
  cat "${EDGE_FIX}/pods-${4}.txt" 2>/dev/null
  exit 0
fi
if [ "$1" = "get" ] && [ "$2" = "configmap" ]; then
  if [ "${KUBECTL_PAUSE_EXISTS:-0}" = "1" ]; then
    exit 0
  fi
  if [ -n "${KUBECTL_PAUSE_ERR:-}" ]; then
    printf '%s\n' "$KUBECTL_PAUSE_ERR" >&2
    exit 1
  fi
  echo "Error from server (NotFound): configmaps \"$3\" not found" >&2
  exit 1
fi
if [ "$1" = "get" ] && [ "$2" = "pods" ]; then
  cat "${KUBECTL_PODS_FILE:-/dev/null}"
  exit 0
fi
echo "stub kubectl: unsupported invocation: $*" >&2
exit 1
EOF
  chmod +x "$1/kubectl"
}

make_gh_stub() {
  cat > "$1/gh" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "api" ]; then
  printf 'gh api %s\n' "$2" >> "$GH_CALL_LOG"
  if [ "${GH_API_FAIL:-0}" = "1" ]; then
    echo "gh: simulated api failure" >&2
    exit 1
  fi
  case "$2" in
    */commits/*)
      printf '{"sha":"%s"}' "${GH_TAG_SHA:-}"
      exit 0
      ;;
    */actions/runs/*/jobs*)
      # A run's jobs (rule 4's "did it really build?", rule 10's failed
      # jobs). GH_JOBS_FAIL=1 fails just this call. A fixture named
      # "$GH_JOBS_FILE.<run-id>" answers for that run alone.
      if [ "${GH_JOBS_FAIL:-0}" = "1" ]; then
        echo "gh: simulated jobs api failure" >&2
        exit 1
      fi
      id="${2#*/actions/runs/}"
      id="${id%%/*}"
      if [ -f "${GH_JOBS_FILE}.${id}" ]; then
        cat "${GH_JOBS_FILE}.${id}"
        exit 0
      fi
      cat "$GH_JOBS_FILE"
      exit 0
      ;;
    */actions/workflows/*/runs*)
      # Rule 4 asks images.yml, ci.yml and checks.yml in turn. A fixture
      # named "$GH_RUNS_FILE.<workflow>" answers for that workflow alone;
      # every other workflow gets $GH_RUNS_FILE.
      wf="${2#*/actions/workflows/}"
      wf="${wf%%/*}"
      if [ -f "${GH_RUNS_FILE}.${wf}" ]; then
        cat "${GH_RUNS_FILE}.${wf}"
        exit 0
      fi
      ;;
  esac
  cat "$GH_RUNS_FILE"
  exit 0
fi
echo "stub gh: unsupported invocation: $*" >&2
exit 1
EOF
  chmod +x "$1/gh"
}

# make_curl_stub -- logs full argv, and separately appends each `-d` payload
# (release-deploy.sh always emits compact single-line JSON bodies) to
# $CURL_BODY_LOG, one JSON object per line.
make_curl_stub() {
  cat > "$1/curl" <<'EOF'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$CURL_CALL_LOG"
# The Keycloak client-credentials token fetch (machine principal).
case " $* " in
  *"/protocol/openid-connect/token"*) [ "${CURL_TOKEN_FAIL:-0}" = "1" ] && exit 22; printf '{"access_token":"machine-jwt"}'; exit 0 ;;
esac
# A GET (no -X POST) is the open-bug dedup query: answer from the fixture.
case " $* " in
  *" -X POST "*) ;;
  *) [ -n "${CURL_GET_FILE:-}" ] && cat "$CURL_GET_FILE"; [ "${CURL_FAIL:-0}" = "1" ] && exit 1; exit 0 ;;
esac
args=("$@")
i=0
while [ $i -lt ${#args[@]} ]; do
  if [ "${args[$i]}" = "-d" ]; then
    printf '%s\n' "${args[$((i + 1))]}" >> "$CURL_BODY_LOG"
  fi
  i=$((i + 1))
done
[ "${CURL_FAIL:-0}" = "1" ] && exit 1
# CURL_POST_FILE: the daemon's answer to a POST (the created bead).
[ -n "${CURL_POST_FILE:-}" ] && cat "$CURL_POST_FILE"
exit 0
EOF
  chmod +x "$1/curl"
}

# make_yq_stub -- "converts" YAML to JSON by cat-ing a same-named .json
# fixture next to the YAML file it's asked to convert, or the file itself
# when it is already JSON (an overlay committed at a previous tag, which
# rule 3b reads into a temp file). Only present in PATH when a test wants yq
# to exist.
make_yq_stub() {
  cat > "$1/yq" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "-o=json" ]; then
  if [ -f "${2}.json" ]; then
    cat "${2}.json"
    exit 0
  fi
  if jq -e 'type == "object"' "$2" >/dev/null 2>&1; then
    cat "$2"
    exit 0
  fi
  echo "stub yq: no fixture ${2}.json" >&2
  exit 1
fi
echo "stub yq: unsupported invocation: $*" >&2
exit 1
EOF
  chmod +x "$1/yq"
}

# make_python3_stub -- answers the `import yaml` probe per $PY_IMPORT_YAML_EXIT,
# and "converts" via the same .json-fixture-next-to-the-yaml-file trick as
# the yq stub. Only present in PATH when a test wants python3 to exist.
make_python3_stub() {
  cat > "$1/python3" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "-c" ] && [ "$2" = "import yaml" ]; then
  exit "${PY_IMPORT_YAML_EXIT:-0}"
fi
if [ "$1" = "-c" ]; then
  file="$3"
  if [ -f "${file}.json" ]; then
    cat "${file}.json"
    exit 0
  fi
  echo "stub python3: no fixture ${file}.json" >&2
  exit 1
fi
echo "stub python3: unsupported invocation: $*" >&2
exit 1
EOF
  chmod +x "$1/python3"
}

# ── per-test setup/teardown ─────────────────────────────────────────────

CALVER="2026.62.5"
TAG_SHA="0123456789abcdef0123456789abcdef01234567"

# build_fixture_tree -- the stubs, the fake checkout and the edge fixtures
# every test starts from, written under $TMP. Nothing in it depends on the
# test, so setup builds it once per suite and copies it: building it is ~60
# forks, a third of a test's cost, and the light CI runner is short of CPU
# (gd-zVb2CwXaE7).
build_fixture_tree() {
  BIN="${TMP}/bin"
  make_min_path "$BIN"
  make_helm_stub "$BIN"
  make_kubectl_stub "$BIN"
  make_gh_stub "$BIN"
  make_curl_stub "$BIN"

  # Fake checkout: release-deploy.sh resolves <chart-dir>/<values> against
  # `git -C SCRIPT_DIR rev-parse --show-toplevel`, so give it a real repo
  # with the expected relative layout instead of touching the real one.
  REPO="${TMP}/repo"
  mkdir -p "${REPO}/gardenio/scripts" "${REPO}/gardenio/helm/gardenio" "${REPO}/deploy-values"
  PATH="${BIN}:${PATH}" git -C "$REPO" init -q
  PATH="${BIN}:${PATH}" git -C "$REPO" config user.email t@t
  PATH="${BIN}:${PATH}" git -C "$REPO" config user.name t
  # setup() cp -a's this repo; a commit's detached auto-maintenance can hold
  # objects/maintenance.lock mid-copy and fail cp (tag 2026.309.16 checks).
  PATH="${BIN}:${PATH}" git -C "$REPO" config maintenance.auto false
  PATH="${BIN}:${PATH}" git -C "$REPO" config gc.auto 0
  cp "${SCRIPT_DIR}/release-deploy.sh" "${REPO}/gardenio/scripts/release-deploy.sh"
  cp "${SCRIPT_DIR}/verify-image-tags.sh" "${REPO}/gardenio/scripts/verify-image-tags.sh"
  chmod +x "${REPO}/gardenio/scripts/release-deploy.sh" "${REPO}/gardenio/scripts/verify-image-tags.sh"
  # The real pack script needs yq + helm and has its own suite
  # (scripts/pack-gardenio-chart_test.sh); here it is stubbed.
  mkdir -p "${REPO}/scripts"
  make_pack_stub "${REPO}/scripts/pack-gardenio-chart.sh"
  # The real theme generator needs python3 (stubbed in this suite) and has
  # its own tests (gardenio/scripts/keycloak-theme-configmap_test.py).
  make_theme_stub "${REPO}/gardenio/scripts/keycloak-theme-configmap.py"
  printf 'replicaCount: 3\n' > "${REPO}/deploy-values/gardenio-k3s.yaml"
  # Rule 4c: both edges as the tag ships them, live and in the manifest
  # alike (so a default run applies them and tests or reloads nothing).
  EDGE_FIX="${TMP}/edge"
  mkdir -p "${REPO}/gardenio/helm/gardenio-ziti" "${EDGE_FIX}/live"
  printf 'public-edge-7d9f\n' > "${EDGE_FIX}/pods-public-edge.txt"
  printf 'viewer-authproxy-a\nviewer-authproxy-b\n' > "${EDGE_FIX}/pods-gardenio.txt"
  edge_manifest public-edge public-edge default.conf 'server { listen 8443; }'
  edge_manifest viewer-authproxy gardenio default.conf.template 'server { listen 8097; }'
  edge_live public-edge public-edge default.conf 'server { listen 8443; }'
  edge_live gardenio viewer-authproxy default.conf.template 'server { listen 8097; }'
  ( cd "$REPO" && PATH="${BIN}:${PATH}" git add -A && PATH="${BIN}:${PATH}" git commit -q -m init )
}

# FIXTURE_TEMPLATE -- the suite's one build_fixture_tree, removed at exit.
FIXTURE_TEMPLATE=""

setup() {
  if [ -z "$FIXTURE_TEMPLATE" ]; then
    FIXTURE_TEMPLATE="$(mktemp -d)"
    trap 'rm -rf "$FIXTURE_TEMPLATE"' EXIT
    TMP="$FIXTURE_TEMPLATE"
    build_fixture_tree
  fi
  TMP="$(mktemp -d)"
  cp -a "${FIXTURE_TEMPLATE}/." "$TMP"
  BIN="${TMP}/bin"
  REPO="${TMP}/repo"
  EDGE_FIX="${TMP}/edge"

  HELM_CALL_LOG="${TMP}/helm-calls.log"
  KUBECTL_CALL_LOG="${TMP}/kubectl-calls.log"
  GH_CALL_LOG="${TMP}/gh-calls.log"
  CURL_CALL_LOG="${TMP}/curl-calls.log"
  CURL_BODY_LOG="${TMP}/curl-bodies.log"
  : > "$HELM_CALL_LOG"
  : > "$KUBECTL_CALL_LOG"
  : > "$GH_CALL_LOG"
  : > "$CURL_CALL_LOG"
  : > "$CURL_BODY_LOG"
  PACK_CALL_LOG="${TMP}/pack-calls.log"
  : > "$PACK_CALL_LOG"
  THEME_CALL_LOG="${TMP}/theme-calls.log"
  KUBECTL_APPLY_LOG="${TMP}/kubectl-apply-stdin.log"
  : > "$THEME_CALL_LOG"
  : > "$KUBECTL_APPLY_LOG"

  UNDER_TEST_IN_REPO="${REPO}/gardenio/scripts/release-deploy.sh"
  OVERLAY_JSON="${REPO}/deploy-values/gardenio-k3s.yaml.json"

  # Default fixtures: a healthy release one revision behind the target
  # calver, no values drift (both sides reduce to {} once global.imageTag is
  # set aside), green images.yml, ci.yml and checks.yml runs of the tag, and
  # pods already on the target tag.
  HELM_STATUS_FILE="${TMP}/helm-status.json"
  HELM_GET_VALUES_FILE="${TMP}/helm-get-values.json"
  GH_RUNS_FILE="${TMP}/gh-runs.json"
  KUBECTL_PODS_FILE="${TMP}/kubectl-pods.tsv"

  printf '{"version":5,"info":{"status":"deployed"}}' > "$HELM_STATUS_FILE"
  printf '{"global":{"imageTag":"2026.62.4"}}' > "$HELM_GET_VALUES_FILE"
  printf '{"workflow_runs":[{"id":101,"status":"completed","conclusion":"success","head_sha":"%s"}]}' "$TAG_SHA" > "$GH_RUNS_FILE"
  # ...whose ci.yml run really built: a heavy job's sentinel step ran.
  GH_JOBS_FILE="${TMP}/gh-jobs.json"
  printf '{"jobs":[{"name":"gate: superseded? + changed paths","steps":[{"name":"superseded -- run-wide skip","conclusion":"skipped"}]},{"name":"build + test (go) (kbeads)","steps":[{"name":"heavy work starting","conclusion":"success"}]}]}' > "$GH_JOBS_FILE"
  GH_TAG_SHA="$TAG_SHA"
  printf 'gardenio-controller-abc\tregistry.example/gardenio/controller:%s\n' "$CALVER" > "$KUBECTL_PODS_FILE"

  unset HELM_STATUS_FAIL HELM_GET_VALUES_FAIL HELM_UPGRADE_EXIT HELM_UPGRADE_OUTPUT
  unset HELM_ROLLBACK_EXIT HELM_ROLLBACK_OUTPUT KUBECTL_PAUSE_EXISTS GH_API_FAIL GH_JOBS_FAIL
  unset CURL_FAIL CURL_GET_FILE CURL_POST_FILE PY_IMPORT_YAML_EXIT KD_URL KD_TOKEN KD_OPERATED_BY KD_OIDC_TOKEN_URL KD_OIDC_CLIENT_ID KD_OIDC_CLIENT_SECRET_FILE KD_OIDC_CA_FILE CURL_TOKEN_FAIL
  unset HELM_STDERR_WARN HELM_STATUS_ERR HELM_STATUS_POST_FILE HELM_STATUS_POST_FAIL HELM_HISTORY_FILE HELM_HISTORY_FAIL
  unset KUBECTL_PAUSE_ERR PACK_FAIL PACK_NO_FILE THEME_FAIL KUBECTL_APPLY_FAIL
  unset KUBECTL_APPLY_FAIL_MATCH EDGE_EXEC_STALE EDGE_NGINX_T_FAIL EDGE_RELOAD_FAIL EDGE_CM_GET_FAIL
  EDGE_SYNC_TIMEOUT=5
  EDGE_SYNC_INTERVAL=1
}

# edge_manifest <name> <ns> <key> <conf> -- the tag's gardenio-ziti/<name>.yaml:
# the nginx ConfigMap <name> with <key>: <conf>, and a Deployment. The YAML
# is a placeholder; the yq stub serves the .json beside it.
edge_manifest() {
  local f="${REPO}/gardenio/helm/gardenio-ziti/${1}.yaml"
  printf '# fixture\n' > "$f"
  jq -nc --arg n "$1" --arg ns "$2" --arg k "$3" --arg v "$4" \
    '{apiVersion: "v1", kind: "ConfigMap", metadata: {name: $n, namespace: $ns}, data: {($k): $v}},
     {apiVersion: "apps/v1", kind: "Deployment", metadata: {name: $n, namespace: $ns}}' > "${f}.json"
}

# edge_live <ns> <name> <key> <conf> -- the live ConfigMap, with the server
# fields a snapshot must drop.
edge_live() {
  jq -nc --arg n "$2" --arg ns "$1" --arg k "$3" --arg v "$4" \
    '{apiVersion: "v1", kind: "ConfigMap", metadata: {name: $n, namespace: $ns, resourceVersion: "41", uid: "u-1"}, data: {($k): $v}}' \
    > "${EDGE_FIX}/live/${1}_${2}.json"
}

teardown() {
  rm -rf "$TMP"
}

# reaches_drift_check <bin-dir> -- puts the yq stub in place and a matching
# {} overlay fixture, so a test can get past rule 3b with no drift and
# exercise rules 4+ without caring about the drift check itself.
reaches_drift_check() {
  make_yq_stub "$BIN"
  printf '{}' > "$OVERLAY_JSON"
}

# run_under_test [args...] -- runs release-deploy.sh with the curated PATH
# and every stub's control vars wired through. Sets OUT and RC; the
# `|| RC=$?` keeps a non-zero exit from tripping the caller's set -e.
run_under_test() {
  RC=0
  OUT="$(PATH="${BIN}" \
    HELM_CALL_LOG="$HELM_CALL_LOG" HELM_STATUS_FILE="$HELM_STATUS_FILE" \
    HELM_STATUS_FAIL="${HELM_STATUS_FAIL:-0}" HELM_STATUS_ERR="${HELM_STATUS_ERR:-}" \
    HELM_STATUS_POST_FILE="${HELM_STATUS_POST_FILE:-}" HELM_STATUS_POST_FAIL="${HELM_STATUS_POST_FAIL:-0}" \
    HELM_STDERR_WARN="${HELM_STDERR_WARN:-0}" KUBECTL_PAUSE_ERR="${KUBECTL_PAUSE_ERR:-}" \
    HELM_GET_VALUES_FILE="$HELM_GET_VALUES_FILE" HELM_GET_VALUES_FAIL="${HELM_GET_VALUES_FAIL:-0}" \
    HELM_UPGRADE_EXIT="${HELM_UPGRADE_EXIT:-0}" HELM_UPGRADE_OUTPUT="${HELM_UPGRADE_OUTPUT:-}" \
    HELM_ROLLBACK_EXIT="${HELM_ROLLBACK_EXIT:-0}" HELM_ROLLBACK_OUTPUT="${HELM_ROLLBACK_OUTPUT:-}" \
    HELM_HISTORY_FILE="${HELM_HISTORY_FILE:-}" HELM_HISTORY_FAIL="${HELM_HISTORY_FAIL:-0}" \
    KUBECTL_CALL_LOG="$KUBECTL_CALL_LOG" KUBECTL_PAUSE_EXISTS="${KUBECTL_PAUSE_EXISTS:-0}" KUBECTL_PODS_FILE="$KUBECTL_PODS_FILE" \
    GH_CALL_LOG="$GH_CALL_LOG" GH_RUNS_FILE="$GH_RUNS_FILE" GH_API_FAIL="${GH_API_FAIL:-0}" GH_TAG_SHA="${GH_TAG_SHA:-}" \
    GH_JOBS_FILE="$GH_JOBS_FILE" GH_JOBS_FAIL="${GH_JOBS_FAIL:-0}" \
    CURL_CALL_LOG="$CURL_CALL_LOG" CURL_BODY_LOG="$CURL_BODY_LOG" CURL_FAIL="${CURL_FAIL:-0}" CURL_GET_FILE="${CURL_GET_FILE:-}" CURL_POST_FILE="${CURL_POST_FILE:-}" \
    PACK_CALL_LOG="$PACK_CALL_LOG" PACK_FAIL="${PACK_FAIL:-0}" PACK_NO_FILE="${PACK_NO_FILE:-0}" \
    THEME_CALL_LOG="$THEME_CALL_LOG" THEME_FAIL="${THEME_FAIL:-0}" PY_IMPORT_YAML_EXIT="${PY_IMPORT_YAML_EXIT:-0}" \
    KUBECTL_APPLY_LOG="$KUBECTL_APPLY_LOG" KUBECTL_APPLY_FAIL="${KUBECTL_APPLY_FAIL:-0}" \
    KUBECTL_APPLY_FAIL_MATCH="${KUBECTL_APPLY_FAIL_MATCH:-}" EDGE_FIX="$EDGE_FIX" \
    EDGE_SYNC_TIMEOUT="$EDGE_SYNC_TIMEOUT" EDGE_SYNC_INTERVAL="$EDGE_SYNC_INTERVAL" \
    EDGE_EXEC_STALE="${EDGE_EXEC_STALE:-0}" EDGE_NGINX_T_FAIL="${EDGE_NGINX_T_FAIL:-}" \
    EDGE_RELOAD_FAIL="${EDGE_RELOAD_FAIL:-}" EDGE_CM_GET_FAIL="${EDGE_CM_GET_FAIL:-0}" \
    KD_URL="${KD_URL:-}" KD_TOKEN="${KD_TOKEN:-}" KD_OPERATED_BY="${KD_OPERATED_BY:-}" \
    KD_OIDC_TOKEN_URL="${KD_OIDC_TOKEN_URL:-}" KD_OIDC_CLIENT_ID="${KD_OIDC_CLIENT_ID:-}" \
    KD_OIDC_CLIENT_SECRET_FILE="${KD_OIDC_CLIENT_SECRET_FILE:-}" KD_OIDC_CA_FILE="${KD_OIDC_CA_FILE:-}" CURL_TOKEN_FAIL="${CURL_TOKEN_FAIL:-0}" \
    bash "$UNDER_TEST_IN_REPO" "$@" 2>&1)" || RC=$?
}

assert_rc() {
  local want="$1" desc="$2"
  if [ "$RC" -eq "$want" ]; then
    pass "$desc"
  else
    fail "$desc" "expected exit $want, got $RC. Output:
$OUT"
  fi
}

assert_out_contains() {
  local needle="$1" desc="$2"
  if grep -qF -- "$needle" <<<"$OUT"; then
    pass "$desc"
  else
    fail "$desc" "expected output to contain '$needle'. Output:
$OUT"
  fi
}

assert_file_contains() {
  local file="$1" needle="$2" desc="$3"
  if [ -f "$file" ] && grep -qF -- "$needle" "$file"; then
    pass "$desc"
  else
    fail "$desc" "expected $file to contain '$needle'. Contents:
$(cat "$file" 2>/dev/null || echo '(missing)')"
  fi
}

assert_file_not_contains() {
  local file="$1" needle="$2" desc="$3"
  if [ -f "$file" ] && grep -qF -- "$needle" "$file"; then
    fail "$desc" "expected $file NOT to contain '$needle'. Contents:
$(cat "$file")"
  else
    pass "$desc"
  fi
}
