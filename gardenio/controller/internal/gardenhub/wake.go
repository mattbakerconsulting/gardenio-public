package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Wake and restart from the farm (gd-3xrmzPMA1Y, Homestead Phase 3 step 3c): a sleeping or down gardener's card and its
// Commons thread header (web/wake.js) post here. There was no restart action on /agents to reach (cmd/beads-viewer
// agent_restart.go only words the card's "Restart:" line), so this is the smallest route that does it, and it sends the
// revive `gb wake` and the wake-scanner send: one PATCH of beadsapi.ReviveAgentFields, which clears agent_state so the
// reconciler recreates the pod under the same name and the agent resumes its conversation and its task. A closed bead
// is reopened in the same PATCH, and only when it was parked to be woken (successor=same), as `gb wake` requires.
//
// beads-viewer registers it behind its operator gate and classes it admin-only (route_classes.go); CanWake checks again
// here, so a page that may not restart never gets a 200. The write goes on the request context, which carries the
// operator's own bearer and X-Operated-By, so the daemon audits it as the operator, as every other viewer write is; the
// audit comment on the agent's bead names them too.
//
// A gardener whose pod is still running is not revived: clearing its state would not stop it. When it is stale (alive in
// name, no progress for GardenerStaleAfter) it is restarted in place instead (gd-LUmZlr6CKM). The viewer can reach
// neither its coop nor the controller (beads-viewer's egress NetworkPolicy allows the beads daemon only), so the route
// writes a restart request on the agent bead (beadsapi.FieldRestartRequestedAt) and waits a few seconds for the
// controller's answer there (cmd/controller farm_restart.go): the controller asks coop to restart the session, and gb init
// relaunches it in the same pod with its conversation and its task. A gardener that has made progress within the hour
// is refused; ask it in its thread.

// AgentReviver is the slice of the daemon client a revive writes with.
type AgentReviver interface {
	UpdateBead(ctx context.Context, id string, req beadsapi.UpdateBeadRequest) error
	AddComment(ctx context.Context, id, author, text string) error
}

const wakeTimeout = 10 * time.Second

// restartPoll and restartWait are how often and how long a restart in place reads the agent bead for the controller's
// answer before saying it was asked but not yet answered. Variables so the tests need not wait.
var restartPoll, restartWait = 250 * time.Millisecond, 8 * time.Second

// restartPendingFor is how long an unanswered restart request stands, the controller's farmRestartMaxAge: within it a
// second Restart is refused as already on its way, after it the controller will not act on it and a new one may be asked.
const restartPendingFor = 2 * time.Minute

// wakeResult is the JSON body of every answer from POST /garden/gardener/{id}/wake. Error is the sentence the card shows.
// InPlace marks a restart in place of a running session; Pending one the controller had not yet answered (202).
type wakeResult struct {
	OK      bool   `json:"ok"`
	ID      string `json:"id,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Respawn int    `json:"respawn,omitempty"`
	Max     int    `json:"max,omitempty"`
	InPlace bool   `json:"in_place,omitempty"`
	Pending bool   `json:"pending,omitempty"`
	Error   string `json:"error,omitempty"`
}

// wakeRunning are the agent states of a pod that is up: a revive would clear the state and leave the pod as it is.
var wakeRunning = map[string]bool{"working": true, "spawning": true, "yielding": true}

// wakeLeaving are the states of an agent on its way out; it is woken once it has stopped, not while it stops.
var wakeLeaving = map[string]bool{"stopping": true, "stop_requested": true}

// Wake serves POST /garden/gardener/{id}/wake. It answers 200 once the revive is written, or once the controller has
// restarted a stale gardener's session in place, and refuses, without writing, a bead that is not an agent, an agent that
// is running and has made progress within the hour, one that is stopping or already being restarted, a closed agent that
// was not parked to be woken, one whose goal was recorded as unachievable, and one that has spent its respawn budget.
func (h *Handler) Wake(w http.ResponseWriter, r *http.Request) {
	if crossOrigin(r) {
		writeWake(w, http.StatusForbidden, wakeResult{Error: "Restart from the garden page itself."})
		return
	}
	if h.Beads == nil || h.Agents == nil {
		writeWake(w, http.StatusServiceUnavailable, wakeResult{Error: "Restarting is not enabled here."})
		return
	}
	me := ""
	if h.Viewer != nil {
		me = h.Viewer(r)
	}
	ctx := r.Context()
	if me == "" {
		me = beadsapi.OperatorFromContext(ctx)
	}
	if me == "" {
		writeWake(w, http.StatusUnauthorized, wakeResult{Error: "Your session ended. Reload the page to sign in again."})
		return
	}
	if !h.can(r, h.CanWake) {
		writeWake(w, http.StatusForbidden, wakeResult{Error: "Only a gardenio admin may restart gardeners."})
		return
	}
	if beadsapi.OperatorFromContext(ctx) == "" {
		ctx = beadsapi.WithOperator(ctx, me)
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		writeWake(w, http.StatusBadRequest, wakeResult{Error: "Say which gardener."})
		return
	}
	ctx, cancel := context.WithTimeout(ctx, wakeTimeout)
	defer cancel()

	res := wakeResult{ID: id}
	b, err := readOne(ctx, h.Beads, id, "There is no gardener "+id+", or you cannot see it.")
	if err != nil {
		ae := &adoptError{code: http.StatusBadGateway, msg: "The beads daemon did not answer. Try again."}
		errors.As(err, &ae)
		res.Error = ae.msg
		writeWake(w, ae.code, res)
		return
	}
	res.Agent = agentName(b)
	count, max, code, msg, inPlace := wakeCheck(b, time.Now())
	if msg != "" {
		res.Error = msg
		writeWake(w, code, res)
		return
	}
	if inPlace {
		h.restartInPlace(ctx, w, me, res)
		return
	}
	req := beadsapi.UpdateBeadRequest{Fields: beadsapi.ReviveAgentFields(count+1, beadsapi.WakeReasonManual)}
	if b.Status == "closed" {
		open := "open"
		req.Status = &open
	}
	if err := h.Agents.UpdateBead(ctx, id, req); err != nil {
		writeWakeFailed(w, err, res)
		return
	}
	// Best-effort, as in `gb wake`: the revive is written, and a lost comment does not undo it.
	_ = h.Agents.AddComment(ctx, id, me, fmt.Sprintf(
		"Restarted from the garden by %s: `%s` revived (respawn %d/%d), so the reconciler recreates its pod under the same name "+
			"and it resumes its conversation and its task. (gd-3xrmzPMA1Y)", me, res.Agent, count+1, max))
	if h.Farm != nil {
		h.Farm.Invalidate() // the gardener shows as starting on the next refresh, not the next timer
	}
	res.OK, res.Respawn, res.Max = true, count+1, max
	writeWake(w, http.StatusOK, res)
}

// restartInPlace asks the controller to restart the running session of the stale gardener res names, on the agent bead,
// and waits restartWait for its answer there. 200 when coop took the restart, 502 with the controller's reason when it
// did not, 202 when no answer came in time (the request stands, and the farm shows the gardener once it is back).
func (h *Handler) restartInPlace(ctx context.Context, w http.ResponseWriter, me string, res wakeResult) {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	req := beadsapi.UpdateBeadRequest{Fields: map[string]any{
		beadsapi.FieldRestartRequestedAt: stamp,
		beadsapi.FieldRestartRequestedBy: me,
	}}
	if err := h.Agents.UpdateBead(ctx, res.ID, req); err != nil {
		writeWakeFailed(w, err, res)
		return
	}
	res.InPlace = true
	result, answered := h.awaitRestart(ctx, res.ID, stamp)
	// Best-effort, as for the revive: the request is written, and a lost comment does not undo it.
	switch {
	case !answered:
		_ = h.Agents.AddComment(ctx, res.ID, me, fmt.Sprintf(
			"Restart asked from the garden by %s: `%s` had made no progress for an hour, so the controller was asked to "+
				"restart its session in place; it had not answered within %s. (gd-LUmZlr6CKM)", me, res.Agent, restartWait))
		res.OK, res.Pending = true, true
		writeWake(w, http.StatusAccepted, res)
	case result == beadsapi.RestartResultOK:
		_ = h.Agents.AddComment(ctx, res.ID, me, fmt.Sprintf(
			"Restarted in place from the garden by %s: `%s` had made no progress for an hour, so its session was interrupted "+
				"and started again in the same pod, resuming its conversation and its task. (gd-LUmZlr6CKM)", me, res.Agent))
		if h.Farm != nil {
			h.Farm.Invalidate()
		}
		res.OK = true
		writeWake(w, http.StatusOK, res)
	default:
		_ = h.Agents.AddComment(ctx, res.ID, me, fmt.Sprintf(
			"Restart from the garden by %s did not go through: %s (gd-LUmZlr6CKM)", me, result))
		res.Error = res.Agent + " was not restarted. " + result
		writeWake(w, http.StatusBadGateway, res)
	}
}

// awaitRestart reads agent bead id every restartPoll until the controller has answered request stamp, and returns its
// result; false when restartWait (or the request) ends first.
func (h *Handler) awaitRestart(ctx context.Context, id, stamp string) (string, bool) {
	deadline := time.NewTimer(restartWait)
	defer deadline.Stop()
	tick := time.NewTicker(restartPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-deadline.C:
			return "", false
		case <-tick.C:
		}
		b, err := h.Beads.GetBead(ctx, id)
		if err != nil || b == nil || b.Fields[beadsapi.FieldRestartAnswered] != stamp {
			continue
		}
		if r := strings.TrimSpace(b.Fields[beadsapi.FieldRestartResult]); r != "" {
			return r, true
		}
		return "The controller answered without saying how it went.", true
	}
}

// writeWakeFailed answers a write the daemon did not take: 403 when it refused the operator, else 502.
func writeWakeFailed(w http.ResponseWriter, err error, res wakeResult) {
	var ae *beadsapi.APIError
	if errors.As(err, &ae) && ae.StatusCode == http.StatusForbidden {
		res.Error = "The beads daemon did not let you restart " + res.Agent + "."
		writeWake(w, http.StatusForbidden, res)
		return
	}
	res.Error = "The beads daemon did not take the restart. Try again."
	writeWake(w, http.StatusBadGateway, res)
}

// wakeCheck says whether agent bead b may be revived at now: its respawn count and cap, or the status and sentence that
// refuse it, or inPlace for a running gardener that is stale and is restarted in place instead. The gates are `gb wake`'s
// and the wake-scanner's (successor=same for a closed bead, goal_unachievable, the budget) plus the one an open bead
// needs: no pod still up, unless it is stale and no restart of it is already on its way.
func wakeCheck(b *beadsapi.BeadDetail, now time.Time) (count, max, code int, msg string, inPlace bool) {
	name := agentName(b)
	if b.Type != "agent" {
		return 0, 0, http.StatusBadRequest, b.ID + " is not a gardener.", false
	}
	f := b.Fields
	state := strings.TrimSpace(f[beadsapi.FieldAgentState])
	switch {
	case b.Status == "closed" && f[beadsapi.FieldSuccessor] != beadsapi.SuccessorSame:
		return 0, 0, http.StatusConflict, name + " has stopped for good; it was not parked to be woken.", false
	case b.Status != "closed" && wakeRunning[state] && progressIdle(b, now) < GardenerStaleAfter:
		return 0, 0, http.StatusConflict, name + " is still running (" + state + ") and has made progress within the hour. Ask it in its thread.", false
	case b.Status != "closed" && wakeRunning[state] && restartPending(f, now):
		return 0, 0, http.StatusConflict, name + " is already being restarted. It shows on the next update.", false
	case b.Status != "closed" && wakeRunning[state]:
		return 0, 0, 0, "", true
	case b.Status != "closed" && wakeLeaving[state]:
		return 0, 0, http.StatusConflict, name + " is stopping. Wake it once it has stopped.", false
	case strings.EqualFold(strings.TrimSpace(f[beadsapi.FieldGoalUnachievable]), "true"):
		return 0, 0, http.StatusConflict, name + " stopped because its goal cannot be reached, so it is not restarted.", false
	}
	count = wakeInt(f[beadsapi.FieldRespawnCount], 0)
	max = wakeInt(f[beadsapi.FieldMaxRespawns], beadsapi.DefaultMaxRespawns)
	if count >= max {
		return count, max, http.StatusConflict, fmt.Sprintf("%s has used its %d restarts. `gb wake --force %s` restarts it past that.", name, max, name), false
	}
	return count, max, 0, "", false
}

// restartPending reports whether the agent fields f carry a restart request the controller has not answered and still may.
func restartPending(f map[string]string, now time.Time) bool {
	req := strings.TrimSpace(f[beadsapi.FieldRestartRequestedAt])
	if req == "" || req == strings.TrimSpace(f[beadsapi.FieldRestartAnswered]) {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, req)
	return err == nil && now.Sub(at) <= restartPendingFor
}

// agentName is the agent's name as the farm gives it (fields.agent, else the title), else the bead id.
func agentName(b *beadsapi.BeadDetail) string {
	if n := agentFromBead(b).name; n != "" {
		return n
	}
	return b.ID
}

func writeWake(w http.ResponseWriter, code int, res wakeResult) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(res)
}

// wakeInt parses a string-coded count, def when absent or malformed (as `gb wake` and the wake-scanner read it).
func wakeInt(v string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}
