package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// mailControlEscalationApproved is the control: label on the mail that tells
// a local executor its escalation was approved (spec §8.2 step 3). Keep in
// step with gardenio/controller/internal/mailproto.
const mailControlEscalationApproved = mailControlPrefix + "escalation-approved"

// escalationRunnerOnlineWindow is how long after its last heartbeat a runner
// still counts as online: three missed 30 s beats, as the bridge's
// runnerOnlineWindow (bridge/placement.go).
const escalationRunnerOnlineWindow = 90 * time.Second

// deadLocalAgentStates are the agent_state values whose mail would never be
// read, as the bridge's deadAgentStates (bridge/nudge.go).
var deadLocalAgentStates = map[string]bool{"done": true, "failed": true, "stopping": true, "budget_exceeded": true}

type escalationApprovedMailKey struct{}

// withEscalationApprovedMail marks ctx as finishEscalationApproval sending its
// control:escalation-approved mail once the approval is authorized, so a
// coordinator approver (a non-human identity) is not re-judged against the
// control: mail rules. It has no header path; set it only there.
func withEscalationApprovedMail(ctx context.Context) context.Context {
	return context.WithValue(ctx, escalationApprovedMailKey{}, true)
}

func isEscalationApprovedMail(ctx context.Context) bool {
	v, _ := ctx.Value(escalationApprovedMailKey{}).(bool)
	return v
}

func beadFieldMap(raw json.RawMessage) map[string]any {
	var f map[string]any
	_ = json.Unmarshal(raw, &f)
	if f == nil {
		f = map[string]any{}
	}
	return f
}

func fieldStr(f map[string]any, k string) string {
	s, _ := f[k].(string)
	return s
}

// escalationLocalTargetWarnOnly reports whether BEADS_ESCALATION_LOCAL_TARGET
// is "warn", read per request. Unset (or "enforce"), a local escalation that
// names no runner is refused at create, as spec §8.1 asks and P7 Task 13
// landed. "warn" lets one through with a warning, for a fleet that still files
// --execution-target local with no --target-runner or --target-owner.
func escalationLocalTargetWarnOnly() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("BEADS_ESCALATION_LOCAL_TARGET")), "warn")
}

// escalationLocalTargetMissing is the refusal (or, in warn mode, the warning)
// for a local escalation that names no runner.
const escalationLocalTargetMissing = "escalation with execution_target=local must name target_runner (a runner bead id) or target_owner"

// liveTargetRunner returns runnerID's bead when it is an open, unrevoked
// runner, else the 400 the create guard and approve both give.
func (s *BeadsServer) liveTargetRunner(ctx context.Context, runnerID string) (*model.Bead, error) {
	b, err := s.store.GetBead(ctx, runnerID)
	if err != nil || b == nil || b.Type != "runner" || b.Status == model.StatusClosed ||
		fieldStr(beadFieldMap(b.Fields), "state") == runnerStateRevoked {
		return nil, inputError(fmt.Sprintf("target_runner %s is not a registered runner", runnerID))
	}
	return b, nil
}

// sameOperator reports whether owner (a runner owner or target_owner, stored
// as oidc:<subject>) names the approver subject.
func sameOperator(owner, approver string) bool {
	return owner != "" && approver != "" && (strings.EqualFold(owner, approver) || strings.EqualFold(owner, "oidc:"+approver))
}

// localApprovalForbidden is checkLocalApproval's refusal of the approver: a
// 403, where inputError (an unregistered runner) is a 400.
type localApprovalForbidden string

func (e localApprovalForbidden) Error() string { return string(e) }

// checkLocalApproval re-runs the runner check at approve time, for an
// escalation esc (with any approve-time execution_target already applied)
// that will dispatch locally to a named runner, and ties the runner to the
// approver: only the runner's owner (or target_owner, when no runner is named)
// may approve work on their machine (spec §8.2 step 2). The four-eyes path for
// a fleet operator is not built, so any other approver, a coordinator lane
// included, is refused. This narrows who may approve; it fails closed.
func (s *BeadsServer) checkLocalApproval(ctx context.Context, esc *model.Bead, approvedBy string) error {
	f := beadFieldMap(esc.Fields)
	if runnerID := fieldStr(f, "target_runner"); runnerID != "" {
		runner, err := s.liveTargetRunner(ctx, runnerID)
		if err != nil {
			return err
		}
		owner := fieldStr(beadFieldMap(runner.Fields), "owner")
		if owner == "" {
			owner = runner.Owner
		}
		if !sameOperator(owner, approvedBy) {
			return localApprovalForbidden(fmt.Sprintf("escalation runs on runner %s (%s), owned by %s; only its owner may approve it", runnerID, runner.Title, owner))
		}
		return nil
	}
	if owner := fieldStr(f, "target_owner"); !sameOperator(owner, approvedBy) {
		return localApprovalForbidden(fmt.Sprintf("escalation runs on a runner owned by %s; only that owner may approve it", owner))
	}
	return nil
}

// resolveEscalationRunner picks the runner of a local escalation:
// target_runner, else the single online runner target_owner owns, else "".
func (s *BeadsServer) resolveEscalationRunner(ctx context.Context, f map[string]any) (string, error) {
	if r := fieldStr(f, "target_runner"); r != "" {
		return r, nil
	}
	owner := fieldStr(f, "target_owner")
	if owner == "" {
		return "", nil
	}
	runners, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"runner"},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Fields: map[string]string{"owner": owner},
	})
	if err != nil {
		return "", err
	}
	now := time.Now()
	var online []string
	for _, r := range runners {
		rf := beadFieldMap(r.Fields)
		seen, perr := time.Parse(time.RFC3339, fieldStr(rf, "last_seen_at"))
		if fieldStr(rf, "owner") == owner && fieldStr(rf, "state") == runnerStateOnline && perr == nil && now.Sub(seen) <= escalationRunnerOnlineWindow {
			online = append(online, r.ID)
		}
	}
	if len(online) == 1 {
		return online[0], nil
	}
	return "", nil
}

// escalationNamesRunner reports whether an escalation's fields route it to a
// runner: execution_target=local with target_runner or target_owner.
func escalationNamesRunner(f map[string]any) bool {
	return fieldStr(f, "execution_target") == "local" && (fieldStr(f, "target_runner") != "" || fieldStr(f, "target_owner") != "")
}

// localAgentOnRunner returns the name of the live agent joined on runnerID
// (fields.runtime_id), or "" when none is.
func (s *BeadsServer) localAgentOnRunner(ctx context.Context, runnerID string) (string, error) {
	agents, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"agent"},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Fields: map[string]string{runtimeIDField: runnerID},
	})
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		af := beadFieldMap(a.Fields)
		if fieldStr(af, runtimeIDField) != runnerID || deadLocalAgentStates[fieldStr(af, "agent_state")] || fmt.Sprint(af["stop_requested"]) == "true" {
			continue
		}
		if n := fieldStr(af, "agent"); n != "" {
			return n, nil
		}
		if a.Assignee != "" {
			return a.Assignee, nil
		}
		return a.Title, nil
	}
	return "", nil
}

// emitEscalationApprovedMail sends a local escalation's executor its one
// notification (spec §8.1, "mail from inside the approve handlers"), after
// finishEscalationApproval took the dispatch claim and flipped the status: it
// sends only for execution_dispatch=local and an escalation naming a runner,
// so a server replay or a verdict is never also mailed. The mail goes to the
// executor startEscalationExecutor recorded (the agent joined on the runner);
// with none recorded it is parked, addressed to runner:<id>, for gb runner
// join to re-address on its drain (spec §10). It records only
// executor_mail_id and returns the mail id and whether an agent was
// addressed. Anything else returns "", false, nil.
func (s *BeadsServer) emitEscalationApprovedMail(ctx context.Context, esc *model.Bead) (string, bool, error) {
	f := beadFieldMap(esc.Fields)
	if fieldStr(f, "execution_dispatch") != dispatchLocal || !escalationNamesRunner(f) {
		return "", false, nil
	}
	runnerID, err := s.resolveEscalationRunner(ctx, f)
	if err != nil {
		return "", false, err
	}
	if runnerID == "" {
		return "", false, fmt.Errorf("no runner resolvable for escalation %s", esc.ID)
	}
	agent := fieldStr(f, "executor")
	assignee := "runner:" + runnerID
	if agent != "" {
		assignee = agent
	}
	opType := fieldStr(f, "operation_type")
	spec, _ := json.Marshal(f["operation_spec"])
	mailFields, _ := json.Marshal(map[string]string{"escalation_id": esc.ID})
	mail, err := s.createBead(withEscalationApprovedMail(ctx), createBeadInput{
		Title:    fmt.Sprintf("Escalation %s approved: %s", esc.ID, opType),
		Type:     "mail",
		Kind:     string(model.KindData),
		Assignee: assignee,
		Description: fmt.Sprintf("%s\n\nReport with: gb escalation close %s --status success --result '<json>' (failure or partial if it did not fully succeed)",
			spec, esc.ID),
		Labels:    []string{"from:kbeads", mailControlEscalationApproved},
		Fields:    mailFields,
		CreatedBy: "kbeads",
	})
	if err != nil {
		return "", false, err
	}
	if err := s.mergeBeadFields(ctx, esc.ID, map[string]any{"executor_mail_id": mail.ID}); err != nil {
		slog.Warn("escalation executor mail sent but not recorded", "escalation", esc.ID, "mail", mail.ID, "error", err)
	}
	return mail.ID, agent != "", nil
}

// checkRunnerApprover re-checks the runner a local escalation names, and that
// the approver owns it (checkLocalApproval). It writes the error response and
// returns false when the approval may not proceed.
func (s *BeadsServer) checkRunnerApprover(w http.ResponseWriter, r *http.Request, target *model.Bead, approvedBy string) bool {
	err := s.checkLocalApproval(r.Context(), target, approvedBy)
	var refused localApprovalForbidden
	var invalid inputError
	switch {
	case errors.As(err, &refused):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to resolve the escalation's runner")
	default:
		return true
	}
	return false
}

// notifyRunnerExecutor mails the runner's agent for a local dispatch to a
// named runner (spec §8.2 step 3), once, and reports whether a joined agent
// was addressed. A failed mail never fails the approval.
func (s *BeadsServer) notifyRunnerExecutor(ctx context.Context, bead *model.Bead) bool {
	if sent, _, _ := exactFieldString(bead.Fields, "executor_mail_id"); sent != "" {
		return false
	}
	mailID, delivered, err := s.emitEscalationApprovedMail(ctx, bead)
	if err != nil {
		slog.Warn("escalation approved but no executor mail was sent", "bead", bead.ID, "error", err)
		return false
	}
	if mailID != "" {
		slog.Info("escalation executor notified by mail", "bead", bead.ID, "mail", mailID, "delivered", delivered)
	}
	return delivered
}
