package wall

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/lands/provision"
)

func liveAgent(f *fakeSource, land, role, state string) {
	f.add("agent", role, "open", []string{"project:" + land}, map[string]string{"project": land, "role": role, "agent_state": state})
}

func TestMaxLiveAgents_DefaultMatchesTheProvisioner(t *testing.T) {
	if DefaultMaxLiveAgents != provision.DefaultMaxLiveAgents {
		t.Errorf("wall default %d, provisioner writes %d", DefaultMaxLiveAgents, provision.DefaultMaxLiveAgents)
	}
	for v, want := range map[string]int{"": 3, "0": 3, "-1": 3, "x": 3, "1": 1, "5": 5} {
		if got := MaxLiveAgents(map[string]string{FieldMaxLiveAgents: v}); got != want {
			t.Errorf("max_live_agents %q = %d, want %d", v, got, want)
		}
	}
}

// Each refusal of the cap, and what holds no slot.
func TestLiveCap(t *testing.T) {
	ctx := context.Background()
	f := &fakeSource{}
	fields := map[string]string{FieldMaxLiveAgents: "3"}
	liveAgent(f, "tomato-patch", "steward", "working")
	liveAgent(f, "tomato-patch", "lands-gardener", "working")
	// None of these hold a slot.
	for _, s := range []string{"done", "failed", "killed", AgentState, "queued", "stopping"} {
		liveAgent(f, "tomato-patch", "lands-gardener", s)
	}
	f.add("agent", "closed", "closed", []string{"project:tomato-patch"}, map[string]string{"project": "tomato-patch", "role": "lands-gardener", "agent_state": "working"})
	liveAgent(f, "other-land", "lands-gardener", "working")

	// Steward + one gardener of 3: one more gardener fits.
	if r := LiveCap(ctx, f, "tomato-patch", fields, "lands-gardener", "kd-new"); r.Refused() || !r.Tenant {
		t.Fatalf("2 of 3 live = %+v, want admitted", r)
	}
	liveAgent(f, "tomato-patch", "lands-gardener", "spawning")
	r := LiveCap(ctx, f, "tomato-patch", fields, "lands-gardener", "kd-new")
	if r.Code != CodeMaxLiveAgents || !strings.Contains(r.Reason, "at most 3") {
		t.Fatalf("3 of 3 live = %+v, want %s", r, CodeMaxLiveAgents)
	}
	// The steward's slot is held even when it is not running.
	f.beads[0].Fields["agent_state"] = "done"
	if r := LiveCap(ctx, f, "tomato-patch", fields, "lands-gardener", "kd-new"); r.Code != CodeMaxLiveAgents {
		t.Errorf("two gardeners, no steward, cap 3 = %+v, want refused (the steward's slot is held)", r)
	}
	// The steward is never refused.
	if r := LiveCap(ctx, f, "tomato-patch", fields, "steward", "kd-st"); r.Refused() {
		t.Errorf("steward on a full land = %+v", r)
	}
	// A role list naming the steward is not the steward (gd-U9AV7jG8hF).
	if r := LiveCap(ctx, f, "tomato-patch", fields, "crew,steward", "kd-new"); r.Code != CodeMaxLiveAgents {
		t.Errorf("crew,steward on a full land = %+v, want %s", r, CodeMaxLiveAgents)
	}
	// A land whose agents cannot be read is refused.
	f.failType = "agent"
	if r := LiveCap(ctx, f, "tomato-patch", fields, "lands-gardener", "kd-new"); r.Code != CodeMaxLiveAgents || !strings.Contains(r.Reason, "could not be counted") {
		t.Errorf("unreadable agents = %+v, want refused", r)
	}
}

// A live "crew,steward" agent holds a slot: only StewardSpawn is the steward
// (gd-U9AV7jG8hF).
func TestLiveCap_RoleListHoldsASlot(t *testing.T) {
	f := &fakeSource{}
	liveAgent(f, "tomato-patch", "steward", "working")
	liveAgent(f, "tomato-patch", "crew,steward", "working")
	fields := map[string]string{FieldMaxLiveAgents: "2"}
	if r := LiveCap(context.Background(), f, "tomato-patch", fields, "lands-gardener", "kd-new"); r.Code != CodeMaxLiveAgents {
		t.Errorf("steward + crew,steward, cap 2 = %+v, want %s", r, CodeMaxLiveAgents)
	}
}

func TestLiveCapFull(t *testing.T) {
	for _, c := range []struct {
		others, limit int
		full          bool
	}{{0, 1, true}, {0, 2, false}, {1, 2, true}, {1, 3, false}, {2, 3, true}} {
		if got := LiveCapFull(c.others, c.limit); got != c.full {
			t.Errorf("LiveCapFull(%d, %d) = %v, want %v", c.others, c.limit, got, c.full)
		}
	}
}
