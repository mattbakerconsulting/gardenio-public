package healthsnapshot

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/podmanager"
)

// SchemaVersion is the wire-format version of the snapshot JSON. Bump when
// renaming or removing tile fields; clients should refuse unknown major
// versions.
const SchemaVersion = 1

// Tile is the per-section R5 chart-catalog-shaped envelope: a payload (data),
// the renderer hint (spec), and tile metadata (meta). Keeping the shape
// uniform lets the htmx beads-viewer render each tile through the same
// template helper, and lets the eventual chart-catalog migration swap the
// data source without reshaping the wire format.
type Tile struct {
	Data any            `json:"data"`
	Spec map[string]any `json:"spec"`
	Meta map[string]any `json:"meta"`
}

// Snapshot is the full payload returned by GET /healthz/snapshot.
type Snapshot struct {
	SchemaVersion int       `json:"schema_version"`
	Now           time.Time `json:"now"`
	Tiles         Tiles     `json:"tiles"`
}

// Tiles is the named set of tiles in a snapshot.
type Tiles struct {
	PodCounts       Tile `json:"pod_counts"`
	RecentReaps     Tile `json:"recent_reaps"`
	RecentRotations Tile `json:"recent_rotations"`
	LastReconcile   Tile `json:"last_reconcile"`
}

// PodPhaseCount is one row of the pod_counts tile.
type PodPhaseCount struct {
	Phase string `json:"phase"`
	Count int    `json:"count"`
}

// PodLister is the narrow podmanager.Manager subset Snapshotter uses.
// Defining it here lets tests pass a stub without depending on K8s clients.
type PodLister interface {
	ListAgentPods(ctx context.Context, namespace string, labelSelector map[string]string) ([]corev1.Pod, error)
}

// Snapshotter builds a Snapshot from a Tracker plus a live pod listing.
type Snapshotter struct {
	tracker   *Tracker
	pods      PodLister
	namespace string
	now       func() time.Time
}

// NewSnapshotter returns a Snapshotter. tracker may be nil — in that case
// the recent_* and last_reconcile tiles render as empty.
func NewSnapshotter(tracker *Tracker, pods PodLister, namespace string) *Snapshotter {
	return &Snapshotter{
		tracker:   tracker,
		pods:      pods,
		namespace: namespace,
		now:       time.Now,
	}
}

// Build assembles a Snapshot. Listing pods can fail (transient API server
// blip); when it does, the pod_counts tile reports an error in its meta but
// the rest of the snapshot still renders.
func (s *Snapshotter) Build(ctx context.Context) Snapshot {
	now := s.now()
	return Snapshot{
		SchemaVersion: SchemaVersion,
		Now:           now,
		Tiles: Tiles{
			PodCounts:       s.podCountsTile(ctx),
			RecentReaps:     s.recentReapsTile(),
			RecentRotations: s.recentRotationsTile(),
			LastReconcile:   s.lastReconcileTile(),
		},
	}
}

func (s *Snapshotter) podCountsTile(ctx context.Context) Tile {
	rows := []PodPhaseCount{}
	meta := map[string]any{}

	if s.pods == nil {
		meta["error"] = "no pod lister configured"
		return Tile{
			Data: rows,
			Spec: map[string]any{"kind": "bar", "x": "phase", "y": "count"},
			Meta: meta,
		}
	}

	pods, err := s.pods.ListAgentPods(ctx, s.namespace, nil)
	if err != nil {
		meta["error"] = err.Error()
		return Tile{
			Data: rows,
			Spec: map[string]any{"kind": "bar", "x": "phase", "y": "count"},
			Meta: meta,
		}
	}

	counts := map[corev1.PodPhase]int{}
	for _, p := range pods {
		counts[p.Status.Phase]++
	}
	// Emit a stable ordering so the htmx renderer doesn't churn the DOM
	// across polls when counts are equal.
	for _, phase := range []corev1.PodPhase{
		corev1.PodPending,
		corev1.PodRunning,
		corev1.PodSucceeded,
		corev1.PodFailed,
		corev1.PodUnknown,
	} {
		rows = append(rows, PodPhaseCount{Phase: string(phase), Count: counts[phase]})
	}
	meta["total"] = len(pods)
	meta["namespace"] = s.namespace
	return Tile{
		Data: rows,
		Spec: map[string]any{"kind": "bar", "x": "phase", "y": "count"},
		Meta: meta,
	}
}

func (s *Snapshotter) recentReapsTile() Tile {
	events := s.tracker.RecentReaps()
	if events == nil {
		events = []ReapEvent{}
	}
	return Tile{
		Data: events,
		Spec: map[string]any{"kind": "event-list", "fields": []string{"timestamp", "checked", "reaped"}},
		Meta: map[string]any{"count": len(events), "max_kept": MaxRecentReaps},
	}
}

func (s *Snapshotter) recentRotationsTile() Tile {
	events := s.tracker.RecentRotations()
	if events == nil {
		events = []RotationEvent{}
	}
	return Tile{
		Data: events,
		Spec: map[string]any{"kind": "event-list", "fields": []string{"timestamp", "event", "project", "from_account", "to_account"}},
		Meta: map[string]any{"count": len(events), "max_kept": MaxRecentRotations},
	}
}

func (s *Snapshotter) lastReconcileTile() Tile {
	last := s.tracker.LastReconcile()
	lastOK := s.tracker.LastSuccessfulReconcile()
	meta := map[string]any{}
	var data any
	if last != nil {
		data = last
		meta["last_at"] = last.CompletedAt
		meta["last_duration_ms"] = last.DurationMs
		if last.Error != "" {
			meta["last_error"] = last.Error
		}
	}
	if lastOK != nil {
		meta["last_ok_at"] = lastOK.CompletedAt
		meta["last_ok_duration_ms"] = lastOK.DurationMs
	}
	return Tile{
		Data: data,
		Spec: map[string]any{"kind": "single-stat", "field": "completed_at"},
		Meta: meta,
	}
}

// Compile-time check that K8sManager satisfies PodLister.
var _ PodLister = (*podmanager.K8sManager)(nil)
