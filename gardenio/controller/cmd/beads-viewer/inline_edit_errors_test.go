package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const adminDenied = "insufficient permissions: update on type account requires gardenio-admin"

// TestDaemonWriteFailure pins the daemon-error → (banner, status) mapping:
// 4xx keeps its class and carries the daemon message, everything else is a
// generic 502 (gd-7Ml6sbsdu1).
func TestDaemonWriteFailure(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		{"403 keeps class + role", &beadsapi.APIError{StatusCode: 403, Message: adminDenied}, 403, "Failed to update labels: " + adminDenied},
		{"409 keeps class", &beadsapi.APIError{StatusCode: 409, Message: "stale write"}, 409, "Failed to update labels: stale write"},
		{"400 keeps class", &beadsapi.APIError{StatusCode: 400, Message: "bad label"}, 400, "Failed to update labels: bad label"},
		{"empty 4xx message falls back to base", &beadsapi.APIError{StatusCode: 403}, 403, "Failed to update labels."},
		{"401 stays 502 (would trip edge re-auth)", &beadsapi.APIError{StatusCode: 401, Message: "bad token"}, 502, "Failed to update labels."},
		{"500 stays 502, internals hidden", &beadsapi.APIError{StatusCode: 500, Message: "pq: secret"}, 502, "Failed to update labels."},
		{"transport error stays 502", errors.New("dial tcp: refused"), 502, "Failed to update labels."},
		{"whitespace collapsed", &beadsapi.APIError{StatusCode: 403, Message: "no\n\n  way"}, 403, "Failed to update labels: no way"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, status := daemonWriteFailure(tc.err, "Failed to update labels.")
			if status != tc.wantStatus || msg != tc.wantMsg {
				t.Errorf("got (%q, %d), want (%q, %d)", msg, status, tc.wantMsg, tc.wantStatus)
			}
		})
	}
}

// TestUpdateLabels_Daemon403_KeepsStatusAndShowsRole is the gd-FUkUFiusJP
// regression: a daemon 403 must reach the browser as 403 with the required role
// in the editor banner, flagged so htmx swaps it.
func TestUpdateLabels_Daemon403_KeepsStatusAndShowsRole(t *testing.T) {
	f := labelsFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: adminDenied}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedPost("/b/gd-lab1/labels", url.Values{"labels": {"draft:kept"}}))

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get(inlineErrorHeader) == "" {
		t.Errorf("response must carry %s so htmx swaps the error body", inlineErrorHeader)
	}
	body := w.Body.String()
	for _, want := range []string{"requires gardenio-admin", "draft:kept", `hx-post="/b/gd-lab1/labels"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body=%s", want, body)
		}
	}
}

// TestUpdateLabels_Daemon500_Stays502 covers the 5xx half: generic message,
// 502, still swappable.
func TestUpdateLabels_Daemon500_Stays502(t *testing.T) {
	f := labelsFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "pq: secret detail"}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedPost("/b/gd-lab1/labels", url.Values{"labels": {"x"}}))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	if w.Header().Get(inlineErrorHeader) == "" {
		t.Errorf("502 editor response must carry %s", inlineErrorHeader)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to update labels") {
		t.Errorf("expected generic banner, body=%s", body)
	}
	if strings.Contains(body, "secret detail") {
		t.Errorf("5xx daemon internals must not reach the browser, body=%s", body)
	}
}

// TestInlineEditors_Daemon403_RenderMessage runs the 403 through every inline
// editor: status preserved, role shown, swap flag set.
func TestInlineEditors_Daemon403_RenderMessage(t *testing.T) {
	editors := []struct {
		name, path string
		form       url.Values
	}{
		{"labels", "/b/gd-lab1/labels", url.Values{"labels": {"a"}}},
		{"priority", "/b/gd-lab1/priority", url.Values{"priority": {"1"}}},
		{"assignee", "/b/gd-lab1/assignee", url.Values{"assignee": {"bob"}}},
		{"owner", "/b/gd-lab1/owner", url.Values{"owner": {"bob"}}},
		{"title", "/b/gd-lab1/title", url.Values{"title": {"New title"}}},
		{"description", "/b/gd-lab1/description", url.Values{"description": {"new body"}}},
		{"status", "/b/gd-lab1/status", url.Values{"status": {"closed"}}},
	}
	for _, ed := range editors {
		t.Run(ed.name, func(t *testing.T) {
			f := labelsFake()
			f.updateErr = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: adminDenied}
			mux := mkServer(t, f)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, authedPost(ed.path, ed.form))

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
			}
			if w.Header().Get(inlineErrorHeader) == "" {
				t.Errorf("missing %s", inlineErrorHeader)
			}
			if !strings.Contains(w.Body.String(), "requires gardenio-admin") {
				t.Errorf("daemon role message not rendered, body=%s", w.Body.String())
			}
		})
	}
}

// TestLayout_SwapsInlineEditorErrors guards the client half: htmx drops 4xx/5xx
// bodies by default, so the layout must opt flagged responses back in.
func TestLayout_SwapsInlineEditorErrors(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{"X-BV-Inline-Error", "evt.detail.shouldSwap = true", "evt.detail.isError = false"} {
		if !strings.Contains(body, want) {
			t.Errorf("layout missing inline-error swap token %q", want)
		}
	}
}
