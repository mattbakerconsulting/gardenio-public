package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// mrBead is a small helper that seeds a fakeDaemon with a type=merge-request
// bead so the preview handler exercises the MR branch.
func mrBead(id string, fields map[string]string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:          id,
		Title:       "Add login button",
		Type:        "merge-request",
		Status:      "open",
		Description: "Adds the login button to the navbar.",
		Fields:      fields,
	}
}

func TestHandleBeadPreview_MRPartialFires(t *testing.T) {
	f := newFake()
	f.beads["gd-mr1"] = mrBead("gd-mr1", map[string]string{
		"mr_url":   "https://gitlab.com/example/proj/-/merge_requests/42",
		"mr_state": "opened",
		"mr_iid":   "42",
	})
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-mr1/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="bead-preview bead-preview-mr"`,
		`data-bead-id="gd-mr1"`,
		`tag-type`,
		`>merge-request<`,
		`mr-state mr-state-opened`,
		`>opened<`,
		`href="https://gitlab.com/example/proj/-/merge_requests/42"`,
		`target="_blank"`,
		`rel="noopener"`,
		`!42`,
		`Add login button`,
		`Adds the login button`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("MR preview missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "<html") {
		t.Errorf("preview must be a fragment, not a page\nbody=%s", body)
	}
}

func TestHandleBeadPreview_MRFallsBackWhenURLAndStateMissing(t *testing.T) {
	f := newFake()
	f.beads["gd-mr2"] = mrBead("gd-mr2", map[string]string{})
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-mr2/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Card chrome still renders.
	for _, want := range []string{
		`class="bead-preview bead-preview-mr"`,
		`data-bead-id="gd-mr2"`,
		`>merge-request<`,
		`Add login button`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("MR preview (no fields) missing %q\nbody=%s", want, body)
		}
	}
	// State badge and MR link rows are suppressed when their fields are absent.
	for _, banned := range []string{
		`mr-state mr-state-`,
		`bead-preview-mr-link`,
		`↗`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("MR preview should suppress %q when mr_url/mr_state absent\nbody=%s", banned, body)
		}
	}
}

func TestHandleBeadPreview_MRURLAliases(t *testing.T) {
	// gitlab-bridge has historically written the MR URL under a few keys;
	// the popover should resolve any of them so old beads still render a link.
	cases := []struct {
		name   string
		fields map[string]string
	}{
		{"url alias", map[string]string{"url": "https://gitlab.com/x/y/-/merge_requests/7"}},
		{"gitlab_mr_url alias", map[string]string{"gitlab_mr_url": "https://gitlab.com/x/y/-/merge_requests/7"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.beads["gd-alias"] = mrBead("gd-alias", tc.fields)
			mux := mkServer(t, f)
			req := httptest.NewRequest("GET", "/b/gd-alias/preview", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			if !strings.Contains(w.Body.String(), `href="https://gitlab.com/x/y/-/merge_requests/7"`) {
				t.Errorf("MR preview did not resolve URL alias %s\nbody=%s", tc.name, w.Body.String())
			}
		})
	}
}

// TestHandleBeadPreview_GenericTypeStillUsesBaseTemplate guards against
// regressions in the type branch — non-MR beads must continue rendering via
// the original bead_preview partial (no mr-state markup, no MR link row).
func TestHandleBeadPreview_GenericTypeStillUsesBaseTemplate(t *testing.T) {
	mux := mkServer(t, newFake()) // gd-aaa is type=task
	req := httptest.NewRequest("GET", "/b/gd-aaa/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="bead-preview"`) {
		t.Errorf("generic preview lost base bead-preview class\nbody=%s", body)
	}
	for _, banned := range []string{
		`bead-preview-mr`,
		`bead-preview-mr-link`,
		`mr-state-`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("generic (type=task) preview must not contain %q — MR markup leaked\nbody=%s", banned, body)
		}
	}
}
