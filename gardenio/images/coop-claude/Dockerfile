# coop-claude: coop with Claude Code installed (was image-claude in the RWX-era coop pipeline).
# Builds coop from this tree (coop/ at the repo root; build context = .).
# `--features s3` on coop is load-bearing.
FROM rust:1.93-bookworm AS builder
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler libprotobuf-dev pkg-config libssl-dev && \
    rm -rf /var/lib/apt/lists/*
COPY coop/ /build/coop/
RUN cd /build/coop && cargo build --release -p coop --features s3 -p coopmux && \
    mkdir -p /out && install -m 0755 target/release/coop target/release/coopmux /out/ && \
    install -m 0755 deploy/k8s-launch.sh /out/coop-launch && rm -rf target

FROM ubuntu:24.04
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    git python3 build-essential openssh-client jq ripgrep fd-find tree ca-certificates curl && \
    rm -rf /var/lib/apt/lists/*
# Unpinned upstream installer (Anthropic's) -- a reproducibility caveat, kept as the old pipeline had it.
RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH=/root/.local/bin:$PATH
COPY --from=builder /out/coop /usr/local/bin/coop
ENTRYPOINT ["coop"]
