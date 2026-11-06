# bridge-log-shipper: the non-AVA webhook-audit transport (task ka-0000000320,
# epic ka-0000000914). Streams the jira-bridge + gitlab-bridge pods' webhook
# receive-path stdout off the Kubernetes pod-log API and POSTs it to the
# in-cluster VictoriaLogs /insert/jsonline, so the two inbound webhook doors that
# stay NON-AVA by design have a durable, queryable audit record. See
# internal/bridgeshipper for why this reuses the sanctioned stream->filter->sink
# shape of traefik-log-shipper rather than a per-bridge sidecar / DaemonSet /
# CloudWatch hop.
#
# Mirrors the traefik-log-shipper layout (distroless, nonroot,
# ENTRYPOINT on the binary). images.yml builds this Dockerfile for release.
#
# Build:
#   docker build -t gardenio/bridge-log-shipper:latest images/bridge-log-shipper/ \
#     --build-arg VERSION=$(git describe --tags --always)
#
# gd-B2c8OinSs7: the builder stage is a pull from the shared controller-base
# image (images/_controller-base/) instead of recompiling controller/ +
# clicreds/ in this image's own build -- the same rewiring the bridge images
# got in phase 2, finished for the remaining fourteen on 2026-09-22. See
# gardenio/images/CACHE_STRATEGY.md. .github/workflows/images.yml's
# `controller-base` job builds+pushes that image tagged by ${{ github.sha }}
# before this one runs, and passes CONTROLLER_BASE because .github/images.json
# marks this image "controllerBase": true. CONTROLLER_BASE has no default -- a
# build that forgets to pass it fails loudly instead of silently pulling a
# stale or nonexistent :latest.

ARG CONTROLLER_BASE

FROM ${CONTROLLER_BASE} AS controller_base

# ── Runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=controller_base /out/bridge-log-shipper /bridge-log-shipper

USER nonroot:nonroot

ENTRYPOINT ["/bridge-log-shipper"]
