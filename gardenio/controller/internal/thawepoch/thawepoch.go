// Package thawepoch implements the thaw-epoch signal — Stage 0 of the agent
// rehydration design (gardenio/docs/gvisor-runsc-cr/rehydration-design.md,
// ka-0000001515). It is the foundation the downstream rehydration stages gate
// on: credential re-mint (ka-0000007211) and NATS reconnect (ka-0000001901).
//
// # Why a signal is needed
//
// When a checkpointed gVisor agent is thawed into a fresh kubelet-owned pod,
// the process never restarts — main() does not run again, so boot-time init
// cannot re-establish the agent's external identity and connections. The
// in-RAM copies of every credential and the 4-tuple of every long-lived TCP
// connection are stale or dead (rehydration-design §0). We therefore need an
// explicit signal *into* the running process telling it "you were just thawed;
// re-hydrate before you resume issuing work."
//
// # The contract (on-disk)
//
//	$stateDir/thaw-epoch.json              marker, written by the controller post-restore
//	$stateDir/thaw-epoch.<consumer>.done   ack, written by each agent-side consumer
//
// The marker carries a monotonic epoch plus the identity of the pod the agent
// was thawed into. Each consumer (the gb supervisor and coop) runs a Watcher
// that latches the epoch, runs its rehydration Handler, and writes an Ack. The
// controller runs WaitForAcks to block releasing the agent until every required
// consumer acks the current epoch — or the deadline expires, in which case the
// thaw fails closed (C5).
//
// Stage 0 (this package) deliberately ships *only* the signal mechanics: the
// marker contract, the watcher loop with poll backstop, the ack protocol, the
// deadline gate, and the drift-detect self-trigger. The actual rehydration
// stages register themselves as Handlers in the downstream build beads.
package thawepoch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MarkerName is the filename of the thaw-epoch marker within the state dir.
const MarkerName = "thaw-epoch.json"

// ArmedName returns the filename of a consumer's "watcher armed" sentinel within
// the state dir. A consumer writes this once, at boot, the moment its thaw
// Watcher is live and able to observe a marker + write an ack. Its presence is
// the controller's pre-freeze proof that THIS consumer can participate in a
// future rehydration handshake.
//
// Why this exists (ka-0000006667 / ka-0000007918): a runsc agent pod becomes
// k8s-health-ready well before its agent loop (coop + gb) finishes booting and
// arms its watchers. Freezing in that window produces a checkpoint with no live
// watcher inside it — on thaw the controller's rehydration gate can never be
// acked (the watcher that would ack does not exist in the restored image), so
// the thaw fail-closes to a cold start even though `runsc restore` returned
// rc=0. The fix is to NOT freeze a pod whose watchers never armed: the absent
// sentinel tells Freeze to fall through to a normal teardown (clean cold start
// next spawn) instead of minting a structurally un-thawable snapshot.
func ArmedName(consumer string) string {
	return fmt.Sprintf("thaw-epoch.%s.armed", sanitize(consumer))
}

// ArmedPath returns a consumer's armed-sentinel path within stateDir.
func ArmedPath(stateDir, consumer string) string {
	return filepath.Join(stateDir, ArmedName(consumer))
}

// WriteArmed marks a consumer's thaw watcher as live by creating its armed
// sentinel in stateDir. Idempotent: re-arming (a restarted-not-thawed watcher)
// simply rewrites the same empty file. Best-effort by contract — a consumer
// that cannot write its sentinel should still arm its watcher; the guard fails
// safe (a missing sentinel only ever SKIPS a freeze, never forces one).
func WriteArmed(stateDir, consumer string) error {
	if sanitize(consumer) == "" {
		return errors.New("thawepoch: armed consumer must be set")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("thawepoch: mkdir state dir: %w", err)
	}
	// An empty marker file: presence is the whole signal. Atomic create so a
	// concurrent reader never sees a half-written name.
	return writeFileAtomic(ArmedPath(stateDir, consumer), nil)
}

// IsArmed reports whether a consumer's armed sentinel is present in stateDir.
// A read error other than not-exist is surfaced so the caller can fail safe.
func IsArmed(stateDir, consumer string) (bool, error) {
	_, err := os.Stat(ArmedPath(stateDir, consumer))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("thawepoch: stat armed sentinel: %w", err)
}

// DefaultDeadline is the proposed total rehydration budget (rehydration-design
// §5): thaw itself is sub-300ms, STS+NATS reconnect each <1s, and the rest is
// retry headroom on a freshly-networked pod. Comfortably under the 15-min
// presence reaper. Tunable via the marker's DeadlineMS field or the
// THAW_REHYDRATE_DEADLINE env (parsed by callers).
const DefaultDeadline = 10 * time.Second

// DefaultPollInterval is the watcher poll backstop cadence. The design treats
// poll as the safe floor on gVisor (inotify works but is not guaranteed across
// a restore); 1s keeps the worst-case detection latency well under the
// deadline.
const DefaultPollInterval = time.Second

// Consumer identifies an agent-side rehydration consumer. Each consumer writes
// its own ack file so the controller can wait on all of them independently.
const (
	ConsumerGB   = "gb"   // the Go supervisor (heartbeat, cred resolution, nats-bridge)
	ConsumerCoop = "coop" // the coop process (the four NATS transports)
)

// Marker is the thaw-epoch marker the controller writes post-restore. Required
// fields (epoch, pod_uid, node, ts) match the design's Stage-0 spec; the
// optional fields carry hints the rehydration stages consume (frozen_at for the
// poison stage, deadline_ms for the gate).
type Marker struct {
	// Epoch is monotonic across thaws of the same logical agent. A consumer
	// runs its Handler exactly once per epoch increment.
	Epoch int64 `json:"epoch"`
	// PodUID is the kubelet pod UID of the pod the agent was thawed into
	// (Pod B). Used by the drift backstop and the C1 identity checks.
	PodUID string `json:"pod_uid"`
	// Node is the node the thawed pod landed on.
	Node string `json:"node"`
	// TS is when the controller wrote the marker (post-restore). Marshalled
	// as RFC3339; coop reads it as an opaque string (it has no date crate).
	TS time.Time `json:"ts"`
	// FrozenAt, when set, is the wall-clock of the freeze. The poison stage
	// drops any in-RAM credential whose on-disk source predates this.
	FrozenAt time.Time `json:"frozen_at,omitzero"`
	// DeadlineMS, when >0, overrides DefaultDeadline for this thaw.
	DeadlineMS int64 `json:"deadline_ms,omitempty"`
}

// Deadline returns the rehydration budget for this thaw: the marker's
// DeadlineMS if positive, else DefaultDeadline.
func (m Marker) Deadline() time.Duration {
	if m.DeadlineMS > 0 {
		return time.Duration(m.DeadlineMS) * time.Millisecond
	}
	return DefaultDeadline
}

// MarkerPath returns the marker path within stateDir.
func MarkerPath(stateDir string) string { return filepath.Join(stateDir, MarkerName) }

// AckPath returns the ack path for a consumer within stateDir.
func AckPath(stateDir, consumer string) string {
	return filepath.Join(stateDir, fmt.Sprintf("thaw-epoch.%s.done", consumer))
}

// Write atomically writes the marker into stateDir. The controller calls this
// after runsc restore succeeds and before releasing the agent. The write is a
// temp-file + rename so a watcher never observes a partial marker.
//
// Write enforces monotonicity: it refuses to write an epoch that is not greater
// than the epoch already on disk (a re-frozen-then-thawed agent must never
// replay an old epoch, rehydration-design §8). Use NextEpoch to derive the
// next value.
func Write(stateDir string, m Marker) error {
	if m.Epoch <= 0 {
		return fmt.Errorf("thawepoch: epoch must be positive, got %d", m.Epoch)
	}
	if cur, ok, err := Read(stateDir); err != nil {
		return err
	} else if ok && m.Epoch <= cur.Epoch {
		return fmt.Errorf("thawepoch: epoch %d not greater than current %d (non-monotonic)", m.Epoch, cur.Epoch)
	}
	if m.TS.IsZero() {
		m.TS = time.Now().UTC()
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("thawepoch: mkdir state dir: %w", err)
	}
	return writeJSONAtomic(MarkerPath(stateDir), m)
}

// Read reads the current marker. ok is false (with nil error) when no marker
// exists yet.
func Read(stateDir string) (Marker, bool, error) {
	var m Marker
	b, err := os.ReadFile(MarkerPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("thawepoch: read marker: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, fmt.Errorf("thawepoch: parse marker: %w", err)
	}
	return m, true, nil
}

// NextEpoch returns one greater than the epoch currently on disk (1 if none),
// so the controller can advance the marker monotonically.
func NextEpoch(stateDir string) (int64, error) {
	cur, ok, err := Read(stateDir)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 1, nil
	}
	return cur.Epoch + 1, nil
}

// Ack is the per-consumer rehydration result for one epoch.
type Ack struct {
	Epoch    int64  `json:"epoch"`
	Consumer string `json:"consumer"`
	// OK is true when every required stage of the consumer's Handler
	// completed. A false OK is the consumer fail-closing (C5).
	OK bool `json:"ok"`
	// Error is a human-readable failure reason when OK is false.
	Error string `json:"error,omitempty"`
	// Stages records per-stage status ("ok"/"failed"/"skipped") for
	// observability; the gate only consults OK + Epoch.
	Stages map[string]string `json:"stages,omitempty"`
	// TS is the ack write time as a string. A string (not time.Time) lets
	// coop — which has no date crate — write the ack in the shared format
	// without the gate failing to parse it.
	TS string `json:"ts"`
}

// WriteAck atomically writes a consumer's ack into stateDir.
func WriteAck(stateDir string, a Ack) error {
	if a.Consumer == "" {
		return errors.New("thawepoch: ack consumer must be set")
	}
	if a.TS == "" {
		a.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("thawepoch: mkdir state dir: %w", err)
	}
	return writeJSONAtomic(AckPath(stateDir, a.Consumer), a)
}

// ReadAck reads a consumer's ack. ok is false (nil error) when absent.
func ReadAck(stateDir, consumer string) (Ack, bool, error) {
	var a Ack
	b, err := os.ReadFile(AckPath(stateDir, consumer))
	if errors.Is(err, os.ErrNotExist) {
		return a, false, nil
	}
	if err != nil {
		return a, false, fmt.Errorf("thawepoch: read ack: %w", err)
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return a, false, fmt.Errorf("thawepoch: parse ack: %w", err)
	}
	return a, true, nil
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("thawepoch: marshal: %w", err)
	}
	return writeFileAtomic(path, b)
}

// writeFileAtomic writes b to path via a temp-file + rename so a concurrent
// reader never observes a partial file. b may be nil/empty (e.g. a sentinel
// whose presence is the whole signal).
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("thawepoch: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("thawepoch: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("thawepoch: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("thawepoch: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("thawepoch: rename: %w", err)
	}
	return nil
}

// sanitize trims a consumer name to a safe, log-friendly token.
func sanitize(s string) string { return strings.TrimSpace(s) }
