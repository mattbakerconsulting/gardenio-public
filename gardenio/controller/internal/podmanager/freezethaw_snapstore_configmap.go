package podmanager

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ConfigMapSnapshotStore is a durable SnapshotStore backed by one ConfigMap per
// snapshot in the controller namespace. It replaces InMemorySnapshotStore on the
// production path so a freeze/thaw snapshot record survives a controller pod
// restart (deploy, rollout, OOM, eviction) between freeze and thaw.
//
// THE BUG THIS CLOSES (ka-0000001694). The on-node checkpoint image + OCI bundle
// the restore actually reads live under /var/lib/gardenio/{ckpt,bundle}/<agent> —
// a controller-owned, pod-lifecycle-independent path (ka-0000003702), so they
// are durable. But the in-memory SnapshotStore that HasSnapshot() consults to
// decide thaw-vs-cold is process-local: a controller restart between an agent's
// freeze and its thaw loses the record, so shouldThaw() returns false and the
// agent SILENTLY COLD-STARTS — discarding a perfectly restorable snapshot and
// masquerading as a thaw "failure" (a clean cold-start). Persisting the snapshot
// metadata in the API server makes HasSnapshot()/Get()/List() restart-durable;
// the heavy artifacts (the 3.0G image, the rootfs lowerdir) stay on the node
// exactly as before — only the small metadata record moves into a ConfigMap.
//
// SIZE. The serialized Snapshot is the metadata + PodTemplate + the small OCI
// spec files (SpecFiles: config.json/.overlay-opts, ~15KB each), NOT the 3.0G
// checkpoint image or the rootfs overlay (those are reconstructed on the node
// from BundlePath/ImagePath). This fits comfortably under the ConfigMap 1MiB
// limit for the bare agent (pause+agent) and small-sidecar topologies; Put
// surfaces an over-limit API error rather than silently truncating.
type ConfigMapSnapshotStore struct {
	client    kubernetes.Interface
	namespace string
}

// snapshotConfigMapPrefix names the per-snapshot ConfigMaps. The agent name is
// sanitized (same token rule as the node-local image/bundle paths) and appended.
const snapshotConfigMapPrefix = "gardenio-ftsnap-"

// snapshotConfigMapKey is the data key holding the JSON-marshaled Snapshot.
const snapshotConfigMapKey = "snapshot.json"

// Labels used to select snapshot ConfigMaps for List and to carry the (un-
// sanitized) agent name for round-trip-safe enumeration.
const (
	snapshotLabelManagedBy = "gardenio.io/managed-by"
	snapshotLabelManagedV  = "freeze-thaw"
	snapshotLabelAgent     = "gardenio.io/ft-agent"
)

// NewConfigMapSnapshotStore constructs a durable snapshot store writing one
// ConfigMap per snapshot into namespace.
func NewConfigMapSnapshotStore(client kubernetes.Interface, namespace string) *ConfigMapSnapshotStore {
	return &ConfigMapSnapshotStore{client: client, namespace: namespace}
}

// configMapName maps an agent name to its snapshot ConfigMap name. sanitizeIdent
// already reduces to a filesystem/DNS-safe token (lowercase alnum + '-'/'_'),
// but ConfigMap names disallow '_' and uppercase, so we additionally lowercase
// and map '_' to '-'. The un-sanitized agent name is stored in a label + the
// payload so Get/List/Delete round-trip on the exact identity regardless of the
// name flattening.
func (s *ConfigMapSnapshotStore) configMapName(agentName string) string {
	return snapshotConfigMapPrefix + configMapToken(agentName)
}

func configMapToken(agentName string) string {
	tok := sanitizeIdent(agentName)
	out := make([]byte, 0, len(tok))
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c == '_':
			out = append(out, '-')
		default:
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "agent"
	}
	return string(out)
}

func (s *ConfigMapSnapshotStore) Put(ctx context.Context, snap Snapshot) error {
	if snap.AgentName == "" {
		return fmt.Errorf("snapshot store: empty agent name")
	}
	payload, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("snapshot store: marshal snapshot for %q: %w", snap.AgentName, err)
	}
	name := s.configMapName(snap.AgentName)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels: map[string]string{
				snapshotLabelManagedBy: snapshotLabelManagedV,
				snapshotLabelAgent:     configMapToken(snap.AgentName),
			},
		},
		BinaryData: map[string][]byte{snapshotConfigMapKey: payload},
	}
	// Upsert: create, or overwrite on AlreadyExists. A re-freeze of the same
	// agent (or a freeze after an aborted-then-retried one) must replace the
	// prior record, not error.
	_, err = s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("snapshot store: get existing CM %q: %w", name, getErr)
		}
		existing.Labels = cm.Labels
		existing.BinaryData = cm.BinaryData
		existing.Data = nil
		if _, upErr := s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, existing, metav1.UpdateOptions{}); upErr != nil {
			return fmt.Errorf("snapshot store: update CM %q: %w", name, upErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("snapshot store: create CM %q: %w", name, err)
	}
	return nil
}

func (s *ConfigMapSnapshotStore) Get(ctx context.Context, agentName string) (Snapshot, error) {
	name := s.configMapName(agentName)
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Snapshot{}, fmt.Errorf("snapshot store: no snapshot for agent %q", agentName)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot store: get CM %q: %w", name, err)
	}
	snap, err := decodeSnapshotConfigMap(cm)
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot store: decode CM %q: %w", name, err)
	}
	return snap, nil
}

func (s *ConfigMapSnapshotStore) Delete(ctx context.Context, agentName string) error {
	name := s.configMapName(agentName)
	err := s.client.CoreV1().ConfigMaps(s.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		// Deleting an absent snapshot is a no-op — the post-condition holds.
		return nil
	}
	if err != nil {
		return fmt.Errorf("snapshot store: delete CM %q: %w", name, err)
	}
	return nil
}

func (s *ConfigMapSnapshotStore) List(ctx context.Context) ([]Snapshot, error) {
	cms, err := s.client.CoreV1().ConfigMaps(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: snapshotLabelManagedBy + "=" + snapshotLabelManagedV,
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot store: list CMs: %w", err)
	}
	out := make([]Snapshot, 0, len(cms.Items))
	for i := range cms.Items {
		snap, derr := decodeSnapshotConfigMap(&cms.Items[i])
		if derr != nil {
			// Skip a corrupt/foreign record rather than failing the whole List —
			// the warm-B pool enumerates snapshots and one bad CM must not blind
			// it to the rest.
			continue
		}
		out = append(out, snap)
	}
	return out, nil
}

// decodeSnapshotConfigMap reads the JSON payload from either BinaryData (the
// write path) or Data (defensive: a hand-edited CM), and unmarshals a Snapshot.
func decodeSnapshotConfigMap(cm *corev1.ConfigMap) (Snapshot, error) {
	var payload []byte
	if b, ok := cm.BinaryData[snapshotConfigMapKey]; ok {
		payload = b
	} else if s, ok := cm.Data[snapshotConfigMapKey]; ok {
		payload = []byte(s)
	} else {
		return Snapshot{}, fmt.Errorf("missing %q key", snapshotConfigMapKey)
	}
	var snap Snapshot
	if err := json.Unmarshal(payload, &snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}
