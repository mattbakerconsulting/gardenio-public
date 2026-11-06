# core-log-shipper: general gardenio-pod slog transport (pod-log API -> VictoriaLogs).
# Dials out only; no listening port. The RWX-era image pipeline crane-appended it
# onto ubuntu:24.04; the binary is static, so distroless.
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
# Build (context is gardenio/):
#   docker build -t gardenio/core-log-shipper:latest -f images/core-log-shipper/Dockerfile \
#     --build-arg CONTROLLER_BASE=ghcr.io/mattbakerconsulting/gardenio/cache/controller-base:<sha> .

ARG CONTROLLER_BASE

FROM ${CONTROLLER_BASE} AS controller_base

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=controller_base /out/core-log-shipper /core-log-shipper

USER nonroot:nonroot

ENTRYPOINT ["/core-log-shipper"]
