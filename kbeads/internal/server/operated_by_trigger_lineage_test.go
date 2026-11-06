package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// The tests below pin the machine-branch trigger lineage (decision
// gd-dI7PfsFlvr "lineage", gd-Hc34EENbO9 component 2, trigger-engine): a
// MACHINE principal holding the trigger_lineage capability whose create carries
// fields.trigger_id is attributed to the referenced trigger bead's HUMAN
// author, dereferenced server-side. No header is consulted; any other machine,
// any non-trigger reference, any non-human author and any non-create write
// stays attributed to the machine subject.

func triggerEngineCtx() context.Context {
	return machineCtx("gardenio-trigger-engine", "gardenio-operator", "gardenio-machine-trigger-ops")
}

func TestResolveOperatedByPure_Machine_TriggerLineage_FromOperatedByColumn(t *testing.T) {
	srv := newChainWalkServer(&model.Bead{
		ID: "gd-trig", Type: "trigger", Kind: model.KindData,
		CreatedBy: "oidc:author@example.com", OperatedBy: "oidc:author@example.com",
		Fields: []byte(`{"operated_by":"oidc:author@example.com"}`),
	})
	ctx := WithTriggerLineageSubject(triggerEngineCtx(), "gd-trig")

	value, source := srv.resolveOperatedByPure(ctx)
	if value != "oidc:author@example.com" || source != operatedBySourceTriggerLineage {
		t.Fatalf("(%q,%q); want (oidc:author@example.com,%s)", value, source, operatedBySourceTriggerLineage)
	}
}

// A chain heir created by the engine carries operated_by=machine:… in its
// column but inherits the predecessor's human in fields.operated_by; the
// fields value is the fallback, honoured because the engine itself created it.
func TestResolveOperatedByPure_Machine_TriggerLineage_FallsBackToFieldsOperatedBy(t *testing.T) {
	srv := newChainWalkServer(&model.Bead{
		ID: "gd-heir", Type: "trigger", Kind: model.KindData,
		CreatedBy: "machine:gardenio-trigger-engine", OperatedBy: "machine:gardenio-trigger-engine",
		Fields: []byte(`{"operated_by":"slack:U0OPERATOR1"}`),
	})
	ctx := WithTriggerLineageSubject(triggerEngineCtx(), "gd-heir")

	value, source := srv.resolveOperatedByPure(ctx)
	if value != "slack:U0OPERATOR1" || source != operatedBySourceTriggerLineage {
		t.Fatalf("(%q,%q); want (slack:U0OPERATOR1,%s)", value, source, operatedBySourceTriggerLineage)
	}
}

// A schedule bead is the other autonomous type and qualifies the same way.
func TestResolveOperatedByPure_Machine_TriggerLineage_ScheduleBead(t *testing.T) {
	srv := newChainWalkServer(&model.Bead{
		ID: "gd-sched", Type: "schedule", Kind: model.KindConfig,
		OperatedBy: "oidc:author@example.com",
	})
	ctx := WithTriggerLineageSubject(triggerEngineCtx(), "gd-sched")
	if value, _ := srv.resolveOperatedByPure(ctx); value != "oidc:author@example.com" {
		t.Fatalf("value=%q; want the schedule's human author", value)
	}
}

func TestResolveOperatedByPure_Machine_TriggerLineage_StaysMachineWhen(t *testing.T) {
	const self = "machine:gardenio-trigger-engine"
	cases := []struct {
		name string
		bead *model.Bead
		ctx  func() context.Context
	}{
		{
			name: "no seed on ctx",
			bead: &model.Bead{ID: "gd-trig", Type: "trigger", OperatedBy: "oidc:author@example.com"},
			ctx:  triggerEngineCtx,
		},
		{
			name: "referenced bead is missing",
			bead: &model.Bead{ID: "gd-other", Type: "trigger", OperatedBy: "oidc:author@example.com"},
			ctx:  func() context.Context { return WithTriggerLineageSubject(triggerEngineCtx(), "gd-trig") },
		},
		{
			name: "referenced bead is not a trigger or schedule",
			bead: &model.Bead{ID: "gd-trig", Type: model.TypeTask, OperatedBy: "oidc:author@example.com", Fields: []byte(`{"operated_by":"oidc:author@example.com"}`)},
			ctx:  func() context.Context { return WithTriggerLineageSubject(triggerEngineCtx(), "gd-trig") },
		},
		{
			name: "trigger author is a sentinel, fields non-human too",
			bead: &model.Bead{ID: "gd-trig", Type: "trigger", OperatedBy: operatorLegacyPreAttribution, Fields: []byte(`{"operated_by":"fleet:slack-bridge"}`)},
			ctx:  func() context.Context { return WithTriggerLineageSubject(triggerEngineCtx(), "gd-trig") },
		},
		{
			// fields is client-supplied: a human-created trigger whose column
			// is a sentinel must not lend its fields.operated_by to the engine.
			name: "fields.operated_by on a trigger the engine did not create",
			bead: &model.Bead{ID: "gd-trig", Type: "trigger", CreatedBy: "agent:gardenio-crew-1", OperatedBy: operatorLegacyPreAttribution, Fields: []byte(`{"operated_by":"oidc:victim@example.com"}`)},
			ctx:  func() context.Context { return WithTriggerLineageSubject(triggerEngineCtx(), "gd-trig") },
		},
		{
			name: "trigger author is an agent",
			bead: &model.Bead{ID: "gd-trig", Type: "trigger", OperatedBy: "agent:gardenio-crew-1", Fields: []byte(`{}`)},
			ctx:  func() context.Context { return WithTriggerLineageSubject(triggerEngineCtx(), "gd-trig") },
		},
		{
			name: "X-Operated-By on ctx is still ignored for a machine",
			bead: &model.Bead{ID: "gd-trig", Type: "trigger", OperatedBy: "agent:gardenio-crew-1"},
			ctx: func() context.Context {
				return WithTriggerLineageSubject(WithOperatedByHeader(triggerEngineCtx(), "oidc:spoof@example.com"), "gd-trig")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newChainWalkServer(c.bead)
			value, source := srv.resolveOperatedByPure(c.ctx())
			if value != self || source != operatedBySourceMachine {
				t.Fatalf("(%q,%q); want (%s,%s)", value, source, self, operatedBySourceMachine)
			}
		})
	}
}

// The capability is what admits the lineage: the canary (no roles) and the
// controller (accounts + token-mint) referencing the same human-authored
// trigger stay attributed to themselves.
func TestResolveOperatedByPure_Machine_TriggerLineage_RequiresCapability(t *testing.T) {
	trig := &model.Bead{ID: "gd-trig", Type: "trigger", OperatedBy: "oidc:author@example.com"}
	for _, c := range []struct {
		client string
		roles  []string
	}{
		{"gardenio-authz-canary", nil},
		{"gardenio-controller", []string{"gardenio-machine-accounts", "gardenio-machine-token-mint"}},
		{"gardenio-beads-viewer", []string{"gardenio-machine-plan-reactor"}},
	} {
		srv := newChainWalkServer(trig)
		ctx := WithTriggerLineageSubject(machineCtx(c.client, c.roles...), "gd-trig")
		value, source := srv.resolveOperatedByPure(ctx)
		if value != "machine:"+c.client || source != operatedBySourceMachine {
			t.Fatalf("%s: (%q,%q); want its own machine subject", c.client, value, source)
		}
	}
	if !machineHasCapability(triggerEngineCtx(), capTriggerLineage) {
		t.Fatal("gardenio-machine-trigger-ops must grant trigger_lineage")
	}
	if machineHasCapability(WithMachineRoles(withAuthMethod(context.Background(), authMethodStatic), []string{"gardenio-machine-trigger-ops"}), capTriggerLineage) {
		t.Fatal("a non-machine auth method never holds the capability")
	}
}

// The seed never reaches the static/fleet branches: those keep their own
// (header, agent-bead walk, spawn-chain) resolution exactly as before.
func TestResolveOperatedByPure_TriggerLineageSeed_IgnoredForOtherAuthTypes(t *testing.T) {
	srv := newChainWalkServer(&model.Bead{ID: "gd-trig", Type: "trigger", OperatedBy: "oidc:author@example.com"})
	ctx := WithTriggerLineageSubject(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"}), "gd-trig")
	if value, _ := srv.resolveOperatedByPure(ctx); value == "oidc:author@example.com" {
		t.Fatal("fleet branch must not consume the trigger-lineage seed")
	}
}

func TestTriggerIDFromFields(t *testing.T) {
	cases := map[string]string{
		``:                                  "",
		`null`:                              "",
		`{}`:                                "",
		`{"trigger_id":""}`:                 "",
		`{"trigger_id":"  gd-trig1 "}`:      "gd-trig1",
		`{"trigger_id":42}`:                 "",
		`{"spawned_by":"trigger:gd-trig1"}`: "",
		`not json`:                          "",
		`{"trigger_id":"gd-t","other":"x"}`: "gd-t",
	}
	for in, want := range cases {
		if got := triggerIDFromFields(json.RawMessage(in)); got != want {
			t.Errorf("triggerIDFromFields(%s)=%q want %q", in, got, want)
		}
	}
	if got := triggerLineageSubjectFromContext(WithTriggerLineageSubject(context.Background(), "  ")); got != "" {
		t.Errorf("blank seed must not be stored, got %q", got)
	}
}

// End to end through the real mux: the trigger-engine's spawn of an agent bead
// (fields.trigger_id + the engine's own operated_by field stamp, exactly what
// actionSpawnAgent sends) lands with operated_by = the trigger's human author;
// the same create by the canary lands as the canary. A comment on the trigger
// by the engine stays attributed to the machine (no seed on a non-create).
func TestMachineIdentity_CreateBead_TriggerLineage_HTTP(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-trigger-engine", "gardenio-authz-canary"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	ms.beads["gd-trig"] = &model.Bead{
		ID: "gd-trig", Title: "nightly sweep", Type: "trigger", Kind: model.KindData, Status: model.StatusOpen,
		CreatedBy: "oidc:author@example.com", OperatedBy: "oidc:author@example.com",
		Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{"action":"spawn_agent","source":"schedule"}`),
	}
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	engine := h.saToken(t, "gardenio-trigger-engine", "gardenio-operator", "gardenio-machine-trigger-ops")
	canary := h.saToken(t, "gardenio-authz-canary", "gardenio-operator")

	spawn := map[string]any{
		"title": "gardenio-sweeper-abc", "type": "agent", "kind": "config",
		"fields": map[string]any{"agent": "gardenio-sweeper-abc", "role": "crew", "project": "gardenio", "trigger_id": "gd-trig", "spawned_by": "trigger:gd-trig", "operated_by": "oidc:author@example.com"},
	}
	var got struct {
		ID         string `json:"id"`
		OperatedBy string `json:"operated_by"`
		CreatedBy  string `json:"created_by"`
	}

	rec := machineDo(t, handler, engine, http.MethodPost, "/v1/beads", spawn)
	if rec.Code != http.StatusCreated {
		t.Fatalf("engine spawn: status=%d body=%s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.OperatedBy != "oidc:author@example.com" {
		t.Fatalf("engine spawn operated_by=%q; want the trigger's human author (body=%s)", got.OperatedBy, rec.Body.String())
	}
	if got.CreatedBy != "machine:gardenio-trigger-engine" {
		t.Fatalf("engine spawn created_by=%q; the creator stays the machine", got.CreatedBy)
	}

	// Same create, with a spoofed X-Operated-By too, by a machine without the
	// capability: attributed to itself.
	r := machineDo(t, handler, canary, http.MethodPost, "/v1/beads", spawn)
	if r.Code != http.StatusCreated {
		t.Fatalf("canary spawn: status=%d body=%s", r.Code, r.Body.String())
	}
	_ = json.Unmarshal(r.Body.Bytes(), &got)
	if got.OperatedBy != "machine:gardenio-authz-canary" {
		t.Fatalf("canary spawn operated_by=%q; want machine:gardenio-authz-canary", got.OperatedBy)
	}

	// A task the engine files for the run (createTaskBead) carries the same
	// stamp and the same attribution.
	task := map[string]any{"title": "run: nightly sweep", "type": "task", "fields": map[string]any{"trigger_id": "gd-trig"}}
	rec = machineDo(t, handler, engine, http.MethodPost, "/v1/beads", task)
	if rec.Code != http.StatusCreated {
		t.Fatalf("engine task: status=%d body=%s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.OperatedBy != "oidc:author@example.com" {
		t.Fatalf("engine task operated_by=%q; want the trigger's human author", got.OperatedBy)
	}

	// A create with no trigger reference (a sweep's rollup doc) is the machine.
	doc := map[string]any{"title": "uncovered lanes", "type": "doc", "fields": map[string]any{}}
	rec = machineDo(t, handler, engine, http.MethodPost, "/v1/beads", doc)
	if rec.Code != http.StatusCreated {
		t.Fatalf("engine doc: status=%d body=%s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.OperatedBy != "machine:gardenio-trigger-engine" {
		t.Fatalf("engine doc operated_by=%q; want machine:gardenio-trigger-engine", got.OperatedBy)
	}
}
