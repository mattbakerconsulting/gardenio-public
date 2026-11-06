package podmanager

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// freezethaw_s3transport.go — the OFF-NODE SnapshotTransport (Δ5-B2,
// ka-0000006160). It moves the heavy checkpoint image + OCI bundle a Snapshot
// references between the node that FROZE the agent and a DIFFERENT node that
// will THAW it, so a thaw is no longer pinned to snap.Node. It is the B2 drop-in
// for the B1 seam: FreezeThawManager.SetSnapshotTransport(NewS3SnapshotTransport(...))
// with NO change to the Freeze/Thaw verbs (they already call Push at freeze and
// Pull at thaw over the SnapshotTransport interface).
//
// TRANSPORT SHAPE — presigned URL + node-side curl, controller IRSA presigns.
// The 3.0G artifacts live ON THE NODE (/var/lib/gardenio/{ckpt,bundle}/<agent>),
// not in the controller, so the controller cannot os.Open them. Rather than
// stream the whole payload through the controller's exec stdout pipe (base64,
// +33%, ~4G in RAM — a non-starter), the controller PRESIGNS S3 PUT/GET URLs
// with its IRSA identity and hands them to a small node-side script run over the
// same ProbeNodeExecutor the freeze/thaw scripts use. The node curls the bytes
// DIRECTLY to/from S3 — the controller never touches the payload. This is why
// the IAM grant lands on the CONTROLLER's IRSA role (s3:PutObject/GetObject on
// the snapshot prefix), not the node instance profile: the node only needs
// network egress + curl + a short-lived presigned URL, no AWS credentials.
//
// KEY LAYOUT — s3://<bucket>/<prefix>/<agent>/<gen>/{snapshot.tar,snapshot.tar.sha256}.
// <gen> is derived deterministically from the snapshot (its CreatedAt nanos) so
// Push (freeze) and Pull (thaw) compute the SAME key from the SAME Snapshot
// record without any extra state — the metadata record the SnapshotStore already
// persists is the shared source of truth. Objects are reaped by an S3 lifecycle
// EXPIRY rule on the prefix (provisioned via terraform, not by this code), so
// the transport has no Delete verb — the SnapshotStore owns metadata deletion,
// the bucket lifecycle owns payload expiry.
//
// INTEGRITY — end-to-end sha256, fail-closed. Push computes sha256 over the tar
// on the origin node and uploads it as a sidecar object. Pull downloads the
// sidecar, downloads the tar, verifies sha256 BEFORE extracting, and only then
// unpacks into place. A checksum mismatch (or any curl/tar non-zero) fails the
// Pull closed — the caller cold-starts, exactly like a reaped image — rather
// than restoring a corrupt checkpoint. Verify-before-extract is why each side
// stages the tar once to a temp file; a fully-streaming transport (the
// criu-image-streamer remote-streaming model, restore-through-lifecycle.md
// "remaining work" #2) that removes even that one stage is a future
// optimization tracked separately — the runsc multi-file layout needs a
// protocol adapter spike, whereas this S3 stage model transfers today.
//
// STAGE COUNT — "stage once each side", never the 3.0G-twice the recon warned
// of. Push tars ImagePath+BundlePath to ONE temp file on the origin node (a tar
// is needed for a single Content-Length'd PUT and for one checksum over the
// whole payload), uploads, and deletes the temp. Pull downloads to ONE temp
// file on the target node, verifies, extracts to the real paths, and deletes
// the temp. Neither side copies the payload twice.

// presignedRequest is a transport-local view of a presigned S3 request — just
// the fields the node-side curl needs. It deliberately does NOT import the AWS
// signer type so the transport core is unit-testable with a fake presigner and
// carries no SDK dependency; the concrete s3PresignAdapter (freezethaw_s3presign.go)
// converts *v4.PresignedHTTPRequest into this shape.
type presignedRequest struct {
	URL     string
	Headers http.Header // signed headers the request must carry (Host is set by curl)
}

// snapshotPresigner mints short-lived presigned S3 URLs for the snapshot
// payload + checksum objects. Push needs PUT, Pull needs GET. It is an interface
// so tests inject a hermetic fake (recordingPresigner) and the S3-backed adapter
// (s3PresignAdapter) stays a thin wrapper over *s3.PresignClient.
type snapshotPresigner interface {
	PresignPut(ctx context.Context, key string) (presignedRequest, error)
	PresignGet(ctx context.Context, key string) (presignedRequest, error)
}

// DefaultSnapshotS3Prefix is the key prefix under which snapshot payloads live
// when the operator does not override it. Kept in one place so the terraform
// lifecycle rule, the IRSA policy resource ARN, and this code agree.
const DefaultSnapshotS3Prefix = "ftsnap"

// defaultPresignTTL bounds how long a minted URL is usable. It must comfortably
// exceed a single push/pull of the ~3.0G payload over the node's egress but stay
// short enough that a leaked URL is not a durable credential. The freeze/thaw
// hard deadlines already cap the whole operation well under this.
const defaultPresignTTL = 30 * time.Minute

// S3SnapshotTransport is the off-node SnapshotTransport: it uploads the heavy
// artifacts to S3 at freeze (Push) and re-materializes them onto the target node
// at thaw (Pull), so a frozen agent can thaw on ANY node, not just snap.Node.
// See the file header for the presigned-URL + node-curl design.
type S3SnapshotTransport struct {
	// exec runs the upload/download scripts on the relevant node (origin for
	// Push, target for Pull) via the same privileged probe the freeze/thaw verbs
	// use. Required.
	exec NodeExecutor
	// presigner mints the short-lived PUT/GET URLs. Required.
	presigner snapshotPresigner
	// bucket is the S3 bucket holding snapshot payloads. Required.
	bucket string
	// prefix is the key prefix (default DefaultSnapshotS3Prefix). Never leading/
	// trailing-slashed in a key.
	prefix string
	logger *slog.Logger
}

// NewS3SnapshotTransport constructs the off-node transport. bucket must be
// non-empty; an empty prefix falls back to DefaultSnapshotS3Prefix. A nil logger
// is replaced with a discard logger so the transport never panics on log.
func NewS3SnapshotTransport(exec NodeExecutor, presigner snapshotPresigner, bucket, prefix string, logger *slog.Logger) (*S3SnapshotTransport, error) {
	if exec == nil {
		return nil, fmt.Errorf("s3 snapshot transport: nil NodeExecutor")
	}
	if presigner == nil {
		return nil, fmt.Errorf("s3 snapshot transport: nil presigner")
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("s3 snapshot transport: empty bucket")
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = DefaultSnapshotS3Prefix
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &S3SnapshotTransport{
		exec:      exec,
		presigner: presigner,
		bucket:    bucket,
		prefix:    prefix,
		logger:    logger,
	}, nil
}

// snapshotGeneration derives the deterministic per-freeze generation token from
// the snapshot. CreatedAt is stamped once at freeze and travels in the persisted
// metadata record, so Push and Pull compute the identical key. UTC UnixNano is
// monotonic-enough across a single agent's freeze cycles and collision-free in
// practice; a zero CreatedAt (older record) degrades to "0", which still round-
// trips within a single freeze→thaw pair.
func snapshotGeneration(snap Snapshot) string {
	return fmt.Sprintf("%d", snap.CreatedAt.UTC().UnixNano())
}

// tarKey / sha256Key are the two object keys for a snapshot. Both derive purely
// from the Snapshot record so freeze and thaw agree with no shared state.
func (t *S3SnapshotTransport) tarKey(snap Snapshot) string {
	return fmt.Sprintf("%s/%s/%s/snapshot.tar", t.prefix, snap.AgentName, snapshotGeneration(snap))
}

func (t *S3SnapshotTransport) sha256Key(snap Snapshot) string {
	return t.tarKey(snap) + ".sha256"
}

// Push is invoked at FREEZE time on the origin node (snap.Node), once the
// checkpoint image + OCI bundle are written to their node-local paths. It tars
// them, checksums the tar, and uploads both the tar and the checksum sidecar to
// S3 via presigned PUTs. Any failure returns an error, which fails the freeze
// closed (no metadata persisted, caller cold-starts) — the same fail-safe as the
// un-thawable-snapshot guards, so a snapshot is only ever recorded when its
// payload is durably in S3.
func (t *S3SnapshotTransport) Push(ctx context.Context, snap Snapshot) error {
	if err := validateSnapshotPaths(snap); err != nil {
		return fmt.Errorf("s3 transport push: %w", err)
	}
	if snap.Node == "" {
		return fmt.Errorf("s3 transport push: snapshot for %s has no origin node", snap.AgentName)
	}
	tarPut, err := t.presigner.PresignPut(ctx, t.tarKey(snap))
	if err != nil {
		return fmt.Errorf("s3 transport push: presign tar PUT for %s: %w", snap.AgentName, err)
	}
	shaPut, err := t.presigner.PresignPut(ctx, t.sha256Key(snap))
	if err != nil {
		return fmt.Errorf("s3 transport push: presign checksum PUT for %s: %w", snap.AgentName, err)
	}
	script := buildS3PushScript(snap, tarPut, shaPut)
	res, err := t.exec.Exec(ctx, snap.Node, script)
	if err != nil {
		return fmt.Errorf("s3 transport push: exec on node %s for %s: %w", snap.Node, snap.AgentName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("s3 transport push: upload script failed on node %s for %s (rc=%d): %s",
			snap.Node, snap.AgentName, res.ExitCode, tailStderr(res.Stderr))
	}
	t.logger.Info("s3 transport: snapshot pushed",
		"agent", snap.AgentName, "node", snap.Node, "bucket", t.bucket, "key", t.tarKey(snap))
	return nil
}

// Pull is invoked at THAW time on the chosen target node, before the restore
// runs. When targetNode is empty or equals snap.Node the payload is already
// node-local (path-b thaw pins Pod B to snap.Node today) and Pull is the
// identity no-op — no S3 round-trip. For a genuine off-node target it downloads
// the checksum + tar via presigned GETs, verifies sha256 BEFORE extracting, and
// unpacks the artifacts into snap.ImagePath / snap.BundlePath on the target
// node. Any failure returns an error, which fails the thaw closed and the caller
// cold-starts — the same fail-safe as a reaped image.
func (t *S3SnapshotTransport) Pull(ctx context.Context, snap Snapshot, targetNode string) error {
	if err := validateSnapshotPaths(snap); err != nil {
		return fmt.Errorf("s3 transport pull: %w", err)
	}
	// Node-local short-circuit: the artifacts already sit on the target node, so
	// there is nothing to move (and re-downloading over the live paths would be
	// wasteful and risk clobbering them). This mirrors NodeLocalSnapshotTransport's
	// identity semantics for the same-node case.
	if targetNode == "" || (snap.Node != "" && targetNode == snap.Node) {
		return nil
	}
	tarGet, err := t.presigner.PresignGet(ctx, t.tarKey(snap))
	if err != nil {
		return fmt.Errorf("s3 transport pull: presign tar GET for %s: %w", snap.AgentName, err)
	}
	shaGet, err := t.presigner.PresignGet(ctx, t.sha256Key(snap))
	if err != nil {
		return fmt.Errorf("s3 transport pull: presign checksum GET for %s: %w", snap.AgentName, err)
	}
	script := buildS3PullScript(snap, tarGet, shaGet)
	res, err := t.exec.Exec(ctx, targetNode, script)
	if err != nil {
		return fmt.Errorf("s3 transport pull: exec on node %s for %s: %w", targetNode, snap.AgentName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("s3 transport pull: download/verify script failed on node %s for %s (rc=%d): %s",
			targetNode, snap.AgentName, res.ExitCode, tailStderr(res.Stderr))
	}
	t.logger.Info("s3 transport: snapshot pulled onto target node",
		"agent", snap.AgentName, "originNode", snap.Node, "targetNode", targetNode,
		"bucket", t.bucket, "key", t.tarKey(snap))
	return nil
}

// validateSnapshotPaths asserts the snapshot carries the two node-local paths
// the tar packs/unpacks. Both must be absolute so the tar's `-C /` relative
// entries reconstruct the exact same absolute layout on the target node.
func validateSnapshotPaths(snap Snapshot) error {
	if snap.AgentName == "" {
		return fmt.Errorf("snapshot has no agent name")
	}
	if !strings.HasPrefix(snap.ImagePath, "/") {
		return fmt.Errorf("snapshot for %s has non-absolute ImagePath %q", snap.AgentName, snap.ImagePath)
	}
	if !strings.HasPrefix(snap.BundlePath, "/") {
		return fmt.Errorf("snapshot for %s has non-absolute BundlePath %q", snap.AgentName, snap.BundlePath)
	}
	return nil
}

// tailStderr trims node stderr to its last few lines so a transport error
// surfaces the actionable tail (the curl/tar failure) without dumping the whole
// script trace into the freeze/thaw log line.
func tailStderr(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return "(no stderr)"
	}
	lines := strings.Split(stderr, "\n")
	const keep = 5
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return strings.Join(lines, " | ")
}

// discardWriter is an io.Writer that drops everything, used for the nil-logger
// fallback so the transport never nil-panics on a log call.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

var _ SnapshotTransport = (*S3SnapshotTransport)(nil)
