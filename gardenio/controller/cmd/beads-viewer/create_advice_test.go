package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestHandleCreateAdviceSheet_RendersForm(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"kbeads":   {Name: "kbeads"},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/advice/new", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-post="/advice"`,
		`name="title"`,
		`name="description"`,
		`name="priority"`,
		`name="scope"`,
		`name="scope_value"`,
		`>Global (everyone)<`,
		`>Project<`,
		`>Role<`,
		`>Agent<`,
		`Create Advice`,
		`value="gardenio"`,
		`value="kbeads"`,
		`value="crew"`,
		`value="gardener"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleCreateAdviceSheet_Unauthenticated(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/advice/new", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleCreateAdvice_HappyPath_Global(t *testing.T) {
	f := newFake()
	f.createResp = "gd-newadvice"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("title", "Always read the banner")
	form.Set("description", "Read the E2E banner before trusting a green result.")
	form.Set("priority", "1")
	form.Set("scope", "global")
	req := httptest.NewRequest("POST", "/advice", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Redirect"); got != "/gd-newadvice" {
		t.Errorf("HX-Redirect = %q, want /gd-newadvice", got)
	}
	if f.createCalls != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", f.createCalls)
	}
	got := f.createLastReq
	if got.Type != "advice" {
		t.Errorf("Type = %q, want advice", got.Type)
	}
	if got.Kind != "advice" {
		t.Errorf("Kind = %q, want advice", got.Kind)
	}
	if got.Title != "Always read the banner" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Description != "Read the E2E banner before trusting a green result." {
		t.Errorf("Description = %q", got.Description)
	}
	if got.Priority != 1 {
		t.Errorf("Priority = %d, want 1", got.Priority)
	}
	if got.CreatedBy != "alice@example.com" {
		t.Errorf("CreatedBy = %q", got.CreatedBy)
	}
	if !slices.Equal(got.Labels, []string{"global"}) {
		t.Errorf("Labels = %v, want [global]", got.Labels)
	}
}

func TestHandleCreateAdvice_ProjectScope(t *testing.T) {
	f := newFake()
	f.createResp = "gd-advice2"
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("description", "Project-scoped advice text.")
	form.Set("scope", "project")
	form.Set("scope_value", "gardenio")
	req := httptest.NewRequest("POST", "/advice", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "bob@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	got := f.createLastReq
	if !slices.Equal(got.Labels, []string{"project:gardenio"}) {
		t.Errorf("Labels = %v, want [project:gardenio]", got.Labels)
	}
	// Title falls back to description when blank.
	if got.Title != "Project-scoped advice text." {
		t.Errorf("Title = %q (expected description fallback)", got.Title)
	}
	// Default priority when not provided.
	if got.Priority != 2 {
		t.Errorf("Priority = %d, want 2 default", got.Priority)
	}
}

func TestHandleCreateAdvice_RoleAndAgentScopes(t *testing.T) {
	cases := []struct {
		kind, value, wantLabel string
	}{
		{"role", "gardener", "role:gardener"},
		{"agent", "gardenio-foo-bar", "agent:gardenio-foo-bar"},
	}
	for _, tc := range cases {
		f := newFake()
		f.createResp = "gd-x"
		mux := mkServer(t, f)

		form := url.Values{}
		form.Set("description", "advice body")
		form.Set("scope", tc.kind)
		form.Set("scope_value", tc.value)
		req := httptest.NewRequest("POST", "/advice", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("[%s] expected 200, got %d body=%s", tc.kind, w.Code, w.Body.String())
		}
		if !slices.Equal(f.createLastReq.Labels, []string{tc.wantLabel}) {
			t.Errorf("[%s] Labels = %v, want [%s]", tc.kind, f.createLastReq.Labels, tc.wantLabel)
		}
	}
}

func TestHandleCreateAdvice_MissingDescription_Rejected(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("title", "Title only")
	form.Set("scope", "global")
	req := httptest.NewRequest("POST", "/advice", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("expected no CreateBead call on validation failure, got %d", f.createCalls)
	}
	if !strings.Contains(w.Body.String(), "Advice text is required") {
		t.Errorf("expected validation error in body; got %s", w.Body.String())
	}
}

func TestHandleCreateAdvice_NonGlobalScopeMissingValue_Rejected(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("description", "advice body")
	form.Set("scope", "project")
	// scope_value omitted → invalid
	req := httptest.NewRequest("POST", "/advice", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("expected no CreateBead call, got %d", f.createCalls)
	}
	if !strings.Contains(w.Body.String(), "Scope value is required") {
		t.Errorf("expected scope-value error; got %s", w.Body.String())
	}
}

func TestHandleCreateAdvice_DaemonError_RendersError(t *testing.T) {
	f := newFake()
	f.createErr = errors.New("daemon exploded")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("description", "advice body")
	form.Set("scope", "global")
	req := httptest.NewRequest("POST", "/advice", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "daemon exploded") {
		t.Errorf("expected daemon error in body; got %s", w.Body.String())
	}
	if w.Header().Get("HX-Redirect") != "" {
		t.Errorf("HX-Redirect should not be set on error")
	}
}

func TestHandleCreateAdvice_Unauthenticated(t *testing.T) {
	mux := mkServer(t, newFake())
	form := url.Values{}
	form.Set("description", "x")
	req := httptest.NewRequest("POST", "/advice", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAdvicePage_RendersNewAdviceButton(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/advice", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`+ New Advice`,
		`hx-get="/advice/new"`,
		`hx-target="#quick-spawn-host"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("advice page missing %q", want)
		}
	}
}

func TestBuildAdviceLabels(t *testing.T) {
	cases := []struct {
		kind, value string
		want        []string
		wantErr     bool
	}{
		{"global", "", []string{"global"}, false},
		{"global", "ignored", []string{"global"}, false},
		{"", "", []string{"global"}, false}, // empty defaults to global
		{"project", "gardenio", []string{"project:gardenio"}, false},
		{"role", "gardener", []string{"role:gardener"}, false},
		{"agent", "x", []string{"agent:x"}, false},
		{"project", "", nil, true},
		{"role", "", nil, true},
		{"agent", "", nil, true},
		{"unknown", "", nil, true},
	}
	for _, tc := range cases {
		got, err := buildAdviceLabels(tc.kind, tc.value)
		if tc.wantErr {
			if err == nil {
				t.Errorf("buildAdviceLabels(%q,%q): want error, got nil", tc.kind, tc.value)
			}
			continue
		}
		if err != nil {
			t.Errorf("buildAdviceLabels(%q,%q): unexpected err %v", tc.kind, tc.value, err)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("buildAdviceLabels(%q,%q) = %v, want %v", tc.kind, tc.value, got, tc.want)
		}
	}
}
