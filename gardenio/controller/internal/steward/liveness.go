package steward

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/wall"
)

// Liveness (steward amendment 2026-10-23 §5, step 9.5). On every wake the
// steward sees the land's live agents and their last change. A gardener
// with no bead change in QuietAfter is asked once in #machines and, silent
// at the next wake, killed and its step reopened with a note; one restarted
// more than MaxRestarts times on its step is killed and the step marked
// blocked with the reason. The playbook states the rules; this file gives
// the facts they are applied to.

const (
	// QuietAfter is how long a gardener may go with no bead change.
	QuietAfter = 45 * time.Minute
	// MaxRestarts is how many restarts on one step a gardener is allowed.
	MaxRestarts = 3
	// liveShown caps the live agents in the prompt.
	liveShown = 20
)

// LiveAgent is one live agent of the land, as the playbook shows it.
type LiveAgent struct {
	ID    string
	Name  string
	Role  string
	State string
	// Task is the step it works, "" when none.
	Task string
	// LastChange is the agent bead's last update: its own writes, its
	// heartbeat and the controller's.
	LastChange time.Time
	// Age is how long ago LastChange was, at the wake; 0 when not known.
	Age time.Duration
	// Quiet is no change in QuietAfter.
	Quiet bool
	// Restarts is its session count less one.
	Restarts int
}

// TooManyRestarts reports a gardener past MaxRestarts on its step.
func (a LiveAgent) TooManyRestarts() bool { return a.Restarts > MaxRestarts }

// liveAgents lists the land's live agents other than its stewards, oldest
// change first, so the quiet ones lead.
func liveAgents(ctx context.Context, src purse.Source, land string, now time.Time) ([]LiveAgent, error) {
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + land},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"}, Limit: 500,
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s's live agents: %w", land, err)
	}
	var out []LiveAgent
	for _, b := range res.Beads {
		if b == nil || b.Type != "agent" || b.Status == "closed" || wall.IsSteward(b.Fields["role"]) || !running[b.Fields["agent_state"]] {
			continue
		}
		if p := strings.TrimSpace(b.Fields["project"]); p != "" && p != land {
			continue
		}
		a := LiveAgent{
			ID: b.ID, Name: firstNonEmpty(b.Fields["agent"], b.Title, b.ID), Role: b.Fields["role"],
			State: b.Fields["agent_state"], Task: strings.TrimSpace(b.Fields["task_id"]), LastChange: b.UpdatedAt,
		}
		if n := purse.FieldInt(b.Fields, "session_count"); n > 1 {
			a.Restarts = n - 1
		}
		if !a.LastChange.IsZero() {
			a.Age = max(0, now.Sub(a.LastChange))
			a.Quiet = a.Age >= QuietAfter
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(x, y LiveAgent) int {
		if c := x.LastChange.Compare(y.LastChange); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	if len(out) > liveShown {
		out = out[:liveShown]
	}
	return out, nil
}

// describeLive is one live agent's line in the prompt.
func describeLive(a LiveAgent) string {
	s := a.Name + " (" + a.ID + ", " + a.Role
	if a.Task != "" {
		s += ", step " + a.Task
	}
	s += ", " + a.State + ")"
	if a.LastChange.IsZero() && a.Age == 0 {
		s += ": last change not known"
	} else {
		s += fmt.Sprintf(": last change %s ago", a.Age.Round(time.Minute))
	}
	if a.Quiet {
		s += ", QUIET"
	}
	if a.Restarts > 0 {
		s += fmt.Sprintf(", %d restart(s)", a.Restarts)
		if a.TooManyRestarts() {
			s += ", PAST THE LIMIT"
		}
	}
	return s
}
