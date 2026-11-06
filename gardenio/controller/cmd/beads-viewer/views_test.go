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

func TestHandleViewsList_Empty_ShowsEmptyState(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/views", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No saved views yet.") {
		t.Errorf("expected empty-state copy, body=%s", body)
	}
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	q := f.listAllQ[0]
	if got := q.Types; len(got) != 1 || got[0] != "view" {
		t.Errorf("types = %v, want [view]", got)
	}
	if got := q.Statuses; len(got) != 1 || got[0] != "open" {
		t.Errorf("statuses = %v, want [open]", got)
	}
}

func TestHandleViewsList_Unauthenticated_ShowsSignInPrompt(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/views", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Sign in to save views.") {
		t.Errorf("expected sign-in prompt for unauthenticated user")
	}
}

func TestHandleViewsList_RendersRowsAndOwnership(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-v1",
				Title:     "My active gardenio work",
				Type:      "view",
				Status:    "open",
				Labels:    []string{"view:owner:alice@example.com"},
				CreatedBy: "alice@example.com",
				CreatedAt: time.Now().Add(-2 * time.Hour),
				Fields: map[string]string{
					"source":   "active",
					"project":  "gardenio",
					"statuses": "in_progress",
					"sort":     "priority",
				},
			},
			{
				ID:        "gd-v2",
				Title:     "Bob's decisions",
				Type:      "view",
				Status:    "open",
				Labels:    []string{"view:owner:bob@example.com"},
				CreatedBy: "bob@example.com",
				CreatedAt: time.Now().Add(-3 * 24 * time.Hour),
				Fields: map[string]string{
					"source":   "decisions",
					"types":    "decision",
					"statuses": "open",
				},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/views", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"My active gardenio work",
		"Bob&#39;s decisions",
		`href="/views/gd-v1"`,
		`href="/views/gd-v2"`,
		"alice@example.com",
		"bob@example.com",
		"(you)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody=%s", want, body)
		}
	}

	// Owned-by-me row carries a delete form; the other row does not.
	if !strings.Contains(body, `action="/views/gd-v1/delete"`) {
		t.Errorf("expected delete form for owned view gd-v1, body=%s", body)
	}
	if strings.Contains(body, `action="/views/gd-v2/delete"`) {
		t.Errorf("did not expect delete form for non-owned view gd-v2")
	}

	// "My views" should sort first.
	if i := strings.Index(body, "gd-v1"); i < 0 {
		t.Fatalf("gd-v1 not in body")
	} else if j := strings.Index(body, "gd-v2"); j < 0 {
		t.Fatalf("gd-v2 not in body")
	} else if i > j {
		t.Errorf("expected gd-v1 (owned) before gd-v2 (not owned)")
	}
}

func TestHandleViewsList_DaemonError_ShowsBanner(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: 500, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/views", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load saved views.") {
		t.Errorf("expected error banner")
	}
}

func TestHandleViewCreate_RequiresAuth(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := strings.NewReader("name=My+view&source=active")
	req := httptest.NewRequest("POST", "/views", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (body=%s)", w.Code, w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("CreateBead should not have been called when unauthenticated")
	}
}

func TestHandleViewCreate_RequiresName(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := strings.NewReader("name=&source=active")
	req := httptest.NewRequest("POST", "/views", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("CreateBead should not have been called with empty name")
	}
}

func TestHandleViewCreate_Success_RedirectsAndPersistsFields(t *testing.T) {
	f := newFake()
	f.createResp = "gd-newview"
	mux := mkServer(t, f)

	form := strings.NewReader(strings.Join([]string{
		"name=My+active+gardenio",
		"source=active",
		"project=gardenio",
		"statuses=in_progress",
		"sort=priority",
		"types=task,bug",
	}, "&"))
	req := httptest.NewRequest("POST", "/views", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body=%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/views/gd-newview" {
		t.Errorf("Location = %q, want /views/gd-newview", got)
	}
	if f.createCalls != 1 {
		t.Fatalf("CreateBead calls = %d, want 1", f.createCalls)
	}
	got := f.createLastReq
	if got.Title != "My active gardenio" {
		t.Errorf("title = %q, want %q", got.Title, "My active gardenio")
	}
	if got.Type != "view" {
		t.Errorf("type = %q, want view", got.Type)
	}
	if got.CreatedBy != "alice@example.com" {
		t.Errorf("created_by = %q, want alice@example.com", got.CreatedBy)
	}
	if !slices.Contains(got.Labels, "view:owner:alice@example.com") {
		t.Errorf("labels = %v, want owner label included", got.Labels)
	}

	var fields map[string]string
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}
	wantFields := map[string]string{
		"source":   "active",
		"project":  "gardenio",
		"statuses": "in_progress",
		"sort":     "priority",
		"types":    "task,bug",
	}
	for k, v := range wantFields {
		if fields[k] != v {
			t.Errorf("field %q = %q, want %q (all=%v)", k, fields[k], v, fields)
		}
	}
}

func TestHandleViewShow_RendersBeadsFromQuery(t *testing.T) {
	f := newFake()
	// Saved view bead.
	f.beads["gd-v1"] = &beadsapi.BeadDetail{
		ID:        "gd-v1",
		Title:     "My active gardenio",
		Type:      "view",
		Status:    "open",
		CreatedBy: "alice@example.com",
		Labels:    []string{"view:owner:alice@example.com"},
		Fields: map[string]string{
			"source":   "active",
			"project":  "gardenio",
			"statuses": "in_progress",
		},
	}
	// listResp is what /views/{id} re-runs (project filter resolves via Labels).
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-task1",
				Title:     "Wire OIDC bypass",
				Type:      "task",
				Status:    "in_progress",
				Priority:  1,
				Assignee:  "alice",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: time.Now().Add(-1 * time.Hour),
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/views/gd-v1", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"My active gardenio",
		"gd-task1",
		"Wire OIDC bypass",
		"alice@example.com",
		"(you)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody=%s", want, body)
		}
	}

	// Confirm the daemon query was reconstructed from the saved fields.
	// The reconstructed query is one of the listAllQ entries — it is no
	// longer guaranteed to be the last one, because the central render seam
	// now issues a trailing user-bead lookup (Types=[user], Kinds=[config])
	// to resolve the viewer's VibeMode (ka-0000007201). Search for the
	// reconstruction rather than assuming its position.
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called for the view show")
	}
	var found bool
	for _, q := range f.listAllQ {
		if slices.Contains(q.Statuses, "in_progress") && slices.Contains(q.Labels, "project:gardenio") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no reconstructed view query (Statuses⊇in_progress, Labels⊇project:gardenio) in %v", f.listAllQ)
	}
}

func TestHandleViewShow_NonViewBead_400(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	// gd-aaa exists in newFake() as a task, not a view.
	req := httptest.NewRequest("GET", "/views/gd-aaa", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-view bead, got %d", w.Code)
	}
}

func TestHandleViewShow_NotFound_404(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/views/gd-missing", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleViewDelete_OwnerCanDelete(t *testing.T) {
	f := newFake()
	f.beads["gd-v1"] = &beadsapi.BeadDetail{
		ID:        "gd-v1",
		Title:     "My view",
		Type:      "view",
		Status:    "open",
		CreatedBy: "alice@example.com",
		Labels:    []string{"view:owner:alice@example.com"},
		Fields:    map[string]string{},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("POST", "/views/gd-v1/delete", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body=%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/views?deleted=1" {
		t.Errorf("Location = %q, want /views?deleted=1", got)
	}
	if f.deleteCalls != 1 {
		t.Errorf("DeleteBead calls = %d, want 1", f.deleteCalls)
	}
	if f.deleteLastID != "gd-v1" {
		t.Errorf("deleted id = %q, want gd-v1", f.deleteLastID)
	}
}

func TestHandleViewDelete_NonOwner_Forbidden(t *testing.T) {
	f := newFake()
	f.beads["gd-v1"] = &beadsapi.BeadDetail{
		ID:        "gd-v1",
		Title:     "Bob's view",
		Type:      "view",
		Status:    "open",
		CreatedBy: "bob@example.com",
		Labels:    []string{"view:owner:bob@example.com"},
		Fields:    map[string]string{},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("POST", "/views/gd-v1/delete", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if f.deleteCalls != 0 {
		t.Errorf("DeleteBead should not have been called")
	}
}

func TestHandleViewDelete_RequiresAuth(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	req := httptest.NewRequest("POST", "/views/gd-v1/delete", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestParseViewSpec_RoundTrip(t *testing.T) {
	original := savedViewSpec{
		Source:   "active",
		Project:  "gardenio",
		Types:    []string{"task", "bug"},
		Statuses: []string{"in_progress", "open"},
		Labels:   []string{"role:crew"},
		Search:   "auth",
		Sort:     "priority",
		Columns:  []string{"id", "title", "ago"},
	}
	got := parseViewSpec(original.toBeadFields())
	if got.Source != original.Source {
		t.Errorf("source: got %q, want %q", got.Source, original.Source)
	}
	if got.Project != original.Project {
		t.Errorf("project: got %q, want %q", got.Project, original.Project)
	}
	if !slices.Equal(got.Types, original.Types) {
		t.Errorf("types: got %v, want %v", got.Types, original.Types)
	}
	if !slices.Equal(got.Statuses, original.Statuses) {
		t.Errorf("statuses: got %v, want %v", got.Statuses, original.Statuses)
	}
	if !slices.Equal(got.Labels, original.Labels) {
		t.Errorf("labels: got %v, want %v", got.Labels, original.Labels)
	}
	if got.Search != original.Search {
		t.Errorf("search: got %q, want %q", got.Search, original.Search)
	}
	if got.Sort != original.Sort {
		t.Errorf("sort: got %q, want %q", got.Sort, original.Sort)
	}
	if !slices.Equal(got.Columns, original.Columns) {
		t.Errorf("columns: got %v, want %v", got.Columns, original.Columns)
	}
}

func TestSavedViewSpec_ToQuery_DecisionsDefaults(t *testing.T) {
	spec := savedViewSpec{Source: "decisions"}
	q := spec.toQuery()
	if !slices.Contains(q.Types, "decision") {
		t.Errorf("expected decision in types, got %v", q.Types)
	}
	if !slices.Contains(q.Statuses, "open") {
		t.Errorf("expected open in statuses, got %v", q.Statuses)
	}
	if q.Sort != "-created_at" {
		t.Errorf("sort = %q, want -created_at", q.Sort)
	}
}

func TestSavedViewSpec_ToQuery_ExplicitOverridesDefaults(t *testing.T) {
	spec := savedViewSpec{
		Source:   "decisions",
		Statuses: []string{"closed"},
		Sort:     "priority",
	}
	q := spec.toQuery()
	if !slices.Equal(q.Statuses, []string{"closed"}) {
		t.Errorf("expected explicit statuses to win, got %v", q.Statuses)
	}
	if q.Sort != "priority" {
		t.Errorf("sort = %q, want priority", q.Sort)
	}
}

func TestSavedViewSpec_ToQuery_ProjectAddsLabel(t *testing.T) {
	spec := savedViewSpec{Source: "active", Project: "gardenio"}
	q := spec.toQuery()
	if !slices.Contains(q.Labels, "project:gardenio") {
		t.Errorf("expected project label, got %v", q.Labels)
	}
}
