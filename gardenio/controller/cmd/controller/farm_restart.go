package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Restart a stale gardener from the farm (gd-LUmZlr6CKM). The garden's Restart on a gardener that is alive in name but
// has made no progress for an hour cannot reach the pod itself: beads-viewer's egress NetworkPolicy allows the beads
// daemon only, and agent ingress admits the controller, coopmux and vmagent. So the garden writes the request on the
// agent bead (beadsapi.FieldRestartRequestedAt / FieldRestartRequestedBy) and the controller, which sees every agent bead
// write on its event stream as an AgentUpdate, does the restart and answers on the same bead.
//
// The restart is the stuck remedy's non-destructive one (restartCoopSessionInPlace): coop's POST /api/v1/session/restart
// makes coop exit with the credential-switch code, and gb init's loop relaunches it in the same pod with --resume and
// re-injects the work prompt. The pod, its workspace and the conversation all stay, and no second pod can race the
// first. A bare coop shutdown would not do: without stop_requested gb init restarts coop in the same pod anyway, and
// with it the agent bead is closed and the identity stops for good. When coop does not answer, nothing is deleted; the
// answer says so and the operator decides.

// farmRestartMaxAge bounds how old a request may be when the controller first sees it. A request is answered off the
// AgentUpdate its own write raises; one that missed it (the controller was restarting) is not acted on by some later,
// unrelated write to the bead an hour on, when the operator has long stopped waiting.
const farmRestartMaxAge = 2 * time.Minute

// farmRestartReason is stamped on the agent bead (controllerRestartReasonField) when the restart is taken, so the
// death/restart report names the operator's restart rather than guessing.
const farmRestartReason = "operator restarted the stale session in place from the garden (workspace preserved)"

// farmRestarts answers each restart request once. The event loop is serial, but events queued before the answer was
// written still carry the request unanswered in their snapshot of the bead's fields, so the last request taken is
// remembered per bead.
type farmRestarts struct {
	mu    sync.Mutex
	taken map[string]string // agent bead id -> FieldRestartRequestedAt last taken
}

var farmRestartsTaken = &farmRestarts{taken: map[string]string{}}

// farmRestartPending reports the request an AgentUpdate's fields carry that is still unanswered, or "".
func farmRestartPending(fields map[string]string) string {
	req := strings.TrimSpace(fields[beadsapi.FieldRestartRequestedAt])
	if req == "" || req == strings.TrimSpace(fields[beadsapi.FieldRestartAnswered]) {
		return ""
	}
	return req
}

// take claims request req on bead id, false when it was already taken.
func (f *farmRestarts) take(id, req string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taken[id] == req {
		return false
	}
	f.taken[id] = req
	return true
}

// handleFarmRestart answers the pending restart request req on agent bead beadID, whose pod is podName in ns: it asks
// coop to restart the session in place, then writes the answer. A request older than farmRestartMaxAge is answered
// without acting.
func handleFarmRestart(ctx context.Context, logger *slog.Logger, pods stuckPodGetter, beads beadFieldUpdater, client *http.Client,
	beadID, req, by, podName, ns, agentName string, now time.Time) {
	if !farmRestartsTaken.take(beadID, req) {
		return
	}
	result := farmRestartDo(ctx, pods, client, req, podName, ns, now)
	fields := map[string]string{beadsapi.FieldRestartAnswered: req, beadsapi.FieldRestartResult: result}
	if result == beadsapi.RestartResultOK {
		fields[controllerRestartReasonField] = farmRestartReason
		logger.Info("restarted a stale agent in place from the garden — pod + workspace preserved",
			"agent", agentName, "bead", beadID, "pod", podName, "requested_by", by)
	} else {
		logger.Warn("did not restart a stale agent from the garden",
			"agent", agentName, "bead", beadID, "pod", podName, "requested_by", by, "result", result)
	}
	if beads == nil {
		return
	}
	if err := beads.UpdateBeadFields(ctx, beadID, fields); err != nil {
		logger.Warn("could not answer the garden's restart request on the agent bead",
			"bead", beadID, "error", err)
	}
}

// farmRestartDo does the restart and says how it went: beadsapi.RestartResultOK, or the sentence the garden shows.
func farmRestartDo(ctx context.Context, pods stuckPodGetter, client *http.Client, req, podName, ns string, now time.Time) string {
	at, err := time.Parse(time.RFC3339Nano, req)
	if err != nil {
		return "The request carried no time the controller could read."
	}
	if now.Sub(at) > farmRestartMaxAge {
		return "The controller saw the request too late to act on it. Ask again."
	}
	pod, err := pods.GetAgentPod(ctx, podName, ns)
	if err != nil || pod == nil {
		return "Its pod is gone, so there is no session to restart. Wake it once it shows as down."
	}
	if pod.Status.PodIP == "" {
		return "Its pod has no address yet, so its session could not be reached."
	}
	if err := restartCoopSessionInPlace(ctx, client, pod.Status.PodIP); err != nil {
		return "Its session did not answer the restart (" + err.Error() + ")."
	}
	return beadsapi.RestartResultOK
}
