package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateDocFields_EmptyFields(t *testing.T) {
	if err := validateDocFields(nil); err != nil {
		t.Fatalf("expected nil error for empty fields, got %v", err)
	}
	if err := validateDocFields(json.RawMessage(``)); err != nil {
		t.Fatalf("expected nil error for zero-length fields, got %v", err)
	}
}

func TestValidateDocFields_NoContent(t *testing.T) {
	fields := json.RawMessage(`{"format":"md","headline":"Hi"}`)
	if err := validateDocFields(fields); err != nil {
		t.Fatalf("expected nil error when content absent, got %v", err)
	}
}

func TestValidateDocFields_ContentURLOnly(t *testing.T) {
	fields := json.RawMessage(`{"format":"md","content_url":"https://example.com/big.md"}`)
	if err := validateDocFields(fields); err != nil {
		t.Fatalf("expected nil error for content_url-only doc, got %v", err)
	}
}

func TestValidateDocFields_AtCapAccepted(t *testing.T) {
	body := strings.Repeat("a", docContentMaxBytes)
	fields, err := json.Marshal(map[string]any{
		"format":  "md",
		"content": body,
	})
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	if err := validateDocFields(fields); err != nil {
		t.Fatalf("expected nil error at cap (%d bytes), got %v", docContentMaxBytes, err)
	}
}

func TestValidateDocFields_OverCapRejected(t *testing.T) {
	body := strings.Repeat("a", docContentMaxBytes+1)
	fields, err := json.Marshal(map[string]any{
		"format":  "md",
		"content": body,
	})
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	err = validateDocFields(fields)
	if err == nil {
		t.Fatalf("expected error for content > %d bytes, got nil", docContentMaxBytes)
	}
	if !strings.Contains(err.Error(), "content_url") {
		t.Errorf("error message should mention content_url remediation, got: %v", err)
	}
}

func TestValidateDocFields_MalformedJSONIgnored(t *testing.T) {
	// Generic validation already rejects malformed JSON; this guard is
	// defensive — should not panic or error.
	fields := json.RawMessage(`{not json`)
	if err := validateDocFields(fields); err != nil {
		t.Fatalf("expected nil error (malformed JSON handled defensively), got %v", err)
	}
}

// --- validateDocPayloadPlacement (ka-0000003524) ---
//
// A type=doc whose body was written to `description` instead of `fields.content`
// renders as a blank card on /bundle because the viewer reads doc payload only
// from fields.content/content_url. These tests pin the write-time guard.

// docBody is a stand-in for the 4.5KB markdown payload from the real incident
// (ka-0000002161): enough prose to clear the mis-author threshold.
var docBody = "# FSM Triage Report Card\n\n" + strings.Repeat("Some markdown table row with real content. ", 20)

func TestValidateDocPayloadPlacement_RejectsBodyInDescription(t *testing.T) {
	// The exact mis-author: 4.5KB in -d, no fields.content.
	err := validateDocPayloadPlacement(nil, docBody)
	if err == nil {
		t.Fatal("expected rejection for doc body in description with no content, got nil")
	}
	if !strings.Contains(err.Error(), "fields.content") {
		t.Errorf("error must name the correct field fields.content, got: %v", err)
	}
}

func TestValidateDocPayloadPlacement_RejectsBodyPlusTypedMention(t *testing.T) {
	// The real incident carried a leading typed parent mention followed by the
	// body — the mention must be stripped but the body still trips the guard.
	desc := "parent: gd-9U5vctvVO7\n\n" + docBody
	err := validateDocPayloadPlacement(nil, desc)
	if err == nil {
		t.Fatal("expected rejection when body follows a typed parent mention, got nil")
	}
}

func TestValidateDocPayloadPlacement_AllowsWithContent(t *testing.T) {
	// The correct authoring: body in fields.content. A description is irrelevant.
	fields := json.RawMessage(`{"format":"md","content":"# Real body\n\ncontent here"}`)
	if err := validateDocPayloadPlacement(fields, docBody); err != nil {
		t.Fatalf("expected no error when fields.content is present, got %v", err)
	}
}

func TestValidateDocPayloadPlacement_AllowsWithContentURL(t *testing.T) {
	fields := json.RawMessage(`{"format":"md","content_url":"https://example.com/big.md"}`)
	if err := validateDocPayloadPlacement(fields, docBody); err != nil {
		t.Fatalf("expected no error when fields.content_url is present, got %v", err)
	}
}

func TestValidateDocPayloadPlacement_AllowsBareTypedMentionDescription(t *testing.T) {
	// `gb bundle add` legitimately writes a bare "parent(gd-XXXX)" typed mention
	// into the doc's description alongside fields.content. Even with NO content
	// (a create-then-fill flow), a description that is only a typed mention must
	// NOT be rejected.
	if err := validateDocPayloadPlacement(nil, "parent(gd-9U5vctvVO7)"); err != nil {
		t.Fatalf("expected no error for typed-mention-only description, got %v", err)
	}
	if err := validateDocPayloadPlacement(nil, "parent: gd-9U5vctvVO7"); err != nil {
		t.Fatalf("expected no error for colon-form typed-mention description, got %v", err)
	}
}

func TestValidateDocPayloadPlacement_AllowsEmptyDescription(t *testing.T) {
	// A doc with no content and no description (create-then-fill) is fine — the
	// blank card is expected and gets the render-time placeholder, not a reject.
	if err := validateDocPayloadPlacement(nil, ""); err != nil {
		t.Fatalf("expected no error for empty description, got %v", err)
	}
}

func TestValidateDocPayloadPlacement_AllowsShortIncidentalNote(t *testing.T) {
	// A short incidental note in the description (well under the threshold) is not
	// a mis-authored body — don't reject it.
	if err := validateDocPayloadPlacement(nil, "see the attached bundle for details"); err != nil {
		t.Fatalf("expected no error for short incidental description, got %v", err)
	}
}
