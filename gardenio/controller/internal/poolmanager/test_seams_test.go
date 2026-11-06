package poolmanager

import (
	"context"
	"gardenio/controller/internal/podmanager"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClaimWarmPlaceholder atomically claims a Ready warm placeholder co-resident
// with the given node and returns its pod name, for the caller (C2 wake path) to
// pass as podmanager.ThawRequest.PlaceholderPod. The claim flips the pod's
// warm-placeholder label from "ready" to "claimed" so a concurrent claim cannot
// hand out the same pod, and the next Reconcile refills the node back to MinSize.
//
// Returns ErrNoWarmPlaceholder when the node has no Ready placeholder, so the
// wake path falls back to the create-then-restore thaw.
func (p *PlaceholderPool) ClaimWarmPlaceholder(ctx context.Context, node, topology string) (string, error) {
	if p == nil {
		return "", ErrNoWarmPlaceholder
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	byBucket, err := p.listPlaceholders(ctx)
	if err != nil {
		return "", err
	}
	ready := p.readyCandidates(byBucket, node, topology)
	if len(ready) == 0 {
		return "", ErrNoWarmPlaceholder
	}

	// Try each Ready candidate; the label flip is the atomic gate. A racing
	// claimer that beat us to the flip leaves the pod no longer labeled "ready",
	// so the patch is a no-op race we detect by re-reading — move to the next.
	for _, name := range ready {
		ok, err := p.markClaimed(ctx, name)
		if err != nil {
			p.logger.Warn("warm placeholder claim flip failed, trying next",
				"pod", name, "node", node, "error", err)
			continue
		}
		if ok {
			p.logger.Info("claimed warm placeholder", "pod", name, "node", node, "topology", topology)
			return name, nil
		}
	}
	return "", ErrNoWarmPlaceholder
}

// markClaimed flips a placeholder's warm-placeholder label to "claimed" using an
// optimistic update guarded by resourceVersion: it re-reads the pod, verifies it
// is still labeled "ready", and writes back. A conflict or a no-longer-ready pod
// returns ok=false so the caller tries another candidate. This makes concurrent
// claims of the same node safe without a separate lock.
func (p *PlaceholderPool) markClaimed(ctx context.Context, name string) (bool, error) {
	pod, err := p.client.CoreV1().Pods(p.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pod.Labels[podmanager.LabelWarmPlaceholder] != podmanager.WarmPlaceholderReady {
		return false, nil // already claimed by a racing caller
	}
	pod.Labels[podmanager.LabelWarmPlaceholder] = podmanager.WarmPlaceholderClaimed
	_, err = p.client.CoreV1().Pods(p.namespace).Update(ctx, pod, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return false, nil // lost the race; caller tries another
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
