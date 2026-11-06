# Beads daemon (kd) — multi-stage build.
# CGO required for libicu (unicode normalization).
#
# Build:
#   docker build --build-arg VERSION=dev --build-arg COMMIT=$(git rev-parse --short HEAD) -t kd .

ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    libicu-dev gcc g++ && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /build
# Build context is the repo root because kbeads/go.mod has
# `replace gardenio/clicreds => ../gardenio/clicreds` and
# `replace gardenio/lands/contract => ../lands/contract` (the Lands map, D.6).
COPY kbeads/go.mod kbeads/go.sum ./
COPY gardenio/clicreds/ /gardenio/clicreds/
COPY lands/contract/ /lands/contract/
RUN go mod download

COPY kbeads/ ./
ARG VERSION=dev
ARG COMMIT=unknown
ARG BRANCH=unknown
ARG COVER=0
# Stamp version/commit/branch/date so `kd version` and the daemon's
# X-Kbeads-Version skew header report the real build (ka-0000002965).
RUN if [ "$COVER" = "1" ]; then \
      COVER_FLAGS="-cover -coverpkg=./..."; \
    else \
      COVER_FLAGS=""; \
    fi && \
    BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" && \
    CGO_ENABLED=1 GOOS=linux go build ${COVER_FLAGS} \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.Build=${COMMIT} \
      -X gardenio/kbeads/internal/buildinfo.Branch=${BRANCH} \
      -X gardenio/kbeads/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /kd ./cmd/kd

FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata libicu74 netcat-openbsd && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd -f -g 1000 beads && \
    useradd -u 1000 -g beads -s /bin/bash -m -d /home/beads beads || true

COPY --from=builder /kd /usr/local/bin/kd

USER 1000
WORKDIR /home/beads

ENTRYPOINT ["kd"]
CMD ["serve"]
