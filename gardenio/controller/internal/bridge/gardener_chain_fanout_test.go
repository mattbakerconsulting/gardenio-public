package bridge

import (
	"encoding/json"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// gardener_chain_fanout_test.go — fan-out / join / branch schema coverage for
// C3 (task ka-0000005273). The single-heir shape is covered in
// gardener_chain_test.go; these lock the new array surface form + the join /
// branch heir fields + the object↔array (Un)Marshal round-trips.

// --- Fan-out: chain.to as an array parses to To + Extra ---

func TestParseTriggerDef_Chain_FanOutArray(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:    "gd-pred-fan",
		Title: "Research gardener (fan-out)",
		Fields: map[string]string{
			"chain": `{
				"to": [
					{"phase": "impl-a", "epic": "gd-ANVVMrCTqf"},
					{"phase": "impl-b", "epic": "gd-U0ogvpVjao"},
					{"phase": "impl-c", "epic": "gd-39etI0NI1M"}
				]
			}`,
		},
	}
	def := ParseTriggerDef(bead, "test")
	if def.Chain == nil {
		t.Fatal("Chain is nil, want parsed fan-out ChainSpec")
	}
	heirs := def.Chain.Heirs()
	if len(heirs) != 3 {
		t.Fatalf("Heirs() = %d, want 3", len(heirs))
	}
	if def.Chain.To == nil || def.Chain.To.Phase != "impl-a" {
		t.Errorf("To.Phase = %v, want impl-a (first heir)", def.Chain.To)
	}
	if len(def.Chain.Extra) != 2 {
		t.Fatalf("Extra = %d, want 2", len(def.Chain.Extra))
	}
	if heirs[1].Phase != "impl-b" || heirs[2].Phase != "impl-c" {
		t.Errorf("heir phases = %q,%q, want impl-b,impl-c", heirs[1].Phase, heirs[2].Phase)
	}
	// Normalize fills per-heir defaults on EVERY heir, not just the first.
	for i, h := range heirs {
		if h.Role != "gardener" {
			t.Errorf("heir[%d].Role = %q, want gardener (default)", i, h.Role)
		}
		if len(h.Inherit) == 0 {
			t.Errorf("heir[%d].Inherit empty, want default set", i)
		}
	}
}

// --- Single-heir object round-trips BYTE-STABLE (back-compat) ---

func TestChainSpec_MarshalJSON_SingleHeirStaysObject(t *testing.T) {
	spec := ChainSpec{
		On:   "goal_satisfied",
		Mode: "spawn",
		To:   &ChainTo{Phase: "impl", Epic: "{{same}}"},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// `to` must be a bare object, not an array, for a single heir.
	var probe struct {
		To json.RawMessage `json:"to"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if len(probe.To) == 0 || probe.To[0] != '{' {
		t.Errorf("single-heir `to` = %s, want a bare object", probe.To)
	}
}

func TestChainSpec_MarshalJSON_FanOutIsArray(t *testing.T) {
	spec := ChainSpec{
		To:    &ChainTo{Phase: "impl-a"},
		Extra: []*ChainTo{{Phase: "impl-b"}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe struct {
		To json.RawMessage `json:"to"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if len(probe.To) == 0 || probe.To[0] != '[' {
		t.Errorf("fan-out `to` = %s, want an array", probe.To)
	}
}

// --- Object↔array round-trip through Marshal/Unmarshal is stable ---

func TestChainSpec_RoundTrip(t *testing.T) {
	cases := []ChainSpec{
		{On: "goal_satisfied", Mode: "spawn", To: &ChainTo{Phase: "impl", Role: "gardener", Inherit: []string{"project"}}},
		{On: "goal_satisfied", Mode: "spawn", To: &ChainTo{Phase: "a"}, Extra: []*ChainTo{{Phase: "b"}, {Phase: "c"}}},
	}
	for i, in := range cases {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		var out ChainSpec
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("case %d unmarshal: %v", i, err)
		}
		// On/Mode survive the map-based MarshalJSON.
		if out.On != in.On {
			t.Errorf("case %d: On = %q, want %q", i, out.On, in.On)
		}
		if out.Mode != in.Mode {
			t.Errorf("case %d: Mode = %q, want %q", i, out.Mode, in.Mode)
		}
		gotHeirs := out.Heirs()
		wantHeirs := in.Heirs()
		if len(gotHeirs) != len(wantHeirs) {
			t.Fatalf("case %d: Heirs() = %d, want %d", i, len(gotHeirs), len(wantHeirs))
		}
		for j := range gotHeirs {
			if gotHeirs[j].Phase != wantHeirs[j].Phase {
				t.Errorf("case %d heir %d: Phase = %q, want %q", i, j, gotHeirs[j].Phase, wantHeirs[j].Phase)
			}
		}
	}
}

// --- Empty array / null `to` is a no-op chain (dropped by ParseTriggerDef) ---

func TestParseTriggerDef_Chain_EmptyArrayIsNoChain(t *testing.T) {
	for _, raw := range []string{`{"to": []}`, `{"to": null}`, `{"to": [{}]}`} {
		bead := &beadsapi.BeadDetail{ID: "gd-x", Fields: map[string]string{"chain": raw}}
		def := ParseTriggerDef(bead, "test")
		if def.Chain != nil {
			t.Errorf("chain %s: Chain = %+v, want nil (no meaningful heir)", raw, def.Chain)
		}
	}
}

// --- Join: join_parent flips the heir preflight to ==0 under the umbrella ---

func TestBuildHeirBeadSpec_JoinParent(t *testing.T) {
	trig := TriggerDef{ID: "gd-pred00001", Project: "gardenio"}
	to := &ChainTo{
		Phase:        "ship",
		TerminalGoal: "All impls merged.",
		Epic:         "gd-gar681qAto",
		JoinParent:   "gd-twppcntwuz",
	}
	trig.Chain = &ChainSpec{To: to}
	trig.Chain.Normalize()

	pred := &beadsapi.BeadDetail{ID: "gd-pred00001", Labels: []string{"gardener-epic:gd-predEpic1"}}
	spec, err := buildHeirBeadSpec(trig, pred, to)
	if err != nil {
		t.Fatalf("buildHeirBeadSpec: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(spec.Req.Fields, &fields); err != nil {
		t.Fatalf("fields unmarshal: %v", err)
	}
	condRaw, _ := fields["conditions"].(string)
	var conds []map[string]any
	if err := json.Unmarshal([]byte(condRaw), &conds); err != nil {
		t.Fatalf("conditions unmarshal: %v (%q)", err, condRaw)
	}
	if len(conds) != 1 {
		t.Fatalf("conditions = %d, want 1", len(conds))
	}
	if conds[0]["op"] != "==" {
		t.Errorf("join preflight op = %v, want ==", conds[0]["op"])
	}
	if conds[0]["parent"] != "gd-twppcntwuz" {
		t.Errorf("join preflight parent = %v, want gd-twppcntwuz (join_parent)", conds[0]["parent"])
	}
}

func TestBuildHeirBeadSpec_NoJoin_DefaultPreflight(t *testing.T) {
	trig := TriggerDef{ID: "gd-pred00002", Project: "gardenio"}
	to := &ChainTo{Phase: "impl", Epic: "gd-x4ywxLwUYB"}
	trig.Chain = &ChainSpec{To: to}
	trig.Chain.Normalize()

	spec, err := buildHeirBeadSpec(trig, &beadsapi.BeadDetail{ID: "gd-pred00002"}, to)
	if err != nil {
		t.Fatalf("buildHeirBeadSpec: %v", err)
	}
	var fields map[string]any
	_ = json.Unmarshal(spec.Req.Fields, &fields)
	var conds []map[string]any
	_ = json.Unmarshal([]byte(fields["conditions"].(string)), &conds)
	if conds[0]["op"] != ">" {
		t.Errorf("default preflight op = %v, want >", conds[0]["op"])
	}
	if conds[0]["parent"] != "gd-x4ywxLwUYB" {
		t.Errorf("default preflight parent = %v, want the heir's own epic", conds[0]["parent"])
	}
}

// --- Branch: branch_decision_key authors a decision_resolved trigger ---

func TestBuildHeirBeadSpec_BranchDecision(t *testing.T) {
	trig := TriggerDef{ID: "gd-pred00003", Project: "gardenio"}
	to := &ChainTo{
		Phase:             "branch-a",
		TerminalGoal:      "Branch A shipped.",
		Epic:              "gd-branchEpicA",
		BranchDecisionKey: "gd-decision001",
	}
	trig.Chain = &ChainSpec{To: to}
	trig.Chain.Normalize()

	spec, err := buildHeirBeadSpec(trig, &beadsapi.BeadDetail{ID: "gd-pred00003"}, to)
	if err != nil {
		t.Fatalf("buildHeirBeadSpec: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(spec.Req.Fields, &fields); err != nil {
		t.Fatalf("fields unmarshal: %v", err)
	}
	trigRaw, ok := fields["triggers"].(string)
	if !ok || trigRaw == "" {
		t.Fatalf("branch heir missing triggers[]; fields=%v", fields)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(trigRaw), &entries); err != nil {
		t.Fatalf("triggers unmarshal: %v", err)
	}
	if len(entries) != 1 || entries[0]["kind"] != "decision_resolved" {
		t.Fatalf("branch trigger = %v, want one decision_resolved entry", entries)
	}
	if entries[0]["decision_key"] != "gd-decision001" {
		t.Errorf("decision_key = %v, want gd-decision001", entries[0]["decision_key"])
	}
	// A triggers[] array must flip the label to trigger:multi.
	foundMulti := false
	for _, l := range spec.Labels {
		if l == "trigger:multi" {
			foundMulti = true
		}
	}
	if !foundMulti {
		t.Errorf("branch heir labels = %v, want trigger:multi", spec.Labels)
	}
}
