#!/usr/bin/env bash
# Assemble the artifacts/ build context for the Lambda MicroVM agent image.
#
# In-repo adaptation of network-fabric-deployment's
# bin/build-gardenio-microvm-artifacts.sh (nfd!15). Differences from the NF
# original, which built from a separate Gardenio checkout:
#   - sources come from THIS repo (the script's own checkout), not a
#     GARDENIO_DIR archive of a pinned ref;
#   - gb / kd / coop are provided as prebuilt linux/arm64 binaries via
#     GB_BIN / KD_BIN / COOP_BIN (CI builds them in dedicated cached tasks;
#     locally, build them first — see README.md);
#   - the Claude Code npm tarballs are provided via CLAUDE_TGZ /
#     CLAUDE_ARM64_TGZ (CI downloads them in a cached task).
# The output layout under artifacts/ is identical to the NF original, so the
# Dockerfile (a byte-for-byte copy of the NF image Dockerfile) needs no edits.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/../../.." && pwd)}"
OUTPUT_DIR="${OUTPUT_DIR:-${SCRIPT_DIR}/artifacts}"

GB_BIN="${GB_BIN:?path to prebuilt linux/arm64 gb binary}"
KD_BIN="${KD_BIN:?path to prebuilt linux/arm64 kd binary}"
COOP_BIN="${COOP_BIN:?path to prebuilt linux/arm64 AL2023 coop binary}"
CLAUDE_TGZ="${CLAUDE_TGZ:?path to @anthropic-ai/claude-code npm tarball}"
CLAUDE_ARM64_TGZ="${CLAUDE_ARM64_TGZ:?path to @anthropic-ai/claude-code-linux-arm64 npm tarball}"

# Version pins + checksums are the repo's single source of truth.
source "${REPO_ROOT}/gardenio/images/agent/install/versions.env"
source "${REPO_ROOT}/gardenio/images/agent/install/checksums.env"

# Re-verify the Claude Code tarballs against the pinned sha512s even when the
# fetch step already did — the assembly must never bake unverified bytes.
printf '%s  %s\n' "${CLAUDE_CODE_SHA512}" "${CLAUDE_TGZ}" | sha512sum -c -
printf '%s  %s\n' "${CLAUDE_CODE_ARM64_SHA512}" "${CLAUDE_ARM64_TGZ}" | sha512sum -c -

for bin in "${GB_BIN}" "${KD_BIN}" "${COOP_BIN}"; do
  test -s "${bin}"
done

commit="${GARDENIO_COMMIT:-$(git -C "${REPO_ROOT}" rev-parse HEAD)}"
rootfs_dir="${OUTPUT_DIR}/rootfs"

rm -rf "${OUTPUT_DIR}"
mkdir -p \
  "${rootfs_dir}/hooks" \
  "${rootfs_dir}/opt/gardenio/bin" \
  "${rootfs_dir}/opt/gardenio/build" \
  "${rootfs_dir}/usr/local/bin"

install -m 0755 "${GB_BIN}" "${rootfs_dir}/usr/local/bin/gb"
install -m 0755 "${KD_BIN}" "${rootfs_dir}/usr/local/bin/kd-bin"
install -m 0755 "${COOP_BIN}" "${rootfs_dir}/usr/local/bin/coop"

install -m 0755 \
  "${REPO_ROOT}/gardenio/images/agent/entrypoint.sh" \
  "${rootfs_dir}/opt/gardenio/bin/entrypoint.sh"
install -m 0755 \
  "${REPO_ROOT}/gardenio/images/agent/kd-wrapper.sh" \
  "${rootfs_dir}/usr/local/bin/kd"
install -m 0644 \
  "${REPO_ROOT}/gardenio/images/agent/install/versions.env" \
  "${rootfs_dir}/opt/gardenio/build/versions.env"
install -m 0644 \
  "${REPO_ROOT}/gardenio/images/agent/install/checksums.env" \
  "${rootfs_dir}/opt/gardenio/build/checksums.env"
install -m 0644 "${CLAUDE_TGZ}" \
  "${rootfs_dir}/opt/gardenio/build/claude-code.tgz"
install -m 0644 "${CLAUDE_ARM64_TGZ}" \
  "${rootfs_dir}/opt/gardenio/build/claude-code-linux-arm64.tgz"
cp -R "${REPO_ROOT}/gardenio/images/agent/hooks/." "${rootfs_dir}/hooks/"
chmod 0755 "${rootfs_dir}/hooks/"*.sh

gb_sha="$(sha256sum "${rootfs_dir}/usr/local/bin/gb" | awk '{print $1}')"
kd_sha="$(sha256sum "${rootfs_dir}/usr/local/bin/kd-bin" | awk '{print $1}')"
coop_sha="$(sha256sum "${rootfs_dir}/usr/local/bin/coop" | awk '{print $1}')"
COOP_VERSION="$(awk -F'"' '/^version = / {print $2; exit}' "${REPO_ROOT}/coop/Cargo.toml")"

jq -n \
  --arg architecture "linux/arm64" \
  --arg gardenioCommit "${commit}" \
  --arg nodeVersion "${NODE_FULL_VERSION}" \
  --arg nodeArm64Sha256 "${NODE_SHA256_ARM64}" \
  --arg claudeCodeVersion "${CLAUDE_CODE_VERSION}" \
  --arg claudeCodeSha512 "${CLAUDE_CODE_SHA512}" \
  --arg claudeCodeArm64Sha512 "${CLAUDE_CODE_ARM64_SHA512}" \
  --arg coopVersion "${COOP_VERSION}" \
  --arg coopSha256 "${coop_sha}" \
  --arg gbSha256 "${gb_sha}" \
  --arg kdSha256 "${kd_sha}" \
  '{
    architecture: $architecture,
    gardenio: {commit: $gardenioCommit},
    runtime: {
      node: {version: $nodeVersion, arm64Sha256: $nodeArm64Sha256},
      claudeCode: {
        version: $claudeCodeVersion,
        packageSha512: $claudeCodeSha512,
        linuxArm64PackageSha512: $claudeCodeArm64Sha512
      },
      coop: {version: $coopVersion, sha256: $coopSha256},
      gb: {sha256: $gbSha256},
      kd: {sha256: $kdSha256}
    }
  }' > "${OUTPUT_DIR}/manifest.json"

echo "Assembled MicroVM agent build context at ${OUTPUT_DIR} (commit ${commit})"
