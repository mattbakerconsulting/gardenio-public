# grafana-assistant-plugin: dist-only artifact image for the custom
# gardenio-assistant-app Grafana app plugin (C4 ka-0000007568, epic
# ka-0000002011 — OSS Grafana chat frontend, Path C).
#
# This image is NOT a server — it is a tiny carrier that ships the COMPILED
# plugin dist at /plugin so a copy-only initContainer in the Grafana pod can
# seed it into /var/lib/grafana/plugins/gardenio-assistant-app. The frontend is
# built with npm and the BACKEND binary (ka-0000000293 — the MCP reverse-proxy
# that repoints the chat box at an in-cluster mcp-grafana) with mage, both here
# (where there is network), so the runtime step is a pure `cp` with zero
# off-cluster egress — honouring the chart's default-deny-egress envelope.
# (We control the whole stack: no plugin signing, no catalog distribution.)
#
# Build (local):
#   docker build -t gardenio/grafana-assistant-plugin:dev \
#     -f gardenio/images/grafana-assistant-plugin/Dockerfile gardenio/grafana-plugins/gardenio-assistant-app
#
# No workflow builds this image today (images.yml does not list it; see
# gardenio/docs/deploy-inventory.md). The runtime base is ubuntu:24.04 so the
# chart initContainer finds a real shell + coreutils for the `cp`.

# ── Frontend builder: compile the plugin to dist/ ───────────────────────────
# Pin Node 24 to match the plugin's verified toolchain (packageManager
# npm@11.12.1; engines.node >=22). The build context is the plugin source dir
# (gardenio/grafana-plugins/gardenio-assistant-app).
FROM node:24-bookworm AS frontend

WORKDIR /build
COPY package.json package-lock.json ./
# .npmrc sets ignore-scripts=true (supply-chain hardening); honour it.
COPY .npmrc ./
RUN npm ci

COPY . ./
RUN npm run build

# ── Backend builder: compile the Go plugin backend (gpx_ binary) ────────────
# The backend is a thin MCP reverse-proxy (ka-0000000293). Build it with mage
# (the Grafana SDK build target, exactly as plugin.json's `executable` expects)
# so dist/ carries gpx_gardenio_assistant_app_linux_amd64 alongside the frontend.
# GOWORK=off keeps this an isolated module build (the repo go.work would
# otherwise pull it into the monorepo workspace).
FROM golang:1.25-bookworm AS backend

WORKDIR /build
ENV GOWORK=off
# Module graph first for layer caching.
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
# mage build:linux emits dist/gpx_gardenio_assistant_app_linux_amd64 (amd64 —
# matches the prod grafana nodeSelector). Install mage, then run the SDK target.
RUN go run github.com/magefile/mage@v1.15.0 build:linux \
  && test -f dist/gpx_gardenio_assistant_app_linux_amd64

# ── Runtime: dist-only carrier on a shell-bearing base ──────────────────────
# ubuntu:24.04 mirrors the crane-append base used by the CI build path, so the
# in-cluster seed-assistant-plugin initContainer (sh -c 'cp -r /plugin/. ...')
# finds the same /bin/sh + coreutils whether the image came from `docker build`
# or `crane append`. The dist is world-readable so the non-root (65534)
# initContainer can read it.
FROM ubuntu:24.04

COPY --from=frontend /build/dist /plugin
# Merge the backend binary into the same /plugin payload Grafana discovers.
COPY --from=backend /build/dist/gpx_gardenio_assistant_app_linux_amd64 /plugin/gpx_gardenio_assistant_app_linux_amd64

# Sanity markers: plugin.json is what Grafana keys plugin discovery on, and the
# gpx_ backend binary is what it spawns as the plugin backend — both are
# asserted by the initContainer before copying, so fail the build if either is
# absent.
RUN test -f /plugin/plugin.json \
  && test -f /plugin/gpx_gardenio_assistant_app_linux_amd64
