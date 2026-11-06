// Package pvcgc implements garbage collection for orphaned workspace PVCs.
//
// Agent pods create PersistentVolumeClaims for workspace storage, but these
// PVCs are never cleaned up after pods complete. This collector periodically
// scans for PVCs with gardenio labels that are not mounted by any pod, and
// deletes them after a configurable TTL.
package pvcgc

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Stats holds the results of a single GC pass.
type Stats struct {
	TotalPVCs   int
	OrphanPVCs  int
	DeletedPVCs int
	Errors      int
}

// Collector garbage-collects orphaned workspace PVCs.
type Collector struct {
	client    kubernetes.Interface
	namespace string
	// extraNamespaces are additional namespaces swept for orphaned PVCs beyond
	// the primary `namespace`. Used for the PSA-privileged gVisor namespace
	// where runsc freeze/thaw agents (and their workspace PVCs) live
	// (ka-0000008076): without this their PVCs are invisible to GC and leak.
	extraNamespaces []string
	ttl             time.Duration
	logger          *slog.Logger
}

// New creates a PVC garbage collector.
// ttl is the minimum age of an orphaned PVC before it is deleted.
func New(client kubernetes.Interface, namespace string, ttl time.Duration, logger *slog.Logger) *Collector {
	return &Collector{
		client:    client,
		namespace: namespace,
		ttl:       ttl,
		logger:    logger,
	}
}

// SetExtraNamespaces configures additional namespaces swept for orphaned PVCs
// beyond the primary namespace. Empty or duplicate-of-primary entries are
// ignored. Used to include the PSA-privileged gVisor namespace where runsc
// freeze/thaw agents run (ka-0000008076).
func (c *Collector) SetExtraNamespaces(namespaces ...string) {
	for _, ns := range namespaces {
		if ns == "" || ns == c.namespace {
			continue
		}
		c.extraNamespaces = append(c.extraNamespaces, ns)
	}
}

// Collect runs a single garbage collection pass over the primary namespace and
// any configured extra namespaces. It lists all PVCs with the gardenio app
// label, checks which are mounted by running pods, and deletes orphans older
// than the configured TTL. Aggregated stats span every swept namespace; a
// failure in one namespace is recorded but does not abort the others.
func (c *Collector) Collect(ctx context.Context) Stats {
	var stats Stats

	for _, ns := range append([]string{c.namespace}, c.extraNamespaces...) {
		c.collectNamespace(ctx, ns, &stats)
	}

	if stats.DeletedPVCs > 0 || stats.OrphanPVCs > 0 {
		c.logger.Info("PVC garbage collection complete",
			"total", stats.TotalPVCs,
			"orphans", stats.OrphanPVCs,
			"deleted", stats.DeletedPVCs,
			"errors", stats.Errors)
	}

	return stats
}

// collectNamespace runs a single GC pass scoped to one namespace, accumulating
// into stats. Each PVC is matched against pods in the same namespace, so a PVC
// is never considered orphaned because a pod that mounts it lives elsewhere.
func (c *Collector) collectNamespace(ctx context.Context, namespace string, stats *Stats) {
	// List all PVCs with the gardenio label.
	pvcList, err := c.client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{"app.kubernetes.io/name": "gardenio"}.String(),
	})
	if err != nil {
		c.logger.Error("failed to list PVCs", "namespace", namespace, "error", err)
		stats.Errors++
		return
	}
	stats.TotalPVCs += len(pvcList.Items)

	if len(pvcList.Items) == 0 {
		return
	}

	// List all pods in the namespace to find which PVCs are in use.
	podList, err := c.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error("failed to list pods", "namespace", namespace, "error", err)
		stats.Errors++
		return
	}

	// Build a set of PVC names that are currently mounted by pods.
	mounted := mountedPVCs(podList.Items)

	now := time.Now()
	for _, pvc := range pvcList.Items {
		if mounted[pvc.Name] {
			continue // PVC is in use by a pod
		}

		stats.OrphanPVCs++

		// Check TTL: only delete PVCs older than the configured duration.
		age := pvcAge(pvc, now)
		if age < c.ttl {
			c.logger.Debug("orphan PVC below TTL, skipping",
				"pvc", pvc.Name, "namespace", namespace, "age", age.Round(time.Second), "ttl", c.ttl)
			continue
		}

		c.logger.Info("deleting orphan PVC",
			"pvc", pvc.Name, "namespace", namespace, "age", age.Round(time.Second))
		if err := c.client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{}); err != nil {
			c.logger.Error("failed to delete PVC", "pvc", pvc.Name, "namespace", namespace, "error", err)
			stats.Errors++
			continue
		}
		stats.DeletedPVCs++
	}
}

// mountedPVCs returns a set of PVC names that are referenced by any pod volume.
func mountedPVCs(pods []corev1.Pod) map[string]bool {
	m := make(map[string]bool)
	for _, pod := range pods {
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				m[vol.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	return m
}

// AnnotationPVCLastActive is duplicated here to avoid an import cycle with
// the podmanager package. Keep in sync with podmanager.AnnotationPVCLastActive.
const AnnotationPVCLastActive = "gardenio.io/pvc-last-active"

// pvcAge returns the age of a PVC based on the AnnotationPVCLastActive
// timestamp set by podmanager.ensurePVC. The annotation is refreshed every
// time an agent pod is created against the PVC, so the age represents
// "time since last agent activity" rather than "time since PVC creation" —
// crucial for thread-agent respawn, where the PVC must outlive the pod
// but the timer should reset on each respawn.
//
// Falls back to CreationTimestamp for PVCs created before the annotation
// rollout (back-compat).
func pvcAge(pvc corev1.PersistentVolumeClaim, now time.Time) time.Duration {
	if raw := pvc.Annotations[AnnotationPVCLastActive]; raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return now.Sub(t)
		}
	}
	return now.Sub(pvc.CreationTimestamp.Time)
}
