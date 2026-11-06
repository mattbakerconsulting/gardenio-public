package podmanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// ProbeNodeExecutor runs freeze/thaw scripts on a node by exec'ing into a
// privileged probe pod scheduled on that node (the hostPath/chroot pattern from
// probe-pod.yaml, the same node-access the PoC harnesses and Phase-2 use). It
// selects the probe pod by label + spec.nodeName, then runs the script under
// `chroot /host bash -s` (program streamed over stdin) so `runsc`,
// /proc/mounts, and the containerd runtime root are the node's, not the probe
// container's.
//
// The script is delivered via STDIN (`bash -s`), NOT as a `bash -c <script>`
// argv element: Linux execve caps a SINGLE argv string at MAX_ARG_STRLEN
// (131072 bytes / 32 pages), independent of the ARG_MAX total. The rendered
// thaw node script base64-inlines the snapshot OCI spec files into its body
// (freezethaw_node.go write_spec), so its serialized size scales with
// ContainerCount/spec size and crosses 128 KiB for multi-container snapshots —
// `bash -c <script>` then fail-closes with `exec /usr/sbin/chroot: argument
// list too long` (E2BIG) before any restore logic runs (bug ka-0000004900,
// gVisor canary #9). stdin has no arg-size ceiling, so the transport is
// size-independent. This is transparent to every node script: they are pure
// bash programs (each opens with `set -uo pipefail`) and pass no top-level
// positional args, so `$0`/`$@` semantics are unchanged.
//
// This is the production NodeExecutor. Tests use FakeNodeExecutor instead, so
// the freeze/thaw orchestration is unit-testable without a live gVisor node.
type ProbeNodeExecutor struct {
	client kubernetes.Interface
	cfg    *rest.Config

	// Namespace is where the probe pods live.
	Namespace string

	// ProbeSelector selects the privileged probe pods (one per gVisor node).
	// Defaults to {"app": "gvisor-probe"} matching probe-pod.yaml.
	ProbeSelector map[string]string

	// ProbeContainer is the container name to exec into. Defaults to "probe".
	ProbeContainer string

	// HostChrootPath is the in-probe path that mounts the node root ("/host"
	// in probe-pod.yaml). The script is run under `chroot <path> bash -s`
	// (program streamed over stdin).
	HostChrootPath string
}

// thawExecOptions builds the PodExecOptions for streaming `script` to the probe
// over stdin (`chroot <chrootPath> bash -s`). It is split out of Exec so the
// transport shape (Command + Stdin=true, never a `bash -c <script>` argv) is
// unit-testable without a live cluster — see freezethaw_exec_test.go, which
// asserts a >128 KiB script never lands in an argv element (the E2BIG bug
// ka-0000004900). `bash -s` reads the program from stdin, which has no
// MAX_ARG_STRLEN ceiling.
func thawExecOptions(container, chrootPath, script string) *corev1.PodExecOptions {
	_ = script // the script is delivered via the stdin stream, never as an argv element
	return &corev1.PodExecOptions{
		Container: container,
		Command:   []string{"chroot", chrootPath, "bash", "-s"},
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}
}

// NewProbeNodeExecutor constructs a probe-backed node executor. cfg is the
// rest.Config used to open the SPDY exec stream (remotecommand needs the raw
// config, not just the typed client).
func NewProbeNodeExecutor(client kubernetes.Interface, cfg *rest.Config, namespace string) *ProbeNodeExecutor {
	return &ProbeNodeExecutor{
		client:         client,
		cfg:            cfg,
		Namespace:      namespace,
		ProbeSelector:  map[string]string{"app": "gvisor-probe"},
		ProbeContainer: "probe",
		HostChrootPath: "/host",
	}
}

// Exec finds the probe pod on `node` and runs `script` on the node's host
// filesystem via `chroot <host> bash -s`, streaming the program over stdin so
// arbitrarily large scripts (the multi-container thaw body inlines base64 OCI
// specs and crosses 128 KiB) are not subject to the MAX_ARG_STRLEN single-argv
// limit (bug ka-0000004900). A non-zero script exit is returned in
// NodeExecResult.ExitCode (not as an error); only stream/transport failures
// return a non-nil error.
func (p *ProbeNodeExecutor) Exec(ctx context.Context, node, script string) (NodeExecResult, error) {
	probe, err := p.probeOnNode(ctx, node)
	if err != nil {
		return NodeExecResult{}, err
	}
	container := p.ProbeContainer
	if container == "" {
		container = "probe"
	}
	chrootPath := p.HostChrootPath
	if chrootPath == "" {
		chrootPath = "/host"
	}

	req := p.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(probe).
		Namespace(p.Namespace).
		SubResource("exec").
		VersionedParams(thawExecOptions(container, chrootPath, script), scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(p.cfg, "POST", req.URL())
	if err != nil {
		return NodeExecResult{}, fmt.Errorf("probe exec: new executor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  strings.NewReader(script),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	res := NodeExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if streamErr != nil {
		// A non-zero exit surfaces as a CodeExitError; extract the code rather
		// than treating it as a transport failure so callers apply their own
		// post-state assertions on the FT_* sentinels.
		if code, ok := exitCode(streamErr); ok {
			res.ExitCode = code
			return res, nil
		}
		return res, fmt.Errorf("probe exec stream on %s: %w", node, streamErr)
	}
	return res, nil
}

// probeOnNode returns the name of a Ready probe pod scheduled on `node`.
func (p *ProbeNodeExecutor) probeOnNode(ctx context.Context, node string) (string, error) {
	sel := labelsSelector(p.ProbeSelector)
	pods, err := p.client.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return "", fmt.Errorf("list probe pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == node && podIsReady(pod) {
			return pod.Name, nil
		}
	}
	return "", fmt.Errorf("no Ready probe pod (selector %q) on node %s", sel, node)
}

func labelsSelector(m map[string]string) string {
	var b bytes.Buffer
	first := true
	for k, v := range m {
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		first = false
	}
	return b.String()
}

// exitCode extracts a process exit code from a remotecommand stream error.
// A non-zero script exit surfaces as an exec.CodeExitError (which implements
// utilexec.ExitError).
func exitCode(err error) (int, bool) {
	var codeErr utilexec.CodeExitError
	if errors.As(err, &codeErr) {
		return codeErr.Code, true
	}
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus(), true
	}
	return 0, false
}

// InMemorySnapshotStore is the default SnapshotStore: a process-local map keyed
// by agent identity. It is sufficient for same-node thaw (the path-b PoC is
// node-local). An off-node store (S3 / criu-image-streamer transport,
// restore-through-lifecycle.md "remaining work" #2) implements the same
// interface without touching the verbs.
type InMemorySnapshotStore struct {
	mu   sync.RWMutex
	snap map[string]Snapshot
}

// NewInMemorySnapshotStore constructs an empty in-memory snapshot store.
func NewInMemorySnapshotStore() *InMemorySnapshotStore {
	return &InMemorySnapshotStore{snap: map[string]Snapshot{}}
}

func (s *InMemorySnapshotStore) Put(_ context.Context, snap Snapshot) error {
	if snap.AgentName == "" {
		return fmt.Errorf("snapshot store: empty agent name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap[snap.AgentName] = snap
	return nil
}

func (s *InMemorySnapshotStore) Get(_ context.Context, agentName string) (Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.snap[agentName]
	if !ok {
		return Snapshot{}, fmt.Errorf("snapshot store: no snapshot for agent %q", agentName)
	}
	return snap, nil
}

// Delete removes the agent's snapshot (C2 interruptible-freeze discard,
// ka-0000005834). Deleting an absent key is a no-op — the post-condition holds.
func (s *InMemorySnapshotStore) Delete(_ context.Context, agentName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snap, agentName)
	return nil
}

func (s *InMemorySnapshotStore) List(_ context.Context) ([]Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Snapshot, 0, len(s.snap))
	for _, snap := range s.snap {
		out = append(out, snap)
	}
	return out, nil
}

// NodeLocalSnapshotTransport is the default SnapshotTransport: a no-op that
// assumes the heavy artifacts (checkpoint image, OCI bundle, rootfs lowerdir)
// stay on the node that froze the agent, which is where the path-b thaw restores
// them (Thaw pins the placeholder Pod B to snap.Node). Push is the identity —
// the artifacts are already durable on-node when the metadata is persisted — and
// Pull is the identity as long as the thaw target IS the origin node. It never
// moves bytes off the node.
//
// It exists so the freeze/thaw verbs are written against the SnapshotTransport
// seam from day one; an off-node transport (S3 / criu-image-streamer, B2) drops
// in via FreezeThawManager.SetSnapshotTransport without touching Freeze/Thaw.
type NodeLocalSnapshotTransport struct{}

// NewNodeLocalSnapshotTransport constructs the node-local no-op transport.
func NewNodeLocalSnapshotTransport() *NodeLocalSnapshotTransport {
	return &NodeLocalSnapshotTransport{}
}

// Push is a no-op: the checkpoint image + bundle already live on snap.Node where
// the node-local thaw reads them, so there is nothing to move.
func (*NodeLocalSnapshotTransport) Push(_ context.Context, _ Snapshot) error {
	return nil
}

// Pull is the identity when the thaw target is the origin node (targetNode ==
// snap.Node, or unset) — the artifacts are already there. Because this transport
// cannot move bytes between nodes, a genuine off-node request (targetNode !=
// snap.Node) is refused fail-closed rather than silently restoring against a
// path that does not exist on the target node; the caller then cold-starts. That
// mismatch cannot arise today (thaw always targets snap.Node), so this is a
// defensive invariant, not a live path.
func (*NodeLocalSnapshotTransport) Pull(_ context.Context, snap Snapshot, targetNode string) error {
	if targetNode != "" && snap.Node != "" && targetNode != snap.Node {
		return fmt.Errorf("node-local snapshot transport: cannot relocate agent %q artifacts from origin node %q to target node %q (no off-node transport configured)",
			snap.AgentName, snap.Node, targetNode)
	}
	return nil
}
