# geo-table: data-only artifact image for the FULL CIDR->{ASN,country} geo table
# the edge/audit L1 enrichment resolver consumes (edge intrusion detection L1b,
# task ka-0000005851; epic ka-0000008216; design ka-0000007287#c3; mechanism
# ka-0000000583).
#
# This image is NOT a server — it is a tiny carrier that ships the built geo
# table at /geo-table/geo-table.txt so a copy-only initContainer in the
# traefik-log-shipper pod can seed it into the emptyDir the shipper reads via
# TRAEFIK_GEO_TABLE_PATH. The upstream iptoasn dump is fetched + range->CIDR
# converted HERE (where the builder has network), so the runtime step is a pure
# `cp` with ZERO off-cluster egress — it runs under default-deny-egress.
#
# WHY AN IMAGE AND NOT A ConfigMap (the design step this task exists for): the
# full converted table is ~1M CIDRs / tens of MiB (measured: 49.5 MiB with the
# iptoasn description column, 26.4 MiB without). A Kubernetes ConfigMap — AND a
# Secret — hard-caps at 1 MiB (the etcd object limit), so the chart's existing
# geoTable.data ConfigMap mount CANNOT carry a global table (it is only for the
# small operator-pinned tables the knownGoodCIDRs pattern uses).
# Filtering the table down to <1 MiB (a country allowlist, or /16 coarsening) was
# rejected: a public edge sees IPs from every country and ASN, so an allowlist
# structurally BLINDS the exact novel-geo/novel-ASN detections this control
# exists for, and /16 coarsening both overflows 1 MiB anyway (measured 1.19 MiB,
# v4-only) and corrupts attribution (a /16 spans multiple ASNs/countries). So the
# FULL table must be carried off-etcd — this image is that carrier.
#
# WHY NOT bake the table into the release images or commit it: the dump is large
# and refreshes on its own cadence (advice ka-0000001560 — non-code data belongs
# out of the release image). It is a standalone artifact image, fetched fresh at
# each build with a sanity floor, and delivered as an IMAGE so a k8s
# initContainer can consume it.
#
# EXACTLY mirrors the grafana-llm-app-plugin / grafana-vl-datasource-plugin
# prebake seam (gardenio/images/grafana-llm-app-plugin/Dockerfile + the
# seed-llm-plugin initContainer in templates/grafana/deployment.yaml): a
# data-only carrier on a shell-bearing ubuntu base so the copy-only initContainer
# (sh -c 'cp -r /geo-table/. ...') finds /bin/sh + coreutils.
#
# NON-DETERMINISM IS INTENTIONAL: unlike the plugin images there is no pinned
# SHA256 — the upstream iptoasn dump republishes ~daily and we WANT fresh geo
# data at each release build. The >1000-entry sanity floor (shared with
# scripts/build-geo-table.py) fails a truncated or
# format-changed upstream fetch loudly rather than shipping a table that would
# blind the console.
#
# Build (local, from the MONOREPO ROOT so scripts/ is in the build context):
#   docker build -t gardenio/geo-table:dev -f gardenio/images/geo-table/Dockerfile .
#
# No workflow builds this image today (images.yml does not list it; see
# gardenio/docs/deploy-inventory.md). The RWX-era CI produced the same table
# and `crane append`ed it onto ubuntu:24.04 at /geo-table.

# ── Builder: fetch the upstream iptoasn dump + range->CIDR convert ───────────
FROM ubuntu:24.04 AS builder

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl python3 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
# The converter is stdlib-only (no pip deps). Copied from the monorepo-root build
# context so the local `docker build` matches the CI path exactly.
COPY scripts/build-geo-table.py ./build-geo-table.py

ARG GEO_UPSTREAM_URL=https://iptoasn.com/data/ip2asn-combined.tsv.gz
RUN set -eu; \
    echo "Fetching iptoasn dump ${GEO_UPSTREAM_URL}"; \
    # --retry-all-errors so a mid-stream HTTP/2 reset (curl 92, seen against the
    # iptoasn mirror) is retried as a transport error — --retry alone does not
    # retry a partially-transferred body.
    curl -fsSL --retry 5 --retry-delay 5 --retry-all-errors "${GEO_UPSTREAM_URL}" -o /tmp/ip2asn.tsv.gz; \
    mkdir -p /geo-table; \
    # ip2asn-combined carries BOTH v4 and v6 in one file; the converter streams
    # gunzipped TSV on stdin. --no-description drops the AS-description column the
    # resolver ignores, halving the table (~49.5 -> ~26.4 MiB) so the shipper's
    # image pull + emptyDir stay small. A zero-length / all-AS0 result hard-fails
    # the converter, so a broken fetch cannot silently produce an empty table.
    zcat /tmp/ip2asn.tsv.gz | python3 build-geo-table.py --no-description > /geo-table/geo-table.txt; \
    LINES=$(grep -cv '^#' /geo-table/geo-table.txt || true); \
    echo "geo-table.txt: ${LINES} CIDR entries"; \
    head -5 /geo-table/geo-table.txt; \
    # Sanity floor: a healthy combined
    # dump is >100k prefixes. A tiny result means a truncated fetch or an upstream
    # format change — fail loudly rather than ship a table that blinds detections.
    if [ "${LINES:-0}" -lt 1000 ]; then \
      echo "ERROR: only ${LINES} entries — upstream fetch/format looks broken" >&2; \
      exit 1; \
    fi

# ── Runtime: data-only carrier on a shell-bearing base ──────────────────────
# ubuntu:24.04 mirrors the crane-append base used by the CI build path, so the
# in-cluster seed-geo-table initContainer (sh -c 'cp -r /geo-table/. ...') finds
# the same /bin/sh + coreutils whether the image came from `docker build` or
# `crane append`. The table is world-readable so the non-root (65534)
# initContainer can read it.
FROM ubuntu:24.04

COPY --from=builder /geo-table /geo-table

# Sanity marker: the built table must survive into the final image.
RUN test -s /geo-table/geo-table.txt
