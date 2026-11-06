package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/podmanager"
)

// beadStatusFetcher is the subset of beadsapi.Client used by the stuck-handler
// closed-bead guard. Defining a small interface here keeps the helper testable
// without spinning up a real HTTP daemon.
type beadStatusFetcher interface {
	GetBeadStatus(ctx context.Context, beadID string) (string, error)
}

// beadIsClosed reports whether the bead with id beadID is in status "closed".
// Errors (transient daemon failures, missing beads) are treated as not-closed
// so the caller proceeds with its default behavior — matching the
// statusreporter precedent (ka-0000005253) where the server-side filter is the
// safety net for races.
//
// Used by the AgentStuck handler to avoid recreating pods for already-closed
// agents whose cached project/role entries may have been torn down, leaving
// buildAgentPodSpec without an image to set on the new pod (ka-0000005268).
func beadIsClosed(ctx context.Context, d beadStatusFetcher, beadID string) bool {
	if d == nil || beadID == "" {
		return false
	}
	status, err := d.GetBeadStatus(ctx, beadID)
	if err != nil {
		return false
	}
	return status == "closed"
}

// stuckInPlaceRestartTimeout bounds the HTTP call to coop's session-restart
// endpoint. The endpoint returns 202 immediately (it only enqueues the switch),
// so this is generous — a slow/unreachable coop must fail fast so the caller can
// fall back to the destructive delete+recreate before the reporter re-flags the
// pod on its next sweep.
const stuckInPlaceRestartTimeout = 5 * time.Second

// stuckInPlaceRestartReason is stamped on the agent bead when the controller
// clears a stuck detection non-destructively, so the death/restart report can
// name the real cause (and distinguish it from a workspace-wiping recreate).
const stuckInPlaceRestartReason = "controller restarted the wedged coop session in place (workspace preserved) after stuck detection"

// stuckPodGetter is the subset of podmanager.Manager the in-place stuck restart
// needs — just fetching the pod to read its IP. Narrowed to an interface so the
// remedy decision is testable without a real K8s client.
type stuckPodGetter interface {
	GetAgentPod(ctx context.Context, name, namespace string) (*corev1.Pod, error)
}

// beadFieldUpdater stamps the restart reason on the agent bead. Narrowed for
// testability (the real implementation is *beadsapi.Client).
type beadFieldUpdater interface {
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// restartCoopSessionInPlace asks the wedged agent's coop to restart its child
// process IN PLACE — force-SIGHUP + respawn with `--resume` — via
// POST /api/v1/session/restart. This is the NON-DESTRUCTIVE stuck remedy
// (ka-0000001648): the pod, its emptyDir workspace, and the conversation all
// survive, so a false-positive stuck detection costs a conversation resume
// rather than a wiped workspace.
//
// It is viable precisely because the stuck heuristic only fires while coop's
// HTTP server is reachable (checkStagnation polls it every sweep) — a stuck
// agent is a wedged *child*, not a dead coop. When coop itself is unreachable
// this returns an error and the caller falls back to delete+recreate, which is
// the only remedy that can replace a pod whose coop is gone.
//
// podIP must be the pod's routable IP; an empty IP yields an error (there is
// nothing to POST to). Any non-2xx response or transport error is returned so
// the caller can decide whether to fall back.
func restartCoopSessionInPlace(ctx context.Context, client *http.Client, podIP string) error {
	if podIP == "" {
		return fmt.Errorf("no pod IP to reach coop")
	}
	if client == nil {
		client = &http.Client{Timeout: stuckInPlaceRestartTimeout}
	}
	url := fmt.Sprintf("http://%s:%d/api/v1/session/restart", podIP, podmanager.CoopDefaultPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("coop session restart returned status %d", resp.StatusCode)
	}
	return nil
}

// tryStuckRestartInPlace attempts the non-destructive stuck remedy for a wedged
// pod. It fetches the pod and, when it has a routable IP, asks coop to restart
// the agent child in place (restartCoopSessionInPlace), preserving the pod and
// its emptyDir workspace.
//
// Returns true when the in-place restart succeeded — the caller must NOT delete
// the pod. Returns false to fall through to the destructive delete+recreate:
// the pod could not be fetched, has no IP yet, or coop is unreachable — the
// only cases where a pod genuinely needs recreation. On success it best-effort
// stamps stuckInPlaceRestartReason on the agent bead so the restart is
// attributable and never mistaken for an AWS/node kill.
func tryStuckRestartInPlace(ctx context.Context, logger *slog.Logger, pods stuckPodGetter, beads beadFieldUpdater, client *http.Client, podName, ns, agentName, agentBeadID string) bool {
	pod, err := pods.GetAgentPod(ctx, podName, ns)
	if err != nil {
		logger.Warn("stuck in-place restart: could not fetch pod, falling back to delete+recreate",
			"pod", podName, "error", err)
		return false
	}
	if pod == nil || pod.Status.PodIP == "" {
		logger.Warn("stuck in-place restart: pod has no IP yet, falling back to delete+recreate",
			"pod", podName)
		return false
	}
	if rerr := restartCoopSessionInPlace(ctx, client, pod.Status.PodIP); rerr != nil {
		logger.Warn("stuck in-place restart failed, falling back to delete+recreate",
			"pod", podName, "pod_ip", pod.Status.PodIP, "error", rerr)
		return false
	}
	logger.Info("restarted stuck agent in place — pod + emptyDir workspace preserved",
		"agent", agentName, "bead", agentBeadID, "pod", podName, "pod_ip", pod.Status.PodIP)
	if agentBeadID != "" && beads != nil {
		if uerr := beads.UpdateBeadFields(ctx, agentBeadID, map[string]string{
			controllerRestartReasonField: stuckInPlaceRestartReason,
		}); uerr != nil {
			logger.Warn("could not stamp in-place stuck-restart reason on agent bead",
				"bead", agentBeadID, "error", uerr)
		}
	}
	return true
}
