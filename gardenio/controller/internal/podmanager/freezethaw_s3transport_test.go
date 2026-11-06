package podmanager

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freezethaw_s3transport_test.go — guards for the OFF-NODE S3 SnapshotTransport
// (Δ5-B2, ka-0000006160). These pin the seam contract an off-node transport must
// honour: node-local Pull is the identity no-op (no S3), off-node Push/Pull run
// on the correct node with the right keys, integrity is end-to-end (sha256 in
// the rendered script), and EVERY failure surface (presign error, non-zero node
// exit, exec transport error, bad snapshot) fails CLOSED so the caller cold-
// starts rather than recording/restoring a payload that never arrived.

// recordingPresigner is a hermetic snapshotPresigner test double: it records the
// keys it was asked to presign and returns deterministic URLs, and can be primed
// to fail either verb.
type recordingPresigner struct {
	putKeys []string
	getKeys []string
	putErr  error
	getErr  error
	headers http.Header
}

func (r *recordingPresigner) PresignPut(_ context.Context, key string) (presignedRequest, error) {
	r.putKeys = append(r.putKeys, key)
	if r.putErr != nil {
		return presignedRequest{}, r.putErr
	}
	return presignedRequest{URL: "https://s3.example/" + key + "?sig=PUT", Headers: r.headers}, nil
}

func (r *recordingPresigner) PresignGet(_ context.Context, key string) (presignedRequest, error) {
	r.getKeys = append(r.getKeys, key)
	if r.getErr != nil {
		return presignedRequest{}, r.getErr
	}
	return presignedRequest{URL: "https://s3.example/" + key + "?sig=GET", Headers: r.headers}, nil
}

var _ snapshotPresigner = (*recordingPresigner)(nil)

// recordingExec is a NodeExecutor double capturing the (node, script) pairs and
// returning a canned result.
type recordingExec struct {
	calls   []recordingExecCall
	result  NodeExecResult
	execErr error
}

type recordingExecCall struct {
	node   string
	script string
}

func (e *recordingExec) Exec(_ context.Context, node, script string) (NodeExecResult, error) {
	e.calls = append(e.calls, recordingExecCall{node: node, script: script})
	if e.execErr != nil {
		return NodeExecResult{}, e.execErr
	}
	return e.result, nil
}

var _ NodeExecutor = (*recordingExec)(nil)

func testSnap() Snapshot {
	return Snapshot{
		AgentName:  "agent-a",
		Node:       "node-1",
		ImagePath:  "/var/lib/gardenio/ckpt/agent-a",
		BundlePath: "/var/lib/gardenio/bundle/agent-a",
		CreatedAt:  time.Unix(1700000000, 123).UTC(),
	}
}

func newTestS3Transport(t *testing.T, exec NodeExecutor, presigner snapshotPresigner) *S3SnapshotTransport {
	t.Helper()
	tr, err := NewS3SnapshotTransport(exec, presigner, "snap-bucket", "", nil)
	if err != nil {
		t.Fatalf("NewS3SnapshotTransport: %v", err)
	}
	return tr
}

func TestNewS3SnapshotTransport_Validation(t *testing.T) {
	exec := &recordingExec{}
	pre := &recordingPresigner{}
	if _, err := NewS3SnapshotTransport(nil, pre, "b", "", nil); err == nil {
		t.Error("nil exec must error")
	}
	if _, err := NewS3SnapshotTransport(exec, nil, "b", "", nil); err == nil {
		t.Error("nil presigner must error")
	}
	if _, err := NewS3SnapshotTransport(exec, pre, "  ", "", nil); err == nil {
		t.Error("empty bucket must error")
	}
	// Empty prefix defaults to ftsnap; a slashed prefix is trimmed.
	tr, err := NewS3SnapshotTransport(exec, pre, "b", "/custom/", nil)
	if err != nil {
		t.Fatalf("valid construction: %v", err)
	}
	if tr.prefix != "custom" {
		t.Errorf("prefix should trim slashes, got %q", tr.prefix)
	}
	tr2 := newTestS3Transport(t, exec, pre)
	if tr2.prefix != DefaultSnapshotS3Prefix {
		t.Errorf("empty prefix should default to %q, got %q", DefaultSnapshotS3Prefix, tr2.prefix)
	}
}

func TestS3Transport_KeysAreDeterministicAndPrefixed(t *testing.T) {
	tr := newTestS3Transport(t, &recordingExec{}, &recordingPresigner{})
	snap := testSnap()
	gen := snapshotGeneration(snap)
	wantTar := "ftsnap/agent-a/" + gen + "/snapshot.tar"
	if tr.tarKey(snap) != wantTar {
		t.Errorf("tarKey = %q, want %q", tr.tarKey(snap), wantTar)
	}
	if tr.sha256Key(snap) != wantTar+".sha256" {
		t.Errorf("sha256Key = %q, want %q", tr.sha256Key(snap), wantTar+".sha256")
	}
	// Same snapshot record → same key (freeze and thaw must agree).
	if tr.tarKey(snap) != tr.tarKey(testSnap()) {
		t.Error("key derivation must be deterministic across identical snapshots")
	}
}

func TestS3Transport_PushUploadsBothObjects(t *testing.T) {
	exec := &recordingExec{result: NodeExecResult{ExitCode: 0}}
	pre := &recordingPresigner{}
	tr := newTestS3Transport(t, exec, pre)
	snap := testSnap()
	if err := tr.Push(context.Background(), snap); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// Presigned a PUT for BOTH the tar and the checksum sidecar.
	if len(pre.putKeys) != 2 {
		t.Fatalf("expected 2 PUT presigns (tar + sha256), got %d: %v", len(pre.putKeys), pre.putKeys)
	}
	if pre.putKeys[0] != tr.tarKey(snap) || pre.putKeys[1] != tr.sha256Key(snap) {
		t.Errorf("PUT keys = %v, want [%s %s]", pre.putKeys, tr.tarKey(snap), tr.sha256Key(snap))
	}
	// Ran on the ORIGIN node.
	if len(exec.calls) != 1 || exec.calls[0].node != snap.Node {
		t.Fatalf("Push must exec once on origin node %q, got %+v", snap.Node, exec.calls)
	}
	// The script tars, checksums, and uploads via both presigned URLs.
	script := exec.calls[0].script
	for _, want := range []string{
		"set -euo pipefail", "tar -C / -cf", "sha256sum",
		snap.ImagePath, snap.BundlePath,
		tr.tarKey(snap), tr.sha256Key(snap),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("push script missing %q\n---\n%s", want, script)
		}
	}
}

func TestS3Transport_PushFailsClosedOnNonZeroExit(t *testing.T) {
	exec := &recordingExec{result: NodeExecResult{ExitCode: 3, Stderr: "s3-push: image path missing"}}
	tr := newTestS3Transport(t, exec, &recordingPresigner{})
	err := tr.Push(context.Background(), testSnap())
	if err == nil {
		t.Fatal("Push must fail closed on non-zero node exit")
	}
	if !strings.Contains(err.Error(), "rc=3") {
		t.Errorf("error should carry the exit code, got: %v", err)
	}
}

func TestS3Transport_PushFailsClosedOnPresignError(t *testing.T) {
	pre := &recordingPresigner{putErr: errors.New("no creds")}
	exec := &recordingExec{}
	tr := newTestS3Transport(t, exec, pre)
	if err := tr.Push(context.Background(), testSnap()); err == nil {
		t.Fatal("Push must fail closed when presign fails")
	}
	if len(exec.calls) != 0 {
		t.Error("Push must not exec on the node when presign already failed")
	}
}

func TestS3Transport_PushFailsClosedOnExecError(t *testing.T) {
	exec := &recordingExec{execErr: errors.New("probe gone")}
	tr := newTestS3Transport(t, exec, &recordingPresigner{})
	if err := tr.Push(context.Background(), testSnap()); err == nil {
		t.Fatal("Push must fail closed on exec transport error")
	}
}

func TestS3Transport_PushRejectsBadSnapshot(t *testing.T) {
	exec := &recordingExec{}
	tr := newTestS3Transport(t, exec, &recordingPresigner{})
	// Non-absolute ImagePath.
	bad := testSnap()
	bad.ImagePath = "relative/path"
	if err := tr.Push(context.Background(), bad); err == nil {
		t.Error("Push must reject a non-absolute ImagePath")
	}
	// Missing origin node.
	noNode := testSnap()
	noNode.Node = ""
	if err := tr.Push(context.Background(), noNode); err == nil {
		t.Error("Push must reject a snapshot with no origin node")
	}
	if len(exec.calls) != 0 {
		t.Error("Push must not exec for an invalid snapshot")
	}
}

func TestS3Transport_PullNodeLocalIsIdentity(t *testing.T) {
	exec := &recordingExec{}
	pre := &recordingPresigner{}
	tr := newTestS3Transport(t, exec, pre)
	snap := testSnap()
	// Same-node target: identity no-op, NO S3, NO exec.
	if err := tr.Pull(context.Background(), snap, snap.Node); err != nil {
		t.Fatalf("same-node Pull must be identity, got: %v", err)
	}
	// Unset target: also identity.
	if err := tr.Pull(context.Background(), snap, ""); err != nil {
		t.Fatalf("unset-target Pull must be identity, got: %v", err)
	}
	if len(exec.calls) != 0 || len(pre.getKeys) != 0 {
		t.Errorf("node-local Pull must not touch S3 or the node (exec=%d get=%d)", len(exec.calls), len(pre.getKeys))
	}
}

func TestS3Transport_PullOffNodeDownloadsVerifiesExtracts(t *testing.T) {
	exec := &recordingExec{result: NodeExecResult{ExitCode: 0}}
	pre := &recordingPresigner{}
	tr := newTestS3Transport(t, exec, pre)
	snap := testSnap()
	if err := tr.Pull(context.Background(), snap, "node-2"); err != nil {
		t.Fatalf("off-node Pull: %v", err)
	}
	// Presigned a GET for BOTH objects.
	if len(pre.getKeys) != 2 {
		t.Fatalf("expected 2 GET presigns, got %d: %v", len(pre.getKeys), pre.getKeys)
	}
	// Ran on the TARGET node, not the origin.
	if len(exec.calls) != 1 || exec.calls[0].node != "node-2" {
		t.Fatalf("off-node Pull must exec on the TARGET node, got %+v", exec.calls)
	}
	script := exec.calls[0].script
	// Integrity: verify-before-extract (sha256 compare gate, then tar -x).
	for _, want := range []string{
		"set -euo pipefail", "sha256sum", "checksum mismatch", "tar -C / -xf",
		snap.ImagePath, snap.BundlePath,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("pull script missing %q\n---\n%s", want, script)
		}
	}
	// The checksum compare must precede the extract (fail-closed ordering).
	if strings.Index(script, "checksum mismatch") > strings.Index(script, "tar -C / -xf") {
		t.Error("pull script must verify checksum BEFORE extracting")
	}
}

func TestS3Transport_PullFailsClosedOnChecksumExit(t *testing.T) {
	// Exit 5 is the script's checksum-mismatch code.
	exec := &recordingExec{result: NodeExecResult{ExitCode: 5, Stderr: "s3-pull: checksum mismatch expected=aa actual=bb"}}
	tr := newTestS3Transport(t, exec, &recordingPresigner{})
	err := tr.Pull(context.Background(), testSnap(), "node-2")
	if err == nil {
		t.Fatal("off-node Pull must fail closed on a checksum-mismatch exit")
	}
	if !strings.Contains(err.Error(), "rc=5") {
		t.Errorf("error should carry the exit code, got: %v", err)
	}
}

func TestS3Transport_PullFailsClosedOnPresignError(t *testing.T) {
	pre := &recordingPresigner{getErr: errors.New("no creds")}
	exec := &recordingExec{}
	tr := newTestS3Transport(t, exec, pre)
	if err := tr.Pull(context.Background(), testSnap(), "node-2"); err == nil {
		t.Fatal("off-node Pull must fail closed on presign error")
	}
	if len(exec.calls) != 0 {
		t.Error("Pull must not exec when presign already failed")
	}
}

func TestS3Transport_SatisfiesSeam(t *testing.T) {
	// A compile-time + runtime assertion that the S3 transport is a drop-in for
	// the B1 seam and can be installed via SetSnapshotTransport.
	var _ SnapshotTransport = (*S3SnapshotTransport)(nil)
	mgr := &FreezeThawManager{}
	tr := newTestS3Transport(t, &recordingExec{}, &recordingPresigner{})
	mgr.SetSnapshotTransport(tr)
	if mgr.transportOrDefault() != tr {
		t.Fatal("S3 transport must install via SetSnapshotTransport")
	}
}

func TestCurlHeaderArgs_SkipsHostSortsDeterministically(t *testing.T) {
	h := http.Header{}
	h.Set("Host", "s3.example")
	h.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	h.Add("X-Amz-Meta-A", "1")
	args := curlHeaderArgs(presignedRequest{Headers: h})
	if strings.Contains(args, "Host:") {
		t.Errorf("Host header must be skipped, got: %s", args)
	}
	if !strings.Contains(args, "X-Amz-Content-Sha256: UNSIGNED-PAYLOAD") {
		t.Errorf("signed header must be rendered, got: %s", args)
	}
	// Deterministic ordering: Content-Sha256 sorts before Meta-A.
	if strings.Index(args, "X-Amz-Content-Sha256") > strings.Index(args, "X-Amz-Meta-A") {
		t.Errorf("headers must be sorted deterministically, got: %s", args)
	}
}
