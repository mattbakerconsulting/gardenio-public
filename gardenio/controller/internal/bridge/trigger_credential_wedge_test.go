package bridge

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// credWedgeAgent builds a type=agent bead fixture for the credential-wedge sweep.
// createdAgo sets created_at; state sets agent_state; extra overrides any field
// (total_output_tokens, total_cost_usd, account, ...).
func credWedgeAgent(id, state string, createdAgo time.Duration, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"agent":       id + "-name",
		"project":     "gardenio",
		"role":        "gardener-delivery",
		"agent_state": state,
		"account":     "gardenio",
	}
	maps.Copy(fields, extra)
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "agent",
		Status: "open",
		Title:  id + "-name",
		Fields: fields,
		// updated_at is deliberately FRESH — the whole point is that SyncAll churns
		// it so an updated_at check reads the wedge as alive. The sweep must ignore
		// it and age from created_at.
		UpdatedAt: time.Now().UTC().Add(-1 * time.Minute),
		CreatedAt: time.Now().UTC().Add(-createdAgo),
	}
}

// credWedgeEngine wires a TriggerEngine with the credential-wedge sweep enabled
// and a recording notifier over the supplied fixtures.
func credWedgeEngine(t *testing.T, notify, createBead bool, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *recordingNotifier) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:              daemon,
		Notifier:            rec.notify,
		Logger:              slog.Default(),
		CredWedgeEnabled:    true,
		CredWedgeNotify:     notify,
		CredWedgeCreateBead: createBead,
	})
	return engine, daemon, rec
}

// THE acceptance (bug ka-0000000592): an agent parked in agent_state=rate_limited
// past the min-age with zero completed turns (zero output tokens, zero cost) —
// while updated_at reads FRESH — is flagged. It WARN-logs, one-shot-comments on
// the agent bead, and notifies; it never spawns or kills.
func TestCredWedgeSweep_Wedged_Alerts(t *testing.T) {
	agent := credWedgeAgent("gd-wedged", "rate_limited", 2*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
		"session_count":       "4",
		"total_request_count": "37",
	})
	engine, daemon, rec := credWedgeEngine(t, true, false, agent)

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 credential-wedge notify, got %d", rec.count())
	}
	if !slices.Contains(daemon.commentedIDs, "gd-wedged") {
		t.Fatalf("expected a one-shot comment on the wedged agent bead, commented=%v", daemon.commentedIDs)
	}
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("credential-wedge sweep must not spawn, spawned %d", len(daemon.spawnedNames))
	}
}

// budget_exceeded is the sibling credential-error state and is flagged identically.
func TestCredWedgeSweep_BudgetExceeded_Alerts(t *testing.T) {
	agent := credWedgeAgent("gd-budget", "budget_exceeded", 2*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
	})
	engine, daemon, rec := credWedgeEngine(t, true, false, agent)

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 1 {
		t.Fatalf("expected 1 alert for budget_exceeded, got %d", rec.count())
	}
	if !slices.Contains(daemon.commentedIDs, "gd-budget") {
		t.Fatalf("expected a comment on the budget_exceeded agent, commented=%v", daemon.commentedIDs)
	}
}

// The critical false-positive guard: an agent that has made real progress (output
// tokens > 0) is merely rate-limited BETWEEN productive turns, not wedged. Must
// NOT alert even when parked in rate_limited past the min-age.
func TestCredWedgeSweep_MadeProgress_NoAlert(t *testing.T) {
	agent := credWedgeAgent("gd-progress", "rate_limited", 2*time.Hour, map[string]string{
		"total_output_tokens": "12345",
		"total_cost_usd":      "0",
	})
	engine, daemon, rec := credWedgeEngine(t, true, false, agent)

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 0 {
		t.Fatalf("an agent that produced output must not alert, got %d", rec.count())
	}
	if len(daemon.commentedIDs) != 0 {
		t.Fatalf("no comment expected for a progressing agent, commented=%v", daemon.commentedIDs)
	}
}

// A non-zero cost with zero tokens also proves progress (a completed turn accrued
// cost) — must NOT alert.
func TestCredWedgeSweep_NonZeroCost_NoAlert(t *testing.T) {
	agent := credWedgeAgent("gd-cost", "rate_limited", 2*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "1.23",
	})
	engine, _, rec := credWedgeEngine(t, true, false, agent)

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 0 {
		t.Fatalf("an agent with non-zero cost must not alert, got %d", rec.count())
	}
}

// Too young: a rate-limit blip inside the grace window is not yet a wedge. Must
// NOT alert.
func TestCredWedgeSweep_TooYoung_NoAlert(t *testing.T) {
	agent := credWedgeAgent("gd-young", "rate_limited", 5*time.Minute, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
	})
	engine, _, rec := credWedgeEngine(t, true, false, agent)

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 0 {
		t.Fatalf("a too-young rate-limit blip must not alert, got %d", rec.count())
	}
}

// A healthy working agent (not in a credential-error state) is never flagged,
// regardless of age or progress.
func TestCredWedgeSweep_HealthyWorking_NoAlert(t *testing.T) {
	agent := credWedgeAgent("gd-working", "working", 6*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
	})
	engine, _, rec := credWedgeEngine(t, true, false, agent)

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 0 {
		t.Fatalf("a non-error-state agent must not alert, got %d", rec.count())
	}
}

// Dedup: an agent that stays wedged across cycles alerts once, then re-alerts
// after it recovers (leaves the error state) and wedges again.
func TestCredWedgeSweep_DedupPerEpisode(t *testing.T) {
	agent := credWedgeAgent("gd-wedged", "rate_limited", 2*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
	})
	engine, _, rec := credWedgeEngine(t, true, false, agent)
	ctx := context.Background()

	engine.SweepCredentialWedge(ctx)
	engine.SweepCredentialWedge(ctx)
	engine.SweepCredentialWedge(ctx)
	if rec.count() != 1 {
		t.Fatalf("expected 1 alert across 3 wedged sweeps (dedup), got %d", rec.count())
	}

	// Recovery: agent leaves the error state (a fallback account served a turn).
	agent.Fields["agent_state"] = "working"
	engine.SweepCredentialWedge(ctx)
	if rec.count() != 1 {
		t.Fatalf("recovery sweep must not alert, got %d", rec.count())
	}

	// New wedge episode re-arms the alert.
	agent.Fields["agent_state"] = "rate_limited"
	engine.SweepCredentialWedge(ctx)
	if rec.count() != 2 {
		t.Fatalf("new wedge episode must re-alert, got %d", rec.count())
	}
}

// Recovery-by-progress also re-arms: if the agent's output ticks up between
// sweeps it is no longer wedged, and a later zero-progress episode re-alerts.
// (Guards against a stuck episode flag that never clears on progress.)
func TestCredWedgeSweep_ProgressClearsEpisode(t *testing.T) {
	agent := credWedgeAgent("gd-wedged", "rate_limited", 2*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
	})
	engine, _, rec := credWedgeEngine(t, true, false, agent)
	ctx := context.Background()

	engine.SweepCredentialWedge(ctx)
	if rec.count() != 1 {
		t.Fatalf("expected 1 initial alert, got %d", rec.count())
	}

	// Progress arrives — episode should clear even though still rate_limited.
	agent.Fields["total_output_tokens"] = "500"
	engine.SweepCredentialWedge(ctx)
	if rec.count() != 1 {
		t.Fatalf("progress must not re-alert, got %d", rec.count())
	}

	// Progress stalls again at a new zero baseline (a fresh episode after a
	// respawn). The episode flag must have cleared, so this re-alerts.
	agent.Fields["total_output_tokens"] = "0"
	engine.SweepCredentialWedge(ctx)
	if rec.count() != 2 {
		t.Fatalf("a fresh zero-progress episode must re-alert, got %d", rec.count())
	}
}

// The create_bead leg files a durable bug bead parented to the wedged agent.
func TestCredWedgeSweep_CreateBeadLeg(t *testing.T) {
	agent := credWedgeAgent("gd-wedged", "rate_limited", 2*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
	})
	engine, daemon, _ := credWedgeEngine(t, false, true, agent)

	engine.SweepCredentialWedge(context.Background())

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 credential-wedge bead, got %d", len(daemon.createdBeads))
	}
	got := daemon.createdBeads[0]
	if got.Type != "bug" {
		t.Errorf("credential-wedge bead type = %q, want bug", got.Type)
	}
	if !slices.Contains(got.Labels, "credential-wedge") || !slices.Contains(got.Labels, "project:gardenio") {
		t.Errorf("credential-wedge bead labels missing expected entries: %v", got.Labels)
	}
	// Parented to the agent bead.
	found := false
	for _, d := range daemon.addedDeps {
		if d.DependsOnID == "gd-wedged" && d.DepType == "parent-child" {
			found = true
		}
	}
	if !found {
		t.Errorf("credential-wedge bead not parented to agent gd-wedged, deps=%v", daemon.addedDeps)
	}
}

// Even with both external legs off, the sweep still WARN-logs and writes the
// one-shot agent-bead comment — the primary signal the task asked for.
func TestCredWedgeSweep_CommentAlwaysWrittenWhenEnabled(t *testing.T) {
	agent := credWedgeAgent("gd-wedged", "rate_limited", 2*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
	})
	engine, daemon, rec := credWedgeEngine(t, false, false, agent)

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 0 {
		t.Fatalf("notify off — no external notify expected, got %d", rec.count())
	}
	if len(daemon.createdBeads) != 0 {
		t.Fatalf("create_bead off — no bead expected, got %d", len(daemon.createdBeads))
	}
	if !slices.Contains(daemon.commentedIDs, "gd-wedged") {
		t.Fatalf("the one-shot comment must always be written when the sweep is enabled, commented=%v", daemon.commentedIDs)
	}
}

// Disabled sweep is a strict no-op.
func TestCredWedgeSweep_Disabled_NoOp(t *testing.T) {
	agent := credWedgeAgent("gd-wedged", "rate_limited", 2*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
	})
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{agent}}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:           daemon,
		Notifier:         rec.notify,
		Logger:           slog.Default(),
		CredWedgeEnabled: false, // off
		CredWedgeNotify:  true,
	})

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 0 || len(daemon.commentedIDs) != 0 {
		t.Fatalf("disabled sweep must be a no-op, got %d alerts / %d comments",
			rec.count(), len(daemon.commentedIDs))
	}
}

// A closed agent bead (status not in {open,in_progress}) is out of the query
// window entirely — nothing to flag. Faithful to the daemon status filter.
func TestCredWedgeSweep_ClosedAgent_NoAlert(t *testing.T) {
	agent := credWedgeAgent("gd-closed", "rate_limited", 2*time.Hour, map[string]string{
		"total_output_tokens": "0",
		"total_cost_usd":      "0",
	})
	agent.Status = "closed"
	engine, _, rec := credWedgeEngine(t, true, false, agent)

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 0 {
		t.Fatalf("a closed agent must not alert, got %d", rec.count())
	}
}

// An empty/unset total_output_tokens field (never written) reads as zero
// progress, so a long-parked error-state agent with no telemetry is still
// flagged — the real gardener-delivery bead had total_output_tokens=0.
func TestCredWedgeSweep_EmptyTelemetry_Alerts(t *testing.T) {
	agent := credWedgeAgent("gd-empty", "rate_limited", 2*time.Hour, nil)
	// no total_output_tokens / total_cost_usd fields at all
	delete(agent.Fields, "total_output_tokens")
	delete(agent.Fields, "total_cost_usd")
	engine, daemon, rec := credWedgeEngine(t, true, false, agent)

	engine.SweepCredentialWedge(context.Background())

	if rec.count() != 1 {
		t.Fatalf("empty-telemetry error-state agent must alert (zero progress), got %d", rec.count())
	}
	if !slices.Contains(daemon.commentedIDs, "gd-empty") {
		t.Fatalf("expected a comment on the empty-telemetry agent, commented=%v", daemon.commentedIDs)
	}
}
