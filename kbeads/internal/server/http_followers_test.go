package server

import (
	"net/http"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestHandleAddFollower(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-fwd1"] = &model.Bead{ID: "gd-fwd1", Title: "Bead to forward", Status: model.StatusOpen}

	rec := doJSON(t, h, "POST", "/v1/beads/gd-fwd1/followers", map[string]any{
		"operator_id": 42,
		"note":        "fyi — you owned this last quarter",
	})
	requireStatus(t, rec, http.StatusCreated)

	var resp struct {
		BeadID     string `json:"bead_id"`
		OperatorID int64  `json:"operator_id"`
		Note       string `json:"note"`
	}
	decodeJSON(t, rec, &resp)
	if resp.BeadID != "gd-fwd1" || resp.OperatorID != 42 || resp.Note != "fyi — you owned this last quarter" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	if _, ok := ms.followers[mockFollowerKey{beadID: "gd-fwd1", operatorID: 42}]; !ok {
		t.Fatalf("expected follower row to be persisted")
	}
}

func TestHandleAddFollower_Idempotent(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-fwd2"] = &model.Bead{ID: "gd-fwd2", Title: "Bead", Status: model.StatusOpen}

	for range 2 {
		rec := doJSON(t, h, "POST", "/v1/beads/gd-fwd2/followers", map[string]any{
			"operator_id": 7,
			"note":        "second forward wins",
		})
		requireStatus(t, rec, http.StatusCreated)
	}

	count := 0
	for k := range ms.followers {
		if k.beadID == "gd-fwd2" && k.operatorID == 7 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one follower row after idempotent re-forward, got %d", count)
	}
}

func TestHandleAddFollower_BadRequest(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-fwd3"] = &model.Bead{ID: "gd-fwd3", Title: "Bead", Status: model.StatusOpen}

	t.Run("missing operator_id", func(t *testing.T) {
		rec := doJSON(t, h, "POST", "/v1/beads/gd-fwd3/followers", map[string]any{"note": "x"})
		requireStatus(t, rec, http.StatusBadRequest)
	})

	t.Run("zero operator_id", func(t *testing.T) {
		rec := doJSON(t, h, "POST", "/v1/beads/gd-fwd3/followers", map[string]any{"operator_id": 0})
		requireStatus(t, rec, http.StatusBadRequest)
	})

	t.Run("negative operator_id", func(t *testing.T) {
		rec := doJSON(t, h, "POST", "/v1/beads/gd-fwd3/followers", map[string]any{"operator_id": -1})
		requireStatus(t, rec, http.StatusBadRequest)
	})
}

func TestHandleAddFollower_BeadNotFound(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads/gd-missing/followers", map[string]any{"operator_id": 1})
	requireStatus(t, rec, http.StatusNotFound)
}

func TestHandleRemoveFollower(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-fwd4"] = &model.Bead{ID: "gd-fwd4", Title: "Bead", Status: model.StatusOpen}
	ms.followers = map[mockFollowerKey]*model.BeadFollower{
		{beadID: "gd-fwd4", operatorID: 99}: {
			BeadID: "gd-fwd4", OperatorID: 99, CreatedAt: time.Now().UTC(),
		},
	}

	rec := doJSON(t, h, "DELETE", "/v1/beads/gd-fwd4/followers/99", nil)
	requireStatus(t, rec, http.StatusNoContent)

	if _, ok := ms.followers[mockFollowerKey{beadID: "gd-fwd4", operatorID: 99}]; ok {
		t.Fatalf("expected follower row to be removed")
	}
}

func TestHandleRemoveFollower_IdempotentMiss(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-fwd5"] = &model.Bead{ID: "gd-fwd5", Title: "Bead", Status: model.StatusOpen}

	rec := doJSON(t, h, "DELETE", "/v1/beads/gd-fwd5/followers/123", nil)
	requireStatus(t, rec, http.StatusNoContent)
}

func TestHandleRemoveFollower_BadOperatorID(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-fwd6"] = &model.Bead{ID: "gd-fwd6", Title: "Bead", Status: model.StatusOpen}

	for _, tc := range []string{"abc", "0", "-1", "1.5"} {
		t.Run(tc, func(t *testing.T) {
			rec := doJSON(t, h, "DELETE", "/v1/beads/gd-fwd6/followers/"+tc, nil)
			requireStatus(t, rec, http.StatusBadRequest)
		})
	}
}

func TestHandleListFollowers(t *testing.T) {
	_, ms, h := newTestServer()
	now := time.Now().UTC()
	ms.followers = map[mockFollowerKey]*model.BeadFollower{
		{beadID: "gd-list1", operatorID: 1}: {
			BeadID: "gd-list1", OperatorID: 1, OperatorDisplay: "Alice", CreatedAt: now,
		},
		{beadID: "gd-list1", operatorID: 2}: {
			BeadID: "gd-list1", OperatorID: 2, OperatorDisplay: "Bob", CreatedAt: now.Add(time.Second),
		},
		{beadID: "gd-other", operatorID: 3}: {
			BeadID: "gd-other", OperatorID: 3, CreatedAt: now,
		},
	}

	rec := doJSON(t, h, "GET", "/v1/beads/gd-list1/followers", nil)
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		Followers []*model.BeadFollower `json:"followers"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Followers) != 2 {
		t.Fatalf("expected 2 followers, got %d", len(resp.Followers))
	}
	// Mock returns oldest-first.
	if resp.Followers[0].OperatorID != 1 || resp.Followers[1].OperatorID != 2 {
		t.Fatalf("unexpected order: %+v", resp.Followers)
	}
}

func TestHandleListFollowers_Empty(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/beads/gd-empty/followers", nil)
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		Followers []*model.BeadFollower `json:"followers"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Followers) != 0 {
		t.Fatalf("expected empty followers, got %d", len(resp.Followers))
	}
}

// TestHandleListFollowedBeadIDs pins the inbox-UNION right side: the daemon
// returns the bead IDs the operator follows, ordered by follow created_at DESC
// (most-recently-forwarded at the top). ka-0000007204.
func TestHandleListFollowedBeadIDs(t *testing.T) {
	_, ms, h := newTestServer()
	now := time.Now().UTC()
	ms.followers = map[mockFollowerKey]*model.BeadFollower{
		{beadID: "gd-old", operatorID: 42}:    {BeadID: "gd-old", OperatorID: 42, CreatedAt: now.Add(-2 * time.Hour)},
		{beadID: "gd-recent", operatorID: 42}: {BeadID: "gd-recent", OperatorID: 42, CreatedAt: now.Add(-1 * time.Minute)},
		{beadID: "gd-mid", operatorID: 42}:    {BeadID: "gd-mid", OperatorID: 42, CreatedAt: now.Add(-1 * time.Hour)},
		{beadID: "gd-other", operatorID: 7}:   {BeadID: "gd-other", OperatorID: 7, CreatedAt: now}, // belongs to a different operator
	}

	rec := doJSON(t, h, "GET", "/v1/operators/42/followed-beads", nil)
	requireStatus(t, rec, http.StatusOK)

	var resp struct {
		BeadIDs []string `json:"bead_ids"`
	}
	decodeJSON(t, rec, &resp)
	want := []string{"gd-recent", "gd-mid", "gd-old"}
	if len(resp.BeadIDs) != len(want) {
		t.Fatalf("expected %d ids, got %d: %v", len(want), len(resp.BeadIDs), resp.BeadIDs)
	}
	for i, id := range want {
		if resp.BeadIDs[i] != id {
			t.Fatalf("at index %d: want %s, got %s (full: %v)", i, id, resp.BeadIDs[i], resp.BeadIDs)
		}
	}
}

func TestHandleListFollowedBeadIDs_Empty(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/operators/55/followed-beads", nil)
	requireStatus(t, rec, http.StatusOK)
	var resp struct {
		BeadIDs []string `json:"bead_ids"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.BeadIDs) != 0 {
		t.Fatalf("expected zero ids, got %d: %v", len(resp.BeadIDs), resp.BeadIDs)
	}
}

func TestHandleListFollowedBeadIDs_BadOperatorID(t *testing.T) {
	_, _, h := newTestServer()
	for _, tc := range []string{"abc", "0", "-1", "1.5"} {
		t.Run(tc, func(t *testing.T) {
			rec := doJSON(t, h, "GET", "/v1/operators/"+tc+"/followed-beads", nil)
			requireStatus(t, rec, http.StatusBadRequest)
		})
	}
}

func TestHandleListFollowedBeadIDs_BadLimit(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/operators/42/followed-beads?limit=abc", nil)
	requireStatus(t, rec, http.StatusBadRequest)

	rec = doJSON(t, h, "GET", "/v1/operators/42/followed-beads?limit=-5", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestHandleListFollowedBeadIDs_RespectsLimit(t *testing.T) {
	_, ms, h := newTestServer()
	now := time.Now().UTC()
	ms.followers = map[mockFollowerKey]*model.BeadFollower{
		{beadID: "gd-a", operatorID: 1}: {BeadID: "gd-a", OperatorID: 1, CreatedAt: now.Add(-3 * time.Hour)},
		{beadID: "gd-b", operatorID: 1}: {BeadID: "gd-b", OperatorID: 1, CreatedAt: now.Add(-2 * time.Hour)},
		{beadID: "gd-c", operatorID: 1}: {BeadID: "gd-c", OperatorID: 1, CreatedAt: now.Add(-1 * time.Hour)},
	}
	rec := doJSON(t, h, "GET", "/v1/operators/1/followed-beads?limit=2", nil)
	requireStatus(t, rec, http.StatusOK)
	var resp struct {
		BeadIDs []string `json:"bead_ids"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.BeadIDs) != 2 {
		t.Fatalf("expected 2 ids, got %d: %v", len(resp.BeadIDs), resp.BeadIDs)
	}
	if resp.BeadIDs[0] != "gd-c" || resp.BeadIDs[1] != "gd-b" {
		t.Fatalf("expected newest-first within limit; got %v", resp.BeadIDs)
	}
}
