package statusreporter

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
)

// sessionDaemon is the optional daemon access stampOOMKilledSessions needs.
// *beadsapi.Client satisfies it; a BeadUpdater without it stamps nothing.
type sessionDaemon interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

var _ sessionDaemon = (*beadsapi.Client)(nil)

// agentOOMTermination returns the agent container's OOMKilled termination,
// current or last, or nil. Sidecars are left out: their OOM does not end the
// agent's session.
func agentOOMTermination(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != podmanager.ContainerName {
			continue
		}
		for _, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
			if t != nil && t.Reason == "OOMKilled" && !t.FinishedAt.IsZero() {
				return t
			}
		}
	}
	return nil
}

// stampOOMKilledSessions writes the exit onto the session beads an OOM kill
// of the agent container ended (gd-DgSORP1h8P).
//
// The kill takes gb init with it, so nothing in the pod closes the session:
// the restarted container registers a resumed session beside it, and the
// session reaper later stamps the dead one `retired` with no exit code, which
// hid every OOM from the session sweep. The kubelet's OOMKilled termination is
// only on the pod, so the reporter writes it: exit_code, restart_cause oomkill,
// status crashed and ended_at the kill, then closes the bead. The reaper keeps
// an exit already written.
//
// A session is the killed one when it is still active on this agent bead and
// started inside the killed container's life. Each kill is stamped once; a
// failed lookup is retried on the next sync.
func (r *HTTPReporter) stampOOMKilledSessions(ctx context.Context, pod *corev1.Pod, beadID string) {
	daemon, ok := r.daemon.(sessionDaemon)
	if !ok || beadID == "" {
		return
	}
	t := agentOOMTermination(pod)
	if t == nil {
		return
	}
	kill := t.FinishedAt.UTC().Format(time.RFC3339)
	r.phaseMu.Lock()
	done := r.oomStamped[beadID] == kill
	r.phaseMu.Unlock()
	if done {
		return
	}

	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"session"},
		Statuses: beadsapi.ActiveStatuses(),
		Fields:   map[string]string{"agent_bead_id": beadID},
		Limit:    50,
	}.OnlyFields("agent_bead_id", "status", "started_at", "ended_at"))
	if err != nil {
		r.logger.Warn("SyncAll: listing sessions of OOM-killed agent failed", "bead", beadID, "pod", pod.Name, "error", err)
		return
	}
	stamped, failed := 0, 0
	for _, s := range res.Beads {
		if !oomKilledSession(s, beadID, t) {
			continue
		}
		fields := map[string]any{
			"exit_code":     int(t.ExitCode),
			"restart_cause": beadsapi.RestartCauseOOMKilled,
			"status":        beadsapi.SessionStatusForExit(int(t.ExitCode), false),
			"ended_at":      kill,
		}
		if err := r.daemon.UpdateBeadFieldsTyped(ctx, s.ID, fields); err != nil {
			failed++
			r.logger.Warn("SyncAll: stamping OOM-killed session failed", "session", s.ID, "bead", beadID, "error", err)
			continue
		}
		if err := daemon.CloseBead(ctx, s.ID, nil); err != nil {
			// The stamp landed; the reaper closes a session with ended_at set.
			r.logger.Warn("SyncAll: closing OOM-killed session failed", "session", s.ID, "bead", beadID, "error", err)
		}
		stamped++
	}
	if failed > 0 {
		return
	}
	r.phaseMu.Lock()
	r.oomStamped[beadID] = kill
	r.phaseMu.Unlock()
	if stamped > 0 {
		r.logger.Info("SyncAll: stamped OOM-killed sessions", "bead", beadID, "pod", pod.Name, "sessions", stamped, "killed_at", kill)
	}
}

// oomKilledSession reports whether an open session bead of beadID was running
// in the container that termination t ended: still active, never stamped
// ended, and started inside that container's life. The resumed session the
// restarted container registered starts after the kill and is left alone.
func oomKilledSession(s *beadsapi.BeadDetail, beadID string, t *corev1.ContainerStateTerminated) bool {
	if s.Type != "session" || s.Status == "closed" || s.Fields["agent_bead_id"] != beadID || s.Fields["ended_at"] != "" {
		return false
	}
	if st := s.Fields["status"]; st != "" && st != "active" {
		return false
	}
	started, err := time.Parse(time.RFC3339, s.Fields["started_at"])
	if err != nil || started.After(t.FinishedAt.Time) {
		return false
	}
	// started_at has second precision; the container start does not round.
	return t.StartedAt.IsZero() || !started.Before(t.StartedAt.Truncate(time.Second))
}
