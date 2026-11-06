package agentwake

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

type spawnCall struct {
	name, project, task, role, prompt string
	fields                            map[string]string
	operator                          string
}

type fakeClient struct {
	updates  map[string]beadsapi.UpdateBeadRequest
	closed   []string
	spawns   []spawnCall
	spawnErr error
}

func newFakeClient() *fakeClient {
	return &fakeClient{updates: map[string]beadsapi.UpdateBeadRequest{}}
}

func (f *fakeClient) UpdateBead(_ context.Context, id string, req beadsapi.UpdateBeadRequest) error {
	f.updates[id] = req
	return nil
}

func (f *fakeClient) AddComment(context.Context, string, string, string) error { return nil }

func (f *fakeClient) CloseBead(_ context.Context, id string, _ map[string]string) error {
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakeClient) SpawnAgent(ctx context.Context, name, project, task, role, prompt string, extra ...map[string]string) (string, error) {
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	fields := map[string]string{}
	for _, m := range extra {
		for k, v := range m {
			fields[k] = v
		}
	}
	f.spawns = append(f.spawns, spawnCall{name, project, task, role, prompt, fields, beadsapi.OperatorFromContext(ctx)})
	return "gd-new", nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func agentBead(status string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{"agent": "crew-a", "project": "gardenio", "task_id": "gd-t"}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{ID: "gd-a", Type: "agent", Status: status, Fields: f}
}

func request(b *beadsapi.BeadDetail, prepared *bool) Request {
	return Request{
		AgentName: "crew-a", AgentBead: b, Reason: beadsapi.WakeReasonCI,
		Project: "gardenio", TaskID: "gd-t", Role: "crew",
		ExtraFields: map[string]string{"spawn_source": "test"},
		Prepare: func(context.Context) (string, bool) {
			*prepared = true
			return "go", true
		},
	}
}

func TestWake_DormantIsWokenInPlaceWithoutPreparingASpawn(t *testing.T) {
	c := newFakeClient()
	prepared := false
	res := Wake(context.Background(), c, quiet(), request(agentBead("closed", map[string]string{
		beadsapi.FieldSuccessor: beadsapi.SuccessorSame,
	}), &prepared))
	if res.Outcome != Woken || res.AgentBeadID != "gd-a" {
		t.Fatalf("result = %+v, want Woken gd-a", res)
	}
	if prepared || len(c.spawns) != 0 {
		t.Errorf("in-place wake also prepared (%v) or spawned (%v)", prepared, c.spawns)
	}
	if got := c.updates["gd-a"].Fields[beadsapi.FieldWakeReason]; got != beadsapi.WakeReasonCI {
		t.Errorf("wake_reason = %v, want ci", got)
	}
	// A dormant identity parked by `gb stop` carries stop_requested=true; left
	// set, the woken pod re-closes itself at boot (gd-NGlLifFXbO).
	if v, ok := c.updates["gd-a"].Fields[beadsapi.FieldStopRequested]; !ok || v != "" {
		t.Errorf("stop_requested = %v (present=%v), want cleared to \"\"", v, ok)
	}
}

func TestWake_PrepareCanDecline(t *testing.T) {
	c := newFakeClient()
	req := request(agentBead("open", map[string]string{beadsapi.FieldAgentState: "stopping"}), new(bool))
	req.Prepare = func(context.Context) (string, bool) { return "", false }
	if res := Wake(context.Background(), c, quiet(), req); res.Outcome != Declined {
		t.Fatalf("outcome = %v, want Declined", res.Outcome)
	}
	if len(c.closed) != 0 || len(c.spawns) != 0 {
		t.Errorf("declined wake still closed %v / spawned %v", c.closed, c.spawns)
	}
}

func TestWake_FrozenIsClosedThenSpawnedOnRunsc(t *testing.T) {
	c := newFakeClient()
	res := Wake(context.Background(), c, quiet(), request(agentBead("open", map[string]string{
		beadsapi.FieldAgentState: "stopping",
	}), new(bool)))
	if res.Outcome != Spawned || !res.Thawed {
		t.Fatalf("result = %+v, want a thaw spawn", res)
	}
	if len(c.closed) != 1 || c.closed[0] != "gd-a" {
		t.Errorf("closed = %v, want the frozen bead", c.closed)
	}
	if rc := c.spawns[0].fields["runtime_class"]; rc != RuntimeClassRunsc {
		t.Errorf("runtime_class = %q, want runsc (the frozen bead had none)", rc)
	}
}

func TestWake_FailedThawSpawnReopensTheFrozenBead(t *testing.T) {
	c := newFakeClient()
	c.spawnErr = errors.New("refused")
	res := Wake(context.Background(), c, quiet(), request(agentBead("open", map[string]string{
		beadsapi.FieldAgentState: "stopping",
	}), new(bool)))
	if res.Outcome != Failed || res.Err == nil {
		t.Fatalf("result = %+v, want Failed with the error", res)
	}
	if st := c.updates["gd-a"].Status; st == nil || *st != "open" {
		t.Errorf("frozen bead not reopened after the failed spawn: %+v", c.updates["gd-a"])
	}
}

func TestWake_SpawnCarriesTheAgentBeadsBindings(t *testing.T) {
	b := agentBead("closed", map[string]string{
		"plan_id":              "gd-plan",
		"slack_thread_channel": "C1",
		"slack_user_id":        "U1",
		beadsapi.FieldThreadID: "gd-head",
	})
	for _, carry := range []bool{false, true} {
		c := newFakeClient()
		req := request(b, new(bool))
		req.Operator = "oidc:op@example.com"
		req.CarrySlackRouting = carry
		if res := Wake(context.Background(), c, quiet(), req); res.Outcome != Spawned {
			t.Fatalf("carry=%v: outcome %v, want Spawned", carry, res.Outcome)
		}
		s := c.spawns[0]
		for k, want := range map[string]string{
			"plan_id": "gd-plan", "operated_by": "oidc:op@example.com", "spawned_by": "gd-a",
			"spawn_source": "test", beadsapi.FieldWakeReason: beadsapi.WakeReasonCI,
		} {
			if s.fields[k] != want {
				t.Errorf("carry=%v: %s = %q, want %q", carry, k, s.fields[k], want)
			}
		}
		if s.operator != "oidc:op@example.com" {
			t.Errorf("carry=%v: ctx operator = %q", carry, s.operator)
		}
		if _, ok := s.fields["runtime_class"]; ok {
			t.Errorf("carry=%v: a non-frozen spawn was pinned to a runtime class", carry)
		}
		slack := s.fields["slack_thread_channel"] == "C1" && s.fields["slack_user_id"] == "U1" &&
			s.fields[beadsapi.FieldThreadID] == "gd-head" && s.fields[beadsapi.FieldSlackThreadBeadID] == "gd-head"
		if slack != carry {
			t.Errorf("carry=%v: Slack routing carried = %v (fields %v)", carry, slack, s.fields)
		}
	}
}

// A wake re-dispatches the crew onto its task, so the controller's claim at pod
// create (gd-Yo28zKgkGx) must run for it: a fresh spawn onto the same task
// keeps the single-claim opt-in, and an in-place wake clears the first-create
// stamp.
func TestWake_KeepsTheSpawnTaskClaimForARedispatch(t *testing.T) {
	t.Run("fresh spawn onto the same task keeps the opt-in", func(t *testing.T) {
		c := newFakeClient()
		b := agentBead("closed", map[string]string{beadsapi.FieldClaimTaskAtSpawn: "true", beadsapi.FieldSpawnTaskClaim: "claimed"})
		if res := Wake(context.Background(), c, quiet(), request(b, new(bool))); res.Outcome != Spawned {
			t.Fatalf("outcome %v, want Spawned", res.Outcome)
		}
		f := c.spawns[0].fields
		if f[beadsapi.FieldClaimTaskAtSpawn] != "true" {
			t.Errorf("%s = %q, want true", beadsapi.FieldClaimTaskAtSpawn, f[beadsapi.FieldClaimTaskAtSpawn])
		}
		if _, ok := f[beadsapi.FieldSpawnTaskClaim]; ok {
			t.Errorf("the old bead's claim stamp was carried: %v", f)
		}
	})
	t.Run("fresh spawn onto another task drops it", func(t *testing.T) {
		c := newFakeClient()
		b := agentBead("closed", map[string]string{beadsapi.FieldClaimTaskAtSpawn: "true", "task_id": "gd-other"})
		Wake(context.Background(), c, quiet(), request(b, new(bool)))
		if v, ok := c.spawns[0].fields[beadsapi.FieldClaimTaskAtSpawn]; ok {
			t.Errorf("%s = %q carried onto a different task", beadsapi.FieldClaimTaskAtSpawn, v)
		}
	})
	t.Run("in-place wake clears the first-create stamp", func(t *testing.T) {
		c := newFakeClient()
		b := agentBead("closed", map[string]string{beadsapi.FieldSuccessor: beadsapi.SuccessorSame, beadsapi.FieldSpawnTaskClaim: "claimed"})
		if res := Wake(context.Background(), c, quiet(), request(b, new(bool))); res.Outcome != Woken {
			t.Fatalf("outcome %v, want Woken", res.Outcome)
		}
		if v, ok := c.updates["gd-a"].Fields[beadsapi.FieldSpawnTaskClaim]; !ok || v != nil {
			t.Errorf("%s = %v (present=%v), want deleted (nil)", beadsapi.FieldSpawnTaskClaim, v, ok)
		}
	})
}
