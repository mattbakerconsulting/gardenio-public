package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestResolveGardenerOwner table-tests the three-tier owner resolution
// (UserBeadID → SlackUserID → trigger operated_by_id/operated_by), including
// the empty-slug "label without a link" degrade and the "nothing resolves"
// case (design doc ka-0000003219 §6).
func TestResolveGardenerOwner(t *testing.T) {
	ptr := func(n int64) *int64 { return &n }

	cases := []struct {
		name        string
		bead        *beadsapi.BeadDetail
		userBeadID  string
		slackUserID string
		seed        func(*fakeDaemon)
		wantDisplay string
		wantSlug    string
	}{
		{
			name:       "tier1-user-bead-wins",
			bead:       &beadsapi.BeadDetail{OperatedByDisplay: "fallback", OperatedBySlug: "fallback-slug"},
			userBeadID: "gd-user1",
			seed: func(f *fakeDaemon) {
				f.beads["gd-user1"] = &beadsapi.BeadDetail{
					ID:     "gd-user1",
					Type:   "user",
					Fields: map[string]string{"display_name": "Alice Owner", "operator_id": "42"},
				}
				f.opSlugResp = map[int64]*beadsapi.GetOperatorSlugResponse{
					42: {Slug: "alice", Found: true},
				}
			},
			wantDisplay: "Alice Owner",
			wantSlug:    "alice",
		},
		{
			name:       "tier1-user-bead-no-slug-renders-label",
			bead:       &beadsapi.BeadDetail{OperatedByDisplay: "fallback", OperatedBySlug: "fallback-slug"},
			userBeadID: "gd-user2",
			seed: func(f *fakeDaemon) {
				// No operator_id ⇒ no slug lookup; display still resolves.
				f.beads["gd-user2"] = &beadsapi.BeadDetail{
					ID:     "gd-user2",
					Type:   "user",
					Fields: map[string]string{"email": "bob@acme.example"},
				}
			},
			wantDisplay: "bob@acme.example",
			wantSlug:    "",
		},
		{
			name:        "tier2-slack-alias",
			bead:        &beadsapi.BeadDetail{OperatedByDisplay: "fallback", OperatedBySlug: "fallback-slug"},
			slackUserID: "U0OPERATOR1",
			seed: func(f *fakeDaemon) {
				f.aliasResp = map[string]*beadsapi.LookupOperatorByAliasResponse{
					"slack|U0OPERATOR1": {OperatorID: 7, Found: true},
				}
				f.opSlugResp = map[int64]*beadsapi.GetOperatorSlugResponse{
					7: {Slug: "carol", Found: true},
				}
				f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
					Operators: []beadsapi.OperatorDirectoryEntry{
						{ID: 7, Display: "Carol Gardener", Email: "carol@acme.example"},
					},
				}
			},
			wantDisplay: "Carol Gardener",
			wantSlug:    "carol",
		},
		{
			name:        "tier2-slack-alias-unresolved-display-falls-to-slack-label",
			bead:        &beadsapi.BeadDetail{},
			slackUserID: "U999",
			seed: func(f *fakeDaemon) {
				f.aliasResp = map[string]*beadsapi.LookupOperatorByAliasResponse{
					"slack|U999": {OperatorID: 9, Found: true},
				}
				// No directory entry ⇒ display falls back to slack:<id>; slug
				// resolver returns the op-<id> sentinel.
			},
			wantDisplay: "slack:U999",
			wantSlug:    "op-9",
		},
		{
			name:        "tier3-trigger-operated-by",
			bead:        &beadsapi.BeadDetail{OperatedByDisplay: "Dave Operator", OperatedBySlug: "dave"},
			wantDisplay: "Dave Operator",
			wantSlug:    "dave",
		},
		{
			name:        "tier3-trigger-operated-by-no-slug",
			bead:        &beadsapi.BeadDetail{OperatedByDisplay: "Eve Operator"},
			wantDisplay: "Eve Operator",
			wantSlug:    "",
		},
		{
			name:        "nothing-resolves",
			bead:        &beadsapi.BeadDetail{},
			wantDisplay: "",
			wantSlug:    "",
		},
		{
			name:        "tier1-falls-through-to-tier3-when-user-bead-missing",
			bead:        &beadsapi.BeadDetail{OperatedByID: ptr(3), OperatedByDisplay: "Frank Operator", OperatedBySlug: "frank"},
			userBeadID:  "gd-missing-user",
			wantDisplay: "Frank Operator",
			wantSlug:    "frank",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			if tc.seed != nil {
				tc.seed(f)
			}
			srv := NewServer(f, testLogger(), "", "", "", nil)
			d, sl := srv.resolveGardenerOwner(context.Background(), tc.bead, tc.userBeadID, tc.slackUserID)
			if d != tc.wantDisplay {
				t.Errorf("display = %q, want %q", d, tc.wantDisplay)
			}
			if sl != tc.wantSlug {
				t.Errorf("slug = %q, want %q", sl, tc.wantSlug)
			}
		})
	}
}

// TestHandleGardenerDetail_OwnerChip asserts the gardener page header renders an
// owner chip linking to /u/{slug} when the owner slug resolves (tier-1 user
// bead path), and renders the plain label without a link when the slug is empty.
func TestHandleGardenerDetail_OwnerChip(t *testing.T) {
	mkGardenerBead := func(spec map[string]any) *beadsapi.BeadDetail {
		raw, _ := json.Marshal(spec)
		return &beadsapi.BeadDetail{
			ID:        "gd-cap",
			Title:     "Owned gardener",
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule", "role:gardener"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}
	}

	t.Run("linked-when-slug-resolves", func(t *testing.T) {
		f := newFake()
		f.beads["gd-cap"] = mkGardenerBead(map[string]any{
			"name": "owned", "user_bead_id": "gd-user1",
		})
		f.beads["gd-user1"] = &beadsapi.BeadDetail{
			ID: "gd-user1", Type: "user",
			Fields: map[string]string{"display_name": "Alice Owner", "operator_id": "42"},
		}
		f.opSlugResp = map[int64]*beadsapi.GetOperatorSlugResponse{42: {Slug: "alice", Found: true}}
		f.liveAgentsResp = &beadsapi.ListBeadsResult{}
		mux := mkServerWithNow(t, f, gardenerPageNow)

		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/gardener/gd-cap", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `href="/u/alice"`) {
			t.Errorf("owner chip missing /u/alice link; body=%s", body)
		}
		if !strings.Contains(body, "Alice Owner") {
			t.Errorf("owner chip missing display name")
		}
	})

	t.Run("plain-label-when-slug-empty", func(t *testing.T) {
		f := newFake()
		f.beads["gd-cap"] = mkGardenerBead(map[string]any{
			"name": "owned", "user_bead_id": "gd-user2",
		})
		// User bead has a display but no operator_id ⇒ no slug ⇒ no link.
		f.beads["gd-user2"] = &beadsapi.BeadDetail{
			ID: "gd-user2", Type: "user",
			Fields: map[string]string{"display_name": "Bob NoSlug"},
		}
		f.liveAgentsResp = &beadsapi.ListBeadsResult{}
		mux := mkServerWithNow(t, f, gardenerPageNow)

		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/gardener/gd-cap", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "Bob NoSlug") {
			t.Errorf("owner label missing")
		}
		if strings.Contains(body, `href="/u/op-`) || strings.Contains(body, `href="/u/Bob`) {
			t.Errorf("owner should render without a link when slug is empty; body=%s", body)
		}
	})
}

// TestHandleUserHome_TheirGardenersSection asserts /u/{slug} renders a "Their
// gardeners" section listing the gardeners whose *resolved* owner is the page
// owner — and only those — with each row linking to /gardener/{id} rather than
// the generic bead page (design doc §6.2.2, ka-0000004692). The section keys on
// the three-tier resolved owner slug, not operated_by_id, so a gardener owned by
// a different operator is excluded even though it comes back in the same list.
func TestHandleUserHome_TheirGardenersSection(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mineOp := int64(42)
	otherOp := int64(99)
	// The "Their gardeners" query is the only Types=[trigger] query the home
	// page issues; route both gardeners through triggerScheduleResp. gd-capA
	// resolves (tier-3 operated_by) to alice; gd-capB resolves to a different
	// operator and must be filtered out.
	ts := time.Now().Add(-90 * time.Minute)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:                "gd-capA",
				Title:             "Delivery gardener",
				Type:              "trigger",
				Status:            "open",
				Labels:            []string{"trigger:schedule", "role:gardener", "project:gardenio"},
				OperatedByID:      &mineOp,
				OperatedByDisplay: "Alice Owner",
				OperatedBySlug:    "alice",
				UpdatedAt:         ts,
			},
			{
				ID:                "gd-capB",
				Title:             "Someone else's gardener",
				Type:              "trigger",
				Status:            "open",
				Labels:            []string{"trigger:schedule", "role:gardener", "project:gardenio"},
				OperatedByID:      &otherOp,
				OperatedByDisplay: "Other Owner",
				OperatedBySlug:    "bob",
				UpdatedAt:         ts,
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/u/alice", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Gardeners now interleave into the single merged activity spine (bug
	// ka-0000002041) rather than their own section card — but each still keeps
	// its /gardener/{id} deep-link and the owner-resolution filter still drops
	// gardeners owned by another operator.
	if !strings.Contains(body, `id="activity"`) {
		t.Errorf("merged activity spine not rendered; body=%s", body)
	}
	if !strings.Contains(body, `href="/gardener/gd-capA"`) {
		t.Errorf("gardener row should link to /gardener/gd-capA; body=%s", body)
	}
	if strings.Contains(body, `href="/gardener/gd-capB"`) {
		t.Errorf("gardener owned by another operator must not appear in alice's stream; body=%s", body)
	}
}
