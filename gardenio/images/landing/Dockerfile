# landing: the public front door at https://clonk.tv (plan gd-tLxfoaw366, route
# gd-Z3eh4eEJ87, gd-gQ8hZLd4AQ). The splash, exported to static files by
# cmd/splash-export, baked into nginx. Request an invite and Sign in are
# absolute links to the app at https://gardenio.clonk.tv; the page fetches
# nothing from beads-viewer.
#
# Served by the chart's landing component with landing.page=image, which
# mounts only default.conf (cache headers, /healthz) over this image and runs
# it as 65534 on a read-only root, with emptyDirs on /tmp, /var/cache/nginx
# and /var/run (templates/landing/deployment.yaml).
#
# Follows the controller-base consumer pattern (images/_controller-base/):
# splash-export comes from the shared compile, runs once at build time, and
# is not in the final image. CONTROLLER_BASE has no default -- a build that
# forgets to pass it fails loudly instead of silently pulling a stale or
# nonexistent :latest.
#
# Build (context is gardenio/):
#   docker build -t gardenio/landing:latest -f images/landing/Dockerfile . \
#     --build-arg CONTROLLER_BASE=<registry>/gardenio/cache/controller-base:<sha>

ARG CONTROLLER_BASE

FROM ${CONTROLLER_BASE} AS controller_base

# ── Export ──────────────────────────────────────────────────────────
# Exec-form RUN: distroless has no shell, and the binary is static.
FROM gcr.io/distroless/static-debian12 AS export

COPY --from=controller_base /out/splash-export /splash-export
RUN ["/splash-export", "-out", "/site", "-app-origin", "https://gardenio.clonk.tv"]

# ── Runtime ─────────────────────────────────────────────────────────
FROM nginx:1-alpine

# Only the splash: no nginx welcome page or 50x.html beside it.
RUN rm -rf /usr/share/nginx/html/*
COPY --from=export /site/ /usr/share/nginx/html/

EXPOSE 80
