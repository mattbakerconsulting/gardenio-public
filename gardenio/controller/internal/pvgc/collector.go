// Package pvgc implements garbage collection for orphaned PersistentVolumes.
//
// When PVCs are deleted, the underlying PVs should be automatically reclaimed
// by the storage provisioner. However, with CSI migration (kubernetes.io/aws-ebs
// → ebs.csi.aws.com), PVs often get stuck in Released state with unremoved
// finalizers. This collector periodically scans for Released PVs whose claims
// belonged to gardenio-managed namespaces, removes their finalizers, and deletes
// them after a configurable TTL.
package pvgc

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Stats holds the results of a single GC pass.
type Stats struct {
	TotalPVs   int
	ReleasedPVs int
	DeletedPVs int
	Errors     int
}

// Collector garbage-collects orphaned PersistentVolumes stuck in Released state.
type Collector struct {
	client     kubernetes.Interface
	namespaces map[string]bool
	ttl        time.Duration
	logger     *slog.Logger
}

// New creates a PV garbage collector.
// namespaces is the set of namespace prefixes that identify gardenio-managed PVs
// (matched against the PV's claimRef.namespace).
// ttl is the minimum time a PV must be in Released state before deletion.
func New(client kubernetes.Interface, namespaces []string, ttl time.Duration, logger *slog.Logger) *Collector {
	nsSet := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		nsSet[ns] = true
	}
	return &Collector{
		client:     client,
		namespaces: nsSet,
		ttl:        ttl,
		logger:     logger,
	}
}

// Collect runs a single garbage collection pass. It lists all PVs in Released
// state, filters to those claimed by gardenio-managed namespaces, and deletes
// those older than the configured TTL (removing stuck finalizers first).
//
// Security note (F7, ka-0000004901): the controller's RBAC for PVs is
// cluster-wide because PersistentVolume is a cluster-scoped resource. The
// isManaged check below is the load-bearing client-side filter that prevents
// the controller from patching or deleting PVs claimed by namespaces outside
// the configured allow-list. The chart additionally ships a
// ValidatingAdmissionPolicy
// (helm/gardenio/templates/controller/pv-gc-admission-policy.yaml) that
// enforces the same constraint at admission time as defense-in-depth.
func (c *Collector) Collect(ctx context.Context) Stats {
	var stats Stats

	pvList, err := c.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error("failed to list PVs", "error", err)
		stats.Errors++
		return stats
	}
	stats.TotalPVs = len(pvList.Items)

	for _, pv := range pvList.Items {
		if pv.Status.Phase != corev1.VolumeReleased {
			continue
		}
		stats.ReleasedPVs++

		// Only GC PVs whose claim belonged to a gardenio-managed namespace.
		// This is a security boundary, not just a correctness filter — see
		// the package comment and F7 in the namespace-isolation audit.
		if !c.isManaged(pv) {
			continue
		}

		age := releasedAge(pv, time.Now())
		if age < c.ttl {
			c.logger.Debug("released PV below TTL, skipping",
				"pv", pv.Name, "age", age.Round(time.Second), "ttl", c.ttl)
			continue
		}

		volumeID := volumeHandle(pv)
		claimName := ""
		if pv.Spec.ClaimRef != nil {
			claimName = pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
		}

		// Remove stuck finalizers before deletion.
		if len(pv.Finalizers) > 0 {
			if err := c.clearFinalizers(ctx, pv.Name); err != nil {
				c.logger.Error("failed to clear finalizers on PV",
					"pv", pv.Name, "error", err)
				stats.Errors++
				continue
			}
		}

		c.logger.Info("deleting released PV",
			"pv", pv.Name, "age", age.Round(time.Second),
			"volume_id", volumeID, "claim", claimName)
		if err := c.client.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{}); err != nil {
			c.logger.Error("failed to delete PV", "pv", pv.Name, "error", err)
			stats.Errors++
			continue
		}
		stats.DeletedPVs++
	}

	if stats.DeletedPVs > 0 || stats.ReleasedPVs > 0 {
		c.logger.Info("PV garbage collection complete",
			"total", stats.TotalPVs,
			"released", stats.ReleasedPVs,
			"deleted", stats.DeletedPVs,
			"errors", stats.Errors)
	}

	return stats
}

// isManaged returns true if the PV's claimRef namespace matches one of the
// managed namespaces.
func (c *Collector) isManaged(pv corev1.PersistentVolume) bool {
	if pv.Spec.ClaimRef == nil {
		return false
	}
	return c.namespaces[pv.Spec.ClaimRef.Namespace]
}

// clearFinalizers patches a PV to remove all finalizers.
func (c *Collector) clearFinalizers(ctx context.Context, name string) error {
	patch := []byte(`{"metadata":{"finalizers":null}}`)
	_, err := c.client.CoreV1().PersistentVolumes().Patch(
		ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// releasedAge returns how long a PV has been in Released state.
// It uses lastPhaseTransitionTime (K8s 1.28+) if available, falling back to
// creationTimestamp.
func releasedAge(pv corev1.PersistentVolume, now time.Time) time.Duration {
	if pv.Status.LastPhaseTransitionTime != nil && !pv.Status.LastPhaseTransitionTime.IsZero() {
		return now.Sub(pv.Status.LastPhaseTransitionTime.Time)
	}
	return now.Sub(pv.CreationTimestamp.Time)
}

// volumeHandle extracts the cloud volume identifier from a PV spec.
func volumeHandle(pv corev1.PersistentVolume) string {
	if pv.Spec.CSI != nil && pv.Spec.CSI.VolumeHandle != "" {
		return pv.Spec.CSI.VolumeHandle
	}
	if pv.Spec.AWSElasticBlockStore != nil {
		return pv.Spec.AWSElasticBlockStore.VolumeID
	}
	return ""
}
