// Package bridge — the Gardenio FSM Outbox (GBFSM) writer.
//
// # Why the outbox exists (epic ka-0000002067 — API-level FSM write isolation)
//
// The prior model let the privileged gardenio2 bridge write the FSM customer
// service desk directly (labels + Affects-Version), gated in-process by the
// field allowlist. Tim's requirement is API-LEVEL security: a fully compromised
// bot token must not be able to touch a customer ticket AT ALL. So gardenio bot
// accounts now hold ZERO write permission on FSM at the Jira permission layer,
// and every FSM-bound write lands in a dedicated internal project — the "Gardenio
// FSM Outbox" (GBFSM). Operator-owned Jira Cloud Automation rules (scope {GBFSM,
// FSM}) validate the request against a hardcoded allowlist and apply it to the
// real FSM ticket as the Automation actor. The bot never touches FSM (read-only
// browse excepted).
//
// # The 1:1 model (reshape ka-0000002637, Phase 2 ka-0000001491)
//
// gardenio writes ONE upserted GBFSM ticket per FSM ticket (CreateGBFSMTicket /
// SearchGBFSM / EditGBFSMTicket here), carrying the analysis in its Description +
// the mutations as FIELDS. This SUPERSEDES the earlier per-mutation intent fan-out,
// whose writer (CreateOutboxIntent) is kept DORMANT in jira_outbox_intents.go until
// Phase 4 retires it.
//
// # The one and only issue-create path (ka-0000002073 constraint, refined)
//
// jira.go deliberately exposes NO issue-create endpoint — the JiraClient there
// cannot POST to the issue-collection endpoint. The GBFSM creates here are the SOLE
// exception, and safe by construction: they hardcode the GBFSM project
// (outboxProjectID) as the create target, and an upsert-EDIT can only ever target a
// SearchGBFSM-verified GBFSM key. The FSM ticket key travels only as DATA in the
// fsm_target_key custom field — NEVER the create/edit target. A crafted or buggy
// FSMTargetKey (even "FSM-1", "PE-1", or "GBFSM-9") therefore cannot redirect the
// write off GBFSM. The AST/behavioral guards in jira_outbox_test.go /
// jira_gbfsm_upsert_test.go pin this invariant.
//
// # Idempotency
//
// A bridge retry (SSE replay after a restart) can re-emit a write. The GBFSM ticket
// carries an idempotency_key keyed on FSM-N, so a re-triage UPSERTS (SearchGBFSM →
// EditGBFSMTicket) rather than creating a second. See outboxIdempotencyKey.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// GBFSM project + intent-type + custom-field identifiers, from the sandbox scaffold
// artifact (ka-0000001322). These are the load-bearing constants of the outbox:
// the project the create is hardcoded to, the three intent work types, and the
// three project-local custom fields the Automation rules read.
const (
	// outboxProjectID is the numeric id of the "Gardenio FSM Outbox" (GBFSM)
	// project. The create body pins fields.project.id to THIS, always — it is the
	// structural guarantee that an outbox intent can only ever be created in GBFSM.
	outboxProjectID = "10037"
	// outboxProjectKey is the outbox project's key, used for diagnostics / logging
	// and as the WritePolicy map key ONLY — it is NEVER sent to Jira (the create
	// hardcodes the numeric outboxProjectID). The project was renamed GFO→GBFSM
	// (Matt, Slack 2026-07-09; epic ka-0000002637): GBFSM-123 maps 1:1 to FSM-123.
	//
	// BACK-COMPAT (explicitly marked): the load-bearing SANDBOX project is still
	// keyed "GFO" (id 10037) — its key was not re-lettered — so the operator E2E
	// rig (JIRA_GFO_PROJECT_KEY, gardenio/tests/e2e/scripts/*) intentionally still
	// says GFO. Because this const is diagnostic-only and the create pins the
	// numeric id, that divergence is behavior-neutral; do NOT re-key the sandbox or
	// the E2E scripts here. Prod GBFSM gets its own id at cutover (ka-0000007073).
	outboxProjectKey = "GBFSM"

	// Intent work-type ids (GBFSM-local issue types).
	outboxTypeAddLabel        = "10040" // Add-Label
	outboxTypeSetVersion      = "10041" // Set-Version
	outboxTypeInternalComment = "10042" // Internal-Comment

	// outboxTypeAnalysis is the GBFSM "Analysis" issue-type id — the work type for
	// the ONE upserted GBFSM ticket per triaged FSM ticket (epic ka-0000002067,
	// task ka-0000001294; 1:1 reshape ka-0000002637).
	//
	// DELIBERATELY EMPTY — arming is operator-owned and per-environment. The sandbox
	// GBFSM scaffold DID land (2026-07-11: Analysis issue type = 10043; see
	// gardenio/tests/e2e/fixtures/outbox-rules/README.md), but this default stays blank
	// on purpose: the id is environment-specific (prod GBFSM gets its own at cutover
	// ka-0000007073), so it is supplied at deploy time via the
	// JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE helm env var rather than hardcoded here. Do NOT
	// bump this const to the sandbox id — that would silently arm the staging outbox
	// surface. While blank, CreateGBFSMTicket fails closed (sends nothing), so the
	// surface is INERT on merge on two independent axes — the outbox toggle
	// (JIRA_OUTBOX_ENABLED, default OFF) AND the unconfigured issue type — for zero
	// prod behavior change.
	outboxTypeAnalysis = ""

	// Project-local custom fields shared across the three intent types.
	outboxFieldTargetKey      = "customfield_10148" // fsm_target_key (REQUIRED)   — the FSM ticket to act on (DATA, never the create target)
	outboxFieldOpValue        = "customfield_10149" // op_value (REQUIRED on Add-Label/Set-Version) — the label / version to apply
	outboxFieldIdempotencyKey = "customfield_10150" // idempotency_key — bead-id + op (+ value)

	// outboxFieldLabels / outboxFieldVersion are the GBFSM custom fields that carry
	// the N triage labels and the recommended Affects-Version as FIELDS on the ONE
	// upserted GBFSM ticket (1:1 reshape, Phase 2 ka-0000001491) — retiring the
	// per-mutation Add-Label / Set-Version intents (dormant until Phase 4).
	//
	// DELIBERATELY EMPTY (like outboxTypeAnalysis): arming is operator-owned and
	// per-environment. The sandbox scaffold DID land these fields (2026-07-11: labels
	// = customfield_10153, version = customfield_10154; see
	// gardenio/tests/e2e/fixtures/outbox-rules/README.md), but the ids are
	// environment-specific, so they are supplied at deploy time via
	// JIRA_OUTBOX_LABELS_FIELD / JIRA_OUTBOX_VERSION_FIELD rather than hardcoded here.
	// While empty they are OMITTED from the create/edit body (the Description still
	// carries the classification in prose), so the writer works unarmed and is armed
	// — without a code change — by wiring those env vars.
	outboxFieldLabels  = ""
	outboxFieldVersion = ""
)

// outboxIdempotencyKey builds the idempotency key for a GBFSM write: a leading key
// (bead-id for a dormant intent, else FSM-N for the collapsed GBFSM ticket) + op,
// plus the value when present so distinct labels/versions do not collide while a
// retry of the IDENTICAL write maps to the same key.
func outboxIdempotencyKey(leadKey, op, value string) string {
	key := strings.TrimSpace(leadKey) + ":" + op
	if v := strings.TrimSpace(value); v != "" {
		key += ":" + v
	}
	return key
}

// GBFSMTicket is the ONE upserted GBFSM ticket per triaged FSM ticket (1:1 reshape,
// Phase 2 ka-0000001491). It is BOTH the analysis surface AND the carrier of every
// mutation the operator-owned Automation rules apply to FSM:
//
//   - Description = the full triage analysis (Body).
//   - the labels field (LabelsFieldID) = the N triage classification labels.
//   - the version field (VersionFieldID) = the recommended Affects-Version.
//   - fsm_target_key (customfield_10148) = the FSM ticket it corresponds to — the
//     AUTHORITATIVE, clickable 1:1 link, carried ONLY as DATA (never the create
//     project), so the GBFSM-only guarantee holds.
//   - idempotency_key (customfield_10150) keyed on FSM-N (not the bead id), so a
//     re-triage of the SAME FSM ticket UPSERTS rather than piling up a second one.
//
// This SUPERSEDES the fan-out (1 Analysis + N Add-Label + 1 Set-Version + 1
// link-back intent) with ONE ticket. GBFSM writes never email a customer (epic
// ka-0000002067 defense-in-depth #3), so the analysis may quote FSM content for the
// FSM team's internal audience without any customer-visible/notifying risk.
type GBFSMTicket struct {
	// IssueTypeID is the GBFSM "Analysis" issue-type id (outboxTypeAnalysis, or an
	// operator override). Empty ⇒ the scaffold is not yet in place and
	// CreateGBFSMTicket fails closed (sends nothing) — the surface stays inert.
	IssueTypeID string
	// FSMTargetKey is the FSM issue key this ticket corresponds to (e.g. "FSM-408").
	// Carried as DATA in the fsm_target_key custom field — NEVER the create
	// project, mirroring OutboxIntent so the same GBFSM-only guarantee holds.
	FSMTargetKey string
	// FSMTargetURL is the human-facing Jira browse URL of the FSM ticket
	// (JiraClient.BrowseURL(FSMTargetKey)). When set, the Description renders a
	// leading clickable "FSM ticket: <key>" hyperlink so a reader of the GBFSM
	// ticket reaches its FSM ticket in ONE click — the GBFSM→FSM half of the 1:1
	// bidirectional link (Phase 3 ka-0000001070). DISPLAY-ONLY: the link is embedded
	// in the GBFSM ticket's OWN Description, so it is a pure GBFSM write that never
	// touches FSM (the ZERO-FSM-write invariant is untouched — rendering a link that
	// POINTS at FSM is not a write to FSM). Empty ⇒ the Description carries no link
	// (a client with no base URL configured still works); the fsm_target_key custom
	// field remains the authoritative machine-readable link regardless.
	FSMTargetURL string
	// Summary is the ticket's one-line summary (board legibility). Empty ⇒ a
	// generated default ("Gardenio analysis: <target>").
	Summary string
	// Body is the full analysis rendered into the ticket Description (ADF).
	// Required — an empty body fails closed (a GBFSM ticket with nothing in it
	// is not worth creating and would just be GBFSM noise).
	Body string
	// IdempotencyKey deduplicates retries (keyed on FSM-N); written to the
	// idempotency_key custom field so a cross-restart SSE replay is a no-op
	// operator-side, exactly like an intent.
	IdempotencyKey string
	// Labels are the N triage classification labels written to the labels field
	// (LabelsFieldID). Omitted from the wire body when LabelsFieldID is empty (the
	// field id is not configured for this environment) or Labels is empty.
	Labels []string
	// LabelsFieldID is the GBFSM custom-field id for Labels (outboxFieldLabels, or
	// an operator override). Empty ⇒ the labels are omitted (see outboxFieldLabels).
	LabelsFieldID string
	// Version is the recommended Affects-Version written to the version field
	// (VersionFieldID). Omitted when VersionFieldID or Version is empty.
	Version string
	// VersionFieldID is the GBFSM custom-field id for Version (outboxFieldVersion, or
	// an operator override). Empty ⇒ the version is omitted (see outboxFieldVersion).
	VersionFieldID string
}

// mutationFields returns the Description + labels + version fields shared by the
// create and edit bodies (the mutable content of the single GBFSM ticket). It
// NEVER includes project / issuetype / fsm_target_key / idempotency_key — those
// are set once at create and are immutable (fsm_target_key is the load-bearing
// link + upsert key). The labels / version fields are included only when their
// (operator-configured) field id is non-empty, so the writer is safe before the
// fields are scaffolded.
func (t GBFSMTicket) mutationFields() map[string]any {
	fields := map[string]any{"description": t.descriptionADF()}
	if lf := strings.TrimSpace(t.LabelsFieldID); lf != "" && len(t.Labels) > 0 {
		fields[lf] = append([]string(nil), t.Labels...)
	}
	if vf := strings.TrimSpace(t.VersionFieldID); vf != "" {
		if v := strings.TrimSpace(t.Version); v != "" {
			fields[vf] = v
		}
	}
	return fields
}

// DedupKey is the stable "gbfsm-outbox:<fsm>:<sorted-labels>:<version>" key that
// identifies an upsert by its EFFECT — the FSM ticket plus the classification
// (labels + version) that would land on the GBFSM ticket. Keyed on FSMTargetKey,
// not any bead id, so a re-triage of the same FSM ticket from a different bead
// maps to the same slot; an identical replay within the TTL window is a no-op,
// while a CHANGED classification produces a new key and re-fires.
//
// It is the SINGLE source of truth for the dedup identity shared by the live
// upsert (syncGBFSMTicket) and the author-ahead backfill (jira_backfill.go), so
// the two paths can never drift apart on what counts as "the same write".
func (t GBFSMTicket) DedupKey() string {
	sorted := append([]string(nil), t.Labels...)
	sort.Strings(sorted)
	return "gbfsm-outbox:" + t.FSMTargetKey + ":" + strings.Join(sorted, ",") + ":" + t.Version
}

// descriptionADF renders the GBFSM ticket Description as ADF: when FSMTargetURL is
// set, a leading paragraph carrying a clickable "FSM ticket: <key>" hyperlink (the
// GBFSM→FSM half of the 1:1 bidirectional link, Phase 3 ka-0000001070) followed by
// the analysis body paragraph; otherwise the plain body paragraph alone.
//
// The link is DISPLAY metadata inside the GBFSM ticket's OWN Description — a pure
// GBFSM write. It renders a hyperlink that POINTS at the FSM browse URL; it does NOT
// write to FSM, so the ZERO-FSM-write invariant (epic ka-0000002067) is untouched.
func (t GBFSMTicket) descriptionADF() map[string]any {
	body := adfInlineText(strings.TrimSpace(t.Body))
	target := strings.TrimSpace(t.FSMTargetKey)
	linkURL := strings.TrimSpace(t.FSMTargetURL)
	if target == "" || linkURL == "" {
		return adfDocParagraphs(body)
	}
	link := []any{
		map[string]any{"type": "text", "text": "FSM ticket: "},
		adfLinkText(target, linkURL),
	}
	return adfDocParagraphs(link, body)
}

// CreateGBFSMTicket creates the ONE GBFSM ticket for a triaged FSM ticket and
// returns its issue key (e.g. "GBFSM-501" / sandbox "GFO-501") so the caller — or
// a future re-triage via SearchGBFSM — can upsert it.
//
// Like CreateOutboxIntent, the create project is HARDCODED to outboxProjectID —
// the FSMTargetKey travels only in the fsm_target_key custom field, so no
// caller-supplied key can redirect the create off GBFSM. This preserves the same
// structural, API-level guarantee: the bridge's only create paths both land in
// the internal outbox and can never touch FSM or a customer-facing project.
//
// Fail-closed: an empty issue-type id (the scaffold is not yet provisioned — the
// default), an empty FSM target key, or an empty body returns an error and sends
// no request. The empty-issue-type case is the load-bearing inertness guarantee:
// until the operator scaffolds the "Analysis" GBFSM issue type and it is wired in
// (const bump or JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE), this method never POSTs.
func (c *JiraClient) CreateGBFSMTicket(ctx context.Context, ticket GBFSMTicket) (string, error) {
	typeID := strings.TrimSpace(ticket.IssueTypeID)
	if typeID == "" {
		return "", fmt.Errorf("outbox: GBFSM ticket has no issue-type id (scaffold pending) — no request sent")
	}
	target := strings.TrimSpace(ticket.FSMTargetKey)
	if target == "" {
		return "", fmt.Errorf("outbox: GBFSM ticket has empty fsm_target_key")
	}
	if strings.TrimSpace(ticket.Body) == "" {
		return "", fmt.Errorf("outbox: GBFSM ticket for %s has empty body", target)
	}

	summary := strings.TrimSpace(ticket.Summary)
	if summary == "" {
		summary = fmt.Sprintf("Gardenio analysis: %s", target)
	}

	// The HARDCODED GBFSM project, the issue type, the summary, the FSM target key
	// (DATA), the analysis + labels + version content, and the idempotency key.
	// project.id is outboxProjectID and nothing else — the FSM key never leaves the
	// custom field.
	fields := ticket.mutationFields()
	fields["project"] = map[string]any{"id": outboxProjectID}
	fields["issuetype"] = map[string]any{"id": typeID}
	fields["summary"] = summary
	fields[outboxFieldTargetKey] = target
	if k := strings.TrimSpace(ticket.IdempotencyKey); k != "" {
		fields[outboxFieldIdempotencyKey] = k
	}

	// Capture the created issue key from the create response so a subsequent
	// re-triage can upsert it. The issue-collection POST is safe only because
	// project.id above is hardcoded to GBFSM (see CreateOutboxIntent).
	var created struct {
		Key string `json:"key"`
	}
	reqBody := map[string]any{"fields": fields}
	if err := c.doJSON(ctx, http.MethodPost, "/rest/api/3/issue", reqBody, &created); err != nil {
		return "", fmt.Errorf("outbox: create GBFSM ticket for %s: %w", target, err)
	}
	return created.Key, nil
}

// SearchGBFSM finds the existing GBFSM ticket (if any) whose fsm_target_key custom
// field equals fsmTargetKey — the read half of the upsert. It is a READ-ONLY GET (a
// JQL search scoped to the GBFSM project), never a write.
//
// SECURITY (Phase 2 fail-closed guard): the returned issue is EditGBFSMTicket's edit
// target, so it MUST be a GBFSM-project ticket. Two checks ensure a crafted
// fsm_target_key can never surface a non-GBFSM ticket as the edit target:
//
//  1. The JQL is scoped `project = <outboxProjectID>`, so Jira only searches GBFSM.
//  2. Every candidate is re-verified: a result whose project id ≠ outboxProjectID is
//     REJECTED (skipped) — even from a compromised/buggy Jira — so it can NEVER
//     become the edit key. (The sandbox key is "GFO", prod "GBFSM"; the key prefix
//     is environment-dependent, so the numeric project id is the authoritative check.)
//
// The JQL uses the EXACT `=` operator (not the fuzzy `~`). `~` is a full-text CONTAINS
// that can suffer an index/tokenizer MISS silently returning empty — which would fall
// through to a second CreateGBFSMTicket, i.e. a DUPLICATE GBFSM ticket for one FSM-N.
// `=` matches the whole stored field value (on a text searcher it is analyzed/case-
// insensitive, not byte-literal — but FSM keys are canonical like "FSM-408", so a
// present ticket is never missed on a tokenization quirk). The byte-exact guarantee is
// the client-side re-verification below, NOT the operator: each candidate's
// fsm_target_key must EQUAL the target verbatim (belt-and-suspenders), so `=` never
// loosens the security guard. This DOES require fsm_target_key (customfield_10148) to be
// a field type whose searcher supports `=` (short/single-line text, NOT a free-text/
// paragraph field) — pinned at scaffold time and verified before arming (see
// docs/gbfsm-outbox-arming-checklist.md). If the field is mistyped so `=` is unsupported,
// the search 400s and the caller fail-safes (logs + no write — never a silent
// duplicate). Returns (nil, nil) when none exists ⇒ the caller creates a fresh one
// (GBFSM-hardcoded, always safe).
func (c *JiraClient) SearchGBFSM(ctx context.Context, fsmTargetKey string) (*JiraIssue, error) {
	target := strings.TrimSpace(fsmTargetKey)
	if target == "" {
		return nil, fmt.Errorf("outbox: SearchGBFSM has empty fsm_target_key")
	}

	// cf[<num>] references the fsm_target_key custom field by its numeric id. `=` is
	// the exact operator (see the doc comment above) — never the fuzzy `~`.
	cfNum := strings.TrimPrefix(outboxFieldTargetKey, "customfield_")
	jql := fmt.Sprintf("project = %s AND cf[%s] = %s ORDER BY created ASC",
		outboxProjectID, cfNum, jqlQuote(target))

	page, err := c.SearchIssuesPage(ctx, jql, []string{"project", outboxFieldTargetKey}, 5, "")
	if err != nil {
		return nil, fmt.Errorf("outbox: search GBFSM for %s: %w", target, err)
	}
	// Collect every GBFSM-verified exact match. The upsert returns the OLDEST (JQL is
	// ORDER BY created ASC) so re-triage converges on one canonical ticket, but we
	// COUNT all matches: >1 means duplicates have already accrued for this FSM-N, which
	// the upsert cannot self-heal (it only ever edits the oldest). Warn so the
	// at-most-one-per-FSM-N invariant on the arming checklist is observable in the
	// bridge logs. GBFSM-side quality only — no isolation invariant is at risk: the
	// edit still targets a single search-verified GBFSM key.
	var chosen *JiraIssue
	matches := 0
	for i := range page.Issues {
		issue := &page.Issues[i]
		// Guard (2): reject any result not provably in the GBFSM project. This is
		// the load-bearing check that a subsequent edit can never target an FSM (or
		// any non-GBFSM) key regardless of what fsm_target_key was supplied.
		if pid := issueProjectID(issue); pid != outboxProjectID {
			continue
		}
		// Belt-and-suspenders: even with `=`, require the fsm_target_key to EQUAL the
		// target verbatim, so a compromised/buggy Jira (or a field still mistyped to
		// honor `~`) can never surface a near-match ("FSM-40" never matches "FSM-408").
		if v, _ := CustomFieldString(issue.RawFields[outboxFieldTargetKey]); v != target {
			continue
		}
		matches++
		if chosen == nil {
			chosen = issue // oldest, since JQL is ORDER BY created ASC
		}
	}
	if matches > 1 && c.logger != nil {
		c.logger.Warn("outbox: multiple GBFSM tickets match one fsm_target_key — duplicates have accrued; "+
			"the upsert converges on the oldest but cannot self-heal the rest "+
			"(assert at-most-one per FSM-N: see docs/gbfsm-outbox-arming-checklist.md)",
			"fsm_target_key", target, "match_count", matches, "chosen_key", chosen.Key)
	}
	return chosen, nil
}

// EditGBFSMTicket upserts the GBFSM ticket named by gbfsmKey — the key returned by
// SearchGBFSM, already verified to be in the GBFSM project — a PUT to
// /rest/api/3/issue/{gbfsmKey} setting Description + labels + version to the latest
// triage classification.
//
// The edit target is ALWAYS the search-verified GBFSM key, NEVER an FSM key:
// fsm_target_key is immutable DATA and deliberately NOT part of the edit body
// (mutationFields excludes it), so re-triage cannot re-point the link, and the URL
// path is built solely from that key. Fail-closed on an empty key.
func (c *JiraClient) EditGBFSMTicket(ctx context.Context, gbfsmKey string, ticket GBFSMTicket) error {
	key := strings.TrimSpace(gbfsmKey)
	if key == "" {
		return fmt.Errorf("outbox: EditGBFSMTicket has empty GBFSM key — refusing to edit")
	}
	if strings.TrimSpace(ticket.Body) == "" {
		return fmt.Errorf("outbox: GBFSM ticket %s edit has empty body", key)
	}
	reqBody := map[string]any{"fields": ticket.mutationFields()}
	if err := c.doJSON(ctx, http.MethodPut, "/rest/api/3/issue/"+url.PathEscape(key), reqBody, nil); err != nil {
		return fmt.Errorf("outbox: edit GBFSM ticket %s: %w", key, err)
	}
	return nil
}

// issueProjectID extracts the numeric project id from a searched issue's raw
// "project" field ({"id":"10037",...}). Returns "" when absent/unparseable — which
// SearchGBFSM treats as a non-GBFSM result and rejects (fail-closed).
func issueProjectID(issue *JiraIssue) string {
	raw, ok := issue.RawFields["project"]
	if !ok {
		return ""
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return strings.TrimSpace(p.ID)
}

// jqlQuote wraps s in a JQL string literal, escaping backslash and double-quote so
// a crafted fsm_target_key cannot break out of the quoted value. The value is
// always a bead-derived FSM key in practice, but the write path treats it as
// untrusted DATA (epic ka-0000002067), so the search JQL is escaped defensively.
func jqlQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
