package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestUp_EmptyWhenNoBeads(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/up", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Nothing on the stack") {
		t.Errorf("expected empty-state heading; body=%s", body)
	}
}

func TestUp_PrefersInProgressBead(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.inProgressResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:        "gd-up-in-progress",
			Title:     "In-progress task",
			Type:      "task",
			Status:    "in_progress",
			Priority:  1,
			Labels:    []string{"project:gardenio"},
			UpdatedAt: now,
		}},
	}
	// listResp is the fallback path; should NOT be hit because in_progress
	// query returns a bead.
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:    "gd-up-open-fallback",
			Title: "Open fallback",
			Type:  "task",
		}},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/up", nil)
	req.Header.Set("X-Auth-Request-Email", "viewer@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "gd-up-in-progress") {
		t.Errorf("expected in_progress bead to render; body=%s", body)
	}
	if strings.Contains(body, "gd-up-open-fallback") {
		t.Errorf("fallback bead must not render when in_progress selection succeeds; body=%s", body)
	}
	// The in_progress query should have been scoped to the viewer's canonical
	// operator id (oidc:<lowercased-email>), not the bare email (ka-0000002505).
	sawScoped := false
	for _, q := range f.listAllQ {
		if slices.Contains(q.Statuses, "in_progress") && slices.Contains(q.OperatedByAliases, "oidc:viewer@example.com") {
			sawScoped = true
		}
	}
	if !sawScoped {
		t.Errorf("expected in_progress query scoped to viewer email; listAllQ=%+v", f.listAllQ)
	}
}

func TestUp_FallsBackToHighestPriorityOpen(t *testing.T) {
	f := newFake()
	// in_progress query yields nothing — fall back to listResp (open).
	f.inProgressResp = &beadsapi.ListBeadsResult{}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:       "gd-up-open-top",
			Title:    "Top open task",
			Type:     "task",
			Status:   "open",
			Priority: 0,
		}},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/up", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "gd-up-open-top") {
		t.Errorf("expected fallback open bead to render; body=%s", body)
	}
	if !strings.Contains(body, "Top open task") {
		t.Errorf("expected title in body; body=%s", body)
	}
	// Confirm the priority-sorted open query was issued.
	gotOpenSort := false
	for _, q := range f.listAllQ {
		if slices.Contains(q.Statuses, "open") && q.Sort == "priority" && q.Limit == 1 {
			gotOpenSort = true
		}
	}
	if !gotOpenSort {
		t.Errorf("expected fallback open query (status=open, sort=priority, limit=1); listAllQ=%+v", f.listAllQ)
	}
}

func TestUp_DeepLinkRendersSpecificBead(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/up/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "gd-aaa") {
		t.Errorf("expected gd-aaa in deep-linked card; body=%s", body)
	}
	if !strings.Contains(body, "Example bead") {
		t.Errorf("expected bead title in deep-linked card; body=%s", body)
	}
}

func TestUp_DeepLinkUnknownBeadReturns404(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/up/gd-does-not-exist", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestSuggestedActionForType_KnownTypes(t *testing.T) {
	cases := map[string]string{
		"task":       "Bundle and close",
		"bug":        "Reproduce and close",
		"epic":       "Pick child",
		"agent":      "Nudge",
		"decision":   "Resolve",
		"escalation": "Acknowledge",
		"molecule":   "Apply",
		"formula":    "Apply",
		"schedule":   "Toggle enabled",
		"trigger":    "Toggle enabled",
	}
	for kind, want := range cases {
		got := suggestedActionForType(kind).Primary
		if got != want {
			t.Errorf("suggestedActionForType(%q).Primary = %q, want %q", kind, got, want)
		}
	}
	// Read-only types (message, doc, bundle, squawk) have no primary action.
	for _, kind := range []string{"message", "squawk", "doc", "bundle"} {
		if got := suggestedActionForType(kind).Primary; got != "" {
			t.Errorf("suggestedActionForType(%q).Primary = %q, want empty", kind, got)
		}
	}
	// Unknown types fall through to the generic Close/Skip pair.
	if got := suggestedActionForType("unknown-type").Primary; got != "Close" {
		t.Errorf("unknown type primary = %q, want Close", got)
	}
}

// --- Phase-2: type-conditional footer + keymap (ka-0000004641) --------------

func TestBuildUpFooter_PerTypePrimary(t *testing.T) {
	cases := []struct {
		typ         string
		wantPrimary string // "" ⇒ read-only (nil primary)
		readOnly    bool
	}{
		{"task", "Close", false},
		{"bug", "Comment + close", false},
		{"epic", "Pick child", false},
		{"agent", "Nudge", false},
		{"decision", "Resolve", false},
		{"escalation", "Acknowledge", false},
		{"molecule", "Apply", false},
		{"formula", "Apply", false},
		{"schedule", "Toggle enabled", false},
		{"trigger", "Toggle enabled", false},
		{"message", "", true},
		{"doc", "", true},
		{"bundle", "", true},
		{"squawk", "", true},
		{"weird-unknown", "Close", false},
	}
	for _, c := range cases {
		f := buildUpFooter(c.typ, false)
		if c.readOnly {
			if !f.ReadOnly {
				t.Errorf("%s: expected ReadOnly footer", c.typ)
			}
			if f.Primary != nil {
				t.Errorf("%s: read-only type must have nil primary, got %q", c.typ, f.Primary.Label)
			}
			continue
		}
		if f.Primary == nil {
			t.Fatalf("%s: expected a primary action, got nil", c.typ)
		}
		if f.Primary.Label != c.wantPrimary {
			t.Errorf("%s: primary = %q, want %q", c.typ, f.Primary.Label, c.wantPrimary)
		}
		if f.Primary.Key != "Enter" || !f.Primary.Primary {
			t.Errorf("%s: primary must be Enter-bound and flagged Primary; got key=%q primary=%v", c.typ, f.Primary.Key, f.Primary.Primary)
		}
		if len(f.Secondaries) > 4 {
			t.Errorf("%s: %d secondaries, PRD caps at 4", c.typ, len(f.Secondaries))
		}
	}
}

func TestBuildUpFooter_GlobalKeymapAlwaysPresent(t *testing.T) {
	// Every footer, even read-only, ships the full §4.2 global keymap.
	wantKeys := []string{"→", "Esc", "c", "r", "s", "e", "f", "g", "?", "/", "⌘K"}
	for _, typ := range []string{"task", "message", "decision"} {
		f := buildUpFooter(typ, false)
		got := map[string]bool{}
		for _, a := range f.Global {
			got[a.Key] = true
		}
		for _, k := range wantKeys {
			if !got[k] {
				t.Errorf("%s: global keymap missing key %q", typ, k)
			}
		}
	}
}

func TestBuildUpFooter_LiveVsDeferred(t *testing.T) {
	// task.Close hits the existing /b/{id}/close endpoint — must be LIVE.
	task := buildUpFooter("task", false)
	if task.Primary.Deferred {
		t.Error("task Close must be live (endpoint exists), not deferred")
	}
	if task.Primary.Endpoint != upEpClose || task.Primary.Method != "POST" || !task.Primary.AutoAdvance {
		t.Errorf("task Close endpoint/method/advance wrong: %+v", task.Primary)
	}
	// agent.Nudge is sibling-owned — must be flagged deferred so dispatch shows
	// the phase toast instead of hitting a missing endpoint.
	agent := buildUpFooter("agent", false)
	if !agent.Primary.Deferred {
		t.Error("agent Nudge must be deferred (agent verbs not yet proxied from /up)")
	}
	if agent.Primary.Endpoint != "" {
		t.Errorf("deferred action must carry no endpoint, got %q", agent.Primary.Endpoint)
	}
	// decision.Resolve hits the existing resolve endpoint — LIVE.
	dec := buildUpFooter("decision", true)
	if dec.Primary.Deferred || dec.Primary.Endpoint != upEpDecResolve {
		t.Errorf("decision Resolve must be live to %s; got %+v", upEpDecResolve, dec.Primary)
	}
	if dec.Primary.Label != "Resolve: recommended" {
		t.Errorf("decision with options should hint recommended; got %q", dec.Primary.Label)
	}
	// Decision with no parsed options still resolves (free-text), plain label.
	dec0 := buildUpFooter("decision", false)
	if dec0.Primary.Label != "Resolve" {
		t.Errorf("decision without options label = %q, want Resolve", dec0.Primary.Label)
	}
}

// The bug primary is "Comment + close": it opens the comment drawer AND carries
// AutoAdvance, which is the signal up.js uses to chain a close POST after the
// comment lands. If AutoAdvance were dropped, the primary would only comment and
// silently leave the bug open — the exact defect self-review caught.
func TestBuildUpFooter_BugPrimaryCommentThenClose(t *testing.T) {
	p := buildUpFooter("bug", false).Primary
	if p == nil {
		t.Fatal("bug footer missing primary")
	}
	if p.Kind != upActionDrawerComment {
		t.Errorf("bug primary kind = %q, want %q (opens the comment drawer)", p.Kind, upActionDrawerComment)
	}
	if !p.AutoAdvance {
		t.Error("bug primary must carry AutoAdvance — that is the up.js signal to close the bug after the comment lands")
	}
	if p.Deferred {
		t.Error("bug primary must not be Deferred — commenting + closing are live endpoints")
	}
}

// The escalate drawer must OPEN for real — only its submit is deferred. If the
// action itself were marked Deferred, up.js's dispatch() would short-circuit to
// a toast and the drawer (an acceptance-criteria deliverable) would be dead.
func TestBuildUpFooter_EscalateDrawerOpensNotDeferred(t *testing.T) {
	f := buildUpFooter("task", false)
	var esc *upAction
	for i := range f.Secondaries {
		if f.Secondaries[i].Key == "e" {
			esc = &f.Secondaries[i]
		}
	}
	if esc == nil {
		t.Fatal("task footer missing the escalate secondary")
	}
	if esc.Kind != upActionDrawerEscalate {
		t.Errorf("escalate kind = %q, want %q", esc.Kind, upActionDrawerEscalate)
	}
	if esc.Deferred {
		t.Error("escalate action must NOT be Deferred — the drawer opens for real; only submit is deferred")
	}
	// The global `e` entry must match (that is the always-available escalate).
	for _, g := range f.Global {
		if g.Key == "e" && g.Deferred {
			t.Error("global escalate key must not be Deferred either")
		}
	}
}

func TestUpMnemonicHTML(t *testing.T) {
	if got := string(upMnemonicHTML("Comment", 0)); got != "<u>C</u>omment" {
		t.Errorf("mnemonic 0 = %q", got)
	}
	if got := string(upMnemonicHTML("Skip", -1)); got != "Skip" {
		t.Errorf("mnemonic -1 = %q, want plain", got)
	}
	if got := string(upMnemonicHTML("Wrap & close", 0)); got != "<u>W</u>rap &amp; close" {
		t.Errorf("mnemonic must escape the rest: %q", got)
	}
	// Out-of-range index renders plain (never panics/indexes past the label).
	if got := string(upMnemonicHTML("Hi", 9)); got != "Hi" {
		t.Errorf("out-of-range mnemonic = %q, want plain", got)
	}
}

func TestMarshalUpKeymap_ShapeMatchesFooter(t *testing.T) {
	f := buildUpFooter("task", false)
	raw := string(marshalUpKeymap(f))
	var got struct {
		Primary     *upAction  `json:"primary"`
		Secondaries []upAction `json:"secondaries"`
		Global      []upAction `json:"global"`
		ReadOnly    bool       `json:"readOnly"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("keymap island is not valid JSON: %v\n%s", err, raw)
	}
	if got.Primary == nil || got.Primary.Label != "Close" {
		t.Errorf("island primary mismatch: %+v", got.Primary)
	}
	if len(got.Global) != len(f.Global) {
		t.Errorf("island global len %d != footer %d", len(got.Global), len(f.Global))
	}
	// The island is embedded in an inline <script>; default json.Marshal
	// escapes <,>,& so it can't break out of the tag. Assert no raw '<'.
	if strings.Contains(raw, "<") {
		t.Errorf("keymap island contains a raw '<' (script-injection risk): %s", raw)
	}
}

func TestUp_RendersTypeConditionalFooter(t *testing.T) {
	f := newFake() // gd-aaa is a task
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/up/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Primary Close button with dispatch attributes.
	if !strings.Contains(body, `class="up-action-primary`) {
		t.Errorf("expected primary action button; body=%s", body)
	}
	if !strings.Contains(body, `data-up-endpoint="/b/{id}/close"`) {
		t.Errorf("expected close endpoint on primary; body=%s", body)
	}
	// The keymap island up.js reads.
	if !strings.Contains(body, "window.UP_KEYMAP =") {
		t.Errorf("expected keymap island; body=%s", body)
	}
	// A secondary with a mnemonic-underlined label.
	if !strings.Contains(body, "<u>C</u>omment") {
		t.Errorf("expected mnemonic-underlined Comment secondary; body=%s", body)
	}
	// The comment drawer + help-triggering global keymap are present.
	if !strings.Contains(body, `data-up-drawer="comment"`) {
		t.Errorf("expected comment drawer; body=%s", body)
	}
	// data-surface="up" is the up.js gate.
	if !strings.Contains(body, `data-surface="up"`) {
		t.Errorf("expected data-surface=up gate; body=%s", body)
	}
}

func TestUp_ReadOnlyTypeHasNoPrimary(t *testing.T) {
	f := newFake()
	f.beads["gd-doc"] = &beadsapi.BeadDetail{
		ID: "gd-doc", Title: "A doc", Type: "doc", Status: "open",
		Labels: []string{"project:gardenio"}, UpdatedAt: time.Now(),
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/up/gd-doc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `class="up-action-primary`) {
		t.Errorf("read-only doc must not render a primary action; body=%s", body)
	}
	if !strings.Contains(body, "up-action-readonly") {
		t.Errorf("read-only doc should render the read-only marker; body=%s", body)
	}
	// Island reflects read-only.
	if !strings.Contains(body, `"readOnly":true`) {
		t.Errorf("keymap island should mark readOnly; body=%s", body)
	}
}

func TestMentionsBead(t *testing.T) {
	if !mentionsBead("feat(gd-abc123): add thing", "", "", "gd-abc123") {
		t.Error("title match should win")
	}
	if !mentionsBead("", "Implements gd-abc123 acceptance criteria", "", "gd-abc123") {
		t.Error("description match should win")
	}
	if !mentionsBead("", "", "feat/gd-abc123-thing", "gd-abc123") {
		t.Error("branch match should win")
	}
	if mentionsBead("nothing to see", "no mention", "main", "gd-abc123") {
		t.Error("no-match must be false")
	}
	if mentionsBead("gd-abc123", "", "", "") {
		t.Error("empty needle must be false")
	}
}

// TestUp_PrefersNeedsYouEscalation: an escalation that needs the viewer
// outranks their in_progress work; one in hand never reaches their /up.
func TestUp_PrefersNeedsYouEscalation(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.escalationsResp = []beadsapi.EscalationDetail{
		{Issue: makeEscBead("gd-up-laptop", "Held by laptop", "helm_deploy", "internal", "open", now.Add(-2*time.Hour),
			map[string]string{"held_by": "coordinator:laptop", "audience": "operator"})},
		{Issue: makeEscBead("gd-up-esc", "Needs the owner", "manual_owner_action", "internal", "open", now.Add(-time.Hour),
			map[string]string{"held_by": "operator", "audience": "owner"})},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{{ID: "gd-up-task", Title: "Task", Type: "task", Status: "in_progress"}}}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/up", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com") // edge identity: operator lane
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "gd-up-esc") || strings.Contains(body, "gd-up-laptop") || strings.Contains(body, "gd-up-task") {
		t.Fatalf("expected the owner-held escalation only; body=%s", body)
	}
}

func TestUp_SkipsEscalationInHand(t *testing.T) {
	f := newFake()
	f.escalationsResp = []beadsapi.EscalationDetail{
		{Issue: makeEscBead("gd-up-laptop", "Held by laptop", "helm_deploy", "internal", "open", time.Now(),
			map[string]string{"held_by": "coordinator:laptop", "audience": "operator"})},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{{ID: "gd-up-task", Title: "Task", Type: "task", Status: "in_progress"}}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/up", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if body := w.Body.String(); !strings.Contains(body, "gd-up-task") || strings.Contains(body, "gd-up-laptop") {
		t.Fatalf("want the in_progress task, not the laptop's escalation; body=%s", body)
	}
	// The /up bead picks are the priority-sorted queries; the server's other
	// lookups (the viewer's user bead) are not /up's to filter.
	picks := 0
	for _, q := range f.listAllQ {
		if q.Sort != "priority" {
			continue
		}
		picks++
		if !slices.Contains(q.ExcludeTypes, "escalation") {
			t.Errorf("/up bead query %+v does not exclude escalations; they come only from the needs-you pick", q)
		}
	}
	if picks == 0 {
		t.Errorf("no /up bead query recorded; queries = %+v", f.listAllQ)
	}
}
