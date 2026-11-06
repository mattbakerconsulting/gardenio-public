# controller-base: one shared compile of every controller/cmd binary that the
# golang:1.25-bookworm -> distroless bridge/service images (images/*) used to
# build individually. gd-B2c8OinSs7 (ci-speed phase 2); see
# gardenio/images/CACHE_STRATEGY.md for the measured before/after. Built +
# pushed by the `controller-base` job in .github/workflows/images.yml
# (tagged by ${{ github.sha }}, never :latest) before the rewired images pull
# their binary from it via a CONTROLLER_BASE build-arg.
#
# ALL 20 consumers are now rewired (2026-09-22). Phase 2 landed this base
# building every binary in the list below and then wired only SIX images onto
# it -- the other fourteen kept their own `COPY controller/ ./` + `go build`
# and so compiled the shared package graph a second time, in their own pod, on
# a whole 4-vCPU node each. That is what made images.yml the dominant burst:
# any change under gardenio/controller/** is in 21 of the 26 path filters, so
# the matrix fanned out to 21-26 heavy jobs at ~150 s each instead of ~50 s.
# Measured over 17 images runs the matrix size was bimodal -- 2, or 21-26 --
# and the big shape was the majority.
#
# INVARIANT, enforced by scripts/check-controller-base.py: every image with
# "controllerBase": true in .github/images.json has its binary in the list
# below and COPYs it from this stage, and every image whose Dockerfile still
# compiles controller/ itself is NOT flagged. Adding a cmd/ binary to an image
# means adding it here, not adding a second builder stage.
#
# Why this exists: images/<name>/Dockerfile each independently does
#   COPY controller/ ./ && COPY clicreds/ /clicreds/ && go build ./cmd/<name>/
# in its own container. `go build`'s output is invalidated by ARG COMMIT (it's
# linked into -ldflags), so the registry buildx cache never hits that RUN
# layer across commits -- every one of the ~19 images independently
# recompiles the entire shared controller/internal package graph AND pays a
# full mode=max cache-export (measured ~90-100s of the ~150-220s job, see the
# strategy doc). Compiling all 19 mains here, once, in one `go build` with one
# GOCACHE, means the shared packages compile once; each downstream image then
# does a trivial COPY of its one binary.
#
# Build (context is gardenio/):
#   docker build -t controller-base:proto -f images/_controller-base/Dockerfile .

FROM golang:1.25-bookworm AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG COVER=0

WORKDIR /build
# beads-viewer's mermaid.min.js and echarts.min.js are not in git (gd-wNQzR9ptZI):
# fetch them, pinned and sha256-checked, before `COPY controller/` so this layer
# caches until the pins in the script change, then drop them into the embed dir.
COPY controller/cmd/beads-viewer/fetch-vendored.sh /vendored/
RUN /vendored/fetch-vendored.sh /vendored/static
COPY controller/ ./
RUN cp /vendored/static/* cmd/beads-viewer/templates/static/
# controller/go.mod has `replace ... => ../clicreds`; module root is /build.
COPY clicreds/ /clicreds/

# One GOCACHE across all 15 `go build` invocations: the first pays for
# compiling shared internal packages, every later one reuses that work.
# Names match controller/cmd/<name> and images/<name>/Dockerfile 1:1.
RUN if [ "$COVER" = "1" ]; then \
      COVER_FLAGS="-cover -covermode=atomic -coverpkg=./..."; \
    else \
      COVER_FLAGS=""; \
    fi && \
    mkdir -p /out && \
    for bin in \
      beads-viewer bridge-log-shipper core-log-shipper edge-auth \
      github-bridge gitlab-bridge grafana-bridge jira-bridge \
      k3s-audit-shipper landgate push-bridge slack-bridge splash-export \
      traefik-log-shipper trigger-engine; \
    do \
      CGO_ENABLED=0 go build ${COVER_FLAGS} \
        -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o "/out/${bin}" "./cmd/${bin}/" || exit 1; \
    done

# gb is deliberately OUTSIDE the loop above and built WITHOUT ${COVER_FLAGS}.
# It is not a service image of its own: beads-viewer ships it as a subprocess
# that renders the agent prime view (cmd/beads-viewer/prime.go), and the
# Dockerfile this replaced built it with no cover flags on purpose -- an
# instrumented gb would write coverage files from inside a request path. CI
# never passes COVER, so this only matters to a local COVER=1 build, which is
# exactly the build that would be surprised by it.
RUN CGO_ENABLED=0 go build \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/gb ./cmd/gb/

# ── Artifact stage ─────────────────────────────────────────────────
# Downstream Dockerfiles pull single binaries via
#   COPY --from=<this-image-tag> /out/<name> /<name>
# A scratch artifact stage keeps the pulled layer to just /out (no Go
# toolchain, no controller source) instead of shipping the ~800MB builder.
FROM scratch AS artifacts
COPY --from=builder /out/ /out/
