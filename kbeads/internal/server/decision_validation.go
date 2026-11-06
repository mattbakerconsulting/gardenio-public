// Server-side guard against ack-only / no-fork decisions.
//
// Decisions are meant to surface real human-judgment forks. Agents (especially
// scheduled gardeners) repeatedly create "decisions" that are status messages
// in disguise: zero options, or a single `[ack]` / `[done]` / `[continue]`
// option. These pollute the operator's queue and turn the decision system
// into an end-of-cycle notification channel.
//
// This validator rejects (or strongly warns) those patterns at the
// createBead choke point so every creation path (gRPC, HTTP, escalation
// replay) is covered. Override is available via createBeadInput.Force,
// which the CLI surfaces as `--force` for legitimate edge cases.
//
// See ka-0000002237 for the design discussion and the smell test that
// motivated the rules below.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// reservedNoOpOptionIDs lists option IDs that signal "acknowledgement only"
// — i.e. no real fork. A decision whose ONLY option is one of these is not
// a decision; it is a status message.
var reservedNoOpOptionIDs = map[string]bool{
	"ack":         true,
	"done":        true,
	"continue":    true,
	"acknowledge": true,
	"wrap":        true,
	"noop":        true,
}

// decisionValidationResult is the outcome of validating a decision's fields.
// RejectReason is non-empty when the decision must be rejected; Warning is
// non-empty when it is suspect but still allowed.
type decisionValidationResult struct {
	RejectReason string
	Hint         string
	Warning      string
}

// errorMessage formats RejectReason+Hint for surfacing to the client.
func (r decisionValidationResult) errorMessage() string {
	if r.Hint == "" {
		return r.RejectReason
	}
	return r.RejectReason + " — " + r.Hint
}

// applyDecisionGuard runs the ack-only / no-fork guard for a decision bead
// being created. force=true skips the rejection logic but still logs the
// bypass so the operator can see when the guard is short-circuited.
//
// Returns inputError when the decision is rejected; nil otherwise. Soft
// warnings (e.g. all-tldr) are logged but do not block creation.
func (s *BeadsServer) applyDecisionGuard(ctx context.Context, bead *model.Bead, force bool) error {
	result := validateDecisionFields(bead.Fields)
	actor := actorFromContext(ctx)

	if force {
		if result.RejectReason != "" || result.Warning != "" {
			slog.Info("decision guard bypassed via force",
				"actor", actor,
				"title", bead.Title,
				"reject_reason", result.RejectReason,
				"warning", result.Warning,
			)
		}
		return nil
	}

	if result.RejectReason != "" {
		slog.Warn("decision rejected by guard",
			"actor", actor,
			"title", bead.Title,
			"reason", result.RejectReason,
		)
		return inputError(result.errorMessage())
	}
	if result.Warning != "" {
		slog.Warn("decision created with guard warning",
			"actor", actor,
			"title", bead.Title,
			"warning", result.Warning,
		)
	}
	return nil
}

// decisionOption is the minimal subset of a decision option used by the guard.
type decisionOption struct {
	ID           string `json:"id"`
	ArtifactType string `json:"artifact_type"`
}

// validateDecisionOptionShape returns a non-nil error when the `options`
// field is present but is NOT a JSON array of objects each carrying a
// non-empty `id`. It exists because the struct-slice decode used elsewhere
// silently tolerates a string-array (["a","b"]) — Go decodes each string
// into a zero-value struct and discards the per-element error, so the slice
// is non-empty and downstream len-based checks pass. We decode into a
// json.RawMessage slice (which accepts any element) and inspect each element
// ourselves: every element must be a JSON object with a non-empty "id".
//
// Returns nil when `options` is absent, is an empty array, or is a valid
// object-array — empty/absent is the concern of Rule 1, not shape.
func validateDecisionOptionShape(fields json.RawMessage) error {
	var f struct {
		Options *json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(fields, &f); err != nil || f.Options == nil {
		// A whole-fields decode error is reported elsewhere (ValidateTypeFields);
		// absent options is Rule 1's job. Either way, not a shape error.
		return nil
	}

	var elems []json.RawMessage
	if err := json.Unmarshal(*f.Options, &elems); err != nil {
		return fmt.Errorf("decision options must be a JSON array, got %s", optionsKindLabel(*f.Options))
	}
	for i, el := range elems {
		var obj struct {
			ID *string `json:"id"`
		}
		if err := json.Unmarshal(el, &obj); err != nil {
			// Element is a string/number/array/bool — not an option object.
			return fmt.Errorf("decision option %d must be an object with an \"id\", got %s", i, optionsKindLabel(el))
		}
		if obj.ID == nil || strings.TrimSpace(*obj.ID) == "" {
			return fmt.Errorf("decision option %d is missing a non-empty \"id\"", i)
		}
	}
	return nil
}

// optionsKindLabel returns a short human-readable label for the JSON kind of
// raw (object/array/string/number/boolean/null), for use in error messages.
func optionsKindLabel(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "empty"
	}
	switch s[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	default:
		return "a number"
	}
}

// validateDecisionFields parses a decision bead's fields and applies the
// "real decision" rules. Returns a result describing whether the decision
// should be rejected (hard fail), warned about (soft signal), or accepted.
//
// Rules (acceptance criteria from ka-0000002237):
//  1. Zero options → reject.
//  2. Single option named ack/done/continue/acknowledge/wrap/noop → reject.
//  3. All options have artifact_type:tldr → soft warn (suggests the fork
//     is not producing meaningfully different artifacts).
//
// The validator is intentionally permissive about other shapes — its job is
// to catch the well-known anti-patterns, not to police every dimension of
// decision quality.
func validateDecisionFields(fields json.RawMessage) decisionValidationResult {
	// Rule 0: option SHAPE. Renderers across gb, the Slack modal, and
	// beads-viewer expect `options` to be a JSON array of OBJECTS
	// ([{id,short,label,artifact_type}]). A JSON array of plain strings
	// (["a","b","c"]) decodes into N zero-value structs WITHOUT failing the
	// outer unmarshal — each element's decode error is discarded — so
	// len(Options) is N>0 and Rule 1 below never fires. The malformed shape
	// would be accepted and then render blank everywhere (ka-0000006724).
	// Detect it explicitly here, before the discard-prone decode below.
	if len(fields) > 0 {
		if shapeErr := validateDecisionOptionShape(fields); shapeErr != nil {
			return decisionValidationResult{
				RejectReason: shapeErr.Error(),
				Hint: "options must be an array of objects, each with an \"id\" — " +
					"e.g. [{\"id\":\"a\",\"short\":\"Option A\",\"label\":\"…\",\"artifact_type\":\"plan\"},…]. " +
					"Prefer 'gb decision create' (it builds this shape for you) over a raw 'kd create'. " +
					"To override, pass --force.",
			}
		}
	}

	var f struct {
		Options []decisionOption `json:"options"`
	}
	if len(fields) > 0 {
		// Structural JSON errors are caught by ValidateTypeFields earlier in
		// createBead; an unmarshal failure here is treated as "no options",
		// which falls into rule 1.
		_ = json.Unmarshal(fields, &f)
	}

	// Rule 1: zero options.
	//
	// The hint must NOT contradict the gb stop gate. Telling a no-op agent to
	// "use gb stop" for a status message was the loop in ka-0000007734: a bare
	// 'gb stop' gated on a decision bead, sending the agent here, and this hint
	// sent it back to a bare 'gb stop'. The clean exit for a done/no-op cycle is
	// a wrap-up — 'gb stop --wrapup {...}' clears the decision gate with no
	// decision required — so point there explicitly rather than at a bare stop.
	if len(f.Options) == 0 {
		return decisionValidationResult{
			RejectReason: "decision has no options",
			Hint: "a decision must offer at least one option. " +
				"If you are done (a status / no-op cycle), do NOT create a decision — " +
				"finish with 'gb stop --wrapup' (a valid wrap-up clears the stop gate, no decision needed); " +
				"for follow-up notes use 'kd comment' on the relevant bead. " +
				"To override, pass --force.",
		}
	}

	// Rule 2: single ack/done/continue/etc option (no fork).
	if len(f.Options) == 1 {
		id := strings.ToLower(strings.TrimSpace(f.Options[0].ID))
		if reservedNoOpOptionIDs[id] {
			return decisionValidationResult{
				RejectReason: fmt.Sprintf("decision has a single %q option (no fork)", id),
				Hint: "if your only option is acknowledgement, call 'gb stop --wrapup' instead. " +
					"To override, pass --force.",
			}
		}
	}

	// Rule 3: all options have artifact_type:tldr (soft warn only).
	allTldr := true
	for _, o := range f.Options {
		if strings.ToLower(strings.TrimSpace(o.ArtifactType)) != "tldr" {
			allTldr = false
			break
		}
	}
	if allTldr {
		return decisionValidationResult{
			Warning: "all options have artifact_type:tldr — tldr is for " +
				"length-bounded Slack summaries; if every option produces only a tldr, " +
				"the 'fork' may not produce meaningfully different artifacts",
		}
	}

	return decisionValidationResult{}
}
