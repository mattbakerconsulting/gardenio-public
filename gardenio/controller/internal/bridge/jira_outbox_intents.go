package bridge

// DORMANT — the per-mutation OUTBOX INTENT writer (Add-Label / Set-Version /
// Internal-Comment as separate GBFSM issues).
//
// The 1:1 reshape (Phase 2, ka-0000001491) collapsed the triage writer to ONE
// upserted GBFSM ticket per FSM ticket (see CreateGBFSMTicket / SearchGBFSM /
// EditGBFSMTicket in jira_outbox.go), which carries the mutations as FIELDS. The
// intent machinery below therefore has NO live caller anymore. It is kept dormant —
// not deleted — until Phase 4 (ka-0000003716) adapts the operator-owned Automation
// rules to read the single ticket, so the two land without a flag-day. Its GBFSM-only
// create guarantee is still pinned by jira_outbox_test.go / the no-bypass scans.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// OutboxIntentType names one of the three append-only intent kinds an outbox
// ticket can carry. It selects the GBFSM issue type and which custom fields are
// required; it is NOT a Jira project and is never a create target.
type OutboxIntentType string

const (
	// IntentAddLabel — the R-LABEL rule adds op_value as a label on the FSM ticket.
	IntentAddLabel OutboxIntentType = "add-label"
	// IntentSetVersion — the R-VERSION rule sets op_value as an Affects-Version.
	IntentSetVersion OutboxIntentType = "set-version"
	// IntentInternalComment — the R-COMMENT rule posts the body as an INTERNAL
	// note on the FSM ticket (visibility hardcoded rule-side, never from payload).
	IntentInternalComment OutboxIntentType = "internal-comment"
)

// outboxTypeID maps an intent type to its GBFSM issue-type id. An unknown type
// returns ("", false) so CreateOutboxIntent fails closed rather than creating a
// mistyped intent the Automation rules would drop into the Failed column.
func outboxTypeID(t OutboxIntentType) (string, bool) {
	switch t {
	case IntentAddLabel:
		return outboxTypeAddLabel, true
	case IntentSetVersion:
		return outboxTypeSetVersion, true
	case IntentInternalComment:
		return outboxTypeInternalComment, true
	default:
		return "", false
	}
}

// OutboxIntent is one append-only FSM-write intent. The bridge creates it as a
// GBFSM ticket; an operator-owned Automation rule validates it and applies the op
// to the FSM ticket named by FSMTargetKey.
type OutboxIntent struct {
	// Type selects the intent kind (Add-Label / Set-Version / Internal-Comment).
	Type OutboxIntentType
	// FSMTargetKey is the FSM issue key the op applies to (e.g. "FSM-408"). It is
	// carried as DATA in the fsm_target_key custom field — NEVER used as the
	// create project. The Automation rule validates it against ^FSM-[0-9]+$.
	FSMTargetKey string
	// OpValue is the label (Add-Label) or version name (Set-Version) to apply.
	// Ignored for Internal-Comment (which carries its text in Body). Required and
	// validated rule-side against the op allowlist for Add-Label / Set-Version.
	OpValue string
	// Body is the internal-note text for Internal-Comment intents (mapped to the
	// GBFSM ticket's Description, which the R-COMMENT rule posts as an INTERNAL note
	// on the FSM ticket). Ignored for Add-Label / Set-Version.
	Body string
	// IdempotencyKey deduplicates retries (bead-id + op [+ value]); see
	// outboxIdempotencyKey.
	IdempotencyKey string
	// AnalysisTicketKey, when non-empty, is a POINTER to the GBFSM ticket this intent
	// belongs to, appended to the intent Summary only (never a custom field or the
	// create project), so it cannot alter what the Automation rule applies to FSM.
	// Dormant after the 1:1 reshape collapsed the writer (ka-0000001491).
	AnalysisTicketKey string
}

// CreateOutboxIntent creates ONE append-only intent ticket in the GBFSM project.
//
// The create project is HARDCODED to outboxProjectID — the intent's FSMTargetKey
// is written only into the fsm_target_key custom field, never into the project,
// so no caller-supplied key can redirect the create off GBFSM. This is the
// structural, API-level guarantee that the bridge cannot create (or otherwise
// write) an issue in FSM or any customer-facing project: this create path can only
// ever land in the internal outbox.
//
// Fail-closed: an unknown intent type, an empty FSM target key, or an empty
// op_value for an Add-Label/Set-Version intent returns an error and sends no
// request — a malformed intent must never reach GBFSM where a rule would drop it
// into the Failed dead-letter column.
func (c *JiraClient) CreateOutboxIntent(ctx context.Context, intent OutboxIntent) error {
	typeID, ok := outboxTypeID(intent.Type)
	if !ok {
		return fmt.Errorf("outbox: unknown intent type %q", intent.Type)
	}
	target := strings.TrimSpace(intent.FSMTargetKey)
	if target == "" {
		return fmt.Errorf("outbox: %s intent has empty fsm_target_key", intent.Type)
	}

	// Common fields on every intent: the HARDCODED GBFSM project, the intent work
	// type, a human-readable summary, the FSM target key (DATA), and the
	// idempotency key. project.id is outboxProjectID and nothing else.
	fields := map[string]any{
		"project":            map[string]any{"id": outboxProjectID},
		"issuetype":          map[string]any{"id": typeID},
		"summary":            outboxSummary(intent, target),
		outboxFieldTargetKey: target,
	}
	if k := strings.TrimSpace(intent.IdempotencyKey); k != "" {
		fields[outboxFieldIdempotencyKey] = k
	}

	switch intent.Type {
	case IntentAddLabel, IntentSetVersion:
		op := strings.TrimSpace(intent.OpValue)
		if op == "" {
			return fmt.Errorf("outbox: %s intent for %s has empty op_value", intent.Type, target)
		}
		fields[outboxFieldOpValue] = op
	case IntentInternalComment:
		body := strings.TrimSpace(intent.Body)
		if body == "" {
			return fmt.Errorf("outbox: internal-comment intent for %s has empty body", target)
		}
		// The comment text rides in the built-in Description (the R-COMMENT rule
		// posts it INTERNAL on the FSM ticket). op_value is not used here.
		fields["description"] = adfParagraph(body)
	}

	body := map[string]any{"fields": fields}
	// One of the two GBFSM create endpoints in the whole bridge (the other is
	// CreateGBFSMTicket). It is the issue-collection POST — safe only because
	// project.id above is hardcoded to GBFSM.
	if err := c.doJSON(ctx, http.MethodPost, "/rest/api/3/issue", body, nil); err != nil {
		return fmt.Errorf("outbox: create %s intent for %s: %w", intent.Type, target, err)
	}
	return nil
}

// outboxSummary renders a short human-readable summary for an intent ticket so
// the GBFSM board is legible to the operator (the security-critical field data is
// in the custom fields, not the summary). When the intent carries an
// AnalysisTicketKey it is appended as a navigable pointer — a cosmetic suffix only;
// the custom fields the Automation rule reads are untouched.
func outboxSummary(intent OutboxIntent, target string) string {
	var base string
	switch intent.Type {
	case IntentAddLabel:
		base = fmt.Sprintf("Add-Label %s: %s", target, strings.TrimSpace(intent.OpValue))
	case IntentSetVersion:
		base = fmt.Sprintf("Set-Version %s: %s", target, strings.TrimSpace(intent.OpValue))
	case IntentInternalComment:
		base = fmt.Sprintf("Internal-Comment %s", target)
	default:
		base = fmt.Sprintf("Intent %s", target)
	}
	if ref := strings.TrimSpace(intent.AnalysisTicketKey); ref != "" {
		base += fmt.Sprintf(" [analysis %s]", ref)
	}
	return base
}
