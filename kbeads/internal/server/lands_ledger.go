package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Lands credit ledger and rate table (spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-track-b-design.md §3.2, §3.4, B-D7; step 8.1).
//
// A tenant land's purse is a ledger of credit_grant beads. One credit is US$1
// billed to the tenant, and the land is charged its LiteLLM spend times the
// llm_multiplier of the credit_rate in force (OQ1 gd-7sTt20714N: markup). The
// operator writes both with gb lands; the types are admin-gated, so the rules
// here are about shape, not who:
//
//   - a credit_grant carries exactly one project label, project:<land>, and
//     its land is a tenant land;
//   - its amount is a nonzero number, and a negative one (a refund or a
//     correction) names a reason;
//   - a credit_rate charges a positive multiplier and no negative compute rate;
//   - neither is edited after it is written: a wrong grant is corrected by a
//     second grant, a wrong rate by the next version.
//
// The tenant-land rule on type:project lives here too: a tenant land keeps
// land_slack_mirror off (B-D11).

const (
	typeCreditGrant model.BeadType = "credit_grant"
	typeCreditRate  model.BeadType = "credit_rate"
)

// landKindTenant is the land_kind of a stranger's land.
const landKindTenant = "tenant"

type creditGrantFields struct {
	Land          string   `json:"land"`
	AmountCredits *float64 `json:"amount_credits"`
	Reason        string   `json:"reason"`
}

type creditRateFields struct {
	LLMMultiplier              *float64 `json:"llm_multiplier"`
	ComputeCreditsPerAgentHour *float64 `json:"compute_credits_per_agent_hour"`
}

// creditGrantLabelError reports a label set that breaks the ledger rule:
// exactly one project label, and it names the grant's land.
func creditGrantLabelError(labels []string, land string) error {
	projects := projectLabelValues(labels)
	if len(projects) == 1 && projects[0] == land {
		return nil
	}
	if len(projects) == 0 {
		return fmt.Errorf("a credit_grant needs the label project:%s", land)
	}
	return fmt.Errorf("a credit_grant carries exactly one project label, project:%s; got %s", land, strings.Join(projects, ", "))
}

// validateCreditGrantCreate enforces the create-time rules: the label, a
// nonzero amount, a reason on a negative one, and a land that is a tenant
// land. The project lookup reads through the store, never the caller's scope.
func validateCreditGrantCreate(ctx context.Context, st store.Store, bead *model.Bead) error {
	var f creditGrantFields
	if len(bead.Fields) > 0 {
		_ = json.Unmarshal(bead.Fields, &f)
	}
	f.Land = strings.TrimSpace(f.Land)
	if f.Land == "" {
		return fmt.Errorf("a credit_grant names its land")
	}
	if err := creditGrantLabelError(bead.Labels, f.Land); err != nil {
		return err
	}
	if f.AmountCredits == nil || *f.AmountCredits == 0 || math.IsNaN(*f.AmountCredits) || math.IsInf(*f.AmountCredits, 0) {
		return fmt.Errorf("amount_credits must be a nonzero number")
	}
	if *f.AmountCredits < 0 && strings.TrimSpace(f.Reason) == "" {
		return fmt.Errorf("a negative credit_grant (a refund or a correction) needs a reason")
	}
	project, err := st.FindProjectByName(ctx, f.Land)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && project == nil):
		return fmt.Errorf("land %q is not a project", f.Land)
	case err != nil:
		return fmt.Errorf("project lookup failed: %w", err)
	}
	var pf struct {
		LandKind string `json:"land_kind"`
	}
	if len(project.Fields) > 0 {
		_ = json.Unmarshal(project.Fields, &pf)
	}
	if pf.LandKind != landKindTenant {
		return fmt.Errorf("land %q is not a tenant land (land_kind=%q); only tenant lands have a purse", f.Land, pf.LandKind)
	}
	return nil
}

// validateCreditRateFields checks the amounts on a rate version.
func validateCreditRateFields(raw json.RawMessage) error {
	var f creditRateFields
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &f)
	}
	if f.LLMMultiplier == nil || !(*f.LLMMultiplier > 0) || math.IsInf(*f.LLMMultiplier, 0) {
		return fmt.Errorf("llm_multiplier must be a number greater than 0 (credits charged per US$1 of LiteLLM spend)")
	}
	if c := f.ComputeCreditsPerAgentHour; c != nil && (!(*c >= 0) || math.IsInf(*c, 0)) {
		return fmt.Errorf("compute_credits_per_agent_hour must not be negative")
	}
	return nil
}

// isLedgerRecordType reports whether beadType is a credit_grant or a
// credit_rate.
func isLedgerRecordType(beadType model.BeadType) bool {
	return beadType == typeCreditGrant || beadType == typeCreditRate
}

// ledgerRecordEditedError is the refusal for any field change on a ledger
// record, nil for every other type.
func ledgerRecordEditedError(beadType model.BeadType) error {
	switch beadType {
	case typeCreditGrant:
		return fmt.Errorf("a credit_grant is a ledger entry and is never edited; correct it with another grant (gb lands credit add with a negative amount and a reason)")
	case typeCreditRate:
		return fmt.Errorf("a credit_rate version is never edited; write the next version with gb lands rates set")
	}
	return nil
}

// ledgerRecordUpdateError refuses a field change on a credit_grant or a
// credit_rate. prior and next are the field blobs before and after the merge;
// a PATCH that leaves them equal (a status or note change) passes.
func ledgerRecordUpdateError(beadType model.BeadType, prior, next json.RawMessage) error {
	if !isLedgerRecordType(beadType) || jsonFieldsEqual(prior, next) {
		return nil
	}
	return ledgerRecordEditedError(beadType)
}

// ledgerFieldEndpointDenied gates the field-level write endpoints
// (/increment, /fields/{f}/edit|append|reorder) and a close body's extra
// fields on a ledger record. Those write through store primitives and never
// reach updateBead's write_policy or ledgerRecordUpdateError, so this runs
// both: a caller below the type's min_role (gardenio-admin; the static token,
// a fleet token, an operator) gets the 403 a PATCH would, and anyone else
// gets the 400, because a grant or a rate version is never edited. It writes
// the response and reports whether the caller must stop.
func (s *BeadsServer) ledgerFieldEndpointDenied(w http.ResponseWriter, r *http.Request, action string, bead *model.Bead) bool {
	if bead == nil || !isLedgerRecordType(bead.Type) {
		return false
	}
	if denial := s.authorizeWriteForBead(r.Context(), action, bead); denial != nil {
		s.auditDenial(r, denial, bead.ID)
		writeAuthzDenial(w, denial)
		return true
	}
	writeError(w, http.StatusBadRequest, ledgerRecordEditedError(bead.Type).Error())
	return true
}

// creditGrantLabelChangeError refuses moving a credit_grant to another land:
// its project labels never change.
func creditGrantLabelChangeError(bead *model.Bead, label string, adding bool) error {
	if bead == nil || bead.Type != typeCreditGrant || !strings.HasPrefix(label, "project:") {
		return nil
	}
	if adding && slices.Contains(bead.Labels, label) {
		return nil
	}
	return fmt.Errorf("a credit_grant's project label is fixed when it is written")
}

// creditGrantLabelsUpdateError is the PATCH twin of creditGrantLabelChangeError.
func creditGrantLabelsUpdateError(prior, next []string) error {
	a, b := projectLabelValues(prior), projectLabelValues(next)
	slices.Sort(a)
	slices.Sort(b)
	if slices.Equal(a, b) {
		return nil
	}
	return fmt.Errorf("a credit_grant's project label is fixed when it is written")
}

// tenantLandMirrorError keeps a tenant land's conversation out of the fleet
// Slack (B-D11): land_kind=tenant with land_slack_mirror=on is refused.
func tenantLandMirrorError(raw json.RawMessage) error {
	var f struct {
		LandKind        string `json:"land_kind"`
		LandSlackMirror string `json:"land_slack_mirror"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &f)
	}
	if f.LandKind == landKindTenant && f.LandSlackMirror == "on" {
		return fmt.Errorf("a tenant land keeps land_slack_mirror off: its conversation never reaches the fleet Slack (Track B B-D11)")
	}
	return nil
}

// jsonFieldsEqual compares two field blobs as JSON values, so key order and
// spacing do not count as a change. An empty blob equals {}.
func jsonFieldsEqual(a, b json.RawMessage) bool {
	norm := func(raw json.RawMessage) any {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || string(raw) == "null" {
			return map[string]any{}
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return string(raw)
		}
		return v
	}
	x, _ := json.Marshal(norm(a))
	y, _ := json.Marshal(norm(b))
	return bytes.Equal(x, y)
}
