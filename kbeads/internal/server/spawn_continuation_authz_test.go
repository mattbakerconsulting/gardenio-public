package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Same-role continuation spawn authorization (gd-VuYwGGYLLT): with privileged
// role beads in place, a respawn / successor into the role an
// agent bead already holds passes without X-Escalate; everything else still
// meets the privileged-role gate.

type contFixture struct {
	srv *BeadsServer
	ms  *mockStore
}

func newContFixture(t *testing.T) *contFixture {
	t.Helper()
	ms := newMockStore()
	makeBypassRoleBead(ms, "gd-role-captain", "captain")
	makeBypassRoleBead(ms, "gd-role-reviewer", "reviewer")
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	return &contFixture{srv: NewBeadsServer(ms, &events.NoopPublisher{}), ms: ms}
}

func (f *contFixture) agent(id, project, role string, status model.Status) {
	f.ms.beads[id] = &model.Bead{ID: id, Type: "agent", Kind: model.KindData, Status: status,
		Fields: agentFields(project, role)}
}

// agentIn is agent with an agent_state, for the resumable-prior cases.
func (f *contFixture) agentIn(id, project, role string, status model.Status, state string) {
	b, _ := json.Marshal(map[string]string{"project": project, "role": role, "agent": "test", "agent_state": state})
	f.ms.beads[id] = &model.Bead{ID: id, Type: "agent", Kind: model.KindData, Status: status, Fields: b}
}

// decision stores a decision bead as the daemon would hold it after the
// requesting agent filed it and somebody answered (or did not answer) it.
func (f *contFixture) decision(id string, fields map[string]string, status model.Status) {
	b, _ := json.Marshal(fields)
	f.ms.beads[id] = &model.Bead{ID: id, Type: model.TypeDecision, Kind: model.KindData, Status: status, Fields: b}
}

// answeredDecision is a decision prior's own credential filed, closed with a
// resolver's answer.
func (f *contFixture) answeredDecision(id, prior string) {
	f.decision(id, map[string]string{
		"prompt": "go?", "requesting_agent_bead_id": prior, "requesting_agent_verified": "true",
		"chosen": "yes", "responded_by": "alice@example.com",
	}, model.StatusClosed)
}

// bridgeFields is a respawn the way the Slack bridge creates it: spawned_by the
// prior bead, and the resolution it is acting on.
func bridgeFields(project, role, prior, source, evidenceKey, evidenceID string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{
		"project": project, "role": role, "agent": "next", "spawned_by": prior,
		"spawn_source": source, evidenceKey: evidenceID,
	})
	return b
}

func decisionResume(project, role, prior, decisionID string) json.RawMessage {
	return bridgeFields(project, role, prior, "decision-resume", "decision_bead_id", decisionID)
}

func contFields(project, role, spawnedBy string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"project": project, "role": role, "agent": "next", "spawned_by": spawnedBy})
	return b
}

func (f *contFixture) gate(ctx context.Context, fields json.RawMessage) error {
	return authorizeAgentSpawn(f.srv.withSameRoleContinuationAuth(ctx, fields), f.ms, fields)
}

func slackBridgeMachineCtx() context.Context {
	return machineCtx("gardenio-slack-bridge", "gardenio-machine-slack-dm-delivery")
}

func slackBridgeFleetCtx() context.Context {
	return WithFleet(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"}), authMethodStatic), "slack-bridge")
}

func TestSameRoleContinuation_BridgeDecisionRespawnPasses(t *testing.T) {
	f := newContFixture(t)
	// The decision respawn closes the frozen bead first, so a cleanly stopped closed prior counts.
	f.agentIn("gd-prior", "gardenio", "captain", model.StatusClosed, "done")
	f.answeredDecision("gd-dec", "gd-prior")
	for name, ctx := range map[string]context.Context{"machine": slackBridgeMachineCtx(), "fleet": slackBridgeFleetCtx()} {
		if err := f.gate(ctx, decisionResume("gardenio", "captain", "gd-prior", "gd-dec")); err != nil {
			t.Errorf("%s: bridge respawn of a captain must pass, got %v", name, err)
		}
	}
}

func TestSameRoleContinuation_BridgeDecisionRespawnPassesOnFreeTextAnswerAndFrozenPrior(t *testing.T) {
	f := newContFixture(t)
	f.agentIn("gd-frozen", "gardenio", "captain", model.StatusOpen, "stopping")
	f.decision("gd-dec", map[string]string{
		"requesting_agent_bead_id": "gd-frozen", "requesting_agent_verified": "true",
		"response_text": "ship it", "responded_by": "alice@example.com",
	}, model.StatusClosed)
	if err := f.gate(slackBridgeMachineCtx(), decisionResume("gardenio", "captain", "gd-frozen", "gd-dec")); err != nil {
		t.Fatalf("free-text answer on a frozen prior must pass, got %v", err)
	}
}

// gd-H17IlFclcl: the bridge's decision-resume of a trigger-fired plan-gardener
// (a privileged role) is admitted only as a same-role continuation, so it
// must name the prior bead in spawned_by. The respawn carries the trigger in
// trigger_lease_id, which the engine's live-run gate reads and this gate does not;
// a respawn that kept spawned_by=trigger:<id> names no prior bead and escalates.
func TestSameRoleContinuation_BridgeResumeOfTriggerGardenerPasses(t *testing.T) {
	f := newContFixture(t)
	makeBypassRoleBead(f.ms, "gd-role-plan-gardener", "plan-gardener")
	prior, _ := json.Marshal(map[string]string{
		"project": "gardenio", "role": "plan-gardener", "agent": "plan-gardener-a", "agent_state": "done",
		"spawned_by": "trigger:gd-trig", "trigger_id": "gd-trig", "schedule_id": "gd-trig",
	})
	f.ms.beads["gd-gardener"] = &model.Bead{ID: "gd-gardener", Type: "agent", Kind: model.KindData, Status: model.StatusClosed, Fields: prior}
	f.answeredDecision("gd-dec", "gd-gardener")

	resume := func(spawnedBy string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{
			"project": "gardenio", "role": "plan-gardener", "agent": "plan-gardener-a",
			"spawned_by": spawnedBy, "trigger_lease_id": "gd-trig",
			"spawn_source": "decision-resume", "decision_bead_id": "gd-dec",
		})
		return b
	}
	for name, ctx := range map[string]context.Context{"machine": slackBridgeMachineCtx(), "fleet": slackBridgeFleetCtx()} {
		if err := f.gate(ctx, resume("gd-gardener")); err != nil {
			t.Errorf("%s: decision-resume of a trigger-fired plan-gardener must pass, got %v", name, err)
		}
	}
	// The shape PR #1653 first shipped: no prior bead, so no continuation.
	requireEscalates(t, f.gate(slackBridgeMachineCtx(), resume("trigger:gd-trig")))
}

func TestSameRoleContinuation_BridgeEscalationRespawnPasses(t *testing.T) {
	f := newContFixture(t)
	f.agentIn("gd-prior", "gardenio", "reviewer", model.StatusClosed, "done")
	b, _ := json.Marshal(map[string]string{"requesting_agent": "gd-prior", "requesting_agent_verified": "true", "result_status": "success"})
	f.ms.beads["gd-esc"] = &model.Bead{ID: "gd-esc", Type: model.TypeEscalation, Kind: model.KindData, Status: model.StatusClosed, Fields: b}
	fields := bridgeFields("gardenio", "reviewer", "gd-prior", "escalation-resume", "escalation_bead_id", "gd-esc")
	if err := f.gate(slackBridgeMachineCtx(), fields); err != nil {
		t.Fatalf("resolved escalation respawn must pass, got %v", err)
	}
}

func TestSameRoleContinuation_AgentSuccessorPasses(t *testing.T) {
	f := newContFixture(t)
	f.agentIn("gd-me", "gardenio", "reviewer", model.StatusInProgress, "working")
	ctx := agentFleetCtx("gd-me", "reviewer-a")
	for _, spawnedBy := range []string{"gd-me", "successor:gd-me"} {
		if err := f.gate(ctx, contFields("gardenio", "reviewer", spawnedBy)); err != nil {
			t.Errorf("spawned_by %q: own successor must pass, got %v", spawnedBy, err)
		}
	}
}

// Each case changes ONE thing from a continuation that passes, so it fails for
// the reason in its name and no other.
func TestSameRoleContinuation_StillEscalatesWhen(t *testing.T) {
	f := newContFixture(t)
	f.agent("gd-cap", "gardenio", "captain", model.StatusOpen)
	f.agent("gd-crew", "gardenio", "crew", model.StatusOpen)
	f.agent("gd-other", "other", "captain", model.StatusOpen)
	f.agent("gd-multi", "gardenio", "captain, crew", model.StatusOpen)
	f.ms.beads["gd-task"] = &model.Bead{ID: "gd-task", Type: "task", Kind: model.KindIssue, Status: model.StatusOpen}

	// Prior agents in the states a continuation must not resume from.
	for _, state := range []string{"failed", "expired", "killed", "retired"} {
		f.agentIn("gd-"+state, "gardenio", "captain", model.StatusOpen, state)
		f.answeredDecision("gd-dec-"+state, "gd-"+state)
	}
	f.agentIn("gd-killed-closed", "gardenio", "captain", model.StatusClosed, "working") // /kill closes without touching agent_state
	f.answeredDecision("gd-dec-killed-closed", "gd-killed-closed")
	f.agentIn("gd-done", "gardenio", "captain", model.StatusClosed, "done")
	f.agentIn("gd-noproject", "", "captain", model.StatusClosed, "done")
	f.answeredDecision("gd-dec-noproject", "gd-noproject")

	// Decisions that are not a resolution of the prior's own request.
	f.decision("gd-dec-self-filed", map[string]string{ // the agent filed it itself: no credential stamp
		"requesting_agent_bead_id": "gd-done", "chosen": "yes",
	}, model.StatusClosed)
	f.decision("gd-dec-bare-close", map[string]string{ // withdrawn by its filer: nothing answered
		"requesting_agent_bead_id": "gd-done", "requesting_agent_verified": "true",
	}, model.StatusClosed)
	f.decision("gd-dec-chosen-expired", map[string]string{
		"requesting_agent_bead_id": "gd-done", "requesting_agent_verified": "true", "chosen": "_expired",
	}, model.StatusClosed)
	// Answered, but attributed to a principal that is neither a human nor the Slack
	// bridge: what a forged *-bridge fleet token, an agent or the aging sweep stamps.
	for id, by := range map[string]string{
		"gd-dec-by-forged-bridge": "x-bridge", "gd-dec-by-agent": "crew-a", "gd-dec-by-aging": "system:decision-aging",
		"gd-dec-by-other-bridge": "gitlab-bridge", "gd-dec-by-nobody": "",
	} {
		f.decision(id, map[string]string{
			"requesting_agent_bead_id": "gd-done", "requesting_agent_verified": "true",
			"chosen": "yes", "responded_by": by,
		}, model.StatusClosed)
	}
	f.decision("gd-dec-open", map[string]string{
		"requesting_agent_bead_id": "gd-done", "requesting_agent_verified": "true", "chosen": "yes",
	}, model.StatusOpen)
	f.answeredDecision("gd-dec-for-cap", "gd-cap") // verified, but for a different agent than gd-done
	f.ms.beads["gd-esc-failed"] = &model.Bead{ID: "gd-esc-failed", Type: model.TypeEscalation, Status: model.StatusClosed,
		Fields: json.RawMessage(`{"requesting_agent":"gd-done","requesting_agent_verified":"true","result_status":"failure"}`)}
	f.ms.beads["gd-esc-other"] = &model.Bead{ID: "gd-esc-other", Type: model.TypeEscalation, Status: model.StatusClosed,
		Fields: json.RawMessage(`{"requesting_agent":"gd-someone-else","requesting_agent_verified":"true","result_status":"success"}`)}
	// Names the prior's own bead id but was not stamped from its credential: what a
	// static-token filer (or an old name-keyed stamp) leaves behind.
	f.ms.beads["gd-esc-unverified"] = &model.Bead{ID: "gd-esc-unverified", Type: model.TypeEscalation, Status: model.StatusClosed,
		Fields: json.RawMessage(`{"requesting_agent":"gd-done","result_status":"success"}`)}
	f.ms.beads["gd-esc-by-name"] = &model.Bead{ID: "gd-esc-by-name", Type: model.TypeEscalation, Status: model.StatusClosed,
		Fields: json.RawMessage(`{"requesting_agent":"test","requesting_agent_verified":"true","result_status":"success"}`)}
	f.answeredDecision("gd-dec", "gd-done")

	crew := agentFleetCtx("gd-crew", "crew-a")
	cap := agentFleetCtx("gd-cap", "cap-a")
	bridge := slackBridgeMachineCtx()
	for name, tc := range map[string]struct {
		ctx    context.Context
		fields json.RawMessage
	}{
		"shared static token names a captain as spawned_by":                        {staticCtx(), decisionResume("gardenio", "captain", "gd-done", "gd-dec")},
		"static token self-attributing with X-Actor":                               {WithAuthIdentity(staticCtx(), AuthIdentity{Type: "static", Subject: "gd-cap"}), contFields("gardenio", "captain", "gd-cap")},
		"crew agent names a captain as its spawned_by":                             {crew, contFields("gardenio", "captain", "gd-cap")},
		"crew agent claims its own bead for a wider role":                          {crew, contFields("gardenio", "captain", "gd-crew")},
		"captain agent names another captain's bead":                               {cap, contFields("gardenio", "captain", "gd-other")},
		"bridge, but the role is wider than the prior's":                           {bridge, decisionResume("gardenio", "reviewer", "gd-done", "gd-dec")},
		"bridge widening a captain+crew continuation with a role the prior lacks":  {bridge, decisionResume("gardenio", "captain,reviewer", "gd-done", "gd-dec")},
		"bridge, but a different project":                                          {bridge, decisionResume("other", "captain", "gd-done", "gd-dec")},
		"bridge, but the prior has an empty project and so does the create":        {bridge, decisionResume("", "captain", "gd-noproject", "gd-dec-noproject")},
		"bridge, prior is not an agent bead":                                       {bridge, decisionResume("gardenio", "captain", "gd-task", "gd-dec")},
		"bridge, prior does not exist":                                             {bridge, decisionResume("gardenio", "captain", "gd-nope", "gd-dec")},
		"bridge, trigger lineage is not a prior agent":                             {bridge, decisionResume("gardenio", "captain", "trigger:gd-trig", "gd-dec")},
		"bridge, no spawned_by at all":                                             {bridge, agentFields("gardenio", "captain")},
		"bridge, no resolution evidence named":                                     {bridge, contFields("gardenio", "captain", "gd-done")},
		"bridge, unknown spawn_source":                                             {bridge, bridgeFields("gardenio", "captain", "gd-done", "thread-resume", "decision_bead_id", "gd-dec")},
		"bridge, decision does not exist":                                          {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-nope")},
		"bridge, decision the agent filed itself (no credential stamp)":            {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-self-filed")},
		"bridge, decision bare-closed with nothing chosen":                         {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-bare-close")},
		"bridge, decision answered by a forged *-bridge fleet":                     {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-by-forged-bridge")},
		"bridge, decision answered by an agent":                                    {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-by-agent")},
		"bridge, decision answered by the aging sweep":                             {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-by-aging")},
		"bridge, decision answered by another bridge":                              {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-by-other-bridge")},
		"bridge, decision with no responded_by":                                    {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-by-nobody")},
		"bridge, decision expired":                                                 {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-chosen-expired")},
		"bridge, decision still open":                                              {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-open")},
		"bridge, decision verified for a different agent than the prior":           {bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec-for-cap")},
		"bridge, escalation with a failure result":                                 {bridge, bridgeFields("gardenio", "captain", "gd-done", "escalation-resume", "escalation_bead_id", "gd-esc-failed")},
		"bridge, escalation filed by a different agent":                            {bridge, bridgeFields("gardenio", "captain", "gd-done", "escalation-resume", "escalation_bead_id", "gd-esc-other")},
		"bridge, escalation naming the prior's bead id but not credential-stamped": {bridge, bridgeFields("gardenio", "captain", "gd-done", "escalation-resume", "escalation_bead_id", "gd-esc-unverified")},
		"bridge, escalation keyed on the agent name, not the bead id":              {bridge, bridgeFields("gardenio", "captain", "gd-done", "escalation-resume", "escalation_bead_id", "gd-esc-by-name")},
		"bridge, prior closed while still working (killed)":                        {bridge, decisionResume("gardenio", "captain", "gd-killed-closed", "gd-dec-killed-closed")},
		"bridge, prior failed":                                                     {bridge, decisionResume("gardenio", "captain", "gd-failed", "gd-dec-failed")},
		"bridge, prior expired":                                                    {bridge, decisionResume("gardenio", "captain", "gd-expired", "gd-dec-expired")},
		"bridge, prior killed":                                                     {bridge, decisionResume("gardenio", "captain", "gd-killed", "gd-dec-killed")},
		"bridge, prior retired":                                                    {bridge, decisionResume("gardenio", "captain", "gd-retired", "gd-dec-retired")},
		"another machine principal":                                                {machineCtx("gardenio-grafana-bridge"), decisionResume("gardenio", "captain", "gd-done", "gd-dec")},
		"default-fleet token whose subject is slack-bridge":                        {fleetCtx("slack-bridge"), decisionResume("gardenio", "captain", "gd-done", "gd-dec")},
	} {
		t.Run(name, func(t *testing.T) {
			requireEscalates(t, f.gate(tc.ctx, tc.fields))
		})
	}

	// The control: every bridge case above differs from this one in a single way.
	if err := f.gate(bridge, decisionResume("gardenio", "captain", "gd-done", "gd-dec")); err != nil {
		t.Fatalf("control continuation must pass, got %v", err)
	}
}

// An agent on its own token cannot continue a lane it no longer holds.
func TestSameRoleContinuation_AgentSuccessorRefusedWhenPriorNotResumable(t *testing.T) {
	f := newContFixture(t)
	for name, tc := range map[string]struct {
		status model.Status
		state  string
	}{
		"retired prior":                 {model.StatusOpen, "retired"},
		"killed prior":                  {model.StatusOpen, "killed"},
		"failed prior":                  {model.StatusOpen, "failed"},
		"closed prior (token outlived)": {model.StatusClosed, "done"},
	} {
		t.Run(name, func(t *testing.T) {
			f.agentIn("gd-me", "gardenio", "reviewer", tc.status, tc.state)
			requireEscalates(t, f.gate(agentFleetCtx("gd-me", "reviewer-a"), contFields("gardenio", "reviewer", "successor:gd-me")))
		})
	}
	t.Run("empty project", func(t *testing.T) {
		f.agentIn("gd-me", "", "reviewer", model.StatusOpen, "working")
		requireEscalates(t, f.gate(agentFleetCtx("gd-me", "reviewer-a"), contFields("", "reviewer", "successor:gd-me")))
	})
}

func TestSameRoleContinuation_NarrowingIsAllowed(t *testing.T) {
	f := newContFixture(t)
	f.agentIn("gd-multi", "gardenio", "captain, crew", model.StatusClosed, "done")
	f.answeredDecision("gd-dec", "gd-multi")
	// Dropping a role element is a narrowing, never a grant.
	if err := f.gate(slackBridgeMachineCtx(), decisionResume("gardenio", "crew", "gd-multi", "gd-dec")); err != nil {
		t.Fatalf("narrowing must pass, got %v", err)
	}
	if err := f.gate(slackBridgeMachineCtx(), decisionResume("gardenio", "captain,crew", "gd-multi", "gd-dec")); err != nil {
		t.Fatalf("same role list must pass, got %v", err)
	}
}

// A role update never rides the continuation: re-roling an existing agent bead
// into a privileged role still escalates for the same callers, even when the
// ctx carries a continuation authorization that would pass a create.
func TestSameRoleContinuation_DoesNotAuthorizeRoleUpdate(t *testing.T) {
	f := newContFixture(t)
	f.agentIn("gd-prior", "gardenio", "captain", model.StatusClosed, "done")
	f.answeredDecision("gd-dec", "gd-prior")
	f.agent("gd-crew", "gardenio", "crew", model.StatusOpen)

	// Positive control: this context really carries the marker, and it passes a create.
	create := decisionResume("gardenio", "captain", "gd-prior", "gd-dec")
	ctx := f.srv.withSameRoleContinuationAuth(slackBridgeMachineCtx(), create)
	if _, ok := sameRoleContinuationFromContext(ctx); !ok {
		t.Fatal("test setup: the continuation marker was not set, so this test proves nothing")
	}
	if err := authorizeAgentSpawn(ctx, f.ms, create); err != nil {
		t.Fatalf("test setup: the create must pass with the marker, got %v", err)
	}

	patch := json.RawMessage(`{"role":"captain","spawned_by":"gd-prior"}`)
	requireEscalates(t, authorizeAgentRoleUpdate(ctx, f.ms, f.ms.beads["gd-crew"], patch))
}

// createBead wires the continuation in: the bridge's respawn and an agent's own
// successor are created, a crew's forged spawned_by is refused.
func TestSameRoleContinuation_CreateBead(t *testing.T) {
	f := newContFixture(t)
	f.agentIn("gd-prior", "gardenio", "captain", model.StatusClosed, "done")
	f.answeredDecision("gd-dec", "gd-prior")
	create := func(ctx context.Context, fields json.RawMessage) error {
		_, err := f.srv.createBead(ctx, createBeadInput{
			Title: "next", Type: "agent", Kind: "data",
			Labels: []string{"project:gardenio"}, Fields: fields,
		})
		return err
	}
	if err := create(slackBridgeMachineCtx(), decisionResume("gardenio", "captain", "gd-prior", "gd-dec")); err != nil {
		t.Fatalf("bridge respawn: %v", err)
	}
	f.agent("gd-crew", "gardenio", "crew", model.StatusOpen)
	requireEscalates(t, create(agentFleetCtx("gd-crew", "crew-a"), contFields("gardenio", "captain", "gd-prior")))
}

// The continuation gate trusts the prior bead's project, so a non-human caller
// cannot rewrite it: an agent that could move its own bead into another project
// could continue into that project's privileged lane.
func TestAgentProjectIsFixedAtSpawn(t *testing.T) {
	f := newContFixture(t)
	f.agentIn("gd-me", "gardenio", "crew", model.StatusOpen, "working")
	bead := f.ms.beads["gd-me"]
	move := json.RawMessage(`{"project":"other"}`)

	for name, ctx := range map[string]context.Context{
		"agent on its own token": agentFleetCtx("gd-me", "crew-a"),
		"static token":           withAuthMethod(context.Background(), authMethodStatic),
		"machine principal":      slackBridgeMachineCtx(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := authorizeAgentProjectUpdate(ctx, bead, move); err == nil {
				t.Fatal("changing an agent bead's project must be refused")
			}
			if err := authorizeAgentRoleUpdate(ctx, f.ms, bead, move); err == nil {
				t.Fatal("authorizeAgentRoleUpdate must apply the project gate")
			}
		})
	}
	t.Run("echoing the stored project is not a change", func(t *testing.T) {
		if err := authorizeAgentProjectUpdate(agentFleetCtx("gd-me", "crew-a"), bead, json.RawMessage(`{"project":"gardenio","agent_state":"done"}`)); err != nil {
			t.Fatalf("read-modify-write echo must pass, got %v", err)
		}
	})
	t.Run("a patch without project passes", func(t *testing.T) {
		if err := authorizeAgentProjectUpdate(agentFleetCtx("gd-me", "crew-a"), bead, json.RawMessage(`{"agent_state":"done"}`)); err != nil {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an OIDC session keeps the right", func(t *testing.T) {
		if err := authorizeAgentProjectUpdate(oidcCtx("admin@example.com", []string{RoleAdmin}), bead, move); err != nil {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("field-mutation endpoints refuse the project key", func(t *testing.T) {
		if d := f.srv.denyAgentRoleKeyWrite(agentFleetCtx("gd-me", "crew-a"), "update", []string{"project"}); d == nil {
			t.Fatal("touching project through a field endpoint must be denied")
		}
	})
	t.Run("non-agent beads are untouched", func(t *testing.T) {
		task := &model.Bead{Type: "task", Fields: json.RawMessage(`{"project":"gardenio"}`)}
		if err := authorizeAgentProjectUpdate(agentFleetCtx("gd-me", "crew-a"), task, move); err != nil {
			t.Fatalf("got %v", err)
		}
	})
}

// responded_by is stamped from the authenticated caller, so the gate trusts only
// the values a human session or the Slack bridge itself can produce.
func TestAnsweredByHumanOrSlackBridge(t *testing.T) {
	for in, want := range map[string]bool{
		"alice@example.com":       true,
		"oidc:alice@example.com":  true,
		"slack:U012ABC":           true,
		"slack-bridge":            true,
		slackBridgeMachineSubject: true,
		"":                        false,
		"static":                  false,
		"x-bridge":                false,
		"crew-a":                  false,
		"gitlab-bridge":           false,
		"bridge:slack":            false,
		"system:decision-aging":   false,
		"fleet:agent":             false,
		"@example.com":            false,
		"a b@example.com":         false,
	} {
		if got := answeredByHumanOrSlackBridge(in); got != want {
			t.Errorf("answeredByHumanOrSlackBridge(%q) = %v, want %v", in, got, want)
		}
	}
}
