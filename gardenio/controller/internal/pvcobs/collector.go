// Package pvcobs emits observability gauges for orphaned Pending
// PersistentVolumeClaims — PVCs stuck in the Pending phase for far longer than
// a healthy WaitForFirstConsumer bind should ever take, with no live pod
// consuming them. These are a silent EBS/storage cost leak: the day-1
// cost-gardener survey (ka-0000002737) found observability/storage-observability-
// tempo-0 (~200d Pending, consumer pod NotFound) and dolt/redis-data-bd-daemon-
// redis-master-0 (~148d Pending, consumer relocated onto a different bound
// volume) — both long-abandoned, neither owned by anyone watching.
//
// Why a controller collector: the cost-gardener has read-only survey authority
// and no delete rights, and these PVCs live in foreign namespaces
// (observability/, dolt/) the GC collectors deliberately do not touch (pvcgc
// only sweeps gardenio-labeled workspace PVCs). The gap is not deletion, it is
// VISIBILITY — nothing publishes a live series an operator can alert on. The
// in-chart vmagent scrapes only annotated release-namespace pods (no
// kube-state-metrics), so a controller-emitted gauge on the already-scraped
// :9092 /metrics endpoint is the only way to land the series on new prod.
//
// The collector lists PVCs (and pods, to establish consumer liveness)
// cluster-wide on an interval, classifies those Pending past a threshold age
// with no referencing pod as orphaned, and publishes count / oldest-age /
// requested-bytes gauges plus a per-PVC info series. It is observe-only: it
// never deletes anything. It is opt-in (interval <= 0 disables it) and gated in
// the chart behind agents.pvcObs.enabled, which also provisions the
// cluster-scoped persistentvolumeclaims:list + pods:list RBAC the controller SA
// does not otherwise hold.
package pvcobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultThreshold is how long a PVC must sit in Pending with no consumer
// before it counts as orphaned. A healthy WaitForFirstConsumer PVC binds within
// seconds of its pod scheduling, so days of Pending means the consumer is gone.
// 7 days is well clear of any legitimate transient Pending (a pod stuck
// unschedulable for a deploy, a slow provisioner) while still catching the
// long-abandoned leaks the cost-gardener found (148d / 199d).
const DefaultThreshold = 7 * 24 * time.Hour

// Stats is the result of a single sweep, returned for logging and tests.
type Stats struct {
	// TotalPVCs is every PVC seen across all namespaces this sweep.
	TotalPVCs int
	// PendingPVCs is every PVC in the Pending phase, regardless of age.
	PendingPVCs int
	// OrphanedPVCs is Pending-past-threshold PVCs with no referencing pod.
	OrphanedPVCs int
	// RequestedBytes is the summed requested storage of the orphaned PVCs.
	RequestedBytes int64
	// MaxAge is the age of the oldest orphaned PVC (0 when none).
	MaxAge time.Duration
}

// Collector periodically classifies orphaned Pending PVCs cluster-wide and
// publishes them as Prometheus gauges.
type Collector struct {
	client    kubernetes.Interface
	threshold time.Duration
	interval  time.Duration
	logger    *slog.Logger

	// now is the clock, overridable in tests; defaults to time.Now.
	now func() time.Time

	// The three summary gauges are label-less and so need no Reset(): every
	// sweep overwrites the single series with the current value (including 0),
	// so a cleaned-up cluster correctly reports 0 rather than a stale
	// high-water mark. This is the poller-gauge pattern; contrast the
	// monotonic counters in internal/metrics.
	count        prometheus.Gauge // gardenio_orphaned_pending_pvcs
	maxAgeGauge  prometheus.Gauge // gardenio_orphaned_pending_pvc_max_age_seconds
	requestBytes prometheus.Gauge // gardenio_orphaned_pending_pvc_requested_bytes

	// info is the per-PVC breakdown, value = age in seconds, keyed on the PVC's
	// namespace/name/storageclass so an operator can see exactly WHICH claims to
	// clean up. Being a vec it CAN accumulate stale series (an orphan that gets
	// deleted, or binds), so it is Reset() at the top of every sweep and
	// repopulated — mirroring the GaugeVec handling in internal/metrics.
	info *prometheus.GaugeVec
}

// New constructs an orphaned-Pending-PVC observability collector. threshold
// defaults to DefaultThreshold when non-positive. The gauges are created here
// but only exported once Register is called on the controller's registry.
func New(client kubernetes.Interface, threshold, interval time.Duration, logger *slog.Logger) *Collector {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	return &Collector{
		client:    client,
		threshold: threshold,
		interval:  interval,
		logger:    logger,
		now:       time.Now,
		count: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "gardenio",
			Name:      "orphaned_pending_pvcs",
			Help:      "Live count of PersistentVolumeClaims stuck Pending past the orphan threshold with no consuming pod, re-derived on every controller sweep. Current-state gauge backing the OrphanedPendingPVCs cost-leak alert (ka-0000002737); the in-chart vmagent does not scrape kube-state-metrics, so this controller gauge is the only source of the series on new prod.",
		}),
		maxAgeGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "gardenio",
			Name:      "orphaned_pending_pvc_max_age_seconds",
			Help:      "Age in seconds of the OLDEST orphaned Pending PVC (0 when none), re-derived on every controller sweep. An orphan aging for months is a long-abandoned storage/EBS cost leak; this catches the severity dimension the raw count misses.",
		}),
		requestBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "gardenio",
			Name:      "orphaned_pending_pvc_requested_bytes",
			Help:      "Total requested storage in bytes summed across all orphaned Pending PVCs (0 when none), re-derived on every controller sweep. Approximates the wasted-capacity dollar exposure so the alert can be prioritized by size.",
		}),
		info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gardenio",
			Name:      "orphaned_pending_pvc_age_seconds",
			Help:      "Per-PVC age in seconds of each orphaned Pending PVC, keyed on namespace/name/storageclass so an operator can identify exactly which claims to clean up. Reset and repopulated every sweep, so a resolved orphan drops out.",
		}, []string{"namespace", "name", "storageclass"}),
	}
}

// Register registers all gauges with reg. Served alongside the other controller
// metrics on the :9092 /metrics endpoint.
func (c *Collector) Register(reg prometheus.Registerer) error {
	for _, col := range []prometheus.Collector{c.count, c.maxAgeGauge, c.requestBytes, c.info} {
		if err := reg.Register(col); err != nil {
			return err
		}
	}
	return nil
}

// storageClassLabel returns a stable label value for a PVC's storage class,
// mapping the nil/empty "use default class" case to "<none>" so the label is
// never an empty string.
func storageClassLabel(pvc *corev1.PersistentVolumeClaim) string {
	if sc := pvc.Spec.StorageClassName; sc != nil && *sc != "" {
		return *sc
	}
	return "<none>"
}

// requestedBytes returns the PVC's requested storage in bytes, or 0 when the
// request is absent or unparseable (a missing request must never NaN the sum).
func requestedBytes(pvc *corev1.PersistentVolumeClaim) int64 {
	q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return 0
	}
	v, ok := q.AsInt64()
	if !ok || v < 0 {
		return 0
	}
	return v
}

// referencedPVCs returns the set of "namespace/name" claim keys referenced by
// any pod volume. A Pending PVC that a live pod still references is NOT
// orphaned — it is mid-scheduling or the pod is unschedulable, a distinct
// problem this collector deliberately does not flag. Keys are namespaced
// because a claimName is only unique within its namespace.
func referencedPVCs(pods []corev1.Pod) map[string]bool {
	refs := make(map[string]bool)
	for i := range pods {
		pod := &pods[i]
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				refs[pod.Namespace+"/"+vol.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	return refs
}

// Collect runs a single sweep: it lists PVCs and pods cluster-wide, classifies
// orphaned Pending PVCs (Pending, older than the threshold, referenced by no
// pod), and sets the gauges. On a list error ALL gauges are left UNTOUCHED (not
// zeroed) — zeroing on a transient API failure would mask a real leak and
// silently clear the alert; leaving the series stale is the safer failure mode
// (vmalert staleness handles it). Stats are returned
// for logging/tests; err is non-nil only on a list failure.
func (c *Collector) Collect(ctx context.Context) (Stats, error) {
	pvcList, err := c.client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error("failed to list PVCs for orphan observability", "error", err)
		return Stats{}, err
	}
	podList, err := c.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error("failed to list pods for orphan observability", "error", err)
		return Stats{}, err
	}

	now := c.now()
	referenced := referencedPVCs(podList.Items)

	var stats Stats
	stats.TotalPVCs = len(pvcList.Items)

	// Build the per-PVC series fresh each sweep so a resolved orphan drops out.
	c.info.Reset()

	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		if pvc.Status.Phase != corev1.ClaimPending {
			continue
		}
		stats.PendingPVCs++

		age := now.Sub(pvc.CreationTimestamp.Time)
		if age < c.threshold {
			continue // still within a plausible bind window
		}
		if referenced[pvc.Namespace+"/"+pvc.Name] {
			continue // a live pod still wants this claim — not orphaned
		}

		stats.OrphanedPVCs++
		stats.RequestedBytes += requestedBytes(pvc)
		if age > stats.MaxAge {
			stats.MaxAge = age
		}
		c.info.WithLabelValues(pvc.Namespace, pvc.Name, storageClassLabel(pvc)).Set(age.Seconds())
	}

	c.count.Set(float64(stats.OrphanedPVCs))
	c.maxAgeGauge.Set(stats.MaxAge.Seconds())
	c.requestBytes.Set(float64(stats.RequestedBytes))
	return stats, nil
}

// RunLoop runs an immediate sweep, then re-sweeps every interval until ctx is
// cancelled. A non-positive interval is a no-op (the collector is disabled) —
// the caller should simply not construct one, but this guards the direct call.
func (c *Collector) RunLoop(ctx context.Context) {
	if c.interval <= 0 {
		return
	}

	if stats, err := c.Collect(ctx); err == nil {
		c.logger.Info("orphaned-pending-PVC observability collector started",
			"threshold", c.threshold, "interval", c.interval,
			"orphans", stats.OrphanedPVCs, "pending", stats.PendingPVCs, "total", stats.TotalPVCs)
	}

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if stats, err := c.Collect(ctx); err == nil && stats.OrphanedPVCs > 0 {
				c.logger.Info("orphaned Pending PVCs observed",
					"orphans", stats.OrphanedPVCs,
					"requested_bytes", stats.RequestedBytes,
					"max_age", stats.MaxAge.Round(time.Second))
			}
		case <-ctx.Done():
			return
		}
	}
}
