package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Dedupe at create (P7.2, gd-MKsBqvFZSt). An escalation or decision that asks
// what an open one of its project already asks is refused with a 409 naming
// the open id, so the filer comments there and the operator sees one ask.
//
//   - escalation: the same registered operation_type (canonicalOperationType,
//     P7.4), the same target_bead_id (an X-Escalate replay of one body on two
//     beads is two asks), and either the same operation_spec digest or the
//     same requesting_task. A merge escalation never matches on task alone:
//     one task may have several PRs, each told apart by its spec.
//   - decision: the same filer lineage (the requesting agent's seat_key, so
//     successive firings of one gardener are one filer) and the same prompt
//     digest.
//
// Force and an escalation replay skip it, as they skip the dedup-ack gate.
// Unlike that gate (dedup_ack.go, off by default) this one is always on: it
// refuses only a repeat, never a create that merely shares a queue.

// duplicateOpenErrorCode is the stable code in the 409 body.
const duplicateOpenErrorCode = "duplicate_open"

// duplicateOpenError is the refusal: ExistingID is the open bead the create
// repeats.
type duplicateOpenError struct {
	BeadType   model.BeadType
	ExistingID string
	Reason     string
}

func (e *duplicateOpenError) Error() string {
	differ := "say how in operation_spec"
	if e.BeadType == model.TypeDecision {
		differ = "say how in the prompt"
	}
	return fmt.Sprintf("open %s %s already asks this (%s): comment there with `kd comment add %s` instead of filing another; if yours asks something different, %s",
		e.BeadType, e.ExistingID, e.Reason, e.ExistingID, differ)
}

// openDuplicateStatuses are the statuses in which an ask is still open.
var openDuplicateStatuses = []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked}

// checkOpenDuplicate returns a *duplicateOpenError when bead repeats an open
// escalation or decision of its project, the oldest such bead named. It runs
// after authz, so a caller never learns of another project's beads.
func (s *BeadsServer) checkOpenDuplicate(ctx context.Context, bead *model.Bead, in createBeadInput) error {
	if bead.Type != model.TypeEscalation && bead.Type != model.TypeDecision {
		return nil
	}
	if in.Force || dedupAckReplayBypass(ctx) {
		return nil
	}
	project := resolveDedupProject(bead)
	if project == "" {
		return nil
	}
	open, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{bead.Type},
		Status: openDuplicateStatuses,
		Labels: []string{"project:" + project},
		Fleet:  FleetFromContext(ctx),
		Sort:   "created_at",
	})
	if err != nil {
		return fmt.Errorf("dedupe: list open %s beads: %w", bead.Type, err)
	}
	if bead.Type == model.TypeEscalation {
		return escalationDuplicate(bead, open)
	}
	return s.decisionDuplicate(ctx, bead, open)
}

func escalationDuplicate(bead *model.Bead, open []*model.Bead) error {
	op := escalationDedupeOp(bead)
	if op == "" {
		return nil
	}
	spec := operationSpecDigest(bead.Fields)
	target, _, _ := exactFieldString(bead.Fields, "target_bead_id")
	task, _, _ := exactFieldString(bead.Fields, "requesting_task")
	if operationClassOf(op) == "merge" {
		task = ""
	}
	for _, o := range open {
		if escalationDedupeOp(o) != op {
			continue
		}
		if t, _, _ := exactFieldString(o.Fields, "target_bead_id"); t != target {
			continue
		}
		if operationSpecDigest(o.Fields) == spec {
			return &duplicateOpenError{BeadType: bead.Type, ExistingID: o.ID, Reason: "same operation_type " + op + " and operation_spec"}
		}
		if t, _, _ := exactFieldString(o.Fields, "requesting_task"); task != "" && t == task {
			return &duplicateOpenError{BeadType: bead.Type, ExistingID: o.ID, Reason: "same operation_type " + op + " for requesting_task " + task}
		}
	}
	return nil
}

// escalationDedupeOp is the registered operation_type, so an alias the
// registry kept as filed (aliasKeepsPolicyRule) still matches its type.
func escalationDedupeOp(b *model.Bead) string {
	op, _, _ := exactFieldString(b.Fields, "operation_type")
	return canonicalOperationType(op)
}

// operationSpecDigest is sha256 over operation_spec re-marshalled with sorted
// keys, so key order and whitespace do not tell two specs apart. A missing,
// null or empty spec digests as {}.
func operationSpecDigest(fields json.RawMessage) string {
	spec := fieldsAsMap(fields)["operation_spec"]
	if m, ok := spec.(map[string]any); spec == nil || ok && len(m) == 0 || spec == "" {
		spec = map[string]any{}
	}
	b, _ := json.Marshal(spec)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *BeadsServer) decisionDuplicate(ctx context.Context, bead *model.Bead, open []*model.Bead) error {
	prompt := decisionPromptDigest(bead)
	var lineage string
	for _, o := range open {
		if decisionPromptDigest(o) != prompt {
			continue
		}
		if lineage == "" {
			lineage = s.decisionFilerLineage(ctx, bead)
		}
		if s.decisionFilerLineage(ctx, o) == lineage {
			return &duplicateOpenError{BeadType: bead.Type, ExistingID: o.ID, Reason: "same filer " + lineage + " and prompt"}
		}
	}
	return nil
}

// decisionPromptDigest is sha256 over the prompt (the title when there is
// none), case and whitespace folded.
func decisionPromptDigest(b *model.Bead) string {
	p, _, _ := exactFieldString(b.Fields, "prompt")
	if strings.TrimSpace(p) == "" {
		p = b.Title
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(p), " "))))
	return hex.EncodeToString(sum[:])
}

// decisionFilerLineage names who filed a decision across respawns: the
// requesting agent's seat_key, else its bead id, else created_by.
func (s *BeadsServer) decisionFilerLineage(ctx context.Context, b *model.Bead) string {
	agentID, _, _ := exactFieldString(b.Fields, decisionRequestingAgentField)
	if agentID == "" {
		return "created_by:" + b.CreatedBy
	}
	if agent, err := s.store.GetBead(ctx, agentID); err == nil && agent != nil {
		if seat, _, _ := exactFieldString(agent.Fields, "seat_key"); seat != "" {
			return "seat:" + seat
		}
	}
	return "agent:" + agentID
}
