# grafana-vl-datasource-plugin: dist-only artifact image for the PINNED,
# CHECKSUMMED VictoriaLogs datasource plugin — the Grafana datasource the Phase-2
# EKS-API forensic dashboard (ka-0000007948) queries. Plan ka-0000003550; Phase-2
# epic ka-0000007666.
#
# This image is NOT a server — it is a tiny carrier that ships the
# victoriametrics-logs-datasource plugin dist at /plugin so a copy-only
# initContainer in the Grafana pod can seed it into
# /var/lib/grafana/plugins/victoriametrics-logs-datasource. The plugin zip is
# fetched + CHECKSUM-VERIFIED here (where the builder has network), so the runtime
# step is a pure `cp` with zero off-cluster egress.
#
# WHY THIS EXISTS: the alternative — boot-time catalog install via
# GF_INSTALL_PLUGINS — fetches from grafana.com at :443, which REQUIRES
# grafana.plugins.allowCatalogEgress:true and thus a 0.0.0.0/0:443+:80 allow on
# the grafana-egress NetworkPolicy. PROD runs with allowCatalogEgress:FALSE under
# default-deny-egress; opening that hole to add a datasource plugin would defeat
# the zero-off-cluster-egress NETWORK guarantee the Grafana pod is held to
# (ka-0000007541). Seeding this pre-baked dist lets PROD keep
# allowCatalogEgress:FALSE — the plugin reaches disk with ZERO network access, and
# at RUNTIME the datasource dials only the in-cluster VictoriaLogs Service (:9428).
#
# EXACTLY mirrors the grafana-llm-app-plugin seam (gardenio/images/
# grafana-llm-app-plugin/Dockerfile + the seed initContainer in
# gardenio/helm/gardenio/templates/grafana/deployment.yaml): a pinned UPSTREAM
# artifact, so the builder stage downloads + verifies a checksum instead of
# compiling in-repo source.
#
# victoriametrics-logs-datasource is COMMERCIAL-signed (the zip carries a signed
# MANIFEST.txt, signatureType: commercial, signedByOrg: victoriametrics) with NO
# rootUrls restriction, so the seeded on-disk copy validates as SIGNED and loads
# with NO GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS — same locked-down posture as
# the grafana-root-signed grafana-llm-app seed. This is a BACKEND datasource
# plugin (plugin.json backend:true, executable
# victoriametrics_logs_backend_plugin) — Grafana spawns the arch-matched backend
# binary carried in the dist; the "any" (universal) package ships every arch.
#
# Build (local):
#   docker build -t gardenio/grafana-vl-datasource-plugin:dev \
#     -f gardenio/images/grafana-vl-datasource-plugin/Dockerfile \
#     gardenio/images/grafana-vl-datasource-plugin
#
# No workflow builds this image today (images.yml does not list it; see
# gardenio/docs/deploy-inventory.md). The runtime base is ubuntu:24.04 so the
# chart initContainer finds a real shell + coreutils for the `cp`.

# ── Builder: download + checksum-verify + extract the plugin dist ────────────
FROM ubuntu:24.04 AS builder

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
# pin.env is the SINGLE SOURCE OF TRUTH for version + checksum + URL. Copy +
# source it so the pin lives in one file only.
COPY pin.env ./
RUN set -eu; \
    . ./pin.env; \
    echo "Fetching victoriametrics-logs-datasource ${VL_DATASOURCE_VERSION}"; \
    curl -fsSL -o plugin.zip "${VL_DATASOURCE_URL}"; \
    # FAIL LOUDLY on checksum mismatch — a tampered/rotated upstream artifact must
    # never be baked in silently.
    echo "${VL_DATASOURCE_SHA256}  plugin.zip" | sha256sum -c -; \
    unzip -q plugin.zip -d extracted; \
    # The zip extracts to a single top-level dir named after the plugin id; lay
    # its CONTENTS at /plugin so the runtime `cp -r /plugin/. /mnt/plugin/` (with
    # /mnt/plugin = .../plugins/victoriametrics-logs-datasource) reproduces the
    # exact plugin dir.
    mkdir -p /plugin; \
    cp -r "extracted/${VL_DATASOURCE_PLUGIN_ID}/." /plugin/; \
    # Sanity: plugin.json is what Grafana keys discovery on; MANIFEST.txt is the
    # signature that lets it load WITHOUT an unsigned-allow; the amd64 backend
    # binary is what Grafana spawns for the datasource on our amd64 nodes. All
    # three must be present.
    test -f /plugin/plugin.json; \
    test -f /plugin/MANIFEST.txt; \
    test -f /plugin/victoriametrics_logs_backend_plugin_linux_amd64

# ── Runtime: dist-only carrier on a shell-bearing base ──────────────────────
# ubuntu:24.04 mirrors the crane-append base used by the CI build path, so the
# in-cluster seed-vl-datasource-plugin initContainer (sh -c 'cp -r /plugin/. ...')
# finds the same /bin/sh + coreutils whether the image came from `docker build` or
# `crane append`. The dist is world-readable so the non-root (65534) initContainer
# can read it.
FROM ubuntu:24.04

COPY --from=builder /plugin /plugin

# Sanity marker: the signed plugin manifest + amd64 backend binary must survive
# into the final image.
RUN test -f /plugin/plugin.json \
    && test -f /plugin/MANIFEST.txt \
    && test -f /plugin/victoriametrics_logs_backend_plugin_linux_amd64
