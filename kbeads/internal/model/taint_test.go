package model

import (
	"strings"
	"testing"
)

func taintHasFieldError(err error, field string) bool {
	ve, ok := err.(*ValidationError)
	if !ok || ve == nil {
		return false
	}
	for _, fe := range ve.Errors {
		if fe.Field == field {
			return true
		}
	}
	return false
}

func TestValidateScrubPolicy_ValidPolicy(t *testing.T) {
	maxBeads := 50
	p := &ScrubPolicy{
		Default: ScrubModeHumanFourEyes,
		Rules: []ScrubRule{
			{When: []byte(`{"older_than_days":30}`), Mode: ScrubModeAuto, MaxBeads: &maxBeads},
			{When: []byte(`{"kind":"detaint"}`), Mode: ScrubModeHuman, Approvers: []string{"team:x"}},
		},
	}
	if err := ValidateScrubPolicy(p, nil); err != nil {
		t.Fatalf("expected valid policy, got error: %v", err)
	}
}

func TestValidateScrubPolicy_DefaultRequired(t *testing.T) {
	err := ValidateScrubPolicy(&ScrubPolicy{}, nil)
	if !taintHasFieldError(err, "scrub_policy.default") {
		t.Fatalf("expected scrub_policy.default error, got %v", err)
	}
}

func TestValidateScrubPolicy_DefaultMayNotBeAuto(t *testing.T) {
	err := ValidateScrubPolicy(&ScrubPolicy{Default: ScrubModeAuto}, nil)
	if !taintHasFieldError(err, "scrub_policy.default") {
		t.Fatalf("expected scrub_policy.default error, got %v", err)
	}
}

func TestValidateScrubPolicy_UnknownDefaultMode(t *testing.T) {
	err := ValidateScrubPolicy(&ScrubPolicy{Default: "yolo"}, nil)
	if !taintHasFieldError(err, "scrub_policy.default") {
		t.Fatalf("expected scrub_policy.default error, got %v", err)
	}
}

func TestValidateScrubPolicy_AutoRuleRequiresMaxBeads(t *testing.T) {
	p := &ScrubPolicy{
		Default: ScrubModeHuman,
		Rules:   []ScrubRule{{When: []byte(`{"kind":"purge"}`), Mode: ScrubModeAuto}},
	}
	err := ValidateScrubPolicy(p, nil)
	if !taintHasFieldError(err, "scrub_policy.rules[0].max_beads") {
		t.Fatalf("expected rules[0].max_beads error, got %v", err)
	}
}

func TestValidateScrubPolicy_AutoRuleRejectsNonPositiveMaxBeads(t *testing.T) {
	zero := 0
	p := &ScrubPolicy{
		Default: ScrubModeHuman,
		Rules:   []ScrubRule{{When: []byte(`{"kind":"purge"}`), Mode: ScrubModeAuto, MaxBeads: &zero}},
	}
	err := ValidateScrubPolicy(p, nil)
	if !taintHasFieldError(err, "scrub_policy.rules[0].max_beads") {
		t.Fatalf("expected rules[0].max_beads error, got %v", err)
	}
}

func TestValidateScrubPolicy_UnknownRuleMode(t *testing.T) {
	p := &ScrubPolicy{
		Default: ScrubModeHuman,
		Rules:   []ScrubRule{{When: []byte(`{"kind":"purge"}`), Mode: "yolo"}},
	}
	err := ValidateScrubPolicy(p, nil)
	if !taintHasFieldError(err, "scrub_policy.rules[0].mode") {
		t.Fatalf("expected rules[0].mode error, got %v", err)
	}
}

func TestValidateScrubPolicy_RuleRequiresWhen(t *testing.T) {
	p := &ScrubPolicy{
		Default: ScrubModeHuman,
		Rules:   []ScrubRule{{Mode: ScrubModeHuman}},
	}
	err := ValidateScrubPolicy(p, nil)
	if !taintHasFieldError(err, "scrub_policy.rules[0].when") {
		t.Fatalf("expected rules[0].when error, got %v", err)
	}
}

func TestDefaultScrubPolicy(t *testing.T) {
	p := DefaultScrubPolicy("team-acme")
	if p.Default != ScrubModeHumanFourEyes {
		t.Fatalf("expected human-four-eyes default, got %q", p.Default)
	}
	if len(p.Approvers) != 1 || p.Approvers[0] != "team:team-acme" {
		t.Fatalf("expected owner team as approver, got %v", p.Approvers)
	}
	if err := ValidateScrubPolicy(p, nil); err != nil {
		t.Fatalf("default policy must itself be valid: %v", err)
	}
}

func TestIsValidPreset(t *testing.T) {
	for _, name := range []PresetName{PresetProduction, PresetInternal, PresetScratch} {
		if !IsValidPreset(name) {
			t.Errorf("expected %q to be a valid preset", name)
		}
	}
	if IsValidPreset("yolo") {
		t.Fatalf("expected unknown preset to be invalid")
	}
}

// ruleForKind finds the first rule in p whose When predicate mentions the
// given fragment verbatim, matching how PresetScrubPolicy encodes rules.
func ruleForKind(p *ScrubPolicy, fragment string) *ScrubRule {
	for i := range p.Rules {
		if strings.Contains(string(p.Rules[i].When), fragment) {
			return &p.Rules[i]
		}
	}
	return nil
}

func TestPresetScrubPolicy_Production(t *testing.T) {
	p := PresetScrubPolicy(PresetProduction, "acme-prod-owners")
	if err := ValidateScrubPolicy(p, nil); err != nil {
		t.Fatalf("production preset must be valid: %v", err)
	}
	if p.Default != ScrubModeHumanFourEyes {
		t.Fatalf("expected human-four-eyes default, got %q", p.Default)
	}
	for _, r := range p.Rules {
		if r.Mode == ScrubModeAuto {
			t.Fatalf("production preset must have no auto rules, got %+v", r)
		}
	}
	detaint := ruleForKind(p, `"detaint"`)
	if detaint == nil || detaint.Mode != ScrubModeHuman {
		t.Fatalf("expected human detaint rule, got %+v", detaint)
	}
	purge := ruleForKind(p, `"purge"`)
	if purge == nil || purge.Mode != ScrubModeHumanFourEyes {
		t.Fatalf("expected human-four-eyes purge rule, got %+v", purge)
	}
}

func TestPresetScrubPolicy_Internal(t *testing.T) {
	p := PresetScrubPolicy(PresetInternal, "acme-internal-owners")
	if err := ValidateScrubPolicy(p, nil); err != nil {
		t.Fatalf("internal preset must be valid: %v", err)
	}

	autoDetaint := ruleForKind(p, `"requester_is_chain_root":true`)
	if autoDetaint == nil || autoDetaint.Mode != ScrubModeAuto {
		t.Fatalf("expected auto attested-chain-root detaint rule, got %+v", autoDetaint)
	}
	if autoDetaint.MaxBeads == nil || *autoDetaint.MaxBeads != 50 {
		t.Fatalf("expected detaint cap 50, got %v", autoDetaint.MaxBeads)
	}
	if autoDetaint.MaxPerDay == nil || *autoDetaint.MaxPerDay != 200 {
		t.Fatalf("expected detaint cap 200/day, got %v", autoDetaint.MaxPerDay)
	}

	autoPurge := ruleForKind(p, `"older_than_days":180`)
	if autoPurge == nil || autoPurge.Mode != ScrubModeAuto {
		t.Fatalf("expected auto purge-after-180-days rule, got %+v", autoPurge)
	}
	if autoPurge.MaxBeads == nil || *autoPurge.MaxBeads <= 0 {
		t.Fatalf("expected positive max_beads cap on purge-180 rule, got %v", autoPurge.MaxBeads)
	}

	// First-match ordering: the specific auto rules must precede the
	// broader human catch-alls for the same kind.
	humanDetaintIdx, autoDetaintIdx := -1, -1
	for i, r := range p.Rules {
		if r.Mode == ScrubModeAuto && strings.Contains(string(r.When), `"requester_is_chain_root":true`) {
			autoDetaintIdx = i
		}
		if r.Mode == ScrubModeHuman && strings.Contains(string(r.When), `"detaint"`) && !strings.Contains(string(r.When), "requester_is_chain_root") {
			humanDetaintIdx = i
		}
	}
	if autoDetaintIdx == -1 || humanDetaintIdx == -1 || autoDetaintIdx >= humanDetaintIdx {
		t.Fatalf("expected auto detaint rule (idx %d) before human detaint catch-all (idx %d)", autoDetaintIdx, humanDetaintIdx)
	}
}

func TestPresetScrubPolicy_Scratch(t *testing.T) {
	p := PresetScrubPolicy(PresetScratch, "acme-sandbox")
	if err := ValidateScrubPolicy(p, nil); err != nil {
		t.Fatalf("scratch preset must be valid: %v", err)
	}

	detaint := ruleForKind(p, `"detaint"`)
	if detaint == nil || detaint.Mode != ScrubModeAuto {
		t.Fatalf("expected auto attested detaint rule, got %+v", detaint)
	}
	if detaint.MaxBeads == nil || *detaint.MaxBeads <= 0 {
		t.Fatalf("expected positive max_beads on scratch detaint rule, got %v", detaint.MaxBeads)
	}

	purge := ruleForKind(p, `"older_than_days":7`)
	if purge == nil || purge.Mode != ScrubModeAuto {
		t.Fatalf("expected auto purge-after-7-days rule, got %+v", purge)
	}
	if purge.MaxBeads == nil || *purge.MaxBeads <= 0 {
		t.Fatalf("expected positive max_beads on scratch purge rule, got %v", purge.MaxBeads)
	}
}

func TestPresetScrubPolicy_UnknownFallsBackToDefault(t *testing.T) {
	got := PresetScrubPolicy("bogus", "acme-team")
	want := DefaultScrubPolicy("acme-team")
	if got.Default != want.Default || len(got.Rules) != 0 {
		t.Fatalf("expected fallback to DefaultScrubPolicy, got %+v", got)
	}
}

// --- effects (docs/adr/taints.md §15.2/§15.4, gd-9YQiNJVW6u) --------------

func TestIsValidEffect(t *testing.T) {
	for _, e := range []string{EffectGate, EffectRestrict, EffectEvict, EffectSink} {
		if !IsValidEffect(e) {
			t.Errorf("expected %q to be a valid effect", e)
		}
	}
	if IsValidEffect("yolo") {
		t.Fatalf("expected unknown effect to be invalid")
	}
}

func TestValidateEffects_UnknownRejected(t *testing.T) {
	err := ValidateEffects([]string{"yolo"})
	if !taintHasFieldError(err, "effects") {
		t.Fatalf("expected effects error, got %v", err)
	}
}

// TestValidateEffects_EmptySetLegal pins §15.2: the empty effects set is a
// legal trace-only dye, not an error.
func TestValidateEffects_EmptySetLegal(t *testing.T) {
	if err := ValidateEffects(nil); err != nil {
		t.Fatalf("expected empty effects to be valid, got %v", err)
	}
	if err := ValidateEffects([]string{}); err != nil {
		t.Fatalf("expected empty effects to be valid, got %v", err)
	}
}

func TestValidateEffects_KnownValuesAccepted(t *testing.T) {
	if err := ValidateEffects([]string{EffectGate, EffectRestrict, EffectEvict, EffectSink}); err != nil {
		t.Fatalf("expected all known effects to be valid, got %v", err)
	}
}

func TestDefaultEffects(t *testing.T) {
	got := DefaultEffects()
	if len(got) != 1 || got[0] != EffectGate {
		t.Fatalf("expected default effects {gate}, got %v", got)
	}
}

func TestHasEffect(t *testing.T) {
	if !HasEffect([]string{EffectGate, EffectSink}, EffectGate) {
		t.Fatalf("expected HasEffect to find gate")
	}
	if HasEffect([]string{EffectSink}, EffectGate) {
		t.Fatalf("expected HasEffect to not find gate")
	}
	if HasEffect(nil, EffectGate) {
		t.Fatalf("expected HasEffect on nil slice to be false")
	}
}

// TestValidateScrubPolicy_LiftRefusedOnGateEffect pins §15.4: detaint_mode
// "lift" is refused when the taint's effects include "gate".
func TestValidateScrubPolicy_LiftRefusedOnGateEffect(t *testing.T) {
	p := &ScrubPolicy{Default: ScrubModeHuman, DetaintMode: DetaintModeLift}
	err := ValidateScrubPolicy(p, []string{EffectGate})
	if !taintHasFieldError(err, "scrub_policy.detaint_mode") {
		t.Fatalf("expected scrub_policy.detaint_mode error, got %v", err)
	}
}

// TestValidateScrubPolicy_LiftAllowedWithoutGateEffect pins the converse:
// lift is legal for a taint whose effects exclude "gate".
func TestValidateScrubPolicy_LiftAllowedWithoutGateEffect(t *testing.T) {
	p := &ScrubPolicy{Default: ScrubModeHuman, DetaintMode: DetaintModeLift}
	if err := ValidateScrubPolicy(p, []string{EffectRestrict}); err != nil {
		t.Fatalf("expected lift without gate effect to be valid, got %v", err)
	}
	if err := ValidateScrubPolicy(p, nil); err != nil {
		t.Fatalf("expected lift with empty effects to be valid, got %v", err)
	}
}

func TestValidateScrubPolicy_UnknownDetaintMode(t *testing.T) {
	p := &ScrubPolicy{Default: ScrubModeHuman, DetaintMode: "yolo"}
	err := ValidateScrubPolicy(p, nil)
	if !taintHasFieldError(err, "scrub_policy.detaint_mode") {
		t.Fatalf("expected scrub_policy.detaint_mode error, got %v", err)
	}
}

func TestIsValidDetaintMode(t *testing.T) {
	for _, m := range []DetaintMode{DetaintModeFork, DetaintModeStrip, DetaintModeLift} {
		if !IsValidDetaintMode(m) {
			t.Errorf("expected %q to be a valid detaint mode", m)
		}
	}
	if IsValidDetaintMode("yolo") {
		t.Fatalf("expected unknown detaint mode to be invalid")
	}
}
