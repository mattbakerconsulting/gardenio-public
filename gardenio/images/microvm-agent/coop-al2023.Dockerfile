FROM public.ecr.aws/amazonlinux/amazonlinux:2023@sha256:590b8c9fdab65c7f5b8a2392739104ed6bc5055433ba8ff2bf0d2fa500db2ea3 AS builder

ARG RUST_VERSION=1.93.0
# Pin the archived, per-release rustup-init binary (immutable per
# (version, target)) instead of the rolling sh.rustup.rs wrapper script,
# whose sha drifts on every upstream rustup push and periodically reds this
# job on every open MR (ka-0000004123). This build target is linux/arm64
# only, so a single aarch64-unknown-linux-gnu sha suffices. rustup-init is
# GPG-verified internally against static.rust-lang.org manifests.
ARG RUSTUP_VERSION=1.29.1
ARG RUSTUP_INIT_SHA256=15f6e4ce9f583b929c996c91562bad6d4454f3281de858b02cdfdef615fac433

RUN dnf install -y \
      ca-certificates \
      clang \
      cmake \
      curl-minimal \
      gcc \
      gcc-c++ \
      make \
      openssl-devel \
      perl \
      pkgconf-pkg-config \
      protobuf-compiler \
    && dnf clean all \
    && rm -rf /var/cache/dnf

RUN curl -fsSL --proto '=https' --tlsv1.2 \
      "https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/aarch64-unknown-linux-gnu/rustup-init" \
      -o /tmp/rustup-init \
    && printf '%s  %s\n' "${RUSTUP_INIT_SHA256}" /tmp/rustup-init | sha256sum -c - \
    && chmod +x /tmp/rustup-init \
    && /tmp/rustup-init -y --profile minimal --default-toolchain "${RUST_VERSION}" \
    && rm -f /tmp/rustup-init

ENV PATH=/root/.cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    CARGO_TARGET_DIR=/target \
    RUSTC_WRAPPER=

WORKDIR /src
COPY . .

RUN cargo build --locked --release -p coop

FROM scratch AS artifact
COPY --from=builder /target/release/coop /coop
