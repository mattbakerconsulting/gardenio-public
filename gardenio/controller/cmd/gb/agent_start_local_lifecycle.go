package main

// The agent bead of a laptop session (gb agent start --local) across restarts
// (gd-3s1xZJyeNI). The laptop runner restarts `gb agent start` after each
// exit, and every start registers a fresh agent bead:
//
//   - supersedeStaleLaptopAgentBeads closes, at start, the beads of earlier
//     runs of the same name that are dead, with a comment saying why. A bead
//     whose close never landed (killed process, a 401 at exit) used to stay
//     open, and the viewer showed the dead run as a working laptop agent.
//   - closeLocalAgentBeadAtExit retries a failed exit close with a fresh
//     token, and when that fails too leaves a dead registry entry, so the
//     next start closes the bead.
//   - localAgentStartAlive lets the stop gate leave the bead to the gb agent
//     start that owns it. On a laptop the Claude Stop hook fires at each turn
//     end while gb and coop keep running, so its escape hatch closed the bead
//     under a live agent.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/localreg"
)

// laptopAgentStaleAfter is how long the actor's presence must be quiet before
// an earlier bead of the same name counts as dead. agentHeartbeatLoop beats
// every 5 minutes and the daemon's presence reaper uses 15.
const laptopAgentStaleAfter = 15 * time.Minute

// laptopAgentReaper is the daemon surface supersedeStaleLaptopAgentBeads uses.
type laptopAgentReaper interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	GetAgentRoster(ctx context.Context, staleThresholdSecs int) (*beadsapi.AgentRosterResponse, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// supersedeStaleLaptopAgentBeads closes the open laptop agent beads named
// agentName, run by one of owners, other than newBeadID, that are dead. It
// returns the IDs it closed. Best-effort: a failed lookup closes nothing.
//
// A bead is dead when this host's local registry holds it with a dead pid
// (its gb agent start was killed), or when it is not the newest candidate
// (one name has one live run), or when the actor's presence heartbeat has
// been quiet for laptopAgentStaleAfter. A bead the registry holds with a live
// pid is never closed.
func supersedeStaleLaptopAgentBeads(ctx context.Context, d laptopAgentReaper, agentName, newBeadID string, owners []string) []string {
	res, err := d.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Fields:   map[string]string{"agent": agentName, "execution_target": "laptop"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not list earlier agent beads for %s: %v\n", agentName, err)
		return nil
	}
	var candidates []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil || b.ID == newBeadID || b.Fields["agent"] != agentName ||
			b.Fields["execution_target"] != "laptop" || !sameLaptopOwner(b, owners) {
			continue
		}
		candidates = append(candidates, b)
	}
	if len(candidates) == 0 {
		return nil
	}
	// Newest first: only the newest can own the actor's presence.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].CreatedAt.After(candidates[j].CreatedAt) })

	reg, regErr := localreg.Read(agentName)
	var heartbeat *beadsapi.RosterActor
	rosterRead := false

	var closed []string
	for i, b := range candidates {
		var why string
		switch {
		case regErr == nil && reg.BeadID == b.ID:
			if localreg.Alive(reg) {
				continue
			}
			why = "its gb agent start on this host exited without closing it"
			if reg.PID > 0 {
				why = fmt.Sprintf("its gb agent start on this host is gone (local registry pid %d is not running)", reg.PID)
			}
		case i > 0:
			why = fmt.Sprintf("a newer run of %s (%s) has registered since", agentName, candidates[0].ID)
		default:
			if !rosterRead {
				rosterRead = true
				roster, err := d.GetAgentRoster(ctx, 0)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not read the agent roster; leaving %s open: %v\n", b.ID, err)
					continue
				}
				for k := range roster.Actors {
					if roster.Actors[k].Actor == agentName {
						heartbeat = &roster.Actors[k]
						break
					}
				}
			}
			stale, desc := laptopHeartbeatStale(heartbeat)
			if !stale {
				continue
			}
			why = desc
		}
		if supersedeLaptopAgentBead(ctx, d, b.ID, newBeadID, agentName, why) {
			closed = append(closed, b.ID)
		}
	}
	return closed
}

// laptopHeartbeatStale reports whether a roster presence entry shows no live
// run, and says what it saw.
func laptopHeartbeatStale(a *beadsapi.RosterActor) (bool, string) {
	switch {
	case a == nil:
		return true, "the daemon has no heartbeat for it"
	case a.Reaped:
		return true, "the daemon's presence reaper marked it dead"
	case time.Duration(a.IdleSecs*float64(time.Second)) >= laptopAgentStaleAfter:
		return true, fmt.Sprintf("its last heartbeat was %s ago", time.Duration(a.IdleSecs*float64(time.Second)).Truncate(time.Second))
	}
	return false, ""
}

// supersedeLaptopAgentBead comments on and closes one dead laptop agent bead.
func supersedeLaptopAgentBead(ctx context.Context, d laptopAgentReaper, oldID, newID, agentName, why string) bool {
	note := fmt.Sprintf("gb agent start --local (%s): closing this stale laptop agent bead, not adopting it: %s. The new run is %s.", agentName, why, newID)
	if err := d.AddComment(ctx, oldID, agentName, note); err != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not comment on stale agent bead %s: %v\n", oldID, err)
	}
	fields := map[string]string{
		"agent_state":  "done",
		"close_reason": "superseded: laptop agent restarted as " + newID,
	}
	if err := d.CloseBead(ctx, oldID, fields); err != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not close stale agent bead %s: %v\n", oldID, err)
		return false
	}
	fmt.Printf("[gb agent start] Closed stale agent bead %s (%s).\n", oldID, why)
	return true
}

// sameLaptopOwner reports whether b was run by one of owners. A laptop bead
// names its owner in operated_by (runner sessions) or created_by (the
// daemon's principal for the create); "oidc:a@b" and "a@b" are the same.
func sameLaptopOwner(b *beadsapi.BeadDetail, owners []string) bool {
	for _, o := range owners {
		o = normalizeLaptopOwner(o)
		if o == "" {
			continue
		}
		if o == normalizeLaptopOwner(b.Fields["operated_by"]) || o == normalizeLaptopOwner(b.CreatedBy) {
			return true
		}
	}
	return false
}

func normalizeLaptopOwner(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "oidc:")
	if s == "unknown" {
		return ""
	}
	return s
}

// localAgentStartPIDEnv carries the pid of the `gb agent start --local` that
// owns the session into coop, the agent and its hooks. Pods never set it.
const localAgentStartPIDEnv = "GB_AGENT_START_PID"

// localAgentStartAlive reports the pid of the gb agent start --local that owns
// this session, and whether it is still running. That process closes the
// agent bead when the session ends, so nothing else should while it lives.
func localAgentStartAlive() (int, bool) {
	pid, err := strconv.Atoi(strings.TrimSpace(os.Getenv(localAgentStartPIDEnv)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, localreg.Alive(localreg.Entry{PID: pid})
}

// beadCloser is the daemon surface closeLocalAgentBeadAtExit uses.
type beadCloser interface {
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// closeLocalAgentBeadAtExit closes a laptop session's agent bead as gb agent
// start exits. A long session can outlive the token it started with (the
// close on 10-28 failed "HTTP 401: invalid token"), so a failed close is
// retried once on a client from fresh(), when fresh is set. If the bead still
// is not closed, a registry entry with no live pid records it, and the next
// start of agentName closes it (supersedeStaleLaptopAgentBeads).
func closeLocalAgentBeadAtExit(d beadCloser, agentName, beadID string, fresh func(context.Context) (beadCloser, error)) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := d.CloseBead(ctx, beadID, nil)
	if err != nil && fresh != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: failed to close agent bead %s: %v; retrying with a fresh token\n", beadID, err)
		fd, ferr := fresh(ctx)
		if ferr != nil {
			err = fmt.Errorf("%w (fresh token: %v)", err, ferr)
		} else {
			err = fd.CloseBead(ctx, beadID, nil)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: failed to close agent bead %s: %v; the next start of %s closes it\n", beadID, err, agentName)
		if _, regErr := localreg.Register(localreg.Entry{Name: agentName, BeadID: beadID}); regErr != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not record the open bead in the local registry: %v\n", regErr)
		}
		return false
	}
	fmt.Printf("[gb agent start] Agent bead %s closed.\n", beadID)
	return true
}

// freshExitDaemon returns the client factory for the exit-close retry: the
// bearer is resolved again (the gb login session refreshes an expired access
// token). A runner session's token was taken out of the env at start and does
// not expire with a login, so it gets no retry.
func freshExitDaemon(runner bool) func(context.Context) (beadCloser, error) {
	if runner || daemon == nil {
		return nil
	}
	return func(ctx context.Context) (beadCloser, error) {
		token := resolveBearer(ctx, os.Stderr)
		if token == "" {
			return nil, errors.New("no bearer token to retry with (gb login)")
		}
		c, err := beadsapi.New(beadsapi.Config{
			HTTPAddr:          daemon.BaseURL(),
			Token:             token,
			ZitiIdentityPath:  zitiIdentityPath,
			ZitiService:       zitiService,
			ZitiTLSServerName: zitiTLSServerName,
		})
		if err != nil {
			return nil, err
		}
		if actor != "" {
			c.SetActor(actor)
		}
		if operatedBy != "" {
			c.SetOperatedBy(operatedBy)
		}
		return c, nil
	}
}
