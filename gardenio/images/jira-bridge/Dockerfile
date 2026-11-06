# jira-bridge: standalone beads→JIRA sync bridge.
# Multi-stage build: Go builder → distroless runtime.
#
# Build:
#   docker build -t gardenio/jira-bridge:latest -f images/jira-bridge/Dockerfile \
#     --build-arg VERSION=$(git describe --tags --always) .
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

COPY --from=controller_base /out/jira-bridge /jira-bridge

USER nonroot:nonroot
EXPOSE 8091

ENTRYPOINT ["/jira-bridge"]
