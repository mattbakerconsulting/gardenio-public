package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/references"
)

// docContentMaxBytes is the inline content cap for type=doc beads. Writes
// exceeding this cap must use the content_url field instead. See plan
// gardenio/plans/agent-artifact-bundles-multi-doc-viewer.md §3.3.3.
const docContentMaxBytes = 256 * 1024

// docDescriptionPayloadThreshold is the length above which a type=doc's
// `description` (after stripping typed mentions) is treated as a mis-authored
// payload rather than incidental prose. `gb bundle add` legitimately writes a
// bare typed mention like "parent(ka-XXXX)" into the description; a real doc
// body is 4.5KB of markdown (the ka-0000002161 incident). 200 chars sits well
// above the former and well below the latter.
const docDescriptionPayloadThreshold = 200

// validateDocFields enforces the type=doc-specific constraints that aren't
// expressible in TypeConfig FieldDef alone — currently the 256KB cap on
// inline `content`. Type-shape and enum/required checks remain handled by
// model.ValidateTypeFields. Called from createBead and updateBead after
// generic field validation.
func validateDocFields(fields json.RawMessage) error {
	if len(fields) == 0 {
		return nil
	}
	var f struct {
		Content    *string `json:"content"`
		ContentURL string  `json:"content_url"`
	}
	if err := json.Unmarshal(fields, &f); err != nil {
		// Generic validation already rejects malformed JSON; defensive.
		return nil
	}
	if f.Content != nil && len(*f.Content) > docContentMaxBytes {
		return fmt.Errorf("doc content exceeds %d bytes (got %d); use content_url for over-cap payloads", docContentMaxBytes, len(*f.Content))
	}
	return nil
}

// docHasContentPayload reports whether the doc's fields carry a renderable
// payload — a non-empty `content` or `content_url`. The beads-viewer bundle
// renderer reads doc payload ONLY from these fields (bundle.go), never from
// `description`, so a doc lacking both renders as a blank card.
func docHasContentPayload(fields json.RawMessage) bool {
	if len(fields) == 0 {
		return false
	}
	var f struct {
		Content    string `json:"content"`
		ContentURL string `json:"content_url"`
	}
	if err := json.Unmarshal(fields, &f); err != nil {
		// Malformed JSON is rejected by generic validation; be conservative
		// and assume a payload exists so we don't double-report.
		return true
	}
	return strings.TrimSpace(f.Content) != "" || strings.TrimSpace(f.ContentURL) != ""
}

// descriptionIsMisauthoredDocPayload reports whether `description` looks like a
// doc body that was mistakenly written to the description instead of
// `fields.content`. It strips the typed mentions the bundle tooling legitimately
// writes into the description (e.g. "parent(ka-XXXX)") and treats what remains as
// prose: more than docDescriptionPayloadThreshold non-mention characters is
// almost certainly a mis-author, not incidental context.
func descriptionIsMisauthoredDocPayload(description string) bool {
	remainder := description
	for _, ref := range references.ExtractTyped(description) {
		remainder = strings.Replace(remainder, ref.Raw, "", 1)
	}
	return len(strings.TrimSpace(remainder)) > docDescriptionPayloadThreshold
}

// validateDocPayloadPlacement rejects the silent-blank-card mis-author: a
// type=doc with no renderable `content`/`content_url` but a substantial prose
// `description`. The beads-viewer bundle renderer never falls back to
// `description`, so such a doc is accepted with exit 0 and then renders blank on
// /bundle — the failure lands on the reader after the author has moved on
// (ka-0000003524; real incident ka-0000002161). Rejecting at write time names the
// correct field so the author fixes it immediately.
func validateDocPayloadPlacement(fields json.RawMessage, description string) error {
	if docHasContentPayload(fields) {
		return nil
	}
	if !descriptionIsMisauthoredDocPayload(description) {
		return nil
	}
	return fmt.Errorf("type=doc requires the body in fields.content, not the -d/--description; the bundle renderer reads doc payload only from fields.content (or fields.content_url) and would render this doc as a blank card. Move the body: kd create ... --type=doc -f content=@<file> (or gb bundle add --doc <file>)")
}
