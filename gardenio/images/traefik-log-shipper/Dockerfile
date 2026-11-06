# traefik-log-shipper: the edge-IDP L0 transport (task ka-0000001563, epic
# ka-0000008216). Streams the Traefik edge pod's JSON access log off the
# Kubernetes pod-log API and POSTs it to the in-cluster VictoriaLogs
# /insert/jsonline, so the edge intrusion-detection layers have a queryable
# per-request record. See internal/traefikshipper for why this is a small stdout
# streamer and not a sidecar / DaemonSet / CloudWatch hop.
#
# Mirrors the bridge-family layout (distroless, nonroot,
# ENTRYPOINT on the binary). images.yml builds this Dockerfile for release.
#
# Build:
#   docker build -t gardenio/traefik-log-shipper:latest images/traefik-log-shipper/ \
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

COPY --from=controller_base /out/traefik-log-shipper /traefik-log-shipper

USER nonroot:nonroot

ENTRYPOINT ["/traefik-log-shipper"]
