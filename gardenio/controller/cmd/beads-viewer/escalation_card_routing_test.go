package main

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildEscalationView_RoutingPanel(t *testing.T) {
	b := escBead2("open")
	b.Fields["held_by"] = "coordinator:merge-desk"
	b.Fields["approvers"] = `["coordinator:merge-desk","coordinator:laptop","operator"]`
	b.Fields["execute_by"] = "coordinator:merge-desk"
	b.Fields["policy_rule"] = "escalation#1"
	b.Fields["claimed_by"] = "role:reviewer"
	v := buildEscalationView(b, nil, "", time.Now())

	if v.HeldBy != "coordinator:merge-desk" || v.HeldByLabel != "coordinator merge-desk" {
		t.Errorf("held = %q / %q", v.HeldBy, v.HeldByLabel)
	}
	if want := []string{"coordinator merge-desk", "coordinator laptop", "the owner"}; !slices.Equal(v.Approvers, want) {
		t.Errorf("approvers = %v, want %v", v.Approvers, want)
	}
	if v.ExecuteByLabel != "coordinator merge-desk" || v.RoutingRule != "escalation#1" || v.ClaimedBy != "role:reviewer" {
		t.Errorf("routing = %+v", v)
	}
	// The spawn block's PolicyRule is the spawn_policy evaluator's, not routing's.
	if v.PolicyRule != "" {
		t.Errorf("routing policy_rule leaked into the spawn block: %q", v.PolicyRule)
	}
	for _, row := range v.Fields {
		switch row.Key {
		case "held_by", "approvers", "execute_by", "claimed_by", "policy_rule":
			t.Errorf("routing field %q double-rendered in the tail", row.Key)
		}
	}
}

// Positive control: an unstamped escalation has no routing panel rows (Global
// Constraint 7); #756's audience chip in the head is what it shows.
func TestBuildEscalationView_UnstampedHasNoLane(t *testing.T) {
	v := buildEscalationView(escBead2("open"), nil, "", time.Now())
	if v.HeldBy != "" || v.HeldByLabel != "" || v.Approvers != nil || v.ExecuteByLabel != "" {
		t.Errorf("unstamped routing = %+v", v)
	}
	if v.Audience == "" {
		t.Error("#756's audience chip lost on an unstamped escalation")
	}
}

// TestBuildEscalationView_LaneNote is gd-cuTEquiXqq: the owner approved a
// laptop-held MinIO ask from the card, not seeing it was the laptop's, and the
// approval ran nothing. The card now says both; a floor item says neither.
func TestBuildEscalationView_LaneNote(t *testing.T) {
	laptopHeld := func() *beadsapi.BeadDetail {
		b := escBead2("in_progress")
		b.Fields["operation_type"], b.Fields["operation_spec"] = "minio_bucket_quota", "{}"
		b.Fields["held_by"] = "coordinator:laptop"
		b.Fields["approvers"] = `["coordinator:laptop","operator"]`
		b.Fields["execute_by"] = "coordinator:laptop"
		b.Fields["audience"] = "operator"
		return b
	}
	cases := []struct {
		name string
		edit func(map[string]string)
		want string
	}{
		{"held by the laptop", func(map[string]string) {}, "coordinator laptop holds this and may approve it; it does not need the owner."},
		{"approved, nothing dispatched", func(f map[string]string) {
			f["approved_by"], f["execution_dispatch"] = "owner@example.com", "none"
		}, "Approved. It waits for coordinator laptop to do the work; the approval runs nothing by itself."},
		{"approved and claimed", func(f map[string]string) { f["approved_by"], f["claimed_by"] = "owner@example.com", "machine:laptop" }, ""},
		{"approved, dispatched locally", func(f map[string]string) { f["approved_by"], f["execution_dispatch"] = "owner@example.com", "local" }, ""},
		{"approved with a result", func(f map[string]string) { f["approved_by"], f["result_status"] = "owner@example.com", "success" }, ""},
		{"floor", func(f map[string]string) { f["operator_only"] = "true" }, ""},
		{"held by the owner", func(f map[string]string) { f["held_by"] = "operator" }, ""},
		{"closed", func(f map[string]string) { f["approved_by"] = "owner@example.com" }, ""},
	}
	for _, c := range cases {
		b := laptopHeld()
		c.edit(b.Fields)
		if c.name == "closed" {
			b.Status = "closed"
		}
		if got := buildEscalationView(b, nil, "", time.Now()).LaneNote; got != c.want {
			t.Errorf("%s: LaneNote = %q, want %q", c.name, got, c.want)
		}
	}
}

// presenceDaemon answers held_by_presence for one escalation.
type presenceDaemon struct {
	beadAPI
	lp *beadsapi.LanePresence
}

func (d *presenceDaemon) EscalationHolderPresence(context.Context, string) (*beadsapi.LanePresence, error) {
	return d.lp, nil
}

// TestFillLaneOffline is gd-t41kYFbLUt: a laptop-held item says the laptop is
// offline, and since when, while its lane is away.
func TestFillLaneOffline(t *testing.T) {
	now := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	held := func() *escalationView {
		b := escBead2("open")
		b.Fields["held_by"], b.Fields["execute_by"] = "coordinator:laptop", "coordinator:laptop"
		return buildEscalationView(b, nil, "", now)
	}
	cases := []struct {
		name string
		lp   *beadsapi.LanePresence
		want string
	}{
		{"away since last night", &beadsapi.LanePresence{Lane: "coordinator:laptop", LastSeenAt: "2026-11-04T22:06:00Z"}, "laptop offline since Nov 4 22:06Z; it holds this until it is back."},
		{"away since today", &beadsapi.LanePresence{Lane: "coordinator:laptop", LastSeenAt: "2026-11-05T00:10:00Z"}, "laptop offline since 00:10Z; it holds this until it is back."},
		{"never seen", &beadsapi.LanePresence{Lane: "coordinator:laptop"}, "laptop offline, not seen yet; it holds this until it is back."},
		{"online", &beadsapi.LanePresence{Lane: "coordinator:laptop", Online: true, LastSeenAt: "2026-11-05T00:59:00Z"}, ""},
		{"not tracked", nil, ""},
	}
	for _, c := range cases {
		s := &Server{daemon: &presenceDaemon{lp: c.lp}, logger: slog.Default(), now: func() time.Time { return now }}
		ev := held()
		s.fillLaneOffline(context.Background(), ev)
		if ev.LaneOffline != c.want {
			t.Errorf("%s: LaneOffline = %q, want %q", c.name, ev.LaneOffline, c.want)
		}
	}
	s := &Server{daemon: &presenceDaemon{lp: cases[0].lp}, logger: slog.Default(), now: func() time.Time { return now }}
	ev := held()
	ev.HeldBy = "operator"
	if s.fillLaneOffline(context.Background(), ev); ev.LaneOffline != "" {
		t.Errorf("owner-held: LaneOffline = %q, want none", ev.LaneOffline)
	}
}
