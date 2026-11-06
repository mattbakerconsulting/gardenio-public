package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestSubmitRating_InvalidScore_RendersErrorPartial(t *testing.T) {
	cases := []struct {
		name  string
		score string
	}{
		{"missing", ""},
		{"zero", "0"},
		{"too-high", "6"},
		{"non-numeric", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.beads["gd-tgt"] = &beadsapi.BeadDetail{
				ID: "gd-tgt", Title: "Target", Type: "task", Status: "open",
			}
			mux := mkServer(t, f)

			form := url.Values{}
			form.Set("score", tc.score)
			req := httptest.NewRequest("POST", "/b/gd-tgt/rating", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-Auth-Request-Email", "rater@example.com")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", w.Code)
			}
			if !strings.Contains(w.Body.String(), "Pick a score between 1 and 5.") {
				t.Errorf("expected validation error in body, body=%s", w.Body.String())
			}
			if f.createCalls != 0 {
				t.Errorf("invalid score should not CreateBead, got %d calls", f.createCalls)
			}
		})
	}
}

func TestSubmitRating_RationaleTooLong_Rejected(t *testing.T) {
	f := newFake()
	f.beads["gd-tgt"] = &beadsapi.BeadDetail{
		ID: "gd-tgt", Title: "Target", Type: "task", Status: "open",
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("score", "4")
	form.Set("rationale", strings.Repeat("x", maxRatingRationaleLen+1))
	req := httptest.NewRequest("POST", "/b/gd-tgt/rating", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "rater@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Rationale is too long") {
		t.Errorf("expected length error, body=%s", w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("over-long rationale should not CreateBead, got %d calls", f.createCalls)
	}
}

func TestSubmitRating_RateOfRating_Rejected(t *testing.T) {
	f := newFake()
	f.beads["gd-rat-target"] = &beadsapi.BeadDetail{
		ID: "gd-rat-target", Title: "rating: Foo · 3★", Type: "rating", Status: "open",
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("score", "4")
	req := httptest.NewRequest("POST", "/b/gd-rat-target/rating", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "rater@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Cannot rate a rating bead.") {
		t.Errorf("expected rate-of-rating rejection, body=%s", w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("rate-of-rating should not CreateBead, got %d calls", f.createCalls)
	}
}

func TestSubmitRating_SelfRating_Rejected(t *testing.T) {
	f := newFake()
	f.beads["gd-mine"] = &beadsapi.BeadDetail{
		ID:       "gd-mine",
		Title:    "My own work",
		Type:     "task",
		Status:   "open",
		Assignee: "rater@example.com",
	}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("score", "5")
	req := httptest.NewRequest("POST", "/b/gd-mine/rating", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "rater@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Cannot rate your own work.") {
		t.Errorf("expected self-rating rejection, body=%s", w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("self-rating should not CreateBead, got %d calls", f.createCalls)
	}
}

func TestSubmitRating_Unauthenticated_Rejected(t *testing.T) {
	f := newFake()
	f.beads["gd-tgt"] = &beadsapi.BeadDetail{ID: "gd-tgt", Title: "Target", Type: "task", Status: "open"}
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("score", "4")
	req := httptest.NewRequest("POST", "/b/gd-tgt/rating", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
