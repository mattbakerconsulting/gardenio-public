package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// ValidationError holds a list of field-level validation errors.
type ValidationError struct {
	Errors []FieldError
}

// FieldError represents a single validation failure on a named field.
type FieldError struct {
	Field   string
	Message string
}

// Error formats the validation error as a semicolon-separated list of field messages.
func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		parts[i] = fe.Field + ": " + fe.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// HasErrors reports whether the validation error contains any field errors.
func (e *ValidationError) HasErrors() bool {
	return len(e.Errors) > 0
}

// ShortTitleMax is the maximum length of Bead.ShortTitle, in runes (plan
// 2026-10-14-short-titles D1).
const ShortTitleMax = 40

// ShortTitleExample is the rendered example ShortTitleHelp shows. It must
// itself pass ValidateBead (the plan's original "forward duration_ms through
// gb hook relay" was 41 runes, one over ShortTitleMax).
const ShortTitleExample = "forward duration_ms via gb hook relay"

// ShortTitleHelp is the one text for short_title: the kd flag help, the
// daemon's gate error and the advice line all quote it (plan D4).
const ShortTitleHelp = "short_title is the name humans see for this bead: in Slack prose and the References footer, in beads-viewer lists and on agent cards. Up to 40 characters, imperative, no ids, timestamps, type words or prefixes. Rendered example: 🌱 " + ShortTitleExample + " (the glyph is added at render time; store only the words)."

// HeraldryExample is the emoji HeraldryHelp types; the heraldry package's
// tests check it parses. None of them is a theme glyph, so none is refused.
const HeraldryExample = "🔥🧭📦"

// HeraldryHelp is the one text for heraldry, a bead's arms: the kd flag
// help, the daemon's error and the advice line all quote it (heraldry spec
// R11, §3.1). Plain words only: no notation, no heraldic terms.
const HeraldryHelp = "heraldry is a bead's arms: an optional mark of three to five emoji in a row, unique in the project, for epics (plans and phases) and projects. Type them in order (" + HeraldryExample + "). Emoji that Slack cards already use for bead types and states are not in the library. 'none' removes the arms."

// NormalizeShortTitle trims a short_title and collapses each run of spaces
// and tabs to one space, so the 40-rune limit is about the words, not stray
// whitespace. Line breaks are kept so ValidateBead can reject them.
func NormalizeShortTitle(s string) string {
	var sb strings.Builder
	space := false
	for _, r := range strings.TrimSpace(s) {
		if r != '\n' && r != '\r' && unicode.IsSpace(r) {
			space = true
			continue
		}
		if space {
			sb.WriteByte(' ')
			space = false
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// ClipShortTitle makes a valid short_title out of s for a creator that derives
// one rather than having it authored (the daemon's own escalations,
// gd-lYZUr0CqY0): line breaks fold to spaces, NormalizeShortTitle collapses
// the rest, and anything over ShortTitleMax runes is cut at a word boundary
// with a trailing ellipsis. A single token too long to break is cut hard.
func ClipShortTitle(s string) string {
	s = NormalizeShortTitle(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
	runes := []rune(s)
	if len(runes) <= ShortTitleMax {
		return s
	}
	limit := ShortTitleMax - 1 // room for the ellipsis
	cut := limit
	for i := limit; i > limit/2; i-- {
		if runes[i] == ' ' {
			cut = i
			break
		}
	}
	return strings.TrimRight(string(runes[:cut]), " ,;:-") + "…"
}

// ValidateBead checks a Bead for constraint violations.
// It returns a *ValidationError if any rules fail, or nil if the bead is valid.
func ValidateBead(b *Bead) error {
	var ve ValidationError

	// Title: required and at most 500 characters.
	title := strings.TrimSpace(b.Title)
	if title == "" {
		ve.Errors = append(ve.Errors, FieldError{Field: "title", Message: "is required"})
	} else if len([]rune(title)) > 500 {
		ve.Errors = append(ve.Errors, FieldError{Field: "title", Message: "must be 500 characters or fewer"})
	}

	// ShortTitle: optional, at most ShortTitleMax runes, single line.
	if st := b.ShortTitle; st != "" {
		if len([]rune(st)) > ShortTitleMax {
			ve.Errors = append(ve.Errors, FieldError{Field: "short_title", Message: "must be 40 characters or fewer"})
		}
		if strings.ContainsAny(st, "\r\n") {
			ve.Errors = append(ve.Errors, FieldError{Field: "short_title", Message: "must be a single line"})
		}
	}

	// Priority: must be 0-4.
	if b.Priority < 0 || b.Priority > 4 {
		ve.Errors = append(ve.Errors, FieldError{
			Field:   "priority",
			Message: fmt.Sprintf("must be between 0 and 4, got %d", b.Priority),
		})
	}

	// Status: must be a valid enum value (closed set).
	if !b.Status.IsValid() {
		ve.Errors = append(ve.Errors, FieldError{
			Field:   "status",
			Message: fmt.Sprintf("invalid value %q", b.Status),
		})
	}

	// Type: must be non-empty (bead types are extensible).
	if strings.TrimSpace(string(b.Type)) == "" {
		ve.Errors = append(ve.Errors, FieldError{
			Field:   "type",
			Message: "is required",
		})
	}

	// Kind: must be a valid enum value (closed set).
	if !b.Kind.IsValid() {
		ve.Errors = append(ve.Errors, FieldError{
			Field:   "kind",
			Message: fmt.Sprintf("invalid value %q", b.Kind),
		})
	}

	// ClosedAt consistency with Status.
	if b.Status == StatusClosed && b.ClosedAt == nil {
		ve.Errors = append(ve.Errors, FieldError{
			Field:   "closed_at",
			Message: "is required when status is closed",
		})
	}
	if b.Status != StatusClosed && b.ClosedAt != nil {
		ve.Errors = append(ve.Errors, FieldError{
			Field:   "closed_at",
			Message: "must be nil when status is not closed",
		})
	}

	// Fields: must be valid JSON if present.
	if len(b.Fields) > 0 && !json.Valid(b.Fields) {
		ve.Errors = append(ve.Errors, FieldError{
			Field:   "fields",
			Message: "contains invalid JSON",
		})
	}

	if ve.HasErrors() {
		return &ve
	}
	return nil
}

// ValidateAdviceLabels enforces advice-targeting label rules. It is
// separate from ValidateBead so the server can choose when to apply it:
// always on create, but only on update when labels are being changed —
// so existing advice with historically-permitted but now-rejected
// combinations can still be updated on other fields. The migration
// (`kd admin migrate advice-targeting`) is the supported path for
// fixing the grandfathered labels themselves.
//
// Returns a slice of FieldError (empty on success) rather than
// wrapping in a ValidationError so callers can combine results.
func ValidateAdviceLabels(labels []string) []FieldError {
	return validateAdviceLabels(labels)
}

// validateAdviceLabels enforces the rules that prevent authors from
// writing ambiguous targeting combinations on an advice bead. These
// combinations were valid historically but have surprising delivery
// semantics — the validator rejects them so the author has to pick
// an unambiguous form (see advice.go MatchesSubscriptions for the
// actual matching rules).
//
// Rules:
//  1. `rig:X` is retired. Any `rig:` label is rejected outright; use
//     `project:X` instead. The operator-gated migration tool rewrites
//     historical rig: labels to project:.
//  2. `global` with any `project:/agent:` label is invalid: the
//     project/agent pre-filter narrows delivery regardless of the
//     OR across unprefixed labels, so `global` is either dead code or
//     the author misunderstood the semantics.
//  3. `global` with `role:` is invalid for the opposite reason — `global`
//     wins the OR and role: has no pre-filter, so the advice delivers
//     to everyone and the role: tag is misleading.
//
// All rules operate on the stripped form of the label (gN: prefixes
// removed) to catch cases like `g0:global` + `g1:project:X`.
func validateAdviceLabels(labels []string) []FieldError {
	hasGlobal := false
	hasProject := false
	hasAgent := false
	hasRole := false
	var rigLabels []string

	for _, l := range labels {
		clean := StripGroupPrefix(l)
		switch {
		case clean == "global":
			hasGlobal = true
		case strings.HasPrefix(clean, "project:"):
			hasProject = true
		case strings.HasPrefix(clean, "rig:"):
			rigLabels = append(rigLabels, l)
		case strings.HasPrefix(clean, "agent:"):
			hasAgent = true
		case strings.HasPrefix(clean, "role:"):
			hasRole = true
		}
	}

	var errs []FieldError

	for _, rl := range rigLabels {
		errs = append(errs, FieldError{
			Field:   "labels",
			Message: fmt.Sprintf(`advice targeting label %q has been retired; use "project:" instead`, rl),
		})
	}

	if hasGlobal && (hasProject || hasAgent) {
		errs = append(errs, FieldError{
			Field: "labels",
			Message: `advice cannot combine "global" with a targeting label (project:/agent:); ` +
				`"global" means everyone and targeting narrows to a scope — pick one`,
		})
	}
	if hasGlobal && hasRole {
		errs = append(errs, FieldError{
			Field: "labels",
			Message: `advice cannot combine "global" with a role: label; ` +
				`role: has no required-match pre-filter, so "global" alone would deliver to every agent anyway — drop one`,
		})
	}

	return errs
}
