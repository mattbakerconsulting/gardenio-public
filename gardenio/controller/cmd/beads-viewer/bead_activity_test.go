package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mustJSON marshals v to a json.RawMessage; tests fail fast if encoding
// breaks (always a programmer error in a fixture).
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

func TestBeadActivity_RendersAllKinds(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		event  beadsapi.Event
		expect []string // substrings that must appear in the body for this event
	}{
		{
			name: "created",
			event: beadsapi.Event{
				ID:        1,
				Topic:     "beads.bead.created",
				BeadID:    "gd-aaa",
				Actor:     "agent-x",
				CreatedAt: now,
				Payload:   mustJSON(t, map[string]any{"bead": map[string]any{"id": "gd-aaa"}}),
			},
			expect: []string{"✦", "agent-x", "created bead"},
		},
		{
			name: "updated",
			event: beadsapi.Event{
				ID:        2,
				Topic:     "beads.bead.updated",
				BeadID:    "gd-aaa",
				Actor:     "agent-x",
				CreatedAt: now,
				Payload: mustJSON(t, map[string]any{
					"changes": map[string]any{"status": "in_progress", "assignee": "bot-1"},
				}),
			},
			expect: []string{"✎", "agent-x", "updated"},
		},
		{
			name: "closed",
			event: beadsapi.Event{
				ID:        3,
				Topic:     "beads.bead.closed",
				BeadID:    "gd-aaa",
				Actor:     "agent-x",
				CreatedAt: now,
				Payload:   mustJSON(t, map[string]any{"closed_by": "operator-1"}),
			},
			expect: []string{"✓", "closed bead", "operator-1"},
		},
		{
			name: "deleted",
			event: beadsapi.Event{
				ID:        4,
				Topic:     "beads.bead.deleted",
				BeadID:    "gd-aaa",
				Actor:     "agent-x",
				CreatedAt: now,
				Payload:   mustJSON(t, map[string]any{"bead_id": "gd-aaa"}),
			},
			expect: []string{"✗", "deleted bead"},
		},
		{
			name: "transferred",
			event: beadsapi.Event{
				ID:        5,
				Topic:     "beads.bead.transferred",
				BeadID:    "gd-aaa",
				Actor:     "agent-x",
				CreatedAt: now,
				Payload:   mustJSON(t, map[string]any{"from_fleet": "alpha", "to_fleet": "beta"}),
			},
			expect: []string{"→", "alpha", "beta"},
		},
		{
			name: "dependency-added",
			event: beadsapi.Event{
				ID:        6,
				Topic:     "beads.dependency.added",
				BeadID:    "gd-aaa",
				Actor:     "agent-x",
				CreatedAt: now,
				Payload: mustJSON(t, map[string]any{
					"dependency": map[string]any{
						"bead_id":       "gd-aaa",
						"depends_on_id": "gd-parent",
						"type":          "parent-child",
					},
				}),
			},
			expect: []string{"⊕", "gd-parent", "parent-child"},
		},
		{
			name: "dependency-removed",
			event: beadsapi.Event{
				ID:        7,
				Topic:     "beads.dependency.removed",
				BeadID:    "gd-aaa",
				Actor:     "agent-x",
				CreatedAt: now,
				Payload: mustJSON(t, map[string]any{
					"depends_on_id": "gd-parent",
					"type":          "blocks",
				}),
			},
			expect: []string{"⊖", "gd-parent", "blocks"},
		},
		{
			name: "label-added",
			event: beadsapi.Event{
				ID:        8,
				Topic:     "beads.label.added",
				BeadID:    "gd-aaa",
				Actor:     "agent-x",
				CreatedAt: now,
				Payload:   mustJSON(t, map[string]any{"label": "project:gardenio"}),
			},
			expect: []string{"🏷&#43;", "project:gardenio"},
		},
		{
			name: "label-removed",
			event: beadsapi.Event{
				ID:        9,
				Topic:     "beads.label.removed",
				BeadID:    "gd-aaa",
				Actor:     "agent-x",
				CreatedAt: now,
				Payload:   mustJSON(t, map[string]any{"label": "stale"}),
			},
			expect: []string{"🏷−", "stale"},
		},
		{
			name: "comment-short",
			event: beadsapi.Event{
				ID:        10,
				Topic:     "beads.comment.added",
				BeadID:    "gd-aaa",
				CreatedAt: now,
				Payload: mustJSON(t, map[string]any{
					"comment": map[string]any{
						"id":     101,
						"author": "reviewer",
						"text":   "lgtm",
					},
				}),
			},
			expect: []string{"💬", "reviewer", "lgtm"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.events = map[string][]beadsapi.Event{"gd-aaa": {tc.event}}
			mux := mkServer(t, f)
			req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()
			for _, want := range tc.expect {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q in body:\n%s", want, body)
				}
			}
		})
	}
}

func TestBeadActivity_HtmxPollAttrs(t *testing.T) {
	f := newFake()
	f.events = map[string][]beadsapi.Event{"gd-aaa": {}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-get="/b/gd-aaa/activity"`,
		`hx-trigger="every 15s [document.visibilityState==='visible']"`,
		`hx-swap="outerHTML"`,
		`id="bead-activity"`,
		`data-bead-id="gd-aaa"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

func TestBeadActivity_ActorFallbackChain(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		event beadsapi.Event
		want  string
	}{
		{
			// Regression for ka-0000005471: the ACTING identity (actor) leads,
			// NOT operated_by. Leading with the operator made a human read as
			// if they performed an action their agent's stop-gate actually did.
			name: "actor-wins-over-operated_by",
			event: beadsapi.Event{
				ID:         1,
				Topic:      "beads.bead.updated",
				OperatedBy: "slack:U123",
				Actor:      "agent-x",
				CreatedAt:  now,
				Payload: mustJSON(t, map[string]any{
					"bead":    map[string]any{"assignee": "bot-1", "created_by": "static"},
					"changes": map[string]any{"status": "open"},
				}),
			},
			want: "agent-x",
		},
		{
			// operated_by is the LAST-RESORT actor fallback so a row is never
			// actor-less when the daemon only recorded the operator.
			name: "operated_by-as-last-resort",
			event: beadsapi.Event{
				ID:         2,
				Topic:      "beads.bead.updated",
				OperatedBy: "slack:U123",
				CreatedAt:  now,
				Payload: mustJSON(t, map[string]any{
					"changes": map[string]any{"status": "open"},
				}),
			},
			want: "slack:U123",
		},
		{
			name: "assignee-when-no-actor",
			event: beadsapi.Event{
				ID:        3,
				Topic:     "beads.bead.updated",
				CreatedAt: now,
				Payload: mustJSON(t, map[string]any{
					"bead":    map[string]any{"assignee": "bot-1", "created_by": "static"},
					"changes": map[string]any{"status": "open"},
				}),
			},
			want: "bot-1",
		},
		{
			name: "created_by-as-last-resort",
			event: beadsapi.Event{
				ID:        4,
				Topic:     "beads.bead.updated",
				CreatedAt: now,
				Payload: mustJSON(t, map[string]any{
					"bead":    map[string]any{"created_by": "static"},
					"changes": map[string]any{"status": "open"},
				}),
			},
			want: "static",
		},
		{
			name: "comment-author-overrides",
			event: beadsapi.Event{
				ID:         5,
				Topic:      "beads.comment.added",
				OperatedBy: "slack:U123",
				Actor:      "agent-x",
				CreatedAt:  now,
				Payload: mustJSON(t, map[string]any{
					"comment": map[string]any{"id": 1, "author": "reviewer", "text": "lgtm"},
				}),
			},
			want: "reviewer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.events = map[string][]beadsapi.Event{"gd-aaa": {tc.event}}
			mux := mkServer(t, f)
			req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			body := w.Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("expected actor %q in body:\n%s", tc.want, body)
			}
		})
	}
}

func TestBeadActivity_CommentTruncation(t *testing.T) {
	long := strings.Repeat("word ", 100) // 500 chars > 300 truncate
	f := newFake()
	f.events = map[string][]beadsapi.Event{"gd-aaa": {{
		ID:        1,
		Topic:     "beads.comment.added",
		BeadID:    "gd-aaa",
		CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Payload: mustJSON(t, map[string]any{
			"comment": map[string]any{"id": 1, "author": "reviewer", "text": long},
		}),
	}}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "<details") {
		t.Errorf("expected <details> for truncated comment, got:\n%s", body)
	}
	if !strings.Contains(body, "show more") {
		t.Errorf("expected 'show more' summary toggle, got:\n%s", body)
	}
	if !strings.Contains(body, "activity-body-truncated") {
		t.Errorf("expected activity-body-truncated class, got:\n%s", body)
	}
}

func TestBeadActivity_ReadAuthzShortCircuit(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "404",
			err:        &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"},
			wantStatus: http.StatusNotFound,
		},
		{
			// Out of scope answers as missing (beadMissing).
			name:       "403",
			err:        &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "forbidden"},
			wantStatus: http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.getBeadErr = tc.err
			mux := mkServer(t, f)
			req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d", tc.wantStatus, w.Code)
			}
			if f.eventsCalls != 0 {
				t.Errorf("expected GetEvents NOT called on %d short-circuit, got %d calls",
					tc.wantStatus, f.eventsCalls)
			}
		})
	}
}

func TestBeadActivity_NewestFirst_And_Cap(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f := newFake()
	// 60 events spanning 60 minutes — oldest at index 0 per daemon
	// contract; handler must reverse and clip to 50.
	evs := make([]beadsapi.Event, 0, 60)
	for i := range 60 {
		evs = append(evs, beadsapi.Event{
			ID:        int64(i + 1),
			Topic:     "beads.bead.updated",
			BeadID:    "gd-aaa",
			Actor:     "agent-x",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			Payload: mustJSON(t, map[string]any{
				"changes": map[string]any{"status": "open"},
			}),
		})
	}
	f.events = map[string][]beadsapi.Event{"gd-aaa": evs}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	// Header reflects clamped count.
	if !strings.Contains(body, "Activity (50)") {
		t.Errorf("expected 'Activity (50)' header, got:\n%s", body)
	}
	// Newest event (id 60, the last we added) appears before oldest in the rendered list.
	idx60 := strings.Index(body, `data-event-id="60"`)
	idx11 := strings.Index(body, `data-event-id="11"`)
	idx10 := strings.Index(body, `data-event-id="10"`)
	if idx60 < 0 || idx11 < 0 {
		t.Fatalf("expected event ids 60 and 11 in body, got idx60=%d idx11=%d", idx60, idx11)
	}
	if idx60 >= idx11 {
		t.Errorf("expected newest (60) before older (11), got idx60=%d idx11=%d", idx60, idx11)
	}
	if idx10 >= 0 {
		t.Errorf("expected event id 10 to be clipped (only newest 50 of 60 retained), but it appeared at idx %d", idx10)
	}
}

// TestBeadActivity_UpdatedBodyShowsValues verifies that bead.updated rows
// render scalar new values inline (so operators can see WHAT changed from
// the rail without clicking through), and elide non-scalar values
// (maps/arrays) to just the field name so blob updates like `fields` stay
// scannable instead of dumping JSON. Regression for ka-0000005159.
func TestBeadActivity_UpdatedBodyShowsValues(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.events = map[string][]beadsapi.Event{"gd-aaa": {{
		ID:        1,
		Topic:     "beads.bead.updated",
		BeadID:    "gd-aaa",
		Actor:     "agent-x",
		CreatedAt: now,
		Payload: mustJSON(t, map[string]any{
			"changes": map[string]any{
				"status":   "in_progress",
				"assignee": "bot-1",
				"priority": float64(1),
				"fields":   map[string]any{"foo": "bar"}, // non-scalar: name only
			},
		}),
	}}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{"status:in_progress", "assignee:bot-1", "priority:1", "updated"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
	// `fields` is a non-scalar map — render bare name without ":{".
	if strings.Contains(body, "fields:") || strings.Contains(body, "fields:{") {
		t.Errorf("expected non-scalar `fields` to render bare (no value), got:\n%s", body)
	}
	if !strings.Contains(body, "fields") {
		t.Errorf("expected `fields` field name in body, got:\n%s", body)
	}
}

// TestBeadActivity_FirstFivePlusShowMore verifies that the activity rail
// renders the first 5 entries unconditionally and stashes the rest inside a
// <details>/<summary> "Show N more" disclosure so long histories don't drown
// the bead page. Regression for ka-0000005159.
func TestBeadActivity_FirstFivePlusShowMore(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f := newFake()
	evs := make([]beadsapi.Event, 0, 12)
	for i := range 12 {
		evs = append(evs, beadsapi.Event{
			ID:        int64(i + 1),
			Topic:     "beads.bead.updated",
			BeadID:    "gd-aaa",
			Actor:     "agent-x",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			Payload: mustJSON(t, map[string]any{
				"changes": map[string]any{"status": "open"},
			}),
		})
	}
	f.events = map[string][]beadsapi.Event{"gd-aaa": evs}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	// Overflow disclosure must announce 12 - 5 = 7 hidden rows.
	if !strings.Contains(body, "Show 7 more") {
		t.Errorf("expected 'Show 7 more' overflow toggle, got:\n%s", body)
	}
	if !strings.Contains(body, "activity-list-overflow") {
		t.Errorf("expected activity-list-overflow class on the disclosed list, got:\n%s", body)
	}
	if !strings.Contains(body, `class="activity-overflow"`) {
		t.Errorf("expected <details class=\"activity-overflow\"> wrapper, got:\n%s", body)
	}
}

// TestBeadActivity_NoOverflowAtOrBelowFive guards against rendering an empty
// "Show 0 more" toggle when the count is exactly 5 or less.
func TestBeadActivity_NoOverflowAtOrBelowFive(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f := newFake()
	evs := make([]beadsapi.Event, 0, 5)
	for i := range 5 {
		evs = append(evs, beadsapi.Event{
			ID:        int64(i + 1),
			Topic:     "beads.bead.updated",
			BeadID:    "gd-aaa",
			Actor:     "agent-x",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			Payload: mustJSON(t, map[string]any{
				"changes": map[string]any{"status": "open"},
			}),
		})
	}
	f.events = map[string][]beadsapi.Event{"gd-aaa": evs}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if strings.Contains(body, "activity-overflow") {
		t.Errorf("expected no overflow disclosure when count <= 5, got:\n%s", body)
	}
	if strings.Contains(body, "Show 0 more") || strings.Contains(body, "Show -") {
		t.Errorf("expected no degenerate 'Show 0/negative more' toggle, got:\n%s", body)
	}
}

// TestBeadActivity_CloseEventLeadsWithCloser is the direct regression for
// bug ka-0000005471: a close event carrying a human operated_by AND a distinct
// agent closer (closed_by) must lead the row with the CLOSER as the subject of
// the verb, and render operated_by only as a muted "(operated by …)"
// qualifier — never as the actor. Before the fix, activityActor led with
// operated_by, so the row read "<human> closed bead (by <agent>)", sending the
// operator hunting a phantom agent-killer.
func TestBeadActivity_CloseEventLeadsWithCloser(t *testing.T) {
	now := time.Date(2026, 7, 8, 20, 11, 0, 0, time.UTC)
	f := newFake()
	f.events = map[string][]beadsapi.Event{"gd-aaa": {{
		ID:         1,
		Topic:      "beads.bead.closed",
		BeadID:     "gd-aaa",
		OperatedBy: "slack:U0OPERATOR1", // the human operator
		CreatedAt:  now,
		Payload:    mustJSON(t, map[string]any{"closed_by": "monorepo-030etmf-qoq"}),
	}}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	// The closer must LEAD as the bolded actor.
	if !strings.Contains(body, "<strong>monorepo-030etmf-qoq</strong>") {
		t.Errorf("expected closer to lead as the actor, got:\n%s", body)
	}
	// The operator is a muted secondary attribution, not the actor.
	if !strings.Contains(body, "(operated by slack:U0OPERATOR1)") {
		t.Errorf("expected muted '(operated by …)' qualifier, got:\n%s", body)
	}
	// The operator must NEVER be rendered as the bolded actor/subject.
	if strings.Contains(body, "<strong>slack:U0OPERATOR1</strong>") {
		t.Errorf("operator must not be the bolded actor/subject, got:\n%s", body)
	}
	// The old redundant "(by <closer>)" parenthetical in the body is gone.
	if strings.Contains(body, "closed bead (by ") {
		t.Errorf("expected no '(by <closer>)' body parenthetical, got:\n%s", body)
	}
}

// TestBeadActivity_OperatorOmittedWhenSameAsActor verifies the muted operator
// qualifier is suppressed when operated_by equals the acting identity, so a
// human's own edit does not render a redundant "(operated by <self>)"
// (bug ka-0000005471).
func TestBeadActivity_OperatorOmittedWhenSameAsActor(t *testing.T) {
	now := time.Date(2026, 7, 8, 20, 11, 0, 0, time.UTC)
	f := newFake()
	f.events = map[string][]beadsapi.Event{"gd-aaa": {{
		ID:         1,
		Topic:      "beads.bead.updated",
		BeadID:     "gd-aaa",
		Actor:      "slack:U0OPERATOR1",
		OperatedBy: "slack:U0OPERATOR1",
		CreatedAt:  now,
		Payload:    mustJSON(t, map[string]any{"changes": map[string]any{"status": "open"}}),
	}}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-aaa/activity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if strings.Contains(body, "operated by") {
		t.Errorf("expected no operator qualifier when operator == actor, got:\n%s", body)
	}
}

func TestBuildActivityRows_EmptyAndNil(t *testing.T) {
	if rows := buildActivityRows(nil); rows != nil {
		t.Errorf("expected nil rows for nil input, got %v", rows)
	}
	if rows := buildActivityRows([]beadsapi.Event{}); rows != nil {
		t.Errorf("expected nil rows for empty input, got %v", rows)
	}
}

// TestBuildActivityRows_SlimTriggerUpdate: kbeads stores a trigger or agent
// bead.updated event without bead.fields, with changes.fields as the changed
// keys only (gd-1F8R0SQtiQ). The activity rail still renders it, attributed to
// the bead's assignee, listing what changed.
func TestBuildActivityRows_SlimTriggerUpdate(t *testing.T) {
	payload := json.RawMessage(`{"bead":{"id":"gd-trg","type":"trigger","status":"open","assignee":"engine"},` +
		`"changes":{"fields":{"last_note":"t2"},"last_note":"t2"},` +
		`"fields_old":{"last_note":"t1"},"fields_omitted":true}`)
	rows := buildActivityRows([]beadsapi.Event{{
		ID: 1, Topic: "beads.bead.updated", BeadID: "gd-trg", Payload: payload, CreatedAt: time.Now(),
	}})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Body != "updated fields, last_note:t2" {
		t.Errorf("body = %q, want %q", rows[0].Body, "updated fields, last_note:t2")
	}
	if rows[0].Actor != "engine" {
		t.Errorf("actor = %q, want the bead's assignee", rows[0].Actor)
	}
}
