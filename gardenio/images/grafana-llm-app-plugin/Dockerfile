# grafana-llm-app-plugin: dist-only artifact image for the PINNED, CHECKSUMMED
# grafana-llm-app foundation plugin (C6/prod prereq ka-0000007541, epic
# ka-0000002011 — Path C chat box).
#
# This image is NOT a server — it is a tiny carrier that ships the grafana-llm-app
# plugin dist at /plugin so a copy-only initContainer in the Grafana pod can seed
# it into /var/lib/grafana/plugins/grafana-llm-app. The plugin zip is fetched +
# CHECKSUM-VERIFIED here (where the builder has network), so the runtime step is a
# pure `cp` with zero off-cluster egress.
#
# WHY THIS EXISTS: grafana-llm-app proxies ALL chat LLM completions + the embedded
# mcp-grafana and runs INSIDE the Grafana pod. The alternative — boot-time catalog
# install via GF_INSTALL_PLUGINS — fetches from grafana.com at :443, which REQUIRES
# grafana.plugins.allowCatalogEgress:true and thus a 0.0.0.0/0:443+:80 allow on the
# grafana-egress NetworkPolicy. With that hole open nothing at the NETWORK layer
# stops the in-pod plugin from POSTing completions off-cluster. Seeding this
# pre-baked dist lets PROD run with allowCatalogEgress:FALSE under
# default-deny-egress — making "zero off-cluster egress" a NETWORK guarantee.
#
# EXACTLY mirrors the C4 grafana-assistant-plugin seam (gardenio/images/
# grafana-assistant-plugin/Dockerfile + the seed initContainer in
# gardenio/helm/gardenio/templates/grafana/deployment.yaml). Difference: that plugin
# is built from in-repo source; this one is a pinned UPSTREAM artifact, so the
# builder stage downloads + verifies a checksum instead of compiling.
#
# grafana-llm-app is Grafana-ROOT-SIGNED (the zip carries a signed MANIFEST.txt,
# signatureType: grafana), so the seeded on-disk copy validates as SIGNED and
# loads with NO GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS — strictly more
# locked-down than the C4 custom (unsigned) plugin.
#
# Build (local):
#   docker build -t gardenio/grafana-llm-app-plugin:dev \
#     -f gardenio/images/grafana-llm-app-plugin/Dockerfile \
#     gardenio/images/grafana-llm-app-plugin
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
    echo "Fetching grafana-llm-app ${GRAFANA_LLM_APP_VERSION} (${GRAFANA_LLM_APP_OS}/${GRAFANA_LLM_APP_ARCH})"; \
    curl -fsSL -o plugin.zip "${GRAFANA_LLM_APP_URL}"; \
    # FAIL LOUDLY on checksum mismatch — a tampered/rotated upstream artifact must
    # never be baked in silently.
    echo "${GRAFANA_LLM_APP_SHA256}  plugin.zip" | sha256sum -c -; \
    unzip -q plugin.zip -d extracted; \
    # The zip extracts to a single top-level dir named after the plugin id; lay
    # its CONTENTS at /plugin so the runtime `cp -r /plugin/. /mnt/plugin/` (with
    # /mnt/plugin = .../plugins/grafana-llm-app) reproduces the exact plugin dir.
    mkdir -p /plugin; \
    cp -r "extracted/${GRAFANA_LLM_APP_PLUGIN_ID}/." /plugin/; \
    # Sanity: plugin.json is what Grafana keys discovery on; MANIFEST.txt is the
    # signature that lets it load WITHOUT an unsigned-allow. Both must be present.
    test -f /plugin/plugin.json; \
    test -f /plugin/MANIFEST.txt

# ── Runtime: dist-only carrier on a shell-bearing base ──────────────────────
# ubuntu:24.04 mirrors the crane-append base used by the CI build path, so the
# in-cluster seed-llm-plugin initContainer (sh -c 'cp -r /plugin/. ...') finds the
# same /bin/sh + coreutils whether the image came from `docker build` or
# `crane append`. The dist is world-readable so the non-root (65534) initContainer
# can read it.
FROM ubuntu:24.04

COPY --from=builder /plugin /plugin

# Sanity marker: the signed plugin manifest must survive into the final image.
RUN test -f /plugin/plugin.json && test -f /plugin/MANIFEST.txt
