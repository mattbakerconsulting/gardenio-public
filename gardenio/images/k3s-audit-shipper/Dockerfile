# k3s-audit-shipper: tails the k3s kube-apiserver audit log from a hostPath mount
# and ships it, redacted, to VictoriaLogs (gd-ZrPUxj8wyU). Dials out only; no
# listening port.
#
# Runs as ROOT, unlike the sibling shippers (which are `static:nonroot`): the
# apiserver writes /var/log/k3s-audit/audit.log as 0600 root and the shipper has
# to read it. The manifest runs the container with every capability dropped, a
# read-only root filesystem and a read-only audit mount, so root here is uid 0
# owning the file, not a privileged process.
#
# Build (context is gardenio/):
#   docker build -t gardenio/k3s-audit-shipper:latest -f images/k3s-audit-shipper/Dockerfile \
#     --build-arg CONTROLLER_BASE=ghcr.io/mattbakerconsulting/gardenio/cache/controller-base:<sha> .

ARG CONTROLLER_BASE

FROM ${CONTROLLER_BASE} AS controller_base

FROM gcr.io/distroless/static-debian12

COPY --from=controller_base /out/k3s-audit-shipper /k3s-audit-shipper

USER 0:0

ENTRYPOINT ["/k3s-audit-shipper"]
