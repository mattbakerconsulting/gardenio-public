package model

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validBead returns a Bead that passes all validation rules.
func validBead() Bead {
	return Bead{
		Title:    "Implement login flow",
		Priority: 2,
		Status:   StatusOpen,
		Type:     TypeTask,
		Kind:     KindIssue,
	}
}

// fieldErrors extracts a *ValidationError from err or fails the test.
func fieldErrors(t *testing.T, err error) []FieldError {
	t.Helper()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	return ve.Errors
}

// hasFieldError reports whether the error list contains an error for the given field.
func hasFieldError(errs []FieldError, field string) bool {
	for _, fe := range errs {
		if fe.Field == field {
			return true
		}
	}
	return false
}

func TestValidate_TitleRequired(t *testing.T) {
	b := validBead()
	b.Title = ""
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "title") {
		t.Error("expected error on field 'title' for empty title")
	}
}

func TestValidate_TitleWhitespaceOnly(t *testing.T) {
	b := validBead()
	b.Title = "   \t\n  "
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "title") {
		t.Error("expected error on field 'title' for whitespace-only title")
	}
}

func TestValidate_TitleTooLong(t *testing.T) {
	b := validBead()
	b.Title = strings.Repeat("a", 501)
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "title") {
		t.Error("expected error on field 'title' for title exceeding 500 chars")
	}
}

func TestValidate_TitleExactly500(t *testing.T) {
	b := validBead()
	b.Title = strings.Repeat("a", 500)
	if err := ValidateBead(&b); err != nil {
		t.Errorf("title with exactly 500 chars should be valid, got: %v", err)
	}
}

func TestValidate_ShortTitleTooLong(t *testing.T) {
	b := validBead()
	b.ShortTitle = strings.Repeat("a", ShortTitleMax+1)
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "short_title") {
		t.Fatal("expected error on field 'short_title' for 41 runes")
	}
	if got := ValidateBead(&b).Error(); !strings.Contains(got, "short_title: must be 40 characters or fewer") {
		t.Errorf("error = %q, want it to contain %q", got, "short_title: must be 40 characters or fewer")
	}
}

func TestValidate_ShortTitleExactly40(t *testing.T) {
	b := validBead()
	// 40 multibyte runes (120 bytes): the limit counts runes, not bytes.
	b.ShortTitle = strings.Repeat("é", 20) + strings.Repeat("🌱", 20)
	if err := ValidateBead(&b); err != nil {
		t.Errorf("short_title with exactly 40 runes should be valid, got: %v", err)
	}
}

func TestValidate_ShortTitleNewlineRejected(t *testing.T) {
	for _, st := range []string{"forward duration_ms\nthrough relay", "forward\r\nrelay"} {
		b := validBead()
		b.ShortTitle = st
		errs := fieldErrors(t, ValidateBead(&b))
		if !hasFieldError(errs, "short_title") {
			t.Errorf("expected error on field 'short_title' for %q", st)
		}
	}
}

func TestValidate_ShortTitleEmptyIsUnset(t *testing.T) {
	b := validBead()
	b.ShortTitle = ""
	if err := ValidateBead(&b); err != nil {
		t.Errorf("empty short_title should be valid (unset), got: %v", err)
	}
}

// TestShortTitleHelp_ExampleIsValid: the rendered example in the one help
// text (kd flag, gate error, advice line) must itself be a valid short_title.
func TestShortTitleHelp_ExampleIsValid(t *testing.T) {
	b := validBead()
	b.ShortTitle = NormalizeShortTitle(ShortTitleExample)
	if b.ShortTitle != ShortTitleExample {
		t.Errorf("ShortTitleExample %q is not normalized (got %q)", ShortTitleExample, b.ShortTitle)
	}
	if err := ValidateBead(&b); err != nil {
		t.Errorf("ShortTitleExample %q (%d runes) is not a valid short_title: %v", ShortTitleExample, len([]rune(ShortTitleExample)), err)
	}
	if !strings.Contains(ShortTitleHelp, "Rendered example: 🌱 "+ShortTitleExample+" ") {
		t.Errorf("ShortTitleHelp does not render ShortTitleExample: %q", ShortTitleHelp)
	}
}

// TestShortTitleHelp_DocQuotesIt: gardenio/docs/short-titles.md quotes
// ShortTitleHelp verbatim in a fenced block, so the doc cannot drift from the
// text kd, the daemon and the advice line show.
func TestShortTitleHelp_DocQuotesIt(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "gardenio", "docs", "short-titles.md"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("gardenio/docs/short-titles.md not present (kbeads checked out alone)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "```text\n"+ShortTitleHelp+"\n```") {
		t.Errorf("gardenio/docs/short-titles.md does not quote model.ShortTitleHelp verbatim in a ```text block; update it to:\n%s", ShortTitleHelp)
	}
}

// TestHeraldryHelp_DocQuotesIt: gardenio/docs/heraldry/README.md quotes
// HeraldryHelp verbatim in a fenced block, as short-titles.md quotes
// ShortTitleHelp, so the page cannot drift from what kd and the daemon say.
func TestHeraldryHelp_DocQuotesIt(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "gardenio", "docs", "heraldry", "README.md"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("gardenio/docs/heraldry/README.md not present (kbeads checked out alone)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "```text\n"+HeraldryHelp+"\n```") {
		t.Errorf("gardenio/docs/heraldry/README.md does not quote model.HeraldryHelp verbatim in a ```text block; update it to:\n%s", HeraldryHelp)
	}
}

func TestNormalizeShortTitle(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "",
		"   ":                   "",
		"  fix   the\tbridge  ": "fix the bridge",
		"forward duration_ms":   "forward duration_ms",
	} {
		if got := NormalizeShortTitle(in); got != want {
			t.Errorf("NormalizeShortTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClipShortTitle(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"spawn infra agent": "spawn infra agent",
		"grant a,\n b":      "grant a, b",
		// 41 runes: cut at the last word boundary, ellipsis appended.
		"grant acme-production, acme-staging, acme": "grant acme-production, acme-staging…",
		// one unbreakable token: cut hard.
		strings.Repeat("x", 50): strings.Repeat("x", ShortTitleMax-1) + "…",
	} {
		got := ClipShortTitle(in)
		if got != want {
			t.Errorf("ClipShortTitle(%q) = %q, want %q", in, got, want)
		}
		b := validBead()
		b.ShortTitle = got
		if err := ValidateBead(&b); err != nil {
			t.Errorf("ClipShortTitle(%q) = %q fails ValidateBead: %v", in, got, err)
		}
	}
}

func TestValidate_TitleValid(t *testing.T) {
	b := validBead()
	b.Title = "A perfectly normal title"
	if err := ValidateBead(&b); err != nil {
		t.Errorf("expected no error for valid title, got: %v", err)
	}
}

func TestValidate_PriorityTooLow(t *testing.T) {
	b := validBead()
	b.Priority = -1
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "priority") {
		t.Error("expected error on field 'priority' for value -1")
	}
}

func TestValidate_PriorityTooHigh(t *testing.T) {
	b := validBead()
	b.Priority = 5
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "priority") {
		t.Error("expected error on field 'priority' for value 5")
	}
}

func TestValidate_PriorityZero(t *testing.T) {
	b := validBead()
	b.Priority = 0
	if err := ValidateBead(&b); err != nil {
		t.Errorf("priority 0 should be valid, got: %v", err)
	}
}

func TestValidate_PriorityFour(t *testing.T) {
	b := validBead()
	b.Priority = 4
	if err := ValidateBead(&b); err != nil {
		t.Errorf("priority 4 should be valid, got: %v", err)
	}
}

func TestValidate_InvalidStatus(t *testing.T) {
	b := validBead()
	b.Status = Status("bogus")
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "status") {
		t.Error("expected error on field 'status' for invalid value")
	}
}

func TestValidate_PartialStatusValid(t *testing.T) {
	// 'partial' is a first-class status (ka-0000007305 §2.3); a bead in
	// 'partial' with no closed_at must validate cleanly.
	b := validBead()
	b.Status = StatusPartial
	if err := ValidateBead(&b); err != nil {
		t.Errorf("partial bead without ClosedAt should be valid, got: %v", err)
	}
}

func TestValidate_PartialWithClosedAt(t *testing.T) {
	// Unlike 'closed', 'partial' is terminal-for-progression but does NOT
	// represent a clean completion, so it must not stamp closed_at.
	b := validBead()
	b.Status = StatusPartial
	now := time.Now()
	b.ClosedAt = &now
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "closed_at") {
		t.Error("expected error on field 'closed_at' when status is partial but ClosedAt is set")
	}
}

func TestValidate_EmptyType(t *testing.T) {
	b := validBead()
	b.Type = BeadType("")
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "type") {
		t.Error("expected error on field 'type' for empty value")
	}
}

func TestValidate_CustomTypeValid(t *testing.T) {
	b := validBead()
	b.Type = BeadType("workflow")
	if err := ValidateBead(&b); err != nil {
		t.Errorf("custom bead type 'workflow' should be valid, got: %v", err)
	}
}

func TestValidate_InvalidKind(t *testing.T) {
	b := validBead()
	b.Kind = Kind("bogus")
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "kind") {
		t.Error("expected error on field 'kind' for invalid value")
	}
}

func TestValidate_ClosedWithoutClosedAt(t *testing.T) {
	b := validBead()
	b.Status = StatusClosed
	b.ClosedAt = nil
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "closed_at") {
		t.Error("expected error on field 'closed_at' when status is closed but ClosedAt is nil")
	}
}

func TestValidate_ClosedWithClosedAt(t *testing.T) {
	b := validBead()
	b.Status = StatusClosed
	now := time.Now()
	b.ClosedAt = &now
	if err := ValidateBead(&b); err != nil {
		t.Errorf("closed bead with ClosedAt set should be valid, got: %v", err)
	}
}

func TestValidate_OpenWithClosedAt(t *testing.T) {
	b := validBead()
	b.Status = StatusOpen
	now := time.Now()
	b.ClosedAt = &now
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "closed_at") {
		t.Error("expected error on field 'closed_at' when status is open but ClosedAt is set")
	}
}

func TestValidate_FieldsInvalidJSON(t *testing.T) {
	b := validBead()
	b.Fields = json.RawMessage(`{not json}`)
	errs := fieldErrors(t, ValidateBead(&b))
	if !hasFieldError(errs, "fields") {
		t.Error("expected error on field 'fields' for invalid JSON")
	}
}

func TestValidate_FieldsValidJSON(t *testing.T) {
	b := validBead()
	b.Fields = json.RawMessage(`{"key": "value"}`)
	if err := ValidateBead(&b); err != nil {
		t.Errorf("valid JSON fields should pass, got: %v", err)
	}
}

func TestValidate_FieldsNil(t *testing.T) {
	b := validBead()
	b.Fields = nil
	if err := ValidateBead(&b); err != nil {
		t.Errorf("nil fields should pass, got: %v", err)
	}
}

func TestValidate_FullyValidBead(t *testing.T) {
	b := validBead()
	if err := ValidateBead(&b); err != nil {
		t.Errorf("expected no error for a fully valid bead, got: %v", err)
	}
}

func TestValidationError_Error(t *testing.T) {
	ve := &ValidationError{
		Errors: []FieldError{
			{Field: "title", Message: "is required"},
			{Field: "priority", Message: "must be between 0 and 4, got 9"},
		},
	}
	got := ve.Error()
	want := "validation failed: title: is required; priority: must be between 0 and 4, got 9"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestValidateAdviceLabels_GlobalPlusProjectRejected(t *testing.T) {
	errs := ValidateAdviceLabels([]string{"global", "project:beads"})
	if len(errs) == 0 {
		t.Error("expected error for global + project: combo")
	}
}

func TestValidateAdviceLabels_GlobalPlusRigRejected(t *testing.T) {
	errs := ValidateAdviceLabels([]string{"global", "rig:beads"})
	if len(errs) == 0 {
		t.Error("expected error for global + rig: combo")
	}
}

func TestValidateAdviceLabels_GlobalPlusAgentRejected(t *testing.T) {
	errs := ValidateAdviceLabels([]string{"global", "agent:beads/crews/quartz"})
	if len(errs) == 0 {
		t.Error("expected error for global + agent: combo")
	}
}

func TestValidateAdviceLabels_GlobalPlusRoleRejected(t *testing.T) {
	errs := ValidateAdviceLabels([]string{"global", "role:polecat"})
	if len(errs) == 0 {
		t.Error("expected error for global + role: combo")
	}
}

func TestValidateAdviceLabels_BareRigRejected(t *testing.T) {
	errs := ValidateAdviceLabels([]string{"rig:beads"})
	if len(errs) == 0 {
		t.Error("expected error for retired rig: label")
	}
}

func TestValidateAdviceLabels_GroupPrefixedRigRejected(t *testing.T) {
	errs := ValidateAdviceLabels([]string{"g0:rig:beads", "g0:role:polecat"})
	if len(errs) == 0 {
		t.Error("expected error for retired gN:rig: label")
	}
}

func TestValidateAdviceLabels_RigProjectConflictRejected(t *testing.T) {
	errs := ValidateAdviceLabels([]string{"rig:beads", "project:gastown"})
	if len(errs) == 0 {
		t.Error("expected error for rig: combined with project:")
	}
}

func TestValidateAdviceLabels_RigProjectDuplicateRejected(t *testing.T) {
	errs := ValidateAdviceLabels([]string{"rig:beads", "project:beads"})
	if len(errs) == 0 {
		t.Error("expected error for duplicate rig:X + project:X")
	}
}

func TestValidateAdviceLabels_GroupPrefixedCombosAlsoRejected(t *testing.T) {
	errs := ValidateAdviceLabels([]string{"g0:global", "g1:project:beads"})
	if len(errs) == 0 {
		t.Error("expected error when global and project: appear in distinct gN: groups")
	}
}

func TestValidateAdviceLabels_CanonicalFormsAccepted(t *testing.T) {
	canonical := [][]string{
		{"global"},
		{"project:beads"},
		{"role:polecat"},
		{"agent:beads/crews/quartz"},
		{"project:beads", "role:polecat"},       // project + role intersection
		{"g0:project:beads", "g0:role:polecat"}, // same via AND group
		{"g0:role:polecat", "g1:role:mayor"},    // OR of two roles
	}
	for _, labels := range canonical {
		if errs := ValidateAdviceLabels(labels); len(errs) > 0 {
			t.Errorf("canonical advice labels %v should pass, got: %v", labels, errs)
		}
	}
}

func TestValidateAdviceLabels_ExtraNonTargetingLabelsIgnored(t *testing.T) {
	// Free-form labels ("inline:true", "project-note" etc.) are not
	// targeting labels and should not affect validation.
	errs := ValidateAdviceLabels([]string{"project:beads", "inline:true", "topic:ci"})
	if len(errs) > 0 {
		t.Errorf("non-targeting labels should be ignored, got: %v", errs)
	}
}

func TestValidationError_HasErrors(t *testing.T) {
	ve := &ValidationError{}
	if ve.HasErrors() {
		t.Error("HasErrors() should be false for empty Errors slice")
	}
	ve.Errors = append(ve.Errors, FieldError{Field: "x", Message: "y"})
	if !ve.HasErrors() {
		t.Error("HasErrors() should be true when Errors is non-empty")
	}
}
