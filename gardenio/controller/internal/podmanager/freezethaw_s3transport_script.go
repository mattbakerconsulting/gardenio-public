package podmanager

import (
	"fmt"
	"sort"
	"strings"
)

// freezethaw_s3transport_script.go — the node-side bash the S3SnapshotTransport
// runs over the probe NodeExecutor. Rendered by the controller (which holds the
// presigned URLs) and streamed to the node over stdin (`bash -s`, no argv-size
// ceiling — the URLs are long). Both scripts open `set -euo pipefail` and are
// pure programs with no top-level positional args, exactly like the freeze/thaw
// node scripts, so a non-zero exit is a fail-closed signal to the caller.

// curlBase is the shared curl invocation: fail-closed on HTTP >=400 (--fail),
// quiet but error-reporting, bounded retries for transient network blips, and a
// hard time cap that comfortably covers a 3.0G transfer while never hanging the
// freeze/thaw critical section forever. -f makes a 403 (expired/again-signed
// URL) or 404 (missing object) a non-zero exit rather than a 0-byte "success".
const curlBase = "curl --fail --silent --show-error --retry 3 --retry-connrefused --retry-delay 2 --max-time 1800"

// curlHeaderArgs renders the presigned request's signed headers as curl -H
// args, deterministically ordered for testability. Host is skipped — curl sets
// it from the URL authority, and re-sending it can conflict with the request
// line. Header values are single-quoted; presigned SigV4 header values are
// URL/base64 tokens with no single quote, so this is safe.
func curlHeaderArgs(req presignedRequest) string {
	if len(req.Headers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(req.Headers))
	for k := range req.Headers {
		if strings.EqualFold(k, "Host") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		for _, v := range req.Headers[k] {
			fmt.Fprintf(&b, " -H %s", shellQuoteS3(k+": "+v))
		}
	}
	return b.String()
}

// shellQuoteS3 single-quotes a value for safe embedding in the rendered bash.
// A literal single quote is closed, escaped, and reopened ('\” idiom). Used
// for the controller-generated paths + presigned URLs; those never contain a
// single quote in practice, but the idiom keeps the transport robust.
func shellQuoteS3(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildS3PushScript renders the origin-node upload: tar the checkpoint image +
// OCI bundle into ONE temp file (needed for a single Content-Length'd PUT and a
// single checksum over the whole payload), sha256 it, and PUT both the tar and
// the checksum sidecar via their presigned URLs. The temp file is always removed
// (trap), so the origin node never accrues a second full copy of the payload.
func buildS3PushScript(snap Snapshot, tarPut, shaPut presignedRequest) string {
	img := shellQuoteS3(snap.ImagePath)
	bnd := shellQuoteS3(snap.BundlePath)
	tarURL := shellQuoteS3(tarPut.URL)
	shaURL := shellQuoteS3(shaPut.URL)
	return fmt.Sprintf(`set -euo pipefail
IMAGE=%[1]s
BUNDLE=%[2]s
test -d "$IMAGE" || { echo "s3-push: image path $IMAGE missing" >&2; exit 3; }
test -d "$BUNDLE" || { echo "s3-push: bundle path $BUNDLE missing" >&2; exit 3; }
IMG_REL="${IMAGE#/}"
BND_REL="${BUNDLE#/}"
TMP="$(mktemp /var/tmp/ftsnap-push-XXXXXX.tar)"
trap 'rm -f "$TMP" "$TMP.sha"' EXIT
# No gzip: pages.img is dense/incompressible, so compression only burns CPU on
# the freeze critical path. -C / keeps entries relative so extract restores the
# exact absolute layout on the target node.
tar -C / -cf "$TMP" "$IMG_REL" "$BND_REL"
SHA="$(sha256sum "$TMP" | awk '{print $1}')"
printf '%%s' "$SHA" > "$TMP.sha"
%[3]s%[5]s -T "$TMP" %[4]s
%[6]s%[8]s -T "$TMP.sha" %[7]s
echo "s3-push: uploaded $(stat -c %%s "$TMP") bytes sha256=$SHA"
`, img, bnd, curlBase, tarURL, curlHeaderArgs(tarPut), curlBase, shaURL, curlHeaderArgs(shaPut))
}

// buildS3PullScript renders the target-node download+verify+extract: fetch the
// checksum sidecar and the tar via their presigned GETs, verify sha256 BEFORE
// extracting (so a corrupt payload never becomes a corrupt restore), then unpack
// into the exact absolute paths. Idempotent: if the image dir is already
// populated on this node (a retried thaw), it skips the download. The temp file
// is always removed (trap), so the target node stages the payload exactly once.
func buildS3PullScript(snap Snapshot, tarGet, shaGet presignedRequest) string {
	img := shellQuoteS3(snap.ImagePath)
	bnd := shellQuoteS3(snap.BundlePath)
	tarURL := shellQuoteS3(tarGet.URL)
	shaURL := shellQuoteS3(shaGet.URL)
	return fmt.Sprintf(`set -euo pipefail
IMAGE=%[1]s
BUNDLE=%[2]s
if [ -n "$(ls -A "$IMAGE" 2>/dev/null)" ] && [ -n "$(ls -A "$BUNDLE" 2>/dev/null)" ]; then
  echo "s3-pull: artifacts already present at $IMAGE and $BUNDLE, skipping download"
  exit 0
fi
TMP="$(mktemp /var/tmp/ftsnap-pull-XXXXXX.tar)"
trap 'rm -f "$TMP" "$TMP.sha"' EXIT
%[3]s%[5]s -o "$TMP.sha" %[4]s
EXPECTED="$(tr -d '[:space:]' < "$TMP.sha")"
if [ -z "$EXPECTED" ]; then echo "s3-pull: empty checksum sidecar" >&2; exit 4; fi
%[6]s%[8]s -o "$TMP" %[7]s
ACTUAL="$(sha256sum "$TMP" | awk '{print $1}')"
if [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "s3-pull: checksum mismatch expected=$EXPECTED actual=$ACTUAL" >&2
  exit 5
fi
# Verified: unpack into place. -C / restores the absolute layout tar -C / captured.
tar -C / -xf "$TMP"
test -d "$IMAGE" || { echo "s3-pull: image path $IMAGE absent after extract" >&2; exit 6; }
test -d "$BUNDLE" || { echo "s3-pull: bundle path $BUNDLE absent after extract" >&2; exit 6; }
echo "s3-pull: restored $IMAGE and $BUNDLE sha256=$ACTUAL"
`, img, bnd, curlBase, shaURL, curlHeaderArgs(shaGet), curlBase, tarURL, curlHeaderArgs(tarGet))
}
