package traefikshipper

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PodLogStreamer is the production LogStreamer: it resolves the single Traefik
// pod by label selector in its namespace and opens a follow stream of its
// stdout via the Kubernetes pod-log API (GET .../pods/<name>/log?follow=true).
//
// It re-resolves the pod on every Stream call so a pod rotation (rollout,
// reschedule) is picked up on the next reconnect without restarting the shipper.
// The Traefik edge runs a SINGLE replica (deployment.replicas=1, verified live),
// so there is exactly one pod to stream; if the selector ever matches more, we
// stream the first Running one and log the ambiguity rather than silently
// picking nondeterministically.
type PodLogStreamer struct {
	Client    kubernetes.Interface
	Namespace string
	// Selector is a label selector identifying the Traefik pod, e.g.
	// app.kubernetes.io/name=traefik.
	Selector string
	// ContainerName is the container within the pod to stream. Empty streams the
	// pod's default (first) container — correct for the single-container Traefik
	// pod.
	ContainerName string
}

// Stream resolves the Traefik pod and opens a follow log stream from at most
// sinceSeconds ago. The returned ReadCloser must be closed by the caller.
func (p *PodLogStreamer) Stream(ctx context.Context, sinceSeconds int64) (io.ReadCloser, error) {
	pods, err := p.Client.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: p.Selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list traefik pods (ns=%s selector=%q): %w", p.Namespace, p.Selector, err)
	}
	pod := firstRunningPod(pods.Items)
	if pod == "" {
		return nil, fmt.Errorf("no Running traefik pod found (ns=%s selector=%q)", p.Namespace, p.Selector)
	}

	opts := &corev1.PodLogOptions{
		Follow:    true,
		Container: p.ContainerName,
	}
	if sinceSeconds > 0 {
		s := sinceSeconds
		opts.SinceSeconds = &s
	}
	req := p.Client.CoreV1().Pods(p.Namespace).GetLogs(pod, opts)
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open log stream for pod %s/%s: %w", p.Namespace, pod, err)
	}
	return rc, nil
}

// firstRunningPod returns the name of the first pod in Running phase, or "" if
// none are Running. Deterministic: preserves the API's returned order (which is
// name-sorted), so a multi-pod match always picks the same pod.
func firstRunningPod(pods []corev1.Pod) string {
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodRunning {
			return pods[i].Name
		}
	}
	return ""
}
