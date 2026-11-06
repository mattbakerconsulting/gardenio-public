package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// ScrubMode is the approval mode a scrub_policy rule (or its default)
// resolves to, per docs/adr/taints.md §3/§6.3.
type ScrubMode string

const (
	ScrubModeAuto          ScrubMode = "auto"
	ScrubModeHuman         ScrubMode = "human"
	ScrubModeHumanFourEyes ScrubMode = "human-four-eyes"
)

// IsValidScrubMode reports whether m is one of the three modes the ADR
// defines. An unknown mode is rejected at SetTaint time (ADR §3).
func IsValidScrubMode(m ScrubMode) bool {
	switch m {
	case ScrubModeAuto, ScrubModeHuman, ScrubModeHumanFourEyes:
		return true
	}
	return false
}

// Effect is one behavior a taint's effects set may include, per
// docs/adr/taints.md §15.2. Only EffectGate is enforced today; the other
// three are reserved (schema-only, no behavior) until a later task builds
// them.
const (
	EffectGate     = "gate"     // implemented: gates read/write/list per §15.2
	EffectRestrict = "restrict" // reserved
	EffectEvict    = "evict"    // reserved
	EffectSink     = "sink"     // reserved
)

// IsValidEffect reports whether e is one of the four effects §15.2 defines.
func IsValidEffect(e string) bool {
	switch e {
	case EffectGate, EffectRestrict, EffectEvict, EffectSink:
		return true
	}
	return false
}

// DefaultEffects is the effects set a taint gets when none is specified —
// {gate}, matching taints.effects' column default and today's only
// behavior (docs/adr/taints.md §15.2).
func DefaultEffects() []string {
	return []string{EffectGate}
}

// HasEffect reports whether effects includes e.
func HasEffect(effects []string, e string) bool {
	for _, x := range effects {
		if x == e {
			return true
		}
	}
	return false
}

// ValidateEffects rejects any value outside the four §15.2 defines. The
// empty set is legal — a trace-only dye that propagates but gates nothing
// (§15.2).
func ValidateEffects(effects []string) error {
	var ve ValidationError
	for _, e := range effects {
		if !IsValidEffect(e) {
			ve.Errors = append(ve.Errors, FieldError{Field: "effects", Message: fmt.Sprintf("unknown effect %q", e)})
		}
	}
	if ve.HasErrors() {
		return &ve
	}
	return nil
}

// DetaintMode is scrub_policy.detaint_mode (docs/adr/taints.md §6.1/§15.4):
// how a detaint scrub removes a taint from a bead.
type DetaintMode string

const (
	DetaintModeFork  DetaintMode = "fork"  // default: content forks into an untainted copy
	DetaintModeStrip DetaintMode = "strip" // content is stripped in place
	DetaintModeLift  DetaintMode = "lift"  // the bead_taints row is removed in place, no content cascade
)

// IsValidDetaintMode reports whether m is one of the three modes §6.1/§15.4
// define. The zero value ("") is not itself valid — callers that treat an
// unset DetaintMode as "not specified" should check for "" before calling
// this.
func IsValidDetaintMode(m DetaintMode) bool {
	switch m {
	case DetaintModeFork, DetaintModeStrip, DetaintModeLift:
		return true
	}
	return false
}

// ScrubRule is one first-match rule in a taint's scrub_policy.rules
// (docs/adr/taints.md §3/§6.2).
type ScrubRule struct {
	When      json.RawMessage `json:"when"`
	Mode      ScrubMode       `json:"mode"`
	MaxBeads  *int            `json:"max_beads,omitempty"`
	MaxPerDay *int            `json:"max_per_day,omitempty"`
	Approvers []string        `json:"approvers,omitempty"`
}

// ScrubPolicy is the validated shape of taints.scrub_policy (ADR §3, §6.2).
// Rules are evaluated first-match; a bead matching no rule falls to
// Default, which per §3 "may not be auto".
type ScrubPolicy struct {
	Rules   []ScrubRule `json:"rules,omitempty"`
	Default ScrubMode   `json:"default"`
	// Approvers backs Default when it requires approval (human /
	// human-four-eyes) and no rule matched, the same role a rule's own
	// Approvers plays. ADR §10.1: a taint created with no policy gets the
	// owner team here.
	Approvers []string `json:"approvers,omitempty"`
	// DetaintMode selects how a detaint scrub removes the taint (§6.1/§15.4).
	// Empty means the default, "fork".
	DetaintMode DetaintMode `json:"detaint_mode,omitempty"`
}

// ValidateScrubPolicy checks p against docs/adr/taints.md §3, §10.5, and
// §15.4:
//   - Default is required and may never be "auto".
//   - every rule's When predicate is required and Mode must be one of the
//     three known values.
//   - an "auto" rule must declare a positive MaxBeads cap (§10.5).
//   - DetaintMode, if set, must be one of the three known modes.
//   - detaint_mode "lift" is refused when effects includes "gate" (§15.4):
//     lifting removes the bead_taints row with no content cascade, which is
//     unsafe for a taint that is still gating reads/writes.
func ValidateScrubPolicy(p *ScrubPolicy, effects []string) error {
	var ve ValidationError

	switch {
	case p.Default == "":
		ve.Errors = append(ve.Errors, FieldError{Field: "scrub_policy.default", Message: "is required"})
	case p.Default == ScrubModeAuto:
		ve.Errors = append(ve.Errors, FieldError{Field: "scrub_policy.default", Message: `may not be "auto"`})
	case !IsValidScrubMode(p.Default):
		ve.Errors = append(ve.Errors, FieldError{Field: "scrub_policy.default", Message: fmt.Sprintf("unknown mode %q", p.Default)})
	}

	for i, r := range p.Rules {
		field := fmt.Sprintf("scrub_policy.rules[%d]", i)
		if len(r.When) == 0 {
			ve.Errors = append(ve.Errors, FieldError{Field: field + ".when", Message: "is required"})
		}
		if !IsValidScrubMode(r.Mode) {
			ve.Errors = append(ve.Errors, FieldError{Field: field + ".mode", Message: fmt.Sprintf("unknown mode %q", r.Mode)})
			continue
		}
		if r.Mode == ScrubModeAuto && (r.MaxBeads == nil || *r.MaxBeads <= 0) {
			ve.Errors = append(ve.Errors, FieldError{Field: field + ".max_beads", Message: `is required for an "auto" rule`})
		}
	}

	if p.DetaintMode != "" {
		if !IsValidDetaintMode(p.DetaintMode) {
			ve.Errors = append(ve.Errors, FieldError{Field: "scrub_policy.detaint_mode", Message: fmt.Sprintf("unknown mode %q", p.DetaintMode)})
		} else if p.DetaintMode == DetaintModeLift && HasEffect(effects, EffectGate) {
			ve.Errors = append(ve.Errors, FieldError{Field: "scrub_policy.detaint_mode", Message: `"lift" is refused for a taint whose effects include "gate"`})
		}
	}

	if ve.HasErrors() {
		return &ve
	}
	return nil
}

// DefaultScrubPolicy returns the human-four-eyes fallback docs/adr/taints.md
// §10.1 specifies for a taint created with no explicit scrub_policy, with
// the owner team as the sole approver.
func DefaultScrubPolicy(ownerTeam string) *ScrubPolicy {
	return &ScrubPolicy{
		Default:   ScrubModeHumanFourEyes,
		Approvers: []string{"team:" + ownerTeam},
	}
}

// PresetName is one of the named starting-point policies `kd taint create
// --preset` can resolve, per docs/adr/taints.md §10.2. A preset writes an
// ordinary scrub_policy that the owner can freely edit afterward; nothing
// tracks that a taint was created "from a preset".
type PresetName string

const (
	PresetProduction PresetName = "production"
	PresetInternal   PresetName = "internal"
	PresetScratch    PresetName = "scratch"
)

// presetUnspecifiedCapMaxBeads bounds the auto rules docs/adr/taints.md
// §10.2 describes but does not assign an explicit numeric cap to: internal's
// purge-after-180-days rule, and scratch's two auto rules. §10.5 requires
// every "auto" rule to declare a positive max_beads; where the ADR table
// gives no number, this conservative default applies. It is an ordinary
// scrub_policy value the owner can edit like any other — not a special mode.
const presetUnspecifiedCapMaxBeads = 50

// IsValidPreset reports whether name is one of the three presets
// docs/adr/taints.md §10.2 defines.
func IsValidPreset(name PresetName) bool {
	switch name {
	case PresetProduction, PresetInternal, PresetScratch:
		return true
	}
	return false
}

// PresetScrubPolicy resolves a named preset to the scrub_policy
// docs/adr/taints.md §10.2 describes for ownerTeam. Rules are ordered
// most-specific-auto-rule first so a later, broader human/human-four-eyes
// catch-all never shadows them (rules are evaluated first-match, §3).
func PresetScrubPolicy(name PresetName, ownerTeam string) *ScrubPolicy {
	owner := "team:" + ownerTeam

	switch name {
	case PresetProduction:
		// Detaint: human, attested. Purge: human-four-eyes. No auto rules —
		// customer/production data is never scrubbed without a human.
		return &ScrubPolicy{
			Rules: []ScrubRule{
				{
					When:      json.RawMessage(`{"kind":"detaint","attested":true}`),
					Mode:      ScrubModeHuman,
					Approvers: []string{owner},
				},
				{
					When:      json.RawMessage(`{"kind":"purge"}`),
					Mode:      ScrubModeHumanFourEyes,
					Approvers: []string{owner},
				},
			},
			Default:   ScrubModeHumanFourEyes,
			Approvers: []string{owner},
		}

	case PresetInternal:
		// Detaint: auto when attested and the requester is the spawn-chain
		// root, cap 50/200 per day (ADR-stated); else human. Purge: human,
		// except auto after 180 days. Internal-only data that must not
		// leave the team.
		autoMaxBeads := 50
		autoMaxPerDay := 200
		purgeAutoMaxBeads := presetUnspecifiedCapMaxBeads
		return &ScrubPolicy{
			Rules: []ScrubRule{
				{
					When:     json.RawMessage(`{"kind":"purge","older_than_days":180}`),
					Mode:     ScrubModeAuto,
					MaxBeads: &purgeAutoMaxBeads,
				},
				{
					When:      json.RawMessage(`{"kind":"detaint","requester_is_chain_root":true,"attested":true}`),
					Mode:      ScrubModeAuto,
					MaxBeads:  &autoMaxBeads,
					MaxPerDay: &autoMaxPerDay,
				},
				{
					When:      json.RawMessage(`{"kind":"detaint"}`),
					Mode:      ScrubModeHuman,
					Approvers: []string{owner},
				},
				{
					When:      json.RawMessage(`{"kind":"purge"}`),
					Mode:      ScrubModeHuman,
					Approvers: []string{owner},
				},
			},
			Default:   ScrubModeHumanFourEyes,
			Approvers: []string{owner},
		}

	case PresetScratch:
		// Detaint: auto when attested. Purge: auto after 7 days.
		// Experiments, demos, load tests.
		detaintAutoMaxBeads := presetUnspecifiedCapMaxBeads
		purgeAutoMaxBeads := presetUnspecifiedCapMaxBeads
		return &ScrubPolicy{
			Rules: []ScrubRule{
				{
					When:     json.RawMessage(`{"kind":"purge","older_than_days":7}`),
					Mode:     ScrubModeAuto,
					MaxBeads: &purgeAutoMaxBeads,
				},
				{
					When:     json.RawMessage(`{"kind":"detaint","attested":true}`),
					Mode:     ScrubModeAuto,
					MaxBeads: &detaintAutoMaxBeads,
				},
			},
			Default:   ScrubModeHumanFourEyes,
			Approvers: []string{owner},
		}
	}

	return DefaultScrubPolicy(ownerTeam)
}

// Taint is one row of the taints registry (docs/adr/taints.md §3). The name
// is an alias; ID is what bead_taints/taint_grants/taint_audit key on, so a
// rename never orphans anything.
type Taint struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	OwnerTeam   string          `json:"owner_team"`
	ScrubPolicy json.RawMessage `json:"scrub_policy"`
	// Effects is the set of behaviors this taint carries (docs/adr/taints.md
	// §15.2). Only EffectGate is enforced today; an empty set is a legal
	// trace-only dye. Defaults to {gate} when omitted at creation.
	Effects   []string  `json:"effects"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// RetiredAt blocks new applies but does not lift existing gates
	// (ADR §11.8). Nil for an active taint.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
}

// Principal kinds a taint_grants row may name (docs/adr/taints.md §5.1/§8.2).
const (
	PrincipalTeam     = "team"
	PrincipalOperator = "operator"
	PrincipalAgent    = "agent"
	PrincipalFleet    = "fleet"
)

// IsValidPrincipalKind reports whether kind is one of the four principal
// kinds a grant may target.
func IsValidPrincipalKind(kind string) bool {
	switch kind {
	case PrincipalTeam, PrincipalOperator, PrincipalAgent, PrincipalFleet:
		return true
	}
	return false
}

// TaintGrant is one row of taint_grants (docs/adr/taints.md §3): a principal
// authorized to read beads carrying TaintID, and (per p1-taint-crud-api) to
// apply that taint explicitly. Principal is stored bare — a team bead id, an
// operators.id rendered as text, an agent bead id, or a fleet name — never
// the "<kind>:<value>" combined form that only exists at the CLI/API
// boundary. GrantedBy is always server-derived from the authenticated
// caller, never client-supplied (§11 invariant #1).
type TaintGrant struct {
	TaintID       int64     `json:"taint_id"`
	PrincipalKind string    `json:"principal_kind"`
	Principal     string    `json:"principal"`
	GrantedBy     int64     `json:"granted_by"`
	GrantedAt     time.Time `json:"granted_at"`
	// ExpiresAt is nil for an unbounded team/fleet grant. Never nil for an
	// agent grant (§5.2) or a defaulted operator grant (§10.4).
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Sources a bead_taints row may record (docs/adr/taints.md §3/§4.2) — the
// propagation event that applied a taint to a bead. TaintSourceExplicit and
// the two createBead sources are written today; the remaining inherit:*
// sources are written by the propagation hooks of later tasks.
const (
	TaintSourceExplicit      = "explicit"       // kd taint apply, by a grant holder
	TaintSourceInheritCreate = "inherit:create" // createBead: caller agent's taints
	TaintSourceInheritAssign = "inherit:assign" // assignment: agent bead gets the task's taints
	TaintSourceInheritRead   = "inherit:read"   // agent reads a tainted bead
	TaintSourceInheritWrite  = "inherit:write"  // tainted agent writes to a bead
	TaintSourceInheritParent = "inherit:parent" // createBead: parent/triggered-by/derived-from bead's taints; formula pour

	// TaintSourceInheritProjectIntake is written by createBead when the new
	// bead's project bead declares intake_taints (docs/adr/taints.md §10.3).
	// Distinct from TaintSourceInheritCreate/InheritParent so the
	// propagation-source UI (p1-bead-badges, p2-viewer-taint-detail) can show
	// this as a configuration-driven taint rather than one caused by a
	// principal or by propagation from another bead.
	TaintSourceInheritProjectIntake = "inherit:project-intake"
)

// BeadTaint is one row of bead_taints (docs/adr/taints.md §3), joined with
// the owning taint's name for display. AppliedBy is server-derived, never
// client-supplied.
type BeadTaint struct {
	BeadID       string    `json:"bead_id"`
	TaintID      int64     `json:"taint_id"`
	TaintName    string    `json:"taint_name"`
	Source       string    `json:"source"`
	SourceBeadID string    `json:"source_bead_id,omitempty"`
	AppliedBy    string    `json:"applied_by"`
	AppliedAt    time.Time `json:"applied_at"`
}

// Actions a taint_audit row may record (docs/adr/taints.md §3). Grant,
// Revoke, and Apply are written by the grants/apply write surface;
// ScrubApprove/ScrubReject are written by the §6.3 approve/reject path;
// ScrubExecute is written by ScrubTaint itself (§6.5), one row per scrubbed
// bead, whether the kind was purge or detaint (any mode). ScrubSkip is also
// written by ScrubTaint (§6.5) for a detaint/fork bead skipped because the
// original has since been soft-deleted (gd-T5P13cSoBw) — no content was
// touched, but the request still gets a durable record that this id was
// seen and deliberately not forked. PolicyUpdate is written by the taint
// detail page's Policy tab save (task gd-nOnc1Q8Zcv) — taint_audit.action
// is a plain TEXT column with no CHECK constraint, so adding either of
// these values needs no migration.
const (
	TaintAuditGrant        = "grant"
	TaintAuditRevoke       = "revoke"
	TaintAuditApply        = "apply"
	TaintAuditScrubApprove = "scrub-approve"
	TaintAuditScrubReject  = "scrub-reject"
	TaintAuditScrubExecute = "scrub-execute"
	TaintAuditScrubSkip    = "scrub-skip"
	TaintAuditPolicyUpdate = "policy-update"
)

// SystemActorTaintScrub is the mutation_history.actor / events actor
// stamped on every row ScrubTaint's content cascade blanks or overwrites
// (docs/adr/taints.md §6.5), and on the status/close_reason mutation a
// detaint/fork writes on the original bead. It is a sentinel, not a real
// operator identity — internal/server/undo.go refuses to undo any mutation
// carrying this actor (ErrUndoUnsupportedMutation), because "undo" would
// mean restoring destroyed content the scrub was specifically asked to
// destroy.
const SystemActorTaintScrub = "system:taint-scrub"

// ScrubCleanContent is one bead's attested clean replacement content
// (docs/adr/taints.md §6.4's attestation.clean_content[bead_id]), consumed
// by ScrubTaint for a detaint scrub: strip overwrites the bead's prose with
// this, and fork seeds the successor bead from it. Fields is the bead's
// full replacement fields JSON blob (not a partial patch) — nil/empty means
// "no fields", i.e. an empty object.
type ScrubCleanContent struct {
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Notes       string          `json:"notes"`
	Fields      json.RawMessage `json:"fields,omitempty"`
}

// ScrubTaintRequest is the input to Store.ScrubTaint (docs/adr/taints.md
// §6.5). Kind is "purge" or "detaint"; DetaintMode is only consulted when
// Kind is "detaint" and selects which of the three §6.1/§15.4 branches
// runs (fork/strip implemented here, lift is a separate task, anything
// else is refused). CleanContent must hold an entry for every id in
// BeadIDs when Kind is "detaint" — it is the attested clean replacement
// content those branches consume, per §6.4.
//
// ContentSHAs is the same attestation's content_shas (§6.4), carried
// through so ScrubTaint can re-verify "no drift since attestation" itself,
// under the same per-bead FOR UPDATE lock its cascade writes under, instead
// of trusting the caller's own unlocked pre-transaction check
// (handleApproveScrubRequest's checkAttestationDrift) to still hold by the
// time this transaction runs (gd-JnACECPcvy). nil for purge (no attestation
// exists for that kind) and for any detaint request with no attestation on
// record — both correctly skip the re-check.
//
// Actor is the server-derived approver identity (never client-supplied)
// that taint_audit.Actor records for every row this call writes, and that
// a fork branch's successor bead is created under. ActorOperatorID is the
// same identity's operators(id) FK, for created_by_operator_id /
// operated_by_id on the successor bead — 0 when unresolved.
type ScrubTaintRequest struct {
	RequestID       string
	TaintID         int64
	Kind            string
	DetaintMode     DetaintMode
	BeadIDs         []string
	ContentSHAs     map[string]string
	CleanContent    map[string]ScrubCleanContent
	Actor           string
	ActorOperatorID int64
}

// beadContentForHash is the deterministic content view BeadContentSHA
// hashes: title, description, notes, fields — the same fields an
// attestation's clean_content/content_shas capture (docs/adr/taints.md
// §6.4). encoding/json sorts object keys automatically, so two calls over
// equal content always marshal to the same bytes regardless of field JSON
// map key order.
type beadContentForHash struct {
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Notes       string          `json:"notes"`
	Fields      json.RawMessage `json:"fields"`
}

// BeadContentSHA is the single, shared definition of a bead's content hash
// for scrub-attestation drift checks (docs/adr/taints.md §6.4): the attest
// endpoint (which writes content_shas), the approve-time pre-check
// (checkAttestationDrift), and ScrubTaint's own in-transaction re-check
// (gd-JnACECPcvy) all call this same function, so "match" and "drift" mean
// the same thing everywhere it's evaluated. A bead with empty/nil Fields
// hashes as an empty JSON object, matching how Fields is always read back
// from storage (never a bare null).
func BeadContentSHA(b *Bead) string {
	fields := b.Fields
	if len(fields) == 0 {
		fields = json.RawMessage("{}")
	}
	// encoding/json.Marshal cannot fail on this struct (no channels/funcs/
	// cyclic values), so the error is unreachable.
	blob, _ := json.Marshal(beadContentForHash{
		Title:       b.Title,
		Description: b.Description,
		Notes:       b.Notes,
		Fields:      fields,
	})
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// ScrubTaintBeadResult is the per-bead outcome ScrubTaint reports, the
// input to the audit-artifact shape gd-FJUw7nG1DV builds on top of.
// TablesTouched lists the tables actually rewritten for this bead (empty
// for fork, which touches only beads+mutation_history+taint_audit+deps —
// see that branch for its own bookkeeping; also empty for lift, which
// touches only bead_taints+taint_audit). SuccessorBeadID is set only
// for detaint/fork. Skipped is true when no content was touched for this
// bead (ContentSHA/TablesTouched left zero); SkipReason (one of the
// ScrubSkipReason constants below) says why: bead_taints no longer carried
// (bead_id, taint_id) at execution time for purge/strip/lift (already
// scrubbed or explicitly untainted since the request was built,
// gd-0fQDgy18lo) — no taint_audit row written either, since there was
// nothing to scrub or audit — or, fork only, the original bead has since
// been soft-deleted (gd-T5P13cSoBw): soft delete keeps every copy and is
// restorable (ADR taints.md §6.5, §12.E), so forking a successor from it
// makes no sense the way purge/strip's in-place scrub does, but the skip
// still gets a durable taint_audit row (Action=TaintAuditScrubSkip) so the
// request's audit trail records that this id was seen and deliberately not
// forked, not silently dropped. purge and detaint/strip do NOT skip a
// soft-deleted bead — they lock and scrub it exactly like a live one, since
// skipping would leave tainted title/description/notes/fields/
// mutation_history/comments in place for `kd restore` to revive.
type ScrubTaintBeadResult struct {
	BeadID          string          `json:"bead_id"`
	ContentSHA      string          `json:"content_sha"`
	TablesTouched   []string        `json:"tables_touched"`
	SuccessorBeadID string          `json:"successor_bead_id,omitempty"`
	Skipped         bool            `json:"skipped,omitempty"`
	SkipReason      ScrubSkipReason `json:"skip_reason,omitempty"`
}

// ScrubSkipReason enumerates why ScrubTaint skipped a bead instead of
// scrubbing it (ScrubTaintBeadResult.SkipReason).
type ScrubSkipReason string

const (
	// ScrubSkipTaintGone: bead_taints no longer carries (bead_id, taint_id)
	// at execution time.
	ScrubSkipTaintGone ScrubSkipReason = "taint-already-gone"
	// ScrubSkipSoftDeleted: detaint/fork only — the original bead has since
	// been soft-deleted, so there is nothing live to close and fork from.
	ScrubSkipSoftDeleted ScrubSkipReason = "bead-soft-deleted"
)

// ScrubTaintResult is the return value of Store.ScrubTaint: one
// ScrubTaintBeadResult per bead in the request, same order as
// ScrubTaintRequest.BeadIDs.
type ScrubTaintResult struct {
	Beads []ScrubTaintBeadResult `json:"beads"`
}

// ScrubAuditArtifactBead is one bead's entry in ScrubAuditArtifact.Beads
// (docs/adr/taints.md §6.7, gd-FJUw7nG1DV): the per-bead "{id, content_sha,
// tables_touched}" shape the artifact carries. SuccessorBeadID is populated
// only for kind=detaint, detaint_mode=fork entries (omitempty; absent for
// purge/strip/lift, and for a skipped bead).
type ScrubAuditArtifactBead struct {
	ID              string   `json:"id"`
	ContentSHA      string   `json:"content_sha"`
	TablesTouched   []string `json:"tables_touched"`
	SuccessorBeadID string   `json:"successor_bead_id,omitempty"`
}

// ScrubAuditArtifact is the docs/adr/taints.md §6.7 audit artifact: the
// approve response body's record of exactly what a scrub execution did. The
// daemon persists only taint_audit rows (hashes, ids, actors) — this
// artifact itself is never written to any table, only handed back in the
// approve response for `kd scrub approve --artifact <path>` to write to the
// administrator's own machine. DetaintMode is populated only when Kind is
// "detaint" (json omitempty) — absent entirely for a purge artifact.
type ScrubAuditArtifact struct {
	RequestID    string                   `json:"request_id"`
	Taint        string                   `json:"taint"`
	Kind         string                   `json:"kind"`
	Approver     string                   `json:"approver"`
	ApprovalMode ScrubMode                `json:"approval_mode"`
	DetaintMode  DetaintMode              `json:"detaint_mode,omitempty"`
	Beads        []ScrubAuditArtifactBead `json:"beads"`
}

// BuildScrubAuditArtifact assembles the §6.7 artifact from a completed
// ScrubTaint execution. taintName is the taint's display name (req only
// carries TaintID); mode is the resolved approval mode
// (auto/human/human-four-eyes) the approve handler already computed via
// resolveScrubRule — a property of the approval, not of ScrubTaint's own
// request/result shape, so it isn't on req.
//
// tables_touched per bead follows gd-FJUw7nG1DV's four shapes: purge and
// detaint/strip both list the full content-cascade
// (result.Beads[i].TablesTouched, already exactly that list — see
// cascadeContent in scrub_taint.go) plus the two tables the cascade touches
// after returning that list, bead_taints and taint_audit. detaint/fork and
// detaint/lift never go through cascadeContent (their ScrubTaintBeadResult.
// TablesTouched is left empty by design, see that field's doc comment), so
// their shapes are fixed lists reflecting what those branches actually do.
// A skipped bead (stale taint at execution time, gd-0fQDgy18lo) gets an
// entry with no tables_touched and no content_sha — nothing was touched and
// no taint_audit row was written for it.
func BuildScrubAuditArtifact(req *ScrubTaintRequest, taintName string, mode ScrubMode, result *ScrubTaintResult) *ScrubAuditArtifact {
	detaintMode := req.DetaintMode
	if req.Kind == "detaint" && detaintMode == "" {
		detaintMode = DetaintModeFork // scrub_policy default, §6.1
	}

	a := &ScrubAuditArtifact{
		RequestID:    req.RequestID,
		Taint:        taintName,
		Kind:         req.Kind,
		Approver:     req.Actor,
		ApprovalMode: mode,
		Beads:        make([]ScrubAuditArtifactBead, 0, len(result.Beads)),
	}
	if req.Kind == "detaint" {
		a.DetaintMode = detaintMode
	}

	for _, br := range result.Beads {
		entry := ScrubAuditArtifactBead{ID: br.BeadID}
		if !br.Skipped {
			entry.ContentSHA = br.ContentSHA
			entry.TablesTouched = scrubAuditTablesTouched(req.Kind, detaintMode, br.TablesTouched)
			if req.Kind == "detaint" && detaintMode == DetaintModeFork {
				entry.SuccessorBeadID = br.SuccessorBeadID
			}
		}
		a.Beads = append(a.Beads, entry)
	}
	return a
}

// scrubAuditTablesTouched resolves the tables_touched list for one
// non-skipped bead, per kind/detaint_mode. cascaded is
// ScrubTaintBeadResult.TablesTouched, only meaningful for purge/strip.
func scrubAuditTablesTouched(kind string, detaintMode DetaintMode, cascaded []string) []string {
	switch {
	case kind == "purge", kind == "detaint" && detaintMode == DetaintModeStrip:
		out := make([]string, 0, len(cascaded)+2)
		out = append(out, cascaded...)
		return append(out, "bead_taints", "taint_audit")
	case kind == "detaint" && detaintMode == DetaintModeFork:
		return []string{"beads", "taint_audit"}
	case kind == "detaint" && detaintMode == DetaintModeLift:
		return []string{"bead_taints", "taint_audit"}
	default:
		return nil
	}
}

// TaintAuditFilter narrows a ListTaintAudit query for the viewer's Audit tab
// (docs/adr/taints.md §9.1): optionally by Action and/or Actor (exact match,
// empty means unfiltered), paged by Limit/Offset (Limit<=0 means unbounded).
// CreatedAfter additionally restricts to rows at or after that instant (nil
// means unfiltered) — the scrub-policy auto-mode cap check (§10.5, §11
// invariant 5) uses it with Limit:1 to get an efficient "how many
// scrub-execute rows for this taint since midnight UTC" count from `total`
// without materializing the rows themselves.
type TaintAuditFilter struct {
	Action       string
	Actor        string
	CreatedAfter *time.Time
	Limit        int
	Offset       int
}

// TaintAudit is one row of taint_audit (docs/adr/taints.md §3): a hash-only
// record of a taint mutation, never content. Actor is always server-derived,
// never client-supplied. BeadID, RequestID, ApprovalMode, and ContentSHA are
// populated only where applicable to Action — RequestID/ApprovalMode/
// ContentSHA are written by the scrub workflow of a later task.
type TaintAudit struct {
	ID           int64     `json:"id"`
	TaintID      int64     `json:"taint_id"`
	Action       string    `json:"action"`
	BeadID       string    `json:"bead_id,omitempty"`
	RequestID    string    `json:"request_id,omitempty"`
	Actor        string    `json:"actor"`
	ApprovalMode string    `json:"approval_mode,omitempty"`
	ContentSHA   string    `json:"content_sha,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}
