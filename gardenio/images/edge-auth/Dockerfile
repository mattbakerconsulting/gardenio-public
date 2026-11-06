# edge-auth: gardenio-edge-auth Go service (Stage 3 / W-B-12).
# Non-AVA edge that fronts in-cluster Services (kbeads, coopmux) with
# GitLab-PAT-rooted auth so CLI tools can drive the daemons from outside
# the cluster. See: gardenio/plans/stage3-ava-cli-eb-rollout.md.
#
# Multi-stage build: Go builder -> distroless runtime. Mirrors the
# images/beads-viewer/Dockerfile shape so coverage/version stamping
# stays consistent across controller binaries.
#
# Build:
#   docker build -t gardenio/edge-auth:latest -f images/edge-auth/Dockerfile .
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

COPY --from=controller_base /out/edge-auth /edge-auth

USER nonroot:nonroot
EXPOSE 8096

ENTRYPOINT ["/edge-auth"]
