#!/bin/bash
# clis.sh — install binary CLI tools for gardenio agent image.
#
# Used by the agent Dockerfile (RUN /tmp/install/clis.sh).
# All downloads run in parallel for speed. Each one is a backgrounded subshell
# followed by `track <name>`; the script waits on every PID and exits non-zero
# naming each tool that failed. A bare `wait` returns 0 whatever its jobs did,
# so a transient curl error used to ship an image without the tool and a green
# build (helm in 2026.305.2, gd-POfKpGi6Kj).
#
# Every download is SHA256-verified against checksums.env. Do not add new
# downloads without a pinned checksum — no `curl | sh`, no `curl … | tar`
# without `fetch_verify_stdout`, no unpinned "latest" version lookups.
# See docs/agent-install-checksums.md.
set -eo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/versions.env"
source "$SCRIPT_DIR/checksums.env"
source "$SCRIPT_DIR/lib.sh"
# gitleaks' version and sha256s come from CI's security lock (gd-H2ZTTWf8s8).
# The Dockerfile copies it next to this script; in a checkout it is three up.
SECURITY_LOCK="$SCRIPT_DIR/security-tools.lock"
[ -f "$SECURITY_LOCK" ] || SECURITY_LOCK="$SCRIPT_DIR/../../../locks/security-tools.lock"
# shellcheck source=../../../locks/security-tools.lock
source "$SECURITY_LOCK"

ARCH=${TARGETARCH:-amd64}
case "$ARCH" in arm64) ARCH_ALT=aarch64 ;; *) ARCH_ALT=x86_64 ;; esac

PREFIX=${INSTALL_PREFIX:-/usr/local}
mkdir -p "$PREFIX/bin" "$PREFIX/lib/docker/cli-plugins" "$PREFIX/share/helm/plugins"

JOB_PIDS=()
JOB_NAMES=()
# track NAME — record the job just backgrounded so the final loop can wait on it.
track() {
  JOB_PIDS+=("$!")
  JOB_NAMES+=("$1")
}

(
  # kubectl — pinned to KUBECTL_VERSION; sha256 from dl.k8s.io/release/.../kubectl.sha256
  fetch_verify \
    "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl" \
    "$(pick_sha KUBECTL_SHA256)" \
    "$PREFIX/bin/kubectl"
  chmod +x "$PREFIX/bin/kubectl"
) &
track kubectl

(
  # GitHub CLI
  fetch_verify_stdout \
    "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${ARCH}.tar.gz" \
    "$(pick_sha GH_SHA256)" \
    | tar -xz --strip-components=2 -C "$PREFIX/bin" "gh_${GH_VERSION}_linux_${ARCH}/bin/gh"
) &
track gh

(
  # GitLab CLI
  fetch_verify_stdout \
    "https://gitlab.com/gitlab-org/cli/-/releases/v${GLAB_VERSION}/downloads/glab_${GLAB_VERSION}_linux_${ARCH}.tar.gz" \
    "$(pick_sha GLAB_SHA256)" \
    | tar -xz --strip-components=1 -C "$PREFIX/bin" bin/glab
) &
track glab

(
  # Docker CLI + Compose plugin (both published with x86_64/aarch64 naming)
  fetch_verify_stdout \
    "https://download.docker.com/linux/static/stable/${ARCH_ALT}/docker-${DOCKER_VERSION}.tgz" \
    "$(pick_sha_alt DOCKER_SHA256)" \
    | tar -xz --strip-components=1 -C "$PREFIX/bin" docker/docker
  fetch_verify \
    "https://github.com/docker/compose/releases/download/v${DOCKER_COMPOSE_VERSION}/docker-compose-linux-${ARCH_ALT}" \
    "$(pick_sha_alt DOCKER_COMPOSE_SHA256)" \
    "$PREFIX/lib/docker/cli-plugins/docker-compose"
  chmod +x "$PREFIX/lib/docker/cli-plugins/docker-compose"
) &
track docker

(
  # AWS CLI — pinned versioned URL (unversioned awscli-exe-linux-*.zip ships
  # whatever the AWS CLI team cut last, which is not pinnable).
  #
  # AWSCLI_DEST is the ABSOLUTE /opt/aws-cli, for the same reason as PG17_DEST
  # below: the Dockerfile copies /opt/ from this stage, and the old
  # $PREFIX/../opt/aws-cli resolved to /usr/opt/aws-cli, which it does not, so
  # every image shipped a dangling /usr/local/bin/aws (gd-POfKpGi6Kj).
  AWSCLI_DEST="${AWSCLI_DEST:-/opt/aws-cli}"
  fetch_verify \
    "https://awscli.amazonaws.com/awscli-exe-linux-${ARCH_ALT}-${AWSCLI_VERSION}.zip" \
    "$(pick_sha_alt AWSCLI_SHA256)" \
    /tmp/awscli.zip
  unzip -q /tmp/awscli.zip -d /tmp
  /tmp/aws/install --install-dir "$AWSCLI_DEST" --bin-dir "$PREFIX/bin"
  # The installer's own `current -> <install-dir>/<version>` link is absolute;
  # make it relative so the install-dir can move.
  ln -sfn "${AWSCLI_VERSION}" "$AWSCLI_DEST/v2/current"
  ln -sfn "$AWSCLI_DEST/v2/current/bin/aws" "$PREFIX/bin/aws"
  ln -sfn "$AWSCLI_DEST/v2/current/bin/aws_completer" "$PREFIX/bin/aws_completer"
  rm -rf /tmp/awscli.zip /tmp/aws
) &
track aws

(
  # Helm — direct binary download (replaces get-helm-3 pipe).
  fetch_verify_stdout \
    "https://get.helm.sh/helm-v${HELM_VERSION}-linux-${ARCH}.tar.gz" \
    "$(pick_sha HELM_SHA256)" \
    | tar -xz --strip-components=1 -C "$PREFIX/bin" "linux-${ARCH}/helm"
  chmod +x "$PREFIX/bin/helm"
  # Helm plugins — pinned to release tags via `--version`. git-clone-backed
  # installs are still used (helm plugin install has no sha flag), but
  # pinning the version prevents a silently-rewritten HEAD from landing.
  HELM_DATA_HOME="$PREFIX/share/helm" "$PREFIX/bin/helm" plugin install \
    https://github.com/helm-unittest/helm-unittest --version "${HELM_UNITTEST_VERSION}"
  HELM_DATA_HOME="$PREFIX/share/helm" "$PREFIX/bin/helm" plugin install \
    https://github.com/chartmuseum/helm-push --version "${HELM_PUSH_VERSION}"
) &
track helm

(
  # Terraform + Terragrunt
  fetch_verify \
    "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_${ARCH}.zip" \
    "$(pick_sha TERRAFORM_SHA256)" \
    /tmp/terraform.zip
  unzip -q /tmp/terraform.zip -d "$PREFIX/bin"
  rm /tmp/terraform.zip
  fetch_verify \
    "https://github.com/gruntwork-io/terragrunt/releases/download/v${TERRAGRUNT_VERSION}/terragrunt_linux_${ARCH}" \
    "$(pick_sha TERRAGRUNT_SHA256)" \
    "$PREFIX/bin/terragrunt"
  chmod +x "$PREFIX/bin/terragrunt"
) &
track terraform

(
  # uv — direct release tarball (replaces `curl https://astral.sh/uv/install.sh | sh`).
  UV_TARGET="${ARCH_ALT}-unknown-linux-gnu"
  fetch_verify_stdout \
    "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${UV_TARGET}.tar.gz" \
    "$(pick_sha_alt UV_SHA256)" \
    | tar -xz --strip-components=1 -C "$PREFIX/bin" "uv-${UV_TARGET}/uv" "uv-${UV_TARGET}/uvx"
  chmod +x "$PREFIX/bin/uv" "$PREFIX/bin/uvx"
) &
track uv

(
  # Poetry — the installer is a Python script; we verify the script itself
  # and then run it. Poetry's internal downloads are verified against PyPI.
  # POETRY_HOME is under /opt (copied by the Dockerfile): with POETRY_HOME=$PREFIX
  # its venv landed in /usr/local/venv, which is not, and /usr/local/bin/poetry
  # dangled (gd-POfKpGi6Kj).
  POETRY_DEST="${POETRY_DEST:-/opt/poetry}"
  fetch_verify \
    "https://install.python-poetry.org" \
    "${POETRY_INSTALL_SHA256}" \
    /tmp/install-poetry.py
  POETRY_HOME="$POETRY_DEST" python3 /tmp/install-poetry.py
  rm -f /tmp/install-poetry.py
  ln -sfn "$POETRY_DEST/bin/poetry" "$PREFIX/bin/poetry"
) &
track poetry

(
  # Bun — direct release zip (replaces `curl https://bun.sh/install | bash`).
  BUN_DIR="$PREFIX/bun"
  mkdir -p "$BUN_DIR/bin"
  case "$ARCH_ALT" in x86_64) BUN_ASSET=bun-linux-x64.zip ;; aarch64) BUN_ASSET=bun-linux-aarch64.zip ;; esac
  fetch_verify \
    "https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/${BUN_ASSET}" \
    "$(pick_sha_alt BUN_SHA256)" \
    /tmp/bun.zip
  unzip -q -j /tmp/bun.zip -d "$BUN_DIR/bin"
  chmod +x "$BUN_DIR/bin/bun"
  ln -sf "$BUN_DIR/bin/bun" "$BUN_DIR/bin/bunx"
  ln -sf "$BUN_DIR/bin/bun" "$PREFIX/bin/bun"
  ln -sf "$BUN_DIR/bin/bunx" "$PREFIX/bin/bunx"
  rm -f /tmp/bun.zip
) &
track bun

(
  # yq — pinned
  fetch_verify \
    "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_${ARCH}" \
    "$(pick_sha YQ_SHA256)" \
    "$PREFIX/bin/yq"
  chmod +x "$PREFIX/bin/yq"
) &
track yq

(
  # actionlint — lints .github/workflows/*.yml. Crews editing CI fetched it by
  # hand into the scratchpad (session sweep gd-KBdgGYAZOP).
  fetch_verify_stdout \
    "https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}/actionlint_${ACTIONLINT_VERSION}_linux_${ARCH}.tar.gz" \
    "$(pick_sha ACTIONLINT_SHA256)" \
    | tar -xz -C "$PREFIX/bin" actionlint
  chmod +x "$PREFIX/bin/actionlint"
) &
track actionlint

(
  # promtool — `make test-helm` refuses to run without it, and crews were
  # downloading it into the scratchpad by hand (gd-hZl1cKiELM). Only promtool
  # is kept from the prometheus tarball. Upstream ships it unstripped; strip
  # drops the debug info (139MB -> 98MB) and changes nothing it does.
  PROM_DIR="prometheus-${PROMTOOL_VERSION}.linux-${ARCH}"
  fetch_verify_stdout \
    "https://github.com/prometheus/prometheus/releases/download/v${PROMTOOL_VERSION}/${PROM_DIR}.tar.gz" \
    "$(pick_sha PROMETHEUS_SHA256)" \
    | tar -xz --strip-components=1 -C "$PREFIX/bin" "${PROM_DIR}/promtool"
  strip "$PREFIX/bin/promtool"
  chmod +x "$PREFIX/bin/promtool"
) &
track promtool

(
  # stern
  fetch_verify_stdout \
    "https://github.com/stern/stern/releases/download/v${STERN_VERSION}/stern_${STERN_VERSION}_linux_${ARCH}.tar.gz" \
    "$(pick_sha STERN_SHA256)" \
    | tar -xz -C "$PREFIX/bin" stern
) &
track stern

(
  # hadolint — pinned (was "latest")
  fetch_verify \
    "https://github.com/hadolint/hadolint/releases/download/v${HADOLINT_VERSION}/hadolint-Linux-${ARCH_ALT}" \
    "$(pick_sha_alt HADOLINT_SHA256)" \
    "$PREFIX/bin/hadolint"
  chmod +x "$PREFIX/bin/hadolint"
) &
track hadolint

(
  # gitleaks — secret scanner. Gates `gb salvage --push` (EOL Tier-2 quarantine
  # push, ka-0000000146/ka-0000002845): the guarded push fails closed if this
  # binary is absent. Upstream publishes linux_x64 / linux_arm64 tarballs.
  # Pinned by security-tools.lock, sourced above.
  case "$ARCH" in arm64) GL_ARCH=arm64 ;; *) GL_ARCH=x64 ;; esac
  fetch_verify_stdout \
    "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_linux_${GL_ARCH}.tar.gz" \
    "$(pick_sha_alt GITLEAKS_SHA256)" \
    | tar -xz -C "$PREFIX/bin" gitleaks
  chmod +x "$PREFIX/bin/gitleaks"
) &
track gitleaks

(
  # Claudeless
  fetch_verify_stdout \
    "https://github.com/alfredjeanlab/claudeless/releases/download/${CLAUDELESS_VERSION}/claudeless-linux-${ARCH_ALT}.tar.gz" \
    "$(pick_sha_alt CLAUDELESS_SHA256)" \
    | tar -xz -C "$PREFIX/bin" claudeless
  chmod +x "$PREFIX/bin/claudeless"
) &
track claudeless

(
  # k6 load testing tool
  fetch_verify_stdout \
    "https://github.com/grafana/k6/releases/download/v${K6_VERSION}/k6-v${K6_VERSION}-linux-${ARCH}.tar.gz" \
    "$(pick_sha K6_SHA256)" \
    | tar -xz --strip-components=1 -C "$PREFIX/bin" "k6-v${K6_VERSION}-linux-${ARCH}/k6"
) &
track k6

(
  # PostgreSQL 17 client (pg_dump/pg_restore/psql), vendored so the P4.4
  # DB-cutover Jobs use a baked client instead of curling it from
  # apt.postgresql.org inside the frozen window (ka-0000003862 / ka-0000008220).
  # Extracted with dpkg-deb -x (no root, no apt) — the exact mechanism the
  # cutover rehearsal proved working against this image. Binaries land at
  # $PG17_DEST/usr/lib/postgresql/17/bin.
  #
  # PG17_DEST defaults to the ABSOLUTE /opt/pg17 so the Dockerfile's
  # `COPY --from=cli-tools /opt/ /opt/` captures it (the cli-tools stage runs as
  # root). A build that stages under its own root may override PG17_DEST (the
  # RWX-era CI did), but the image must still land pg17 at the /opt/pg17 the
  # cutover Jobs reference. NOTE the path is
  # intentionally NOT $PREFIX-relative: $PREFIX/../opt resolves to /usr/opt,
  # which the Dockerfile does not COPY.
  #
  # DELIBERATELY NOT on the default PATH — the system psql/pg_dump stays pg16, so
  # the only consumer is the cutover toolkit (explicit path). Zero behavior
  # change for the rest of the fleet.
  PG17_DEST="${PG17_DEST:-/opt/pg17}"
  mkdir -p "$PG17_DEST"
  fetch_verify \
    "https://apt.postgresql.org/pub/repos/apt/pool/main/p/postgresql-17/postgresql-client-17_${PGDG_PG17_DEB_FILEVER}_${ARCH}.deb" \
    "$(pick_sha PGDG_PG17_CLIENT_DEB_SHA256)" \
    /tmp/pgc17.deb
  dpkg-deb -x /tmp/pgc17.deb "$PG17_DEST"
  rm -f /tmp/pgc17.deb
  # Sanity: the dump/restore binaries must be present at the documented path.
  test -x "$PG17_DEST/usr/lib/postgresql/17/bin/pg_dump"
  test -x "$PG17_DEST/usr/lib/postgresql/17/bin/pg_restore"
) &
track pg17

# whisper-cli (needs cmake, gcc — skippable via SKIP_WHISPER=1 for parallel builds)
if [ "${SKIP_WHISPER:-0}" != "1" ]; then
(
  fetch_verify_stdout \
    "https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v${WHISPER_CPP_VERSION}.tar.gz" \
    "${WHISPER_CPP_SHA256}" \
    | tar -xz -C /tmp
  cmake -S "/tmp/whisper.cpp-${WHISPER_CPP_VERSION}" -B /tmp/whisper-build \
    -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF
  cmake --build /tmp/whisper-build --config Release -j"$(nproc)" --target whisper-cli
  cp /tmp/whisper-build/bin/whisper-cli "$PREFIX/bin/whisper-cli"
  rm -rf "/tmp/whisper.cpp-${WHISPER_CPP_VERSION}" /tmp/whisper-build
) &
track whisper-cli
fi

failed=()
for i in "${!JOB_PIDS[@]}"; do
  wait "${JOB_PIDS[$i]}" || failed+=("${JOB_NAMES[$i]}")
done
if [ "${#failed[@]}" -gt 0 ]; then
  echo "clis.sh: FAILED to install: ${failed[*]} (see the errors above)" >&2
  exit 1
fi
echo "All CLI tools installed."
