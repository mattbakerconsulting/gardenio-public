#!/bin/bash
# gVisor (runsc) installer for a k3s agent node (Lands gate 3, gd-VtHRlD6gvB).
#
# Run by the gvisor-k3s-installer DaemonSet (templates/gvisor-k3s-installer.yaml)
# inside the host's namespaces. Inputs, from the pod env:
#   GVISOR_VERSION  pinned gVisor release, e.g. 20261026.0
#   GVISOR_SHA512   sha512 of that release's gvisor.tar.bz2
#   GVISOR_URL      where to download gvisor.tar.bz2
#
# Idempotent: a node already on GVISOR_VERSION, with the drop-in in place and
# k3s-agent restarted for it (the marker), is left alone. Otherwise install,
# write the drop-in, restart k3s-agent once, then write the marker. A run that
# dies before the marker is redone in full by the next one.
set -euo pipefail

BIN=/usr/local/bin
DROPIN=/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/runsc.toml
MARKER=/var/lib/gardenio/gvisor/installed
WANT="${GVISOR_VERSION} ${GVISOR_SHA512}"
DROPIN_BODY="[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = \"io.containerd.runsc.v1\""

log() { echo "gvisor-k3s-install: $*"; }

# A k3s server runs the k3s unit; the agent pool runs k3s-agent. Never touch a
# server, and never guess which unit to restart.
if systemctl is-active --quiet k3s.service; then
	log "k3s (server) is active on this node; refusing to install"
	exit 1
fi
if ! systemctl cat k3s-agent.service >/dev/null 2>&1; then
	log "no k3s-agent unit on this node; refusing to install"
	exit 1
fi
if [ "$(uname -m)" != "x86_64" ]; then
	log "unsupported architecture $(uname -m); refusing to install"
	exit 1
fi

have_version() {
	[ -x "$BIN/runsc" ] && "$BIN/runsc" --version 2>/dev/null | grep -qF "$GVISOR_VERSION"
}
have_dropin() {
	[ -f "$DROPIN" ] && [ "$(cat "$DROPIN")" = "$DROPIN_BODY" ]
}
have_marker() {
	[ -f "$MARKER" ] && [ "$(cat "$MARKER")" = "$WANT" ]
}

if have_version && have_dropin && have_marker; then
	log "runsc ${GVISOR_VERSION} already installed and registered; nothing to do"
	exit 0
fi

tmp=$(mktemp -d /var/tmp/gvisor-install.XXXXXX)
trap 'rm -rf "$tmp"' EXIT

log "downloading ${GVISOR_URL}"
python3 - "$GVISOR_URL" "$GVISOR_SHA512" "$tmp" <<'PY'
import hashlib, sys, tarfile, urllib.request

url, want, dest = sys.argv[1:]
path = dest + "/gvisor.tar.bz2"
h = hashlib.sha512()
with urllib.request.urlopen(url, timeout=300) as r, open(path, "wb") as f:
    while chunk := r.read(1 << 20):
        h.update(chunk)
        f.write(chunk)
got = h.hexdigest()
if got != want:
    sys.exit(f"sha512 mismatch for {url}: got {got}, want {want}")
kw = {"filter": "data"} if hasattr(tarfile, "data_filter") else {}
with tarfile.open(path, "r:bz2") as t:
    t.extractall(dest + "/x", **kw)
PY

runsc=$(find "$tmp/x" -type f -name runsc | head -n 1)
if [ -z "$runsc" ]; then
	log "no runsc in the tarball"
	exit 1
fi
src=$(dirname "$runsc")
for f in containerd-shim-runsc-v1 gvisor-bin/gvisor_sentry; do
	if [ ! -f "$src/$f" ]; then
		log "tarball has no $f next to runsc"
		exit 1
	fi
done

# Install every file with the tarball's layout. install(1) unlinks before
# writing, so a binary a running sandbox holds open is replaced, not rewritten.
# runsc goes last: until it is in place the version check above keeps failing.
(cd "$src" && find . -type f ! -path ./runsc) | while read -r f; do
	install -D -m 0755 "$src/$f" "$BIN/${f#./}"
done
install -D -m 0755 "$src/runsc" "$BIN/runsc"
log "installed $("$BIN/runsc" --version | head -n 1)"

mkdir -p "$(dirname "$DROPIN")"
printf '%s\n' "$DROPIN_BODY" >"$DROPIN.tmp"
mv "$DROPIN.tmp" "$DROPIN"
log "wrote $DROPIN"

# k3s-agent has KillMode=process: its pods (this one included) keep running.
log "restarting k3s-agent to register the runsc runtime"
systemctl restart k3s-agent.service

mkdir -p "$(dirname "$MARKER")"
printf '%s\n' "$WANT" >"$MARKER"
log "done: runsc ${GVISOR_VERSION} registered as containerd runtime runsc"
