# grafana-bridge: provisions the chart's Grafana dashboards as type=dashboard
# beads. The chart sets GRAFANA_DASHBOARDS_DIR=/dashboards and expects the JSON
# to ship INSIDE this image (helm/gardenio/dashboards/*.json), world-readable —
# the RWX-era image pipeline layered them in the same way.
# Listens on GRAFANA_LISTEN_ADDR (default :8093); state at STATE_PATH (/data).
#
# Build (context is gardenio/):
#   docker build -t gardenio/grafana-bridge:latest -f images/grafana-bridge/Dockerfile .
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

COPY --from=controller_base /out/grafana-bridge /grafana-bridge
# 755, not 644: BuildKit applies --chmod to the /dashboards/ directory it
# creates as well, and a 644 directory has no search bit, so nonroot could
# list the files but every open() failed with EACCES (2026.285.1 backfill
# imported 0 of 26). The JSON being +x is harmless.
COPY --chmod=755 helm/gardenio/dashboards/*.json /dashboards/

USER nonroot:nonroot
EXPOSE 8093

ENTRYPOINT ["/grafana-bridge"]
