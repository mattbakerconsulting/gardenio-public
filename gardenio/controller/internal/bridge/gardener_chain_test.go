package bridge

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// --- Schema parse (ChainSpec via ParseTriggerDef) ---

func TestParseTriggerDef_Chain(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:    "gd-pred-1",
		Title: "Research gardener",
		Fields: map[string]string{
			"chain": `{
				"to": {
					"phase": "impl",
					"terminal_goal": "All children of {{epic}} merged & green.",
					"prompt_advice": "gd-WlCPu4O7mX",
					"epic": "{{same}}"
				}
			}`,
		},
	}
	def := ParseTriggerDef(bead, "test")
	if def.Chain == nil {
		t.Fatal("Chain is nil, want parsed ChainSpec")
	}
	if def.Chain.To == nil {
		t.Fatal("Chain.To is nil")
	}
	if def.Chain.To.Phase != "impl" {
		t.Errorf("Phase = %q, want impl", def.Chain.To.Phase)
	}
	if def.Chain.To.PromptAdvice != "gd-WlCPu4O7mX" {
		t.Errorf("PromptAdvice = %q", def.Chain.To.PromptAdvice)
	}
	// Defaults filled by Normalize.
	if def.Chain.On != "goal_satisfied" {
		t.Errorf("On = %q, want goal_satisfied (default)", def.Chain.On)
	}
	if def.Chain.Mode != "spawn" {
		t.Errorf("Mode = %q, want spawn (default)", def.Chain.Mode)
	}
	if def.Chain.To.Role != "gardener" {
		t.Errorf("To.Role = %q, want gardener (default)", def.Chain.To.Role)
	}
	if len(def.Chain.To.Inherit) == 0 {
		t.Error("To.Inherit should default to the design inherit set")
	}
}

func TestParseTriggerDef_Chain_EmptyAndInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"no chain field", ""},
		{"empty object", `{}`},
		{"empty to", `{"to":{}}`},
		{"invalid json", `{"to":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]string{}
			if tc.raw != "" {
				fields["chain"] = tc.raw
			}
			def := ParseTriggerDef(&beadsapi.BeadDetail{ID: "gd-x", Fields: fields}, "test")
			if def.Chain != nil {
				t.Errorf("Chain = %+v, want nil for %q", def.Chain, tc.raw)
			}
		})
	}
}

func TestChainSpec_Normalize_ArmMode(t *testing.T) {
	c := &ChainSpec{To: &ChainTo{Phase: "verify"}, Mode: "ARM"}
	c.Normalize()
	if c.Mode != "arm" {
		t.Errorf("Mode = %q, want arm", c.Mode)
	}
}

// --- Cycle / depth guard ---

// mapResolver is an in-memory chainResolver: id → (link, predecessorID).
type mapResolver struct {
	links map[string]ChainLink
	preds map[string]string
	err   error
}

func (m mapResolver) ResolveChainLink(_ context.Context, id string) (ChainLink, string, error) {
	if m.err != nil {
		return ChainLink{}, "", m.err
	}
	l, ok := m.links[id]
	if !ok {
		l = ChainLink{ID: id}
	}
	return l, m.preds[id], nil
}

func TestChainHeirGuard_OK(t *testing.T) {
	// A (root) ← B (succeeds A). New heir C succeeds B — fine.
	r := mapResolver{
		links: map[string]ChainLink{
			"gd-B": {ID: "gd-B", PromptAdvice: "gd-implP"},
			"gd-A": {ID: "gd-A", PromptAdvice: "gd-researchP"},
		},
		preds: map[string]string{"gd-B": "gd-A"},
	}
	n, err := ChainHeirGuard(context.Background(), r, "gd-B", &ChainTo{PromptAdvice: "gd-verifyP"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if n != 2 {
		t.Errorf("chainLen = %d, want 2 (B + A)", n)
	}
}

func TestChainHeirGuard_Cycle(t *testing.T) {
	// A ← B. Heir reuses A's identity (A→B→A) — refuse.
	r := mapResolver{
		links: map[string]ChainLink{
			"gd-B": {ID: "gd-B", PromptAdvice: "gd-implP"},
			"gd-A": {ID: "gd-A", PromptAdvice: "gd-researchP"},
		},
		preds: map[string]string{"gd-B": "gd-A"},
	}
	_, err := ChainHeirGuard(context.Background(), r, "gd-B", &ChainTo{PromptAdvice: "gd-researchP"})
	if !errors.Is(err, errChainCycle) {
		t.Fatalf("err = %v, want errChainCycle", err)
	}
}

func TestChainHeirGuard_TooDeep(t *testing.T) {
	// Build a chain of maxChainDepth existing links; one more must be refused.
	links := map[string]ChainLink{}
	preds := map[string]string{}
	var head string
	for i := range maxChainDepth {
		id := "gd-link" + string(rune('A'+i))
		links[id] = ChainLink{ID: id, PromptAdvice: "p" + id}
		if head != "" {
			preds[id] = head
		}
		head = id
	}
	r := mapResolver{links: links, preds: preds}
	_, err := ChainHeirGuard(context.Background(), r, head, &ChainTo{PromptAdvice: "gd-newHeir"})
	if !errors.Is(err, errChainTooDeep) {
		t.Fatalf("err = %v, want errChainTooDeep", err)
	}
}

// TestChainHeirGuard_ExistingLoop guards ka-0000004188: a stored succeeds:
// chain that already loops refuses any new heir with errChainCycle.
func TestChainHeirGuard_ExistingLoop(t *testing.T) {
	// Stored chain already loops: A ← B ← A.
	r := mapResolver{
		links: map[string]ChainLink{
			"gd-A": {ID: "gd-A", PromptAdvice: "pa"},
			"gd-B": {ID: "gd-B", PromptAdvice: "pb"},
		},
		preds: map[string]string{"gd-A": "gd-B", "gd-B": "gd-A"},
	}
	_, err := ChainHeirGuard(context.Background(), r, "gd-A", &ChainTo{PromptAdvice: "gd-new"})
	if !errors.Is(err, errChainCycle) {
		t.Fatalf("err = %v, want errChainCycle", err)
	}
}

// TestChainHeirGuard_NilHeir guards ka-0000004188: with no heir proposed the
// guard allows and walks nothing.
func TestChainHeirGuard_NilHeir(t *testing.T) {
	n, err := ChainHeirGuard(context.Background(), mapResolver{}, "gd-A", nil)
	if err != nil || n != 0 {
		t.Errorf("got (%d, %v), want (0, nil)", n, err)
	}
}

func TestChainIdentityKey(t *testing.T) {
	if k := chainIdentityKey("gd-p", "impl", "gd-epic"); k != "advice:gd-p" {
		t.Errorf("prompt advice should win: %q", k)
	}
	if k := chainIdentityKey("", "impl", "gd-epic"); k != "phase:impl|epic:gd-epic" {
		t.Errorf("fallback key = %q", k)
	}
	if k := chainIdentityKey("", "", ""); k != "" {
		t.Errorf("empty identity should be empty key, got %q", k)
	}
}

// --- daemonChainResolver ---

type fakeFetcher struct {
	beads    map[string]*beadsapi.BeadDetail
	mentions map[string][]beadsapi.BeadReferenceSummary
}

func (f fakeFetcher) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	return f.beads[id], nil
}
func (f fakeFetcher) GetMentions(_ context.Context, id string, _ int) ([]beadsapi.BeadReferenceSummary, error) {
	return f.mentions[id], nil
}

func strptr(s string) *string { return &s }

func TestDaemonChainResolver_ResolveSucceeds(t *testing.T) {
	f := fakeFetcher{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-B": {ID: "gd-B", Fields: map[string]string{
				"chain": `{"to":{"phase":"impl","prompt_advice":"gd-implP"}}`,
			}},
		},
		mentions: map[string][]beadsapi.BeadReferenceSummary{
			"gd-B": {
				{ID: "gd-A", RelationshipType: strptr("succeeds")},
				{ID: "gd-other", RelationshipType: strptr("related")},
				{ID: "gd-untyped", RelationshipType: nil},
			},
		},
	}
	r := daemonChainResolver{f: f}
	link, pred, err := r.ResolveChainLink(context.Background(), "gd-B")
	if err != nil {
		t.Fatal(err)
	}
	if pred != "gd-A" {
		t.Errorf("pred = %q, want gd-A", pred)
	}
	if link.PromptAdvice != "gd-implP" {
		t.Errorf("PromptAdvice = %q", link.PromptAdvice)
	}
}

// --- Nudge composer ---
