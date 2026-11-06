#!/usr/bin/env bash
# Smoke-test the locally built Lambda MicroVM agent image.
#
# Port of network-fabric-deployment's bin/test-gardenio-microvm-image.sh
# (nfd!15) — same assertions, same lifecycle-hook probes.
set -euo pipefail

image="${1:-gardenio-lambda-microvm:local}"
container="gardenio-microvm-smoke-${RANDOM}"

cleanup() {
  docker stop "${container}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

architecture="$(docker image inspect "${image}" --format '{{.Architecture}}')"
if [[ "${architecture}" != "arm64" ]]; then
  echo "expected arm64 image, got ${architecture}" >&2
  exit 1
fi

docker run --rm --entrypoint /bin/bash "${image}" -ceu '
  test "$(uname -m)" = "aarch64"
  test "$(id -u)" = "1000"
  test -x /usr/local/bin/gb
  test -x /usr/local/bin/kd
  test -x /usr/local/bin/kd-bin
  test -x /usr/local/bin/coop
  test -x /opt/openziti/bin/ziti
  test -x /opt/gardenio/bin/entrypoint.sh
  test -x /hooks/prime.sh
  gb --help >/dev/null
  kd-bin --help >/dev/null
  coop --help | grep -q -- --port-metrics
  coop --version
  node --version
  claude --version
  /opt/openziti/bin/ziti version
'

docker run --rm --detach --name "${container}" "${image}" >/dev/null
for _ in $(seq 1 30); do
  if docker exec "${container}" curl -fsS -X POST \
    http://127.0.0.1:8080/aws/lambda-microvms/runtime/v1/ready >/dev/null; then
    break
  fi
  sleep 1
done

docker exec "${container}" curl -fsS -X POST \
  http://127.0.0.1:8080/aws/lambda-microvms/runtime/v1/validate \
  | jq -e '.status == "valid" and (.versions | keys | length == 6)' >/dev/null

echo "ARM64 Gardenio Lambda MicroVM image smoke test passed: ${image}"
