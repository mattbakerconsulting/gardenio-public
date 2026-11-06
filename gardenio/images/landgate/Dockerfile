# landgate: one land's transparent proxy in front of kbeads (Lands boundary
# gd-uYNbyW4a9Z §6 step 11). It holds no credential, so the image carries
# nothing but the binary.
#
# Builder stage is the shared controller-base image (images/_controller-base/,
# gardenio/images/CACHE_STRATEGY.md); CONTROLLER_BASE has no default so a
# build that forgets it fails loudly.
#
# Build:
#   docker build -t gardenio/landgate:latest -f images/landgate/Dockerfile \
#     --build-arg CONTROLLER_BASE=ghcr.io/mattbakerconsulting/gardenio/cache/controller-base:<sha> .

ARG CONTROLLER_BASE

FROM ${CONTROLLER_BASE} AS controller_base

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=controller_base /out/landgate /landgate

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/landgate"]
