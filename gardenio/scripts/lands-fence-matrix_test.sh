#!/usr/bin/env bash
set -euo pipefail

# lands-fence-matrix_test.sh — hermetic self-test of lands-fence-matrix.sh.
# A fake kubectl plays two clusters: a fenced one, where every probe is
# refused and the script must pass every check, and a leaky one, where every
# probe gets through and the script must fail every check. No cluster, no
# network.

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cat >"$WORK/kubectl" <<'FAKE'
#!/usr/bin/env bash
# Fake kubectl for lands-fence-matrix_test.sh; FENCE_FAKE=fenced|leaky.
mode="${FENCE_FAKE:?}"
state="${FENCE_STATE:?}"
ns=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  [ "${args[$i]}" = "-n" ] && ns="${args[$((i + 1))]}"
done
case "$1" in
  get)
    case "$2" in
      pods)
        case "$*" in
          *field-selector*) echo "agent-0" ;;
          *podIP*) [ "$ns" = "land-fence-b" ] && printf '10.0.0.7 8080 9090\n' ;;
          *"-o name"*) echo "pod/gardenio-keycloak-0" ;;
        esac ;;
      svc) printf 'gardenio-beads 8080\ngardenio-postgres 5432\ngardenio-grafana 3000\n' ;;
    esac
    exit 0 ;;
esac
script="${args[${#args[@]} - 1]}"
leak() { [ "$mode" = leaky ] && echo "$1" || echo "$2"; }
case "$script" in
  *kcadm*)
    cat >/dev/null
    case "$script" in
      *default-roles*) echo offline_access; echo uma_authorization; leak gardenio-viewer "" ;;
      *"create users"*) echo offline_access; echo gardenio-project-member; leak gardenio-viewer "" ;;
    esac ;;
  *"--transport grpc"*)
    printf '== own\n[]\nrc=0\n'
    printf '== configs\n%s\n' "$(leak '[{"key":"bridge_token:x"}]' 'Error: rpc error: code = PermissionDenied')"; leak rc=0 rc=1
    printf '== config\n'; leak rc=0 rc=1
    printf '== other\n%s\nrc=0\n' "$(leak '[{"labels":["project:fence-b"]}]' '[]')" ;;
  *sha256sum*) leak samekey "key-$ns" ;;
  */v1/messages*) echo 1 >"$state/called" ;;
  */key/info*)
    spend=1.0
    [ "$ns" = land-fence-a ] && [ -e "$state/called" ] && spend=1.5
    [ "$mode" = leaky ] && [ -e "$state/called" ] && spend=2.0
    alias="lands-fence-a"; [ "$ns" = land-fence-b ] && alias="lands-fence-b"
    printf '{"key_alias": "%s", "spend": %s}\n' "$(leak lands-fence-b "$alias")" "$spend" ;;
  *BEADS_AUTH_TOKEN*)
    case "$script" in
      *"/v1/lands/v1/lands/fence-b'"*) leak 200 404; echo '{}' ;;
      *"/v1/beads?labels="*) echo 200; leak '{"beads":[{"labels":["project:fence-b"]}]}' '{"beads":[]}' ;;
      *) leak 200 403; echo '{}' ;;
    esac ;;
  *api.github.com/repos/o/fence-a*) echo 200 ;;
  *api.github.com/repos/o/fence-b*) leak 200 404 ;;
  *169.254.169.254*) leak 200 000 ;;
  *serviceaccount/token*) leak yes no ;;
  *kubernetes.default.svc*) leak 200 000 ;;
  env) printf 'BEADS_HTTP_ADDR=http://gardenio-beads.gardenio.svc:8080\nANTHROPIC_BASE_URL=http://gardenio-litellm.gardenio.svc:4000\n' ;;
  *curl*http://*) leak 200 000 ;;
esac
FAKE
chmod +x "$WORK/kubectl"

run() { # MODE -> output, exit status in $WORK/rc
  mkdir -p "$WORK/state-$1"
  set +e
  KUBECTL="$WORK/kubectl" FENCE_FAKE="$1" FENCE_STATE="$WORK/state-$1" \
    KC_ADMIN_USER=admin KC_ADMIN_PASSWORD=pw LLM_POLL_SECONDS=0 LLM_SETTLE_SECONDS=2 \
    "$HERE/lands-fence-matrix.sh" --land-a=fence-a --land-b=fence-b --repo-a=o/fence-a --repo-b=o/fence-b >"$WORK/out-$1" 2>&1
  echo $? >"$WORK/rc-$1"
  set -e
}

failures=0
checks="N1 N2 N3 N4 D1 D2 D3 G1 L1 L2 K1 K2"

run fenced
for c in $checks; do
  grep -q "^PASS $c:" "$WORK/out-fenced" || { echo "fenced cluster: $c did not pass"; failures=$((failures + 1)); }
done
[ "$(cat "$WORK/rc-fenced")" = 0 ] || { echo "fenced cluster: exit $(cat "$WORK/rc-fenced"), want 0"; failures=$((failures + 1)); }

run leaky
for c in $checks; do
  grep -q "^FAIL $c:" "$WORK/out-leaky" || { echo "leaky cluster: $c did not fail"; failures=$((failures + 1)); }
done
[ "$(cat "$WORK/rc-leaky")" = 1 ] || { echo "leaky cluster: exit $(cat "$WORK/rc-leaky"), want 1"; failures=$((failures + 1)); }

# The Keycloak password never reaches a command line.
if grep -rq "pw" "$WORK/out-fenced" "$WORK/out-leaky"; then
  echo "the Keycloak password appears in the output"
  failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
  echo "--- fenced"; cat "$WORK/out-fenced"
  echo "--- leaky"; cat "$WORK/out-leaky"
  exit 1
fi
echo "lands-fence-matrix_test: ok (fenced passes all $(echo "$checks" | wc -w) checks, leaky fails all)"
