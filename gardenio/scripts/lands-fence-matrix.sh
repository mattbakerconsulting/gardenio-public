#!/usr/bin/env bash
set -euo pipefail

# lands-fence-matrix.sh — the live half of the cross-land negative matrix
# (Lands Track B step 10.6, gd-USqiG2sdPU; spec §5.6 "Proving the fence").
# The 10.10 Fences check-in runs it against two test lands, A and B, each
# with a running tenant agent pod.
#
# The daemon half runs in CI: TestCrossLandMatrix
# (kbeads/internal/server/cross_land_matrix_test.go) walks every daemon route
# as A's member, owner, guest, land principal and agent against B's beads.
# What only the live cluster can show is here:
#
#   N1  from A's pod: no port of any pod in land-B answers
#   N2  from A's pod: the Kubernetes API does not answer, or refuses a pod
#       that holds no ServiceAccount token
#   N3  from A's pod: the instance metadata IP does not answer
#   N4  from A's pod: no service in the release namespace answers but the
#       ones A's own pod env addresses (daemon, coop, bridge, LiteLLM, NATS)
#   D1  from A's pod, on its own daemon token: every admin route is refused,
#       and so is every read of a bearer or bus-handler config row
#       (namespace bridge_token, fleet_token, bus.handler; B's land bearer
#       by key: gd-zdo4DIeGWR)
#   D2  from A's pod: B's land is 404 on the Lands API and no bead labelled
#       project:B is listed
#   D3  from A's pod, over the daemon's gRPC port on its own token: A's own
#       list answers, ListConfigs and GetConfig are refused, and nothing of
#       B's is listed (gd-c1DOWNYy7W; threat model precondition 3)
#   G1  A's Lands App token: B's repo is 404 (and A's own repo 200)
#   L1  A's and B's LiteLLM keys differ, and A's key's alias names A only
#   L2  a one-token call on A's key moves A's spend and not B's
#   K1  Keycloak's default-roles-<realm> carries no gardenio-* composite
#   K2  a fresh tenant user (made here, in lands-tenants and project-A,
#       then deleted) holds no gardenio-viewer, -operator or -admin
#
# K1 and K2 are the drift found by hand on 10-25 (gd-llFKMS8Pkc): a leftover
# gardenio-viewer composite on default-roles-gardenio made every new user a
# global reader, and nothing in the chart removes it.
#
# Usage:
#   gardenio/scripts/lands-fence-matrix.sh --land-a=fence-a --land-b=fence-b \
#     --repo-a=owner/fence-a --repo-b=owner/fence-b
#
# Flags (and their env twins):
#   --land-a= --land-b=          LAND_A, LAND_B (required)
#   --pod-a= --pod-b=            POD_A, POD_B: a running pod in land-<land>
#                                (default: the first Running one)
#   --repo-a= --repo-b=          REPO_A, REPO_B: each land's repository;
#                                without them G1 is SKIP
#   --namespace=                 NAMESPACE, the release namespace (gardenio)
#   --grpc-port=                 GRPC_PORT, the daemon's gRPC port (9090) when
#                                A's pod sets no BEADS_GRPC_ADDR
#   --realm=                     REALM, the Keycloak realm (gardenio)
#   --keycloak-pod=              KC_POD (default: the first pod named *keycloak*)
#   --no-keycloak-write          skip K2 (it creates and deletes one user)
#   --no-llm-call                skip L2 (it spends one token on A's key)
# K1 and K2 log in to Keycloak with KC_ADMIN_USER and KC_ADMIN_PASSWORD from
# the caller's env, passed to the pod on stdin, never on a command line;
# without them K1 and K2 are SKIP. The caller needs kubectl exec into land-A,
# land-B and the Keycloak pod.
#
# Prints one PASS, FAIL or SKIP line per check, with what it saw. Exit 0
# only when nothing failed; 1 on any FAIL; 2 on a usage error.

KUBECTL="${KUBECTL:-kubectl}"
NAMESPACE="${NAMESPACE:-gardenio}"
GRPC_PORT="${GRPC_PORT:-9090}"
REALM="${REALM:-gardenio}"
LAND_A="${LAND_A:-}"
LAND_B="${LAND_B:-}"
POD_A="${POD_A:-}"
POD_B="${POD_B:-}"
REPO_A="${REPO_A:-}"
REPO_B="${REPO_B:-}"
KC_POD="${KC_POD:-}"
KC_SERVER="${KC_SERVER:-http://127.0.0.1:8080/auth}"
KC_WRITE=1
LLM_CALL=1
# L2's time box: LiteLLM's spend reaches /key/info within a few minutes.
LLM_SETTLE_SECONDS="${LLM_SETTLE_SECONDS:-360}"
LLM_POLL_SECONDS="${LLM_POLL_SECONDS:-15}"

for arg in "$@"; do
  case "$arg" in
    --land-a=*) LAND_A="${arg#*=}" ;;
    --land-b=*) LAND_B="${arg#*=}" ;;
    --pod-a=*) POD_A="${arg#*=}" ;;
    --pod-b=*) POD_B="${arg#*=}" ;;
    --repo-a=*) REPO_A="${arg#*=}" ;;
    --repo-b=*) REPO_B="${arg#*=}" ;;
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --grpc-port=*) GRPC_PORT="${arg#*=}" ;;
    --realm=*) REALM="${arg#*=}" ;;
    --keycloak-pod=*) KC_POD="${arg#*=}" ;;
    --no-keycloak-write) KC_WRITE=0 ;;
    --no-llm-call) LLM_CALL=0 ;;
    -h|--help) sed -n '3,63p' "$0"; exit 0 ;;
    *) echo "Unknown flag: $arg" >&2; exit 2 ;;
  esac
done
if [ -z "$LAND_A" ] || [ -z "$LAND_B" ] || [ "$LAND_A" = "$LAND_B" ]; then
  echo "need --land-a and --land-b, two different lands" >&2
  exit 2
fi
NS_A="land-$LAND_A"
NS_B="land-$LAND_B"

pass=0
fail=0
skip=0
result() { # VERDICT CHECK WHAT
  echo "$1 $2: $3"
  case "$1" in PASS) pass=$((pass + 1)) ;; FAIL) fail=$((fail + 1)) ;; *) skip=$((skip + 1)) ;; esac
}

# has TEXT NEEDLE: whether TEXT contains NEEDLE. A pattern match, not a
# pipe into grep -q, whose early exit is a SIGPIPE that pipefail would read
# as "not found".
has() { case "$1" in *"$2"*) return 0 ;; esac; return 1; }

# podsh NS POD SCRIPT: runs SCRIPT under sh in POD's first container. Stdin
# goes through, so a secret can reach the pod without a command line.
podsh() { "$KUBECTL" exec -i -n "$1" "$2" -- sh -c "$3"; }

first_running_pod() {
  "$KUBECTL" get pods -n "$1" --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

# code_of NS POD URL: the HTTP status the pod gets for URL, 000 when nothing
# answers within 3 s.
code_of() {
  podsh "$1" "$2" "curl -sS -k -m 3 -o /dev/null -w '%{http_code}' '$3' 2>/dev/null || true" 2>/dev/null || true
}

[ -n "$POD_A" ] || POD_A="$(first_running_pod "$NS_A")"
[ -n "$POD_B" ] || POD_B="$(first_running_pod "$NS_B")"
if [ -z "$POD_A" ] || [ -z "$POD_B" ]; then
  echo "FAIL setup: need a Running pod in $NS_A (got '${POD_A}') and in $NS_B (got '${POD_B}'); start a tenant agent in each" >&2
  exit 1
fi
echo "lands-fence-matrix: A=$LAND_A ($NS_A/$POD_A) B=$LAND_B ($NS_B/$POD_B) release=$NAMESPACE"

# --- N1: B's pods ---------------------------------------------------------
reached=""
probed=0
while read -r ip ports; do
  [ -n "$ip" ] || continue
  [ -n "$ports" ] || ports=8080
  for port in $ports; do
    probed=$((probed + 1))
    c="$(code_of "$NS_A" "$POD_A" "http://$ip:$port/")"
    [ "$c" = "000" ] || reached="$reached $ip:$port=$c"
  done
done < <("$KUBECTL" get pods -n "$NS_B" -o jsonpath='{range .items[*]}{.status.podIP}{" "}{.spec.containers[*].ports[*].containerPort}{"\n"}{end}')
if [ "$probed" -eq 0 ]; then
  result FAIL N1 "no pod IP in $NS_B to probe"
elif [ -n "$reached" ]; then
  result FAIL N1 "A's pod reached B's pods:$reached"
else
  result PASS N1 "$probed port(s) on B's pods: nothing answered"
fi

# --- N2: the Kubernetes API -----------------------------------------------
c="$(code_of "$NS_A" "$POD_A" "https://kubernetes.default.svc/api")"
sa="$(podsh "$NS_A" "$POD_A" 'test -e /var/run/secrets/kubernetes.io/serviceaccount/token && echo yes || echo no' 2>/dev/null || echo unknown)"
case "$c/$sa" in
  000/*) result PASS N2 "kubernetes.default.svc did not answer" ;;
  401/no|403/no) result PASS N2 "kubernetes.default.svc answered $c to a pod with no ServiceAccount token" ;;
  *) result FAIL N2 "kubernetes.default.svc answered $c; ServiceAccount token mounted: $sa" ;;
esac

# --- N3: the metadata IP --------------------------------------------------
c="$(podsh "$NS_A" "$POD_A" "curl -sS -m 3 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' -o /dev/null -w '%{http_code}' http://169.254.169.254/latest/api/token 2>/dev/null || true" 2>/dev/null || true)"
if [ "$c" = "000" ] || [ -z "$c" ]; then
  result PASS N3 "169.254.169.254 did not answer"
else
  result FAIL N3 "169.254.169.254 answered $c"
fi

# --- N4: the release namespace's services ---------------------------------
# The services A may reach are the ones its own env addresses.
allowed="$(podsh "$NS_A" "$POD_A" 'env' 2>/dev/null |
  sed -nE 's#^(BEADS_HTTP_ADDR|BEADS_GRPC_ADDR|COOP_BROKER_URL|COOP_MUX_URL|SLACK_BRIDGE_URL|ANTHROPIC_BASE_URL|BEADS_NATS_URL|COOP_NATS_URL)=([a-z]+://)?([^.:/]+).*#\3#p' |
  sort -u | tr '\n' ' ')"
reached=""
probed=0
while read -r svc ports; do
  [ -n "$svc" ] || continue
  case " $allowed " in *" $svc "*) continue ;; esac
  for port in $ports; do
    probed=$((probed + 1))
    c="$(code_of "$NS_A" "$POD_A" "http://$svc.$NAMESPACE.svc:$port/")"
    [ "$c" = "000" ] || reached="$reached $svc:$port=$c"
  done
done < <("$KUBECTL" get svc -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.ports[*].port}{"\n"}{end}')
if [ -n "$reached" ]; then
  result FAIL N4 "A's pod reached services it has no address for:$reached (allowed: $allowed)"
else
  result PASS N4 "$probed port(s) on other $NAMESPACE services: nothing answered (allowed: $allowed)"
fi

# --- D1, D2: the daemon on A's own token -----------------------------------
# The in-pod snippets below are quoted heredocs: they expand in the pod, not
# here.
daemon_get="$(cat <<'SH'
curl -sS -m 10 -H "Authorization: Bearer $BEADS_AUTH_TOKEN" -o /tmp/fence.body -w "%{http_code}" "http://${BEADS_HTTP_ADDR#http://}$1" 2>/dev/null; echo; head -c 4000 /tmp/fence.body; rm -f /tmp/fence.body
SH
)"
admitted=""
for route in /v1/configs /v1/fleet-tokens /v1/bridge-tokens /v1/stats /v1/taints /v1/operators \
  /v1/admin/audit-blocks-cycles /v1/health/snapshot /v1/bus/status /v1/charts /v1/metrics/acceptance \
  "/v1/configs?namespace=bridge_token" "/v1/configs?namespace=fleet_token" "/v1/configs?namespace=bus.handler" \
  "/v1/configs/bridge_token:land-$LAND_B"; do
  c="$(podsh "$NS_A" "$POD_A" "set -- '$route'; $daemon_get" 2>/dev/null | sed -n 1p)"
  case "$c" in 2*) admitted="$admitted $route=$c" ;; esac
done
if [ -n "$admitted" ]; then
  result FAIL D1 "A's daemon token was admitted to:$admitted"
else
  result PASS D1 "every admin route and sensitive config read refused A's daemon token"
fi
out="$(podsh "$NS_A" "$POD_A" "set -- '/v1/lands/v1/lands/$LAND_B'; $daemon_get" 2>/dev/null || true)"
c="$(printf '%s\n' "$out" | sed -n 1p)"
list="$(podsh "$NS_A" "$POD_A" "set -- '/v1/beads?labels=project:$LAND_B&limit=20'; $daemon_get" 2>/dev/null || true)"
if [ "$c" = "404" ] && ! has "$(printf '%s' "$list" | sed 1d)" "project:$LAND_B"; then
  result PASS D2 "B's land is 404 to A, and A lists no bead labelled project:$LAND_B"
else
  result FAIL D2 "B's land answered A with $c; list of project:$LAND_B: $(printf '%s' "$list" | cut -c1-200)"
fi

# --- D3: the daemon's gRPC port on A's own token ----------------------------
# The land fence opens the daemon's gRPC port to tenant pods, and the HTTP
# gate does not cover it (gd-c1DOWNYy7W). kd in A's pod dials the daemon host
# on $GRPC_PORT; its own list is the positive control, so a port that never
# answers fails here instead of passing vacuously.
daemon_grpc="$(cat <<'SH'
command -v kd >/dev/null 2>&1 || { echo NO-KD; exit 0; }
h="${BEADS_HTTP_ADDR#http://}"; h="${h%%[:/]*}"; s="${BEADS_GRPC_ADDR:-$h:$1}"
g() { kd --transport grpc --server "$s" "$@" 2>&1; echo "rc=$?"; }
echo "== own"; g list --limit 1 --json
echo "== configs"; g config list bridge_token
echo "== config"; g config get "bridge_token:land-$2"
echo "== other"; g list --project "$2" --limit 20 --json
SH
)"
out="$(podsh "$NS_A" "$POD_A" "set -- '$GRPC_PORT' '$LAND_B'; $daemon_grpc" 2>/dev/null || true)"
section() { printf '%s\n' "$out" | sed -n "/^== $1\$/,/^rc=/p"; }
if has "$out" NO-KD; then
  result SKIP D3 "no kd in A's pod"
elif ! has "$(section own)" "rc=0"; then
  result FAIL D3 "A's own list over gRPC failed, so the check proves nothing: $(section own | tr '\n' ' ' | cut -c1-200)"
elif has "$(section configs)" "rc=0" || has "$(section config)" "rc=0"; then
  result FAIL D3 "A's daemon token read config over gRPC: $(section configs | tr '\n' ' ' | cut -c1-200)"
elif has "$(section other)" "project:$LAND_B"; then
  result FAIL D3 "A listed project:$LAND_B over gRPC: $(section other | tr '\n' ' ' | cut -c1-200)"
else
  result PASS D3 "over gRPC A lists its own land, is refused config, and lists nothing of $LAND_B"
fi

# --- G1: A's Lands App token -----------------------------------------------
if [ -z "$REPO_A" ] || [ -z "$REPO_B" ]; then
  result SKIP G1 "no --repo-a/--repo-b"
else
  gh_code() {
    podsh "$NS_A" "$POD_A" "curl -sS -m 10 -o /dev/null -w '%{http_code}' -H \"Authorization: token \$(cat /etc/git-creds/github/token)\" 'https://api.github.com/repos/$1' 2>/dev/null || true" 2>/dev/null || true
  }
  own="$(gh_code "$REPO_A")"
  other="$(gh_code "$REPO_B")"
  if [ "$own" = "200" ] && { [ "$other" = "404" ] || [ "$other" = "403" ]; }; then
    result PASS G1 "A's token reads $REPO_A (200) and not $REPO_B ($other)"
  else
    result FAIL G1 "A's token: $REPO_A answered $own (want 200), $REPO_B answered $other (want 404)"
  fi
fi

# --- L1, L2: LiteLLM keys --------------------------------------------------
# The land key rides in ANTHROPIC_CUSTOM_HEADERS as x-litellm-api-key; only
# its hash leaves the pod. key_of defines key, which prints it.
key_of="$(cat <<'SH'
key() { printf "%s" "$ANTHROPIC_CUSTOM_HEADERS" | sed -nE "s/.*x-litellm-api-key: *Bearer +([^ ,;]+).*/\1/p"; }
SH
)"
key_info="$key_of
$(cat <<'SH'
curl -sS -m 10 -H "Authorization: Bearer $(key)" "${ANTHROPIC_BASE_URL%/}/key/info" 2>/dev/null
SH
)"
key_call="$key_of
$(cat <<'SH'
curl -sS -m 60 -o /dev/null -H "Authorization: Bearer $(key)" -H "Content-Type: application/json" "${ANTHROPIC_BASE_URL%/}/v1/messages" -d "{\"model\":\"${ANTHROPIC_MODEL:-claude-haiku-4-5-20251001}\",\"max_tokens\":1,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
SH
)"
hash_a="$(podsh "$NS_A" "$POD_A" "$key_of
key | sha256sum | cut -c1-16" 2>/dev/null || true)"
hash_b="$(podsh "$NS_B" "$POD_B" "$key_of
key | sha256sum | cut -c1-16" 2>/dev/null || true)"
empty_hash="$(printf '' | sha256sum | cut -c1-16)"
info_a="$(podsh "$NS_A" "$POD_A" "$key_info" 2>/dev/null || true)"
alias_a="$(printf '%s' "$info_a" | sed -nE 's/.*"key_alias": *"([^"]*)".*/\1/p' | sed -n 1p)"
if [ -z "$hash_a" ] || [ "$hash_a" = "$empty_hash" ] || [ "$hash_b" = "$empty_hash" ]; then
  result FAIL L1 "a land pod carries no LiteLLM key in ANTHROPIC_CUSTOM_HEADERS"
elif [ "$hash_a" = "$hash_b" ]; then
  result FAIL L1 "A and B run on the same LiteLLM key"
elif has "$info_a" "$LAND_B" || [ "$alias_a" != "lands-$LAND_A" ]; then
  result FAIL L1 "A's key's alias is '$alias_a' (want lands-$LAND_A), or its info names $LAND_B"
else
  result PASS L1 "A and B hold different keys; A's alias is $alias_a"
fi

spend_of() { # NS POD -> the key's spend, as LiteLLM reports it
  podsh "$1" "$2" "$key_info" 2>/dev/null | sed -nE 's/.*"spend": *([0-9.eE+-]+).*/\1/p' | sed -n 1p
}
if [ "$LLM_CALL" -eq 0 ]; then
  result SKIP L2 "--no-llm-call"
else
  a0="$(spend_of "$NS_A" "$POD_A")"
  b0="$(spend_of "$NS_B" "$POD_B")"
  podsh "$NS_A" "$POD_A" "$key_call" >/dev/null 2>&1 || true
  deadline=$((SECONDS + LLM_SETTLE_SECONDS))
  a1="$a0"
  while [ "$a1" = "$a0" ] && [ "$SECONDS" -lt "$deadline" ]; do
    sleep "$LLM_POLL_SECONDS"
    a1="$(spend_of "$NS_A" "$POD_A")"
  done
  b1="$(spend_of "$NS_B" "$POD_B")"
  if [ -z "$a0" ] || [ -z "$b0" ]; then
    result FAIL L2 "could not read a key's spend (A '$a0', B '$b0')"
  elif [ "$a1" = "$a0" ]; then
    result FAIL L2 "A's spend stayed $a0 for ${LLM_SETTLE_SECONDS}s after its call; the check proves nothing"
  elif [ "$b1" != "$b0" ]; then
    result FAIL L2 "A's call moved A $a0 -> $a1 and B $b0 -> $b1"
  else
    result PASS L2 "A's call moved A $a0 -> $a1; B stayed $b0"
  fi
fi

# --- K1, K2: Keycloak ------------------------------------------------------
[ -n "$KC_POD" ] || KC_POD="$("$KUBECTL" get pods -n "$NAMESPACE" -o name 2>/dev/null | sed -n 's#^pod/##; /keycloak/p' | sed -n 1p)"
if [ -z "${KC_ADMIN_USER:-}" ] || [ -z "${KC_ADMIN_PASSWORD:-}" ] || [ -z "$KC_POD" ]; then
  result SKIP K1 "no KC_ADMIN_USER/KC_ADMIN_PASSWORD, or no Keycloak pod"
  result SKIP K2 "no KC_ADMIN_USER/KC_ADMIN_PASSWORD, or no Keycloak pod"
else
  # kc_login: the Keycloak pod's preamble. It reads the user and password from
  # stdin, logs in to the master realm at $1 and defines kc for what follows.
  kc_login="$(cat <<'SH'
read -r u; read -r p
c=/tmp/fence-kcadm.config
kc() { /opt/keycloak/bin/kcadm.sh "$@" --config "$c"; }
kc config credentials --server "$1" --realm master --user "$u" --password "$p" >/dev/null 2>&1 || { echo LOGIN-FAILED; exit 0; }
SH
)"
  # kc SCRIPT: runs SCRIPT in the Keycloak pod after a master-realm login; the
  # password arrives on stdin.
  kc() {
    printf '%s\n%s\n' "$KC_ADMIN_USER" "$KC_ADMIN_PASSWORD" | podsh "$NAMESPACE" "$KC_POD" "set -- '$KC_SERVER'
$kc_login
$1
rm -f \"\$c\""
  }
  out="$(kc "kc get roles/default-roles-$REALM/composites -r $REALM --fields name --format csv --noquotes" 2>/dev/null || true)"
  bad="$(printf '%s\n' "$out" | grep -E '^gardenio-' | tr '\n' ' ' || true)"
  if [ -z "$out" ] || has "$out" LOGIN-FAILED; then
    result FAIL K1 "could not read default-roles-$REALM ($(printf '%s' "$out" | cut -c1-120))"
  elif [ -n "$bad" ]; then
    result FAIL K1 "default-roles-$REALM carries $bad: every new user gets them"
  else
    result PASS K1 "default-roles-$REALM carries no gardenio-* role ($(printf '%s' "$out" | tr '\n' ' '))"
  fi

  if [ "$KC_WRITE" -eq 0 ]; then
    result SKIP K2 "--no-keycloak-write"
  else
    probe="fence-probe-$(date +%s)"
    out="$(kc "
      uid=\$(kc create users -r $REALM -s username=$probe -s email=$probe@fence.invalid -s enabled=true -i 2>/dev/null) || { echo CREATE-FAILED; exit 0; }
      for g in lands-tenants project-$LAND_A; do
        gid=\$(kc get groups -r $REALM -q search=\$g --fields id,name --format csv --noquotes 2>/dev/null | awk -F, -v n=\$g '\$2==n{print \$1; exit}')
        [ -n \"\$gid\" ] && kc update users/\$uid/groups/\$gid -r $REALM -s realm=$REALM -s userId=\$uid -s groupId=\$gid -n >/dev/null 2>&1 || echo NO-GROUP-\$g
      done
      kc get users/\$uid/role-mappings/realm/composite -r $REALM --fields name --format csv --noquotes 2>/dev/null
      kc delete users/\$uid -r $REALM >/dev/null 2>&1 || echo DELETE-FAILED
    " 2>/dev/null || true)"
    roles="$(printf '%s\n' "$out" | grep -vE '^(NO-GROUP|DELETE-FAILED|CREATE-FAILED|LOGIN-FAILED)' | tr '\n' ' ')"
    if has "$out" LOGIN-FAILED || has "$out" CREATE-FAILED || has "$out" NO-GROUP; then
      result FAIL K2 "could not make the probe user: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
    elif has "$out" gardenio-viewer || has "$out" gardenio-operator || has "$out" gardenio-admin; then
      result FAIL K2 "a fresh tenant user holds: $roles"
    else
      result PASS K2 "a fresh tenant user of $LAND_A holds only: $roles"
    fi
    if has "$out" DELETE-FAILED; then
      result FAIL K2-cleanup "probe user $probe was not deleted; delete it by hand"
    fi
  fi
fi

echo "lands-fence-matrix: $pass passed, $fail failed, $skip skipped"
[ "$fail" -eq 0 ]
