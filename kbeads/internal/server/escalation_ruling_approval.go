package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// An escalation that carries out an owner ruling is approved by that ruling
// (P7.5, owner ruling 6 on gd-7qUpu15BBK; programme
// docs/superpowers/plans/2026-11-02-agent-access-programme.md).
//
// The filer names the ruling in fields.implements_decision and cites the
// chosen option id in operation_spec.chosen. Create refuses the citation
// (rulingApprover) unless the decision is a closed decision in the
// escalation's project, answered by a gardenio-admin on their own human
// session, whose chosen option the spec cites. resolver_admin and
// resolver_subject are the server's resolver stamp (decision_resolver_authz.go),
// which no client writes, so the filer's token decides nothing beyond which
// ruling it cites.
//
// A ruling approves only what the owner was shown and one escalation of it.
// The chosen option must name the operation_type it authorizes, the
// target_bead_id if any, and the operation_spec; options are immutable after
// create (decision_question_immutable.go), so the owner answered exactly that.
// The escalation's operation_type, target_bead_id and operation_spec (less its
// "chosen" key) must equal them (operator ruling gd-u2EEbUjh2o). And no other
// escalation may cite the same ruling: create refuses a second citation, and
// the approve re-checks after the insert, so of two racing creates at most one
// is approved (each sees the other once both rows exist).
//
// After the insert, maybeApproveByRuling stamps the approval for the
// decision's answerer with approval_method=decision and runs the same tail as
// a human approve: claim the dispatch, hold by execute_by, move to
// in_progress, publish, audit, and replay a server dispatch. The answerer held
// gardenio-admin, which passes every approver gate below the floor, so the
// per-approver gates of handleApproveEscalation are not re-run. What the
// ruling cannot approve is created as usual and left for a person
// (rulingApprovalExclusion).

// approvalMethodDecision is the approval_method of an escalation approved by
// the owner ruling it implements.
const approvalMethodDecision = "decision"

// implementsDecisionField names the owner ruling an escalation carries out.
const implementsDecisionField = "implements_decision"

// rulingApprover returns the resolver_subject of the ruling esc cites, "" with
// a nil error when esc cites none, or an inputError saying why the citation
// does not hold.
func (s *BeadsServer) rulingApprover(ctx context.Context, esc *model.Bead) (string, error) {
	if esc == nil || esc.Type != model.TypeEscalation {
		return "", nil
	}
	id := strings.TrimSpace(fieldsStringValue(esc.Fields, implementsDecisionField))
	if id == "" {
		return "", nil
	}
	refuse := func(why string) (string, error) {
		return "", inputError(implementsDecisionField + " " + id + ": " + why)
	}
	dec, err := s.store.GetBead(ctx, id)
	if err != nil || dec == nil || dec.Type != model.TypeDecision {
		return refuse("is not a decision")
	}
	if dec.Status != model.StatusClosed {
		return refuse("is not closed")
	}
	if p := projectFromLabelsOrEmpty(dec.Labels); p == "" || p != projectFromLabelsOrEmpty(esc.Labels) {
		return refuse("is not a decision in this escalation's project")
	}
	var f struct {
		Chosen          string         `json:"chosen"`
		ResolverSubject string         `json:"resolver_subject"`
		ResolverAdmin   bool           `json:"resolver_admin"`
		Options         []rulingOption `json:"options"`
	}
	_ = json.Unmarshal(dec.Fields, &f)
	subject := strings.TrimSpace(f.ResolverSubject)
	if !f.ResolverAdmin || subject == "" {
		return refuse("was not answered by a gardenio-admin on their own sign-in")
	}
	chosen := strings.TrimSpace(f.Chosen)
	if chosen == "" || !specCitesChosen(esc, chosen) {
		return refuse("its chosen option " + strconvQuote(chosen) + " is not in operation_spec.chosen")
	}
	if why := rulingScopeMismatch(f.Options, chosen, esc); why != "" {
		return refuse(why)
	}
	other, err := s.otherRulingCitation(ctx, id, esc.ID)
	if err != nil {
		return refuse("could not check for another escalation citing it: " + err.Error())
	}
	if other != "" {
		return refuse("is already cited by escalation " + other + "; a ruling approves one escalation")
	}
	return subject, nil
}

// rulingOption is the part of a decision option that says what choosing it
// authorizes.
type rulingOption struct {
	ID            string          `json:"id"`
	OperationType string          `json:"operation_type"`
	TargetBeadID  string          `json:"target_bead_id"`
	OperationSpec json.RawMessage `json:"operation_spec"`
}

// rulingScopeMismatch says why the chosen option does not authorize esc's
// operation_type, target_bead_id and operation_spec, or "".
func rulingScopeMismatch(options []rulingOption, chosen string, esc *model.Bead) string {
	var opt *rulingOption
	for i := range options {
		if strings.TrimSpace(options[i].ID) == chosen {
			opt = &options[i]
			break
		}
	}
	if opt == nil || strings.TrimSpace(opt.OperationType) == "" {
		return "its chosen option " + strconvQuote(chosen) + " names no operation_type it authorizes"
	}
	f := escalationStringFields(esc)
	if want, got := canonicalOperationType(opt.OperationType), canonicalOperationType(f["operation_type"]); want != got {
		return "its chosen option authorizes operation_type " + strconvQuote(want) + ", not " + strconvQuote(got)
	}
	if want, got := strings.TrimSpace(opt.TargetBeadID), strings.TrimSpace(f["target_bead_id"]); want != got {
		return "its chosen option authorizes target_bead_id " + strconvQuote(want) + ", not " + strconvQuote(got)
	}
	want, ok := specWithoutChosen(opt.OperationSpec)
	if !ok {
		return "its chosen option " + strconvQuote(chosen) + " names no operation_spec it authorizes"
	}
	if got, _ := specWithoutChosen(json.RawMessage(fieldRaw(esc, "operation_spec"))); !reflect.DeepEqual(want, got) {
		return "its chosen option authorizes a different operation_spec than this escalation's"
	}
	return ""
}

// specWithoutChosen decodes an operation_spec object, or a string holding one,
// without its "chosen" key; ok is false when raw is no object.
func specWithoutChosen(raw json.RawMessage) (spec map[string]any, ok bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	if json.Unmarshal(raw, &spec) != nil || spec == nil {
		return nil, false
	}
	delete(spec, "chosen")
	return spec, true
}

// otherRulingCitation returns the id of an escalation other than selfID that
// cites decisionID, in any status, or "".
func (s *BeadsServer) otherRulingCitation(ctx context.Context, decisionID, selfID string) (string, error) {
	cites, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeEscalation},
		Fields: map[string]string{implementsDecisionField: decisionID},
		Limit:  2,
	})
	if err != nil {
		return "", err
	}
	for _, b := range cites {
		if b != nil && b.ID != selfID {
			return b.ID, nil
		}
	}
	return "", nil
}

// specCitesChosen reports whether esc's operation_spec.chosen is option or a
// list holding it.
func specCitesChosen(esc *model.Bead, option string) bool {
	raw := fieldRaw(esc, "operation_spec")
	var s string
	if json.Unmarshal([]byte(raw), &s) == nil {
		raw = s
	}
	var spec struct {
		Chosen json.RawMessage `json:"chosen"`
	}
	if json.Unmarshal([]byte(raw), &spec) != nil {
		return false
	}
	var one string
	if json.Unmarshal(spec.Chosen, &one) == nil {
		return strings.TrimSpace(one) == option
	}
	var many []string
	_ = json.Unmarshal(spec.Chosen, &many)
	for _, c := range many {
		if strings.TrimSpace(c) == option {
			return true
		}
	}
	return false
}

// rulingApprovalExclusion says why a ruling may not approve esc, or "". The
// D3 floor and the owner's other forced reasons, four-eyes, and a taint grant
// stay a person's approval; a local or service_account executor runs what the
// approver reviewed at approval (a script, a runner they own), which a ruling
// did not see.
func (s *BeadsServer) rulingApprovalExclusion(ctx context.Context, esc *model.Bead) string {
	if r := s.escalationForcedReason(ctx, esc); r != "" {
		return "forced:" + r
	}
	if s.escalationOperatorOnly(ctx, esc) {
		return "operator_only"
	}
	if escalationRequiresFourEyes(esc) {
		return "requires_four_eyes"
	}
	if op := normalizeOperationType(escalationStringFields(esc)["operation_type"]); coordinatorRefusedOperations[op] {
		return "operation:" + op
	}
	if d := escalationDispatchOf(esc); d == dispatchLocal || d == dispatchServiceAccount {
		return "dispatch:" + d
	}
	return ""
}

// answerByStandingRules returns bead as the daemon's standing rules leave it
// after create: a consent-to-push decision answered by standing consent
// (gd-QV2WuUjely), an escalation approved by the owner ruling it implements.
func (s *BeadsServer) answerByStandingRules(ctx context.Context, bead *model.Bead) *model.Bead {
	if closed := s.maybeResolveByStandingConsent(ctx, bead); closed != nil {
		return closed
	}
	if approved := s.maybeApproveByRuling(ctx, bead); approved != nil {
		return approved
	}
	return bead
}

// maybeApproveByRuling approves a just-created escalation by the owner ruling
// it implements. It returns the approved escalation, or nil when it left it
// for a person (no citation, an exclusion, or an error, which is logged).
func (s *BeadsServer) maybeApproveByRuling(ctx context.Context, esc *model.Bead) *model.Bead {
	if esc == nil || esc.Type != model.TypeEscalation || esc.Status != model.StatusOpen {
		return nil
	}
	approvedBy, err := s.rulingApprover(ctx, esc)
	if err != nil || approvedBy == "" {
		if err != nil {
			slog.Warn("ruling approval: citation no longer holds; leaving for a person", "escalation", esc.ID, "error", err)
		}
		return nil
	}
	decisionID := strings.TrimSpace(fieldsStringValue(esc.Fields, implementsDecisionField))
	if why := s.rulingApprovalExclusion(ctx, esc); why != "" {
		slog.Info("ruling approval: excluded; leaving for a person", "escalation", esc.ID, "decision", decisionID, "reason", why)
		return nil
	}
	dispatch, proceed, err := s.claimEscalationDispatch(ctx, esc)
	if err != nil || !proceed {
		slog.Warn("ruling approval: dispatch not claimed", "escalation", esc.ID, "dispatch", dispatch, "error", err)
		return nil
	}
	if err := s.startEscalationExecutor(ctx, esc, dispatch, approvedBy); err != nil {
		slog.Warn("ruling approval: executor did not start; leaving for a person", "escalation", esc.ID, "dispatch", dispatch, "error", err)
		return nil
	}
	extra := map[string]any{
		"approved_by":     approvedBy,
		"approved_at":     time.Now().UTC().Format(time.RFC3339),
		"approval_method": approvalMethodDecision,
	}
	s.stampEscalationHolder(ctx, esc, extra)
	if err := s.mergeEscalationFieldsWithHistory(ctx, esc, approvedBy, extra); err != nil {
		slog.Warn("ruling approval: failed to merge approval fields", "escalation", esc.ID, "error", err)
		return nil
	}

	bead, err := s.store.GetBead(ctx, esc.ID)
	if err != nil || bead == nil {
		slog.Warn("ruling approval: failed to re-read the escalation", "escalation", esc.ID, "error", err)
		return nil
	}
	changes := map[string]any{"approved_by": approvedBy, "execution_dispatch": dispatch}
	if bead.Status == model.StatusOpen {
		bead.Status = model.StatusInProgress
		if err := s.store.UpdateBead(ctx, bead); err != nil {
			slog.Warn("ruling approval: failed to move the escalation to in_progress", "escalation", esc.ID, "error", err)
			return nil
		}
		changes["status"] = string(model.StatusInProgress)
	}
	s.recordAndPublish(ctx, events.TopicBeadUpdated, bead.ID, approvedBy, events.BeadUpdated{Bead: bead, Changes: changes})
	s.auditEscalationApproval(ctx, bead.ID, approvedBy)
	slog.Info("ruling approval: approved by the owner ruling it implements",
		"escalation", bead.ID, "decision", decisionID, "approved_by", approvedBy, "dispatch", dispatch)

	if dispatch == dispatchServer {
		s.replayEscalation(ctx, bead, approvedBy, nil)
	}
	if fresh, err := s.store.GetBead(ctx, bead.ID); err == nil && fresh != nil {
		bead = fresh
	}
	return bead
}
