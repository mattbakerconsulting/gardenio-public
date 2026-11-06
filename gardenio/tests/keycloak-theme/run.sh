#!/usr/bin/env bash
# Keycloak login-theme preview harness runner (plan gd-QXq1fjYuRD, step S1b).
#
#   ./run.sh                      # image from the chart (keycloak.runtime.image)
#   KC_IMAGE=quay.io/keycloak/keycloak KC_TAG=26.5.2 ./run.sh
#   ./run.sh -- tests/screens.spec.js --project=mobile-dark   # extra Playwright args
#
# Starts a throwaway Keycloak in Docker with the repo theme mounted, imports
# realm/gardenio-test-realm.json, waits for /health/ready, runs Playwright, and
# always stops the container. Outputs (all under this directory):
#   screenshots/  axe-report/  playwright-report/  kc.log  realm-webauthn.json
#   realm-webauthn-after-hook.json  flow-executions.json  browser-builtin-executions.json
#   conditional-credential-config.json
#
# Every credential here is a throwaway for a container that lives for minutes.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
CHART_VALUES="$REPO/gardenio/helm/gardenio/values.yaml"
DEPLOY_VALUES="$REPO/deploy-values/gardenio-k3s.yaml"
THEME_DIR="$REPO/gardenio/keycloak-theme/gardenio"
REALM_FILE="$HERE/realm/gardenio-test-realm.json"

KC_HTTP_PORT="${KC_HTTP_PORT:-8080}"
KC_MGMT_PORT="${KC_MGMT_PORT:-9000}"
READY_TIMEOUT="${READY_TIMEOUT:-240}"
NAME="kc-theme-preview-$$"

# TEST-ONLY bootstrap admin for the master realm of a container that is deleted
# when this script exits.
export KC_ADMIN_USER="preview-admin"
export KC_ADMIN_PASSWORD="preview-only-fake-bootstrap-pw"

# --- Step 1: pick the image the chart would run (dispatch inputs override) ---
# deploy-values wins over the chart default, exactly like helm -f would.
chart_val() { # <yq path>
  local v=""
  [ -f "$DEPLOY_VALUES" ] && v="$(yq -r "$1 // \"\"" "$DEPLOY_VALUES")"
  [ -n "$v" ] || v="$(yq -r "$1 // \"\"" "$CHART_VALUES")"
  printf '%s' "$v"
}
# A pinned digest belongs to the chart's tag; drop it when either is overridden.
OVERRIDDEN=0
{ [ -n "${KC_IMAGE:-}" ] || [ -n "${KC_TAG:-}" ]; } && OVERRIDDEN=1
if [ -z "${KC_IMAGE:-}" ]; then
  reg="$(chart_val '.keycloak.runtime.image.registry')"
  repo="$(chart_val '.keycloak.runtime.image.repository')"
  KC_IMAGE="${reg:+$reg/}$repo"
fi
[ -n "${KC_TAG:-}" ] || KC_TAG="$(chart_val '.keycloak.runtime.image.tag')"
KC_DIGEST=""
[ "$OVERRIDDEN" = 1 ] || KC_DIGEST="$(chart_val '.keycloak.runtime.image.digest')"
IMAGE_REF="$KC_IMAGE:$KC_TAG"
[ -z "$KC_DIGEST" ] || IMAGE_REF="$IMAGE_REF@$KC_DIGEST"
export KC_TAG
echo "== Keycloak image: $IMAGE_REF"

# --- Render the realm: passkeys flag only on Keycloak >= 26.4 ----------------
# An older server rejects the unknown realm field on import, so it is added
# only when the tag says it is understood. Non-numeric tags (latest, nightly)
# are assumed new.
WORK="$(mktemp -d)"
chmod 755 "$WORK"
# Keycloak >= 26.5 insists the import file is named <realm>-realm.json.
tag_ge_26_4() {
  if [[ "$KC_TAG" =~ ^([0-9]+)\.([0-9]+) ]]; then
    local maj="${BASH_REMATCH[1]}" min="${BASH_REMATCH[2]}"
    [ "$maj" -gt 26 ] || { [ "$maj" -eq 26 ] && [ "$min" -ge 4 ]; }
  else
    return 0
  fi
}
if tag_ge_26_4; then
  jq '. + {webAuthnPolicyPasswordlessPasskeysEnabled: true}' "$REALM_FILE" > "$WORK/gardenio-realm.json"
  echo "== realm: passkeys policy enabled (tag $KC_TAG >= 26.4)"
else
  cp "$REALM_FILE" "$WORK/gardenio-realm.json"
  echo "== realm: passkeys policy NOT set (tag $KC_TAG < 26.4); passkey spec will skip"
fi
chmod 644 "$WORK/gardenio-realm.json"

# shellcheck disable=SC2317  # invoked via trap
cleanup() {
  local rc=$?
  docker logs "$NAME" > "$HERE/kc.log" 2>&1 || true
  docker rm -f "$NAME" > /dev/null 2>&1 || true
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT

# --- Step 2: the theme mounted is the exact tree the chart ships -------------
[ -f "$THEME_DIR/login/theme.properties" ] || { echo "theme dir missing: $THEME_DIR" >&2; exit 2; }

docker run -d --name "$NAME" \
  -p "$KC_HTTP_PORT:8080" -p "$KC_MGMT_PORT:9000" \
  --add-host=host.docker.internal:host-gateway \
  -e KC_BOOTSTRAP_ADMIN_USERNAME="$KC_ADMIN_USER" \
  -e KC_BOOTSTRAP_ADMIN_PASSWORD="$KC_ADMIN_PASSWORD" \
  -e KC_HTTP_RELATIVE_PATH=/auth \
  -e KC_HEALTH_ENABLED=true \
  -v "$THEME_DIR:/opt/keycloak/themes/gardenio:ro" \
  -v "$WORK:/opt/keycloak/data/import:ro" \
  "$IMAGE_REF" start-dev --import-realm > /dev/null

echo "== waiting for readiness (up to ${READY_TIMEOUT}s)"
deadline=$(( $(date +%s) + READY_TIMEOUT ))
ready=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  if ! docker ps -q -f "name=$NAME" | grep -q .; then
    echo "Keycloak container exited early" >&2; docker logs "$NAME" 2>&1 | tail -40 >&2; exit 3
  fi
  # /health/ready is on the management port; the path prefix depends on the
  # version (relative-path aware from some 26.x), so try both.
  for p in /health/ready /auth/health/ready; do
    if curl -fsS "http://localhost:$KC_MGMT_PORT$p" > /dev/null 2>&1; then ready=1; break 2; fi
  done
  sleep 3
done
[ "$ready" = 1 ] || { echo "Keycloak not ready in ${READY_TIMEOUT}s" >&2; docker logs "$NAME" 2>&1 | tail -60 >&2; exit 3; }
# Ready does not prove the realm import finished; the realm's own endpoint does.
for _ in $(seq 1 30); do
  curl -fsS "http://localhost:$KC_HTTP_PORT/auth/realms/gardenio/.well-known/openid-configuration" > /dev/null 2>&1 && break
  sleep 2
done
curl -fsS "http://localhost:$KC_HTTP_PORT/auth/realms/gardenio/.well-known/openid-configuration" > /dev/null \
  || { echo "realm gardenio was not imported" >&2; docker logs "$NAME" 2>&1 | tail -60 >&2; exit 3; }
echo "== Keycloak is up"

# How the webAuthnPolicyPasswordless* realm attributes were found (see README):
# read the live realm back through kcadm. Best-effort; never fails the run.
docker exec "$NAME" /opt/keycloak/bin/kcadm.sh config credentials \
    --server http://localhost:8080/auth --realm master \
    --user "$KC_ADMIN_USER" --password "$KC_ADMIN_PASSWORD" > /dev/null 2>&1 \
  && docker exec "$NAME" /opt/keycloak/bin/kcadm.sh get realms/gardenio 2>/dev/null \
     | jq 'with_entries(select(.key | test("^webAuthnPolicy")))' > "$HERE/realm-webauthn.json" \
  || echo "(kcadm realm dump skipped)"

# --- Step 2b: the shipped passkey hook block (S3a) ---------------------------
# Renders the chart's first-admin Job, runs the text between its passkeys:begin
# and passkeys:end markers with kcadm against this container (twice), and reads
# the realm back. Sets the RP ID to localhost, which the passkey spec needs.
if tag_ge_26_4; then
  "$HERE/passkey-policy.sh" "$NAME"
else
  echo "== passkey policy hook NOT run (tag $KC_TAG < 26.4)"
fi

# --- Step 2c: one-click Become (gd-rbML2txvGG) --------------------------------
# Runs the hook's scope-mappings block for gardenio-become (twice) and checks
# that a token minted for an impersonated session carries the impersonator.
"$HERE/become-client.sh" "$NAME"

# --- Step 3: Playwright ------------------------------------------------------
cd "$HERE"
[ -d node_modules ] || npm ci
npx playwright install --with-deps chromium || npx playwright install chromium
export KC_BASE_URL="http://localhost:$KC_HTTP_PORT"
rm -rf screenshots axe-report
[ "${1:-}" != "--" ] || shift
set +e
npx playwright test "$@"
rc=$?
set -e

# --- Step 4: the chart's passkey browser flow (S4a) --------------------------
# Second pass, on the same container: build the flow from `helm template` output
# with kcadm, bind it as the realm browser flow, and run the seven sign-in
# scenarios. After the first pass on purpose: it replaces the realm's browser
# flow, which the screenshots must not see. Default runs only; a run with
# Playwright arguments is a targeted one (RUN_FLOW=1 forces the pass, SKIP_FLOW=1
# skips it).
if tag_ge_26_4 && [ "${SKIP_FLOW:-0}" != 1 ] && { [ "$#" -eq 0 ] || [ "${RUN_FLOW:-0}" = 1 ]; }; then
  echo "== building the chart's passkey flow"
  "$HERE/build-flow.sh" "$NAME" passkey || rc=1
  if [ "$rc" = 0 ] || [ -s "$HERE/flow-executions.json" ]; then
    set +e
    KC_FLOW=passkey npx playwright test tests/passkey-flow.spec.js --project=desktop-light --workers=1
    frc=$?
    set -e
    [ "$frc" = 0 ] || rc=$frc
  fi
else
  echo "== passkey flow pass skipped (Playwright args given, SKIP_FLOW=1, or Keycloak < 26.4)"
fi
echo "== screenshots: $(find screenshots -name '*.png' 2>/dev/null | wc -l) png; axe reports: $(find axe-report -name '*.json' 2>/dev/null | wc -l)"
exit "$rc"
