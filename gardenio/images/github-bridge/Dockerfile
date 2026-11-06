# github-bridge: GitHub App webhook receiver and installation-token minter.
#
# gd-B2c8OinSs7: builder stage replaced by a pull from the shared
# controller-base image (images/_controller-base/) instead of recompiling
# controller/ + clicreds/ from scratch in this image's own build. See
# gardenio/images/CACHE_STRATEGY.md for the measured before/after. Wired into
# .github/workflows/images.yml: the `controller-base` job builds+pushes that
# image tagged by ${{ github.sha }} before this one runs. CONTROLLER_BASE has
# no default -- a build that forgets to pass it fails loudly instead of
# silently pulling a stale or nonexistent :latest.
#
# Build:
#   docker build -t gardenio/github-bridge:latest -f images/github-bridge/Dockerfile \
#     --build-arg CONTROLLER_BASE=ghcr.io/joyousaversion/gardenio/cache/controller-base:<sha> .

ARG CONTROLLER_BASE

FROM ${CONTROLLER_BASE} AS controller_base

# Debian, not distroless: the PR conflict scanner (cmd/github-bridge/conflicts.go)
# shells out to git >= 2.38 for `merge-tree --write-tree` and fetches over
# HTTPS. bookworm ships 2.39. docs/pr-conflicts.md, gd-xrDytQgUfR.
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=controller_base /out/github-bridge /github-bridge

USER 65534:65534
EXPOSE 8101

ENTRYPOINT ["/github-bridge"]
