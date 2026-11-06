package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// backfillTestOptions mirrors the real revival-review decision (gd-0T0xKssGaY)
// whose `chosen` was stored as the option LABEL rather than its id.
const backfillTestOptions = `[
	{"id":"revive-as-is","short":"Revive","label":"Re-enable trigger as-is","artifact_type":"diff-summary"},
	{"id":"keep-disabled","short":"Keep dark","label":"Mission still relevant but no capacity","artifact_type":"report"},
	{"id":"archive","short":"Archive","label":"Close the trigger bead","artifact_type":"report"}
]`

func TestNormalizeChosen(t *testing.T) {
	tests := []struct {
		name         string
		chosen       string
		optionsRaw   string
		wantOptionID string
		wantRespText string
	}{
		{
			name:         "already an id passes through",
			chosen:       "keep-disabled",
			optionsRaw:   backfillTestOptions,
			wantOptionID: "keep-disabled",
		},
		{
			name:         "historical label normalizes to id",
			chosen:       "Mission still relevant but no capacity",
			optionsRaw:   backfillTestOptions,
			wantOptionID: "keep-disabled",
		},
		{
			name:         "short normalizes to id",
			chosen:       "Keep dark",
			optionsRaw:   backfillTestOptions,
			wantOptionID: "keep-disabled",
		},
		{
			name:         "case-insensitive label match",
			chosen:       "close the trigger bead",
			optionsRaw:   backfillTestOptions,
			wantOptionID: "archive",
		},
		{
			name:         "free-text sentence -> response_text, empty id",
			chosen:       "Do we stilll need this?",
			optionsRaw:   backfillTestOptions,
			wantOptionID: "",
			wantRespText: "Do we stilll need this?",
		},
		{
			name:         "dismissed sentinel is never normalized",
			chosen:       "dismissed",
			optionsRaw:   backfillTestOptions,
			wantOptionID: "dismissed",
		},
		{
			name:         "promoted_to_escalation sentinel passes through",
			chosen:       "promoted_to_escalation",
			optionsRaw:   backfillTestOptions,
			wantOptionID: "promoted_to_escalation",
		},
		{
			name:         "empty chosen stays empty",
			chosen:       "",
			optionsRaw:   backfillTestOptions,
			wantOptionID: "",
		},
		{
			name:         "no options: value passes through unchanged",
			chosen:       "anything at all",
			optionsRaw:   "",
			wantOptionID: "anything at all",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotResp := normalizeChosen(tt.chosen, tt.optionsRaw)
			if gotID != tt.wantOptionID {
				t.Errorf("normalizeChosen(%q) id = %q, want %q", tt.chosen, gotID, tt.wantOptionID)
			}
			if gotResp != tt.wantRespText {
				t.Errorf("normalizeChosen(%q) responseText = %q, want %q", tt.chosen, gotResp, tt.wantRespText)
			}
		})
	}
}

func TestFlatDecisionResolution_NormalizesHistoricalLabel(t *testing.T) {
	// A decision resolved via the old button path: `chosen` holds the LABEL.
	res, ok := flatDecisionResolution(map[string]string{
		"chosen":       "Mission still relevant but no capacity",
		"responded_by": "matt",
		"responded_at": "2026-07-28T21:00:00Z",
		"options":      backfillTestOptions,
	})
	if !ok {
		t.Fatal("expected resolved=true")
	}
	if res.OptionID != "keep-disabled" {
		t.Errorf("OptionID = %q, want keep-disabled (normalized from label)", res.OptionID)
	}
}

func TestFlatDecisionResolution_FreeTextToResponseText(t *testing.T) {
	// A raw operator sentence misfiled into `chosen` must render as a sentence,
	// not a code-span option id.
	res, ok := flatDecisionResolution(map[string]string{
		"chosen":       "i dont like either of these, there must be a way to wait",
		"responded_by": "matt",
		"options":      backfillTestOptions,
	})
	if !ok {
		t.Fatal("expected resolved=true")
	}
	if res.OptionID != "" {
		t.Errorf("OptionID = %q, want empty for free text", res.OptionID)
	}
	if res.ResponseText != "i dont like either of these, there must be a way to wait" {
		t.Errorf("ResponseText = %q, want the free text", res.ResponseText)
	}
}

func TestFlatDecisionResolution_DismissedUnaffected(t *testing.T) {
	res, ok := flatDecisionResolution(map[string]string{
		"chosen":      "dismissed",
		"canceled":    "true",
		"canceled_by": "matt",
		"options":     backfillTestOptions,
	})
	if !ok {
		t.Fatal("expected resolved=true")
	}
	if !res.Canceled {
		t.Errorf("expected Canceled=true")
	}
	// `dismissed` is a shape marker: on a canceled decision it must not be
	// treated as an option id (the canceled branch keeps chosen as-is).
	if res.OptionID != "dismissed" {
		t.Errorf("OptionID = %q, want dismissed unchanged on cancel", res.OptionID)
	}
}

// TestBeadShow_DecisionType_HistoricalLabelBadges is the end-to-end render
// guard for ka-0000000003: a resolved decision whose `chosen` is an option
// LABEL (not id) must still badge that option in the rendered card.
func TestBeadShow_DecisionType_HistoricalLabelBadges(t *testing.T) {
	f := newFake()
	f.beads["gd-c9r"] = &beadsapi.BeadDetail{
		ID:        "gd-c9r",
		Title:     "Revive or archive the trigger?",
		Type:      "decision",
		Status:    "closed",
		CreatedBy: "gardener-1",
		Fields: map[string]string{
			"prompt":       "Should we revive the dark trigger?",
			"requested_by": "gardener-1",
			"options":      backfillTestOptions,
			// The bug shape: chosen holds the LABEL of keep-disabled.
			"chosen":       "Mission still relevant but no capacity",
			"responded_by": "matt",
			"responded_at": "2026-07-28T21:00:00Z",
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-c9r", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The banner must show the normalized option id, and the keep-disabled
	// option must carry the chosen badge class.
	for _, want := range []string{
		"banner-resolved",
		"<code>keep-disabled</code>", // normalized id in banner, not the sentence
		"opt-chosen",                 // chosen badge on the matched option
	} {
		if !strings.Contains(body, want) {
			t.Errorf("historical-label decision render missing %q\nbody: %s", want, body)
		}
	}
	// The raw label sentence must NOT appear as a code-span id.
	if strings.Contains(body, "<code>Mission still relevant but no capacity</code>") {
		t.Errorf("label leaked into a code-span id\nbody: %s", body)
	}
}
