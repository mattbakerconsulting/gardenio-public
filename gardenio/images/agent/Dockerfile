# Gardenio agent image (multi-stage, parallel BuildKit).
#
# Build (from repo root):
#   docker build -t gardenio-agent -f images/agent/Dockerfile .
#   docker build --target base -t gardenio-agent:base -f images/agent/Dockerfile .  # minimal variant
#
# Tool versions and package lists are defined in install/versions.env.
# CI (.github/workflows/images.yml) builds this Dockerfile on a calver tag.
#
# Architecture (BuildKit parallel stages):
#   gb-builder ─────────────┐
#   go-tools ───────────────┤
#     └ go-warm ────────────┤   (GOMODCACHE + GOCACHE for the deps)
#   rust-tools ─────────────┤
#   cli-tools ──────────────┼──→ agent (COPY --from all builders)
#   terraform-providers ────┤   (depends on cli-tools for terraform binary)
#   whisper-builder ────────┤
#   playwright-builder ─────┤
#   base ───────────────────┘

# ═════════════════════════════════════════════════════════════════
# Parallel builder stages — all start concurrently via BuildKit
# ═════════════════════════════════════════════════════════════════

# ── Go builder — compiles gb (agent orchestration CLI) from source
FROM golang:1.25-bookworm AS gb-builder
WORKDIR /build
# controller/go.mod has `replace gardenio/clicreds => ../clicreds`
# (the credential-handling library gb links). Materialize the sibling clicreds
# module alongside controller/ so this stage is SELF-CONTAINED with build
# context = the repo root (kbeads/ and coop/ are siblings of gardenio/) — no downstream fork has to inject a COPY or
# rewrite the context to build it (ka-0000006785). go.mod/go.sum first so
# `go mod download` can read the replaced module before the full source lands.
COPY gardenio/controller/go.mod gardenio/controller/go.sum ./controller/
COPY gardenio/clicreds/go.mod gardenio/clicreds/go.sum ./clicreds/
RUN cd controller && go mod download
COPY gardenio/controller/ ./controller/
COPY gardenio/clicreds/ ./clicreds/
# Stamp gb's build so `gb version`/`gb --version` and the version-skew signal
# report the real build instead of "dev" (ka-0000002965). images.yml passes
# VERSION and COMMIT as build-args; anything unset keeps its default.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BRANCH=unknown
RUN cd controller && BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" && \
    CGO_ENABLED=0 go build \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} \
        -X gardenio/controller/internal/buildinfo.Branch=${BRANCH} \
        -X gardenio/controller/internal/buildinfo.Date=${BUILD_DATE}" \
      -o /gb ./cmd/gb/

# ── Go toolchain + dev tools (parallel) ─────────────────────────
FROM ubuntu:24.04 AS go-tools
ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
COPY gardenio/images/agent/install/ /tmp/install/
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl jq ca-certificates && rm -rf /var/lib/apt/lists/*
RUN TARGETARCH=${TARGETARCH} /tmp/install/go-tools.sh

# ── Warm Go module + build cache (gd-j2ABjjrEL0) ─────────────────
# A fresh crew pod used to spend 15-20 min compiling client-go & co. before
# its first scoped `go test` ran (20m7s cold vs 2m59s warm for -run CIWait
# ./cmd/gb/). These stages ship GOMODCACHE and a GOCACHE holding every
# third-party and std package the workspace imports, compiled and vetted.
#
# A GOCACHE entry is only reused when its action ID matches, so this stage
# must build EXACTLY as a pod does:
#   - the agent image's own toolchain: FROM go-tools (/usr/local/go at
#     GO_FULL_VERSION), GOTOOLCHAIN=local so a pin below go.work's go line
#     fails here instead of silently warming a downloaded toolchain;
#   - the pod's paths: GOMODCACHE /home/agent/go/pkg/mod and GOCACHE
#     /home/agent/.cache/go-build (Go's defaults for HOME=/home/agent; both
#     are on the pod's overlay, not a mount, so the image content is live);
#   - CGO_ENABLED=1 with ubuntu:24.04's gcc, as AGENT_PACKAGES installs it;
#   - default build flags (no -race, -cover or tags).
# Only dependencies are warmed, never this repo's packages, so the expensive
# layer survives main moving: it re-runs only when go.mod/go.sum or the set
# of imported third-party packages changes.
FROM go-tools AS go-warm-base
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc libc6-dev git && rm -rf /var/lib/apt/lists/*
ENV PATH=/usr/local/go/bin:${PATH} HOME=/home/agent \
    GOPATH=/home/agent/go GOMODCACHE=/home/agent/go/pkg/mod \
    GOCACHE=/home/agent/.cache/go-build GOTOOLCHAIN=local CGO_ENABLED=1 GOFLAGS=

# Every go.work / go.mod / go.sum in the context, and nothing else. Runs on
# every commit (the bind mount is the whole context) but is cheap, and its
# output is byte-identical until a module file changes.
FROM go-warm-base AS go-warm-mods
RUN --mount=type=bind,target=/src \
    cd /src && mkdir -p /mods && \
    find . \( -name node_modules -o -name target -o -name testdata \) -prune -o \
      \( -name go.work -o -name go.work.sum -o -name go.mod -o -name go.sum \) \
      -type f -print0 | xargs -0 cp --parents -t /mods

FROM go-warm-base AS go-warm-dl
COPY --from=go-warm-mods /mods/ /warm/
RUN cd /warm && go mod download

# The third-party + std packages the workspace imports (go-warm.sh imports).
# Also every-commit and cheap: the modules are already downloaded.
FROM go-warm-dl AS go-warm-imports
RUN --mount=type=bind,target=/src,rw \
    /tmp/install/go-warm.sh imports /src /imports.txt

# The expensive one. BuildKit keys it on imports.txt's content, so a commit
# that changes no import and no module file reuses it from the registry cache.
# Cache entries get a far-future mtime: Go's cache trim deletes entries whose
# mtime is over 5 days old, and bumps the mtime of any entry it uses. On a
# pod, the image's entries would be both (this layer outlives many releases),
# so the first go command would delete the whole warm cache, and every hit
# would copy the entry up out of the image layer to touch it.
FROM go-warm-dl AS go-warm
COPY --from=go-warm-imports /imports.txt /warm/imports.txt
RUN /tmp/install/go-warm.sh build /warm /warm/imports.txt && \
    find "$GOCACHE" -type f -exec touch -m -d '2100-01-01 00:00:00' {} + && \
    du -sh "$GOMODCACHE" "$GOCACHE"

# ── Rust toolchain + dev tools (parallel) ───────────────────────
FROM ubuntu:24.04 AS rust-tools
ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
COPY gardenio/images/agent/install/ /tmp/install/
# coop's toolchain pin: rust-tools.sh installs that channel as the default.
COPY coop/rust-toolchain.toml /tmp/install/rust-toolchain.toml
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl gcc libc6-dev pkg-config libssl-dev ca-certificates && \
    rm -rf /var/lib/apt/lists/*
RUN TARGETARCH=${TARGETARCH} /tmp/install/rust-tools.sh

# ── Warm cargo registry (gd-Kec4Xy0UtT, escalation gd-zln3o6sL9N) ─
# The crates coop depends on, fetched once here so `cargo test -p coop` on a
# pod compiles instead of first downloading the index and every crate. Only
# the manifests, the lock and the toolchain pin are copied, each member given
# an empty lib and bin (cargo loads the workspace before it fetches), so the
# layer changes only when a dependency does. registry/src is dropped: cargo
# re-extracts it from registry/cache, offline, in seconds.
FROM rust-tools AS cargo-warm
# rust-tools.sh exports these only inside its own shell; a later RUN in this
# stage has no cargo on PATH without them (2026.308.1 failed: cargo: not found).
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo PATH=/usr/local/cargo/bin:$PATH
RUN --mount=type=bind,target=/src \
    mkdir -p /warm && cd /src/coop && \
    find . -name target -prune -o \
      \( -name Cargo.toml -o -name Cargo.lock -o -name rust-toolchain.toml \) \
      -type f -print0 | xargs -0 cp --parents -t /warm
RUN cd /warm && \
    find . -mindepth 2 -name Cargo.toml -printf '%h\n' | while read -r d; do \
      mkdir -p "$d/src" && : > "$d/src/lib.rs" && : > "$d/src/main.rs"; done && \
    cargo fetch --locked && \
    rm -rf /usr/local/cargo/registry/src && \
    du -sh /usr/local/cargo/registry

# ── kd (beads CLI) — built from kbeads/ in this tree ─────────────
# Replaces the prebuilt release tarball this stage used to download from the
# decommissioned upstream account. Recipe mirrors kbeads/Dockerfile: cgo for
# libicu (unicode normalization), buildinfo stamped via ldflags.
FROM golang:1.25-bookworm AS kd-builder
RUN apt-get update && apt-get install -y --no-install-recommends libicu-dev gcc g++ && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /build
# kbeads/go.mod has `replace gardenio/clicreds => ../gardenio/clicreds` and
# `replace gardenio/lands/contract => ../lands/contract`.
COPY kbeads/go.mod kbeads/go.sum ./kbeads/
COPY gardenio/clicreds/go.mod gardenio/clicreds/go.sum ./gardenio/clicreds/
COPY lands/contract/go.mod lands/contract/go.sum ./lands/contract/
RUN cd kbeads && go mod download
COPY kbeads/ ./kbeads/
COPY gardenio/clicreds/ ./gardenio/clicreds/
COPY lands/contract/ ./lands/contract/
ARG VERSION=dev
ARG COMMIT=unknown
ARG BRANCH=unknown
RUN cd kbeads && BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" && \
    CGO_ENABLED=1 GOOS=linux go build \
      -ldflags="-s -w -X main.Version=${VERSION} -X main.Build=${COMMIT} \
        -X gardenio/kbeads/internal/buildinfo.Branch=${BRANCH} \
        -X gardenio/kbeads/internal/buildinfo.Date=${BUILD_DATE}" \
      -o /kd ./cmd/kd && \
    go clean -cache -modcache

# ── coop + coopmux — built from coop/ in this tree ───────────────
# Replaces the prebuilt release tarball from the decommissioned upstream
# account. `--features s3` on coop is load-bearing (the S3 session
# backend). Reuses the rust-tools stage's
# toolchain; coop/rust-toolchain.toml pins 1.93.0 and rustup auto-installs it.
# target/ and the cargo caches are removed in the same layer: a release build
# is multi-GB and the runner's disk is finite.
FROM rust-tools AS coop-builder
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo PATH=/usr/local/cargo/bin:$PATH
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler libprotobuf-dev && \
    rm -rf /var/lib/apt/lists/*
COPY coop/ /build/coop/
RUN cd /build/coop && \
    cargo build --release -p coop --features s3 -p coopmux && \
    mkdir -p /out && install -m 0755 target/release/coop target/release/coopmux /out/ && \
    install -m 0755 deploy/k8s-launch.sh /out/coop-launch && \
    rm -rf target /usr/local/cargo/registry /usr/local/cargo/git

# ── CLI tools — parallel downloads, no heavy compilation ────────
FROM ubuntu:24.04 AS cli-tools
ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
COPY gardenio/images/agent/install/ /tmp/install/
# clis.sh pins gitleaks from CI's security lock (gd-H2ZTTWf8s8).
COPY gardenio/locks/security-tools.lock /tmp/install/security-tools.lock
# binutils: clis.sh strips promtool. git: `helm plugin install <git url>` clones.
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl jq unzip python3 binutils ca-certificates git && \
    rm -rf /var/lib/apt/lists/*
# Skip whisper-cli here — it builds in its own parallel stage
RUN SKIP_WHISPER=1 TARGETARCH=${TARGETARCH} /tmp/install/clis.sh

# ── whisper-cli — cmake compilation in isolated parallel stage ──
FROM ubuntu:24.04 AS whisper-builder
ARG DEBIAN_FRONTEND=noninteractive
COPY gardenio/images/agent/install/versions.env /tmp/install/versions.env
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl cmake gcc g++ make ca-certificates && \
    rm -rf /var/lib/apt/lists/*
RUN . /tmp/install/versions.env && \
    curl -fsSL "https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v${WHISPER_CPP_VERSION}.tar.gz" \
      | tar -xz -C /tmp && \
    cmake -S "/tmp/whisper.cpp-${WHISPER_CPP_VERSION}" -B /tmp/whisper-build \
      -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF && \
    cmake --build /tmp/whisper-build --config Release -j"$(nproc)" --target whisper-cli && \
    cp /tmp/whisper-build/bin/whisper-cli /usr/local/bin/whisper-cli && \
    rm -rf /tmp/whisper.cpp-* /tmp/whisper-build

# ── Playwright browsers + npm packages (parallel — own Node) ───
FROM ubuntu:24.04 AS playwright-builder
ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
COPY gardenio/images/agent/install/ /tmp/install/
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl jq xz-utils ca-certificates gnupg && \
    rm -rf /var/lib/apt/lists/*
# Install Node.js (own copy — enables full parallelism with base)
RUN TARGETARCH=${TARGETARCH} /tmp/install/node.sh
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
RUN . /tmp/install/versions.env && \
    npm install -g "playwright@${PLAYWRIGHT_VERSION}" @playwright/mcp && \
    npx playwright install chromium && \
    $(npm root -g)/@playwright/mcp/node_modules/.bin/playwright install chromium && \
    chmod -R 777 /ms-playwright
# Google Chrome Stable (for playwright-mcp --browser chrome)
RUN curl -fsSL https://dl.google.com/linux/linux_signing_key.pub \
      | gpg --dearmor -o /etc/apt/keyrings/google-chrome.gpg && \
    echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main" \
      > /etc/apt/sources.list.d/google-chrome.list && \
    apt-get update && apt-get install -y --no-install-recommends google-chrome-stable && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# ── Terraform provider cache (parallel — uses cli-tools terraform) ─
FROM cli-tools AS terraform-providers
COPY gardenio/images/agent/install/ /tmp/install/
RUN /tmp/install/terraform-providers.sh

# ═════════════════════════════════════════════════════════════════
# base — minimal agent with git, Node.js, Claude Code, Codex, coop, kd, gb
# ═════════════════════════════════════════════════════════════════

FROM ubuntu:24.04 AS base

ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH

# ── Shared install scripts ─────────────────────────────────────────
COPY gardenio/images/agent/install/ /tmp/install/

# ── System packages (minimal) ─────────────────────────────────────
RUN . /tmp/install/versions.env && \
    apt-get update && apt-get install -y --no-install-recommends $BASE_PACKAGES && \
    ln -sf /usr/bin/python3 /usr/bin/python && \
    update-ca-certificates && \
    mkdir -p /usr/local/share/ca-certificates && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# ── Node.js + Claude Code ─────────────────────────────────────────
RUN TARGETARCH=${TARGETARCH} /tmp/install/node.sh

# ── Codex CLI (second agent harness, plan gd-8s8FdZu8UA) ──────────
# Static musl binary, so this does NOT depend on the node.sh layer above and
# could run before it; it is ordered after only to keep the harnesses adjacent.
RUN TARGETARCH=${TARGETARCH} /tmp/install/codex.sh

# ── Gemini CLI (third agent harness, ADR 0005) ────────────────────
# A JS bundle: needs the Node layer above, unlike codex.sh.
RUN TARGETARCH=${TARGETARCH} /tmp/install/gemini.sh

# NB: the coop / kd / gb binaries are deliberately NOT copied here. They are
# the only source-derived artifacts in the image, so they are copied in the
# LAST layers of the `agent` stage instead -- see "Source-derived binaries"
# at the end of this file for the measurement that moved them.

# ── Agent user ───────────────────────────────────────────────────
# Rename the default ubuntu user (UID 1000) to agent across passwd,
# shadow, and group for identification consistency.
#
# No sudoers entry is installed: the agent runs unprivileged. If a
# future task legitimately needs a specific privileged command, add
# an allowlisted command-scoped sudoers rule — never NOPASSWD: ALL,
# which would reverse the USER 1000 directive.
RUN sed -i 's/^ubuntu:/agent:/' /etc/passwd && \
    sed -i 's|/home/ubuntu|/home/agent|' /etc/passwd && \
    sed -i 's|/bin/sh|/bin/bash|' /etc/passwd && \
    sed -i 's/^ubuntu:/agent:/' /etc/shadow && \
    sed -i 's/^ubuntu:/agent:/' /etc/group && \
    mkdir -p /home/agent/workspace /home/agent/.ssh && \
    chown -R 1000:1000 /home/agent

# ── SSH known hosts (prevent interactive host key prompts) ────
# Baked-in keys for github.com and gitlab.com so SSH connections
# never block on host key verification. The entrypoint refreshes
# these at runtime via ssh-keyscan if network is available.
RUN ssh-keyscan -t ed25519,ecdsa,rsa github.com gitlab.com \
      > /home/agent/.ssh/known_hosts 2>/dev/null && \
    chmod 700 /home/agent/.ssh && \
    chmod 644 /home/agent/.ssh/known_hosts && \
    chown -R 1000:1000 /home/agent/.ssh

# ── Partial-clone git defaults (system-wide /etc/gitconfig) ──────
# Agent workspaces are blobless partial clones (git clone --filter=blob:none,
# see controller initclone.go), so the agent's own git operations lazily
# backfill file *contents* on demand. These system-level defaults keep that
# fast and tidy for every repo, regardless of $HOME / user (ka-0000000463):
#   - fetch.writeCommitGraph: maintain a commit-graph on every fetch so
#     ancestry walks (git log/merge-base/rebase across long histories) stay
#     O(1)-ish instead of re-parsing commit objects.
#   - gc.cruftPacks: sweep the loose objects a --filter backfill leaves behind
#     into a cruft pack rather than a pile of loose files, so repeated
#     on-demand blob fetches don't bloat the object store.
# The promisor remote itself is auto-configured by the --filter clone
# (remote.origin.promisor / partialclonefilter), so nothing to set here for it.
RUN git config --system fetch.writeCommitGraph true && \
    git config --system gc.cruftPacks true

# ── Hook scripts ─────────────────────────────────────────────────
COPY gardenio/images/agent/hooks/ /hooks/
RUN chmod +x /hooks/*.sh

# ── Claudeless mock scenarios ────────────────────────────────────
COPY gardenio/images/agent/scenarios/ /scenarios/

# ── kubectl ──────────────────────────────────────────────────────
RUN KUBECTL_VERSION=$(curl -fsSL https://dl.k8s.io/release/stable.txt) && \
    curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
      -o /usr/local/bin/kubectl && \
    chmod +x /usr/local/bin/kubectl

# ── Entrypoint ───────────────────────────────────────────────────
COPY gardenio/images/agent/entrypoint.sh /entrypoint.sh
COPY gardenio/images/agent/resolve-auth.sh /resolve-auth.sh
RUN chmod +x /entrypoint.sh /resolve-auth.sh

USER 1000
WORKDIR /home/agent/workspace

ENTRYPOINT ["/entrypoint.sh"]

# ═════════════════════════════════════════════════════════════════
# Full agent — merge all parallel stages into base
# ═════════════════════════════════════════════════════════════════

FROM base AS agent

USER root
ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH

# ── System packages (agent + playwright runtime deps, one apt-get) ──
# The Postgres server goes in after postgresql-common with create_main_cluster
# off, so no 16/main cluster is created or started in the image; pg-scratch
# starts one on demand (gd-ELlwvze8G3).
RUN . /tmp/install/versions.env && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
      $AGENT_PACKAGES $PLAYWRIGHT_SYSTEM_DEPS fonts-noto-core fontconfig postgresql-common && \
    sed -ri 's/^#?[[:space:]]*create_main_cluster[[:space:]]*=.*$/create_main_cluster = false/' /etc/postgresql-common/createcluster.conf && \
    grep -qx 'create_main_cluster = false' /etc/postgresql-common/createcluster.conf && \
    apt-get install -y --no-install-recommends $PG_SERVER_PACKAGES && \
    test -z "$(ls -A /etc/postgresql/16 2>/dev/null)" && \
    ln -sf /usr/bin/python3 /usr/bin/python && \
    ln -sf gcc /usr/bin/cc && \
    apt-get clean && rm -rf /var/lib/apt/lists/*
# Assert the server and pgvector landed where pg-scratch looks for them.
RUN test -x /usr/lib/postgresql/16/bin/initdb \
 && test -x /usr/lib/postgresql/16/bin/postgres \
 && test -f /usr/share/postgresql/16/extension/vector.control
COPY gardenio/images/agent/pg-scratch.sh /usr/local/bin/pg-scratch
RUN chmod 0755 /usr/local/bin/pg-scratch

# ── Go toolchain (from parallel builder) ───────────────────────
COPY --from=go-tools /usr/local/go/ /usr/local/go/
COPY --from=go-tools /usr/local/bin/golangci-lint /usr/local/bin/
COPY --from=go-tools /usr/local/bin/task /usr/local/bin/
COPY --from=go-tools /usr/local/bin/gofumpt /usr/local/bin/staticcheck /usr/local/bin/
RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go && \
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt && \
    ln -sf /usr/local/go/bin/gopls /usr/local/bin/gopls 2>/dev/null || true
# GOTOOLCHAIN=local, as in go-warm-base (see "Warm Go module + build cache"
# above): a go line above GO_FULL_VERSION fails loudly instead of downloading
# a toolchain whose GOCACHE keys miss the warm cache. check-go-toolcache-pin.py
# keeps every go.work/go.mod go line at or below GO_FULL_VERSION (gd-auZ6V08nR1).
ENV PATH="/usr/local/go/bin:${PATH}" GOTOOLCHAIN=local

# ── Rust toolchain (from parallel builder) ─────────────────────
COPY --from=rust-tools /usr/local/rustup/ /usr/local/rustup/
COPY --from=rust-tools /usr/local/cargo/ /usr/local/cargo/
COPY --from=rust-tools /usr/local/bin/quench /usr/local/bin/
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
ENV PATH="${CARGO_HOME}/bin:${PATH}"
# The cargo home is the agent's: cargo takes a lock file in it on every
# command (.package-cache). Its contents keep the a+rw rust-tools.sh set.
RUN chown 1000:1000 /usr/local/cargo

# ── CLI tools (from parallel builder) ──────────────────────────
COPY --from=cli-tools /usr/local/bin/ /usr/local/bin/
COPY --from=cli-tools /usr/local/lib/docker/ /usr/local/lib/docker/
COPY --from=cli-tools /usr/local/share/helm/ /usr/local/share/helm/
COPY --from=cli-tools /usr/local/bun/ /usr/local/bun/
COPY --from=cli-tools /opt/ /opt/
# Recreate bun symlinks (COPY dereferences them)
RUN ln -sf /usr/local/bun/bin/bun /usr/local/bin/bun && \
    ln -sf /usr/local/bun/bin/bunx /usr/local/bin/bunx
# Assert the baked pg17 client landed (P4.4 cutover, ka-0000003862). clis.sh
# vendors it into /opt/pg17 in a backgrounded subshell; fail the build here if a
# partial/failed bake left it absent, rather than shipping a green image that
# silently falls back to an in-window apt fetch during the cutover.
RUN test -x /opt/pg17/usr/lib/postgresql/17/bin/pg_dump \
 && test -x /opt/pg17/usr/lib/postgresql/17/bin/pg_restore
# Same guard for the CLIs whose installs live outside /usr/local/bin or broke
# silently before: helm (a swallowed curl error in 2026.305.2), aws and poetry
# (symlinks into directories this stage never copied), the helm plugins
# (installed by git clone). gd-POfKpGi6Kj.
RUN helm version --short \
 && HELM_PLUGINS=/usr/local/share/helm/plugins helm plugin list | grep -q '^unittest' \
 && aws --version \
 && poetry --version

# ── Terraform provider cache (from parallel builder) ──────────
COPY --from=terraform-providers /opt/terraform-plugin-cache/ /opt/terraform-plugin-cache/
RUN mkdir -p /opt/terragrunt-cache

# ── whisper-cli (from parallel builder) ────────────────────────
COPY --from=whisper-builder /usr/local/bin/whisper-cli /usr/local/bin/

# ── Playwright (from parallel builder) ─────────────────────────
COPY --from=playwright-builder /ms-playwright/ /ms-playwright/
COPY --from=playwright-builder /usr/local/lib/node_modules/playwright/ /usr/local/lib/node_modules/playwright/
COPY --from=playwright-builder /usr/local/lib/node_modules/@playwright/ /usr/local/lib/node_modules/@playwright/
COPY --from=playwright-builder /opt/google/ /opt/google/
# COPY creates /ms-playwright itself root-owned 0755 (the builder's chmod only survives on its contents), so a
# `playwright-core install` as the agent user failed EACCES on mkdir /ms-playwright/__dirlock (gd-vK5BdVNUc6).
RUN ln -sf ../lib/node_modules/playwright/cli.js /usr/local/bin/playwright && \
    chmod 777 /ms-playwright
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

# ── Gardenio channel (MCP-based nudge delivery) ────────────────
COPY gardenio/images/agent/channel/ /channel/
RUN cd /channel && bun install --frozen-lockfile 2>/dev/null || (cd /channel && bun install)

# ── Python packages (boto3 for AWS SDK operations) ──────────────
RUN pip3 install --break-system-packages --no-cache-dir boto3

# ── Environment ──────────────────────────────────────────────────
ENV BUN_INSTALL=/usr/local/bun
ENV PATH="${BUN_INSTALL}/bin:${PATH}"
ENV HELM_PLUGINS=/usr/local/share/helm/plugins
# Terraform/Terragrunt configuration — shared plugin cache + working dir.
ENV TF_PLUGIN_CACHE_DIR=/opt/terraform-plugin-cache
ENV TG_DOWNLOAD_DIR=/opt/terragrunt-cache
ENV TERRAGRUNT_TFPATH=/usr/local/bin/terraform
# Ensure all tools (Python requests, Node.js, httpx) use the system CA bundle.
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
ENV REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
ENV NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt
# Skills and scripts write to $TMPDIR (OUT=$TMPDIR/x); unset, that is /x and
# fails EACCES (gd-hZl1cKiELM). /tmp is the pod's writable tmp volume
# (podmanager MountTmp) and world-writable in the image.
ENV TMPDIR=/tmp

# ── Warm Go caches (from the go-warm stage, gd-j2ABjjrEL0) ─────
# Owned by the agent so go can add entries next to the image's. Kept just
# above the source-derived binaries: it changes when dependencies do, which
# is more often than anything else up here.
COPY --from=go-warm --chown=1000:1000 /home/agent/go/ /home/agent/go/
COPY --from=go-warm --chown=1000:1000 /home/agent/.cache/ /home/agent/.cache/
# ── Warm cargo registry (from the cargo-warm stage, gd-Kec4Xy0UtT) ─
# Here for the same reason: it changes when coop's dependencies do.
COPY --from=cargo-warm --chown=1000:1000 /usr/local/cargo/registry/ /usr/local/cargo/registry/

# ── Source-derived binaries — LAST ON PURPOSE ────────────────────
# coop/coopmux/coop-launch, kd and gb are the ONLY artifacts in this image
# built from this repo's source. Every other layer above depends solely on
# install/ scripts and pinned upstream downloads, so every other layer is
# cacheable across commits. These three are not: `gb-builder` and `kd-builder`
# take `ARG COMMIT`, which is baked into -ldflags, so their build layers miss
# the registry cache on EVERY commit by construction (same root cause found
# for the controller-base family in gd-B2c8OinSs7).
#
# They used to sit at `base` steps 5-9/20, which put ~165-210s of perfectly
# cacheable work DOWNSTREAM of a guaranteed cache miss. Measured over 7
# consecutive PR builds of this image on 2026-09-23 (jobs 107014681577,
# 107019168060, 107019916857, 107020438732, 107023748621, 107028810321,
# 107030371083):
#
#   step                                     status       cost
#   base 6/20  COPY --from=kd-builder /kd     never cached  (the break point)
#   agent 1/26 apt-get AGENT_PACKAGES         never cached  94-138s
#   agent 2-26 toolchain COPYs (go, rust,     never cached  ~65s
#              cli-tools, terraform, whisper,
#              playwright, bun, boto3)
#
# base 3/20's apt-get -- the SAME kind of work, but ABOVE the binary copies --
# was CACHED in all 7 of 7 runs. That contrast is the whole argument: the apt
# install was re-run ~8x/hour purely because it sat below a COPY it has no
# dependency on.
#
# Keep these instructions last. Anything added BELOW them re-runs on every
# commit; anything added above them stays cacheable. In particular kd-bin is
# cgo-linked against libicu and libicu74 arrives with AGENT_PACKAGES in the
# apt layer far above, so this ordering is also the correct one for linkage.
COPY --from=coop-builder /out/coop /out/coopmux /out/coop-launch /usr/local/bin/
# Real kd binary lives at /usr/local/bin/kd-bin so the wrapper at
# /usr/local/bin/kd can intercept the EX_USAGE-family exit code 64
# (operator-refusal sentinel; see ka-0000006731).
COPY --from=kd-builder /kd /usr/local/bin/kd-bin
COPY gardenio/images/agent/kd-wrapper.sh /usr/local/bin/kd
RUN chmod +x /usr/local/bin/kd /usr/local/bin/kd-bin
COPY --from=gb-builder /gb /usr/local/bin/gb

USER 1000

# ── Smoke check (gd-hZl1cKiELM) ──────────────────────────────────
# images.yml skips this image on PRs, so this is where a tool that failed to
# install is caught: clis.sh backgrounds every download and a bare `wait`
# drops their exit codes. Runs as the agent user, so TMPDIR is checked for
# the user that writes to it. A few --version calls; cheap below the
# per-commit binaries.
COPY gardenio/images/agent/smoke.sh /usr/local/lib/gardenio/smoke.sh
RUN VERSIONS_ENV=/tmp/install/versions.env bash /usr/local/lib/gardenio/smoke.sh
# pg-scratch end to end as the agent user: initdb, start, CREATE EXTENSION
# vector, stop. A few seconds; nothing is left running or on disk.
RUN eval "$(pg-scratch start)" && test -n "$LANDS_CONFORMANCE_DATABASE_URL" && pg-scratch stop
