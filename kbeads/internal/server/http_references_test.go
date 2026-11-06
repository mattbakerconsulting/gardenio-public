package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestHandleListMentionedBy_HappyPath(t *testing.T) {
	_, ms, h := newTestServer()
	ms.mentionedBy = []*model.BeadReferenceSummary{
		{ID: "gd-ldkpaoah1s", Title: "alpha", Type: "task", Status: "open", SourceField: "description"},
		{ID: "gd-hsr6nhjuuy", Title: "beta", Type: "epic", Status: "in_progress", SourceField: "comment:7"},
	}
	ms.mentionedByTotal = 2

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by?limit=25&offset=10", nil)
	requireStatus(t, rec, http.StatusOK)

	if ms.lastMentionedByID != "gd-target00001" {
		t.Errorf("store called with id=%q, want gd-target00001", ms.lastMentionedByID)
	}
	if ms.lastMentionedByLimit != 25 || ms.lastMentionedByOffset != 10 {
		t.Errorf("limit=%d offset=%d, want 25/10", ms.lastMentionedByLimit, ms.lastMentionedByOffset)
	}

	var resp struct {
		References []*model.BeadReferenceSummary `json:"references"`
		Total      int                           `json:"total"`
		Limit      int                           `json:"limit"`
		Offset     int                           `json:"offset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if resp.Total != 2 || resp.Limit != 25 || resp.Offset != 10 {
		t.Errorf("envelope total/limit/offset = %d/%d/%d, want 2/25/10", resp.Total, resp.Limit, resp.Offset)
	}
	if len(resp.References) != 2 || resp.References[0].SourceField != "description" || resp.References[1].SourceField != "comment:7" {
		t.Errorf("references = %+v", resp.References)
	}
}

func TestHandleListMentionedBy_DefaultLimit(t *testing.T) {
	_, ms, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by", nil)
	requireStatus(t, rec, http.StatusOK)
	if ms.lastMentionedByLimit != 50 || ms.lastMentionedByOffset != 0 {
		t.Errorf("default limit/offset = %d/%d, want 50/0", ms.lastMentionedByLimit, ms.lastMentionedByOffset)
	}
}

func TestHandleListMentionedBy_LimitClamped(t *testing.T) {
	_, ms, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by?limit=9999", nil)
	requireStatus(t, rec, http.StatusOK)
	if ms.lastMentionedByLimit != 500 {
		t.Errorf("clamped limit = %d, want 500", ms.lastMentionedByLimit)
	}
}

func TestHandleListMentionedBy_BadLimit(t *testing.T) {
	_, _, h := newTestServer()
	for _, q := range []string{"limit=0", "limit=-1", "limit=abc"} {
		rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by?"+q, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: status=%d, want 400; body=%s", q, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleListMentionedBy_BadOffset(t *testing.T) {
	_, _, h := newTestServer()
	for _, q := range []string{"offset=-1", "offset=foo"} {
		rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by?"+q, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: status=%d, want 400", q, rec.Code)
		}
	}
}

func TestHandleListMentionedBy_StoreError(t *testing.T) {
	_, ms, h := newTestServer()
	ms.mentionedByErr = errors.New("boom")
	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestHandleListMentionedBy_NilRowsEncodesEmptyArray(t *testing.T) {
	_, ms, h := newTestServer()
	ms.mentionedBy = nil
	ms.mentionedByTotal = 0

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by", nil)
	requireStatus(t, rec, http.StatusOK)

	// The JSON output must contain "references":[] not "references":null —
	// downstream clients (the kd CLI in particular) range over the slice.
	body := rec.Body.String()
	if !contains(body, `"references":[]`) {
		t.Errorf("body should contain `\"references\":[]`, got: %s", body)
	}
}

func TestHandleListMentions_HappyPath(t *testing.T) {
	_, ms, h := newTestServer()
	ms.mentions = []*model.BeadReferenceSummary{
		{ID: "gd-FNHTKRBDIQ", Title: "real", Type: "task", Status: "open", SourceField: "description"},
		// dangling target — empty title/type/status modeling LEFT JOIN miss.
		{ID: "gd-ZACQZ1SGLD", Title: "", Type: "", Status: "", SourceField: "description"},
	}
	ms.mentionsTotal = 2

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-ldkpaoah1s/mentions", nil)
	requireStatus(t, rec, http.StatusOK)

	if ms.lastMentionsID != "gd-ldkpaoah1s" {
		t.Errorf("store called with id=%q", ms.lastMentionsID)
	}

	var resp struct {
		References []*model.BeadReferenceSummary `json:"references"`
		Total      int                           `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total != 2 || len(resp.References) != 2 {
		t.Errorf("total/len = %d/%d, want 2/2", resp.Total, len(resp.References))
	}
	if resp.References[1].Title != "" || resp.References[1].Type != "" {
		t.Errorf("dangling target should keep empty fields; got %+v", resp.References[1])
	}
}

func TestHandleListMentions_StoreError(t *testing.T) {
	_, ms, h := newTestServer()
	ms.mentionsErr = errors.New("db down")
	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-ldkpaoah1s/mentions", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestHandleListMentionedBy_TypeFilter_Verb checks that ?type=parent threads
// the typed-mention filter through to the store as the next placeholder
// (gd-rZchVhjXC3 / kbeads/docs/research-typed-mention-syntax.md § 10).
func TestHandleListMentionedBy_TypeFilter_Verb(t *testing.T) {
	_, ms, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by?type=parent", nil)
	requireStatus(t, rec, http.StatusOK)

	if ms.lastMentionedByRelType == nil || *ms.lastMentionedByRelType != "parent" {
		t.Fatalf("store received relType=%v, want pointer to 'parent'", ms.lastMentionedByRelType)
	}
}

// TestHandleListMentionedBy_TypeFilter_NullOnly checks that ?type=mentions
// (the user-facing alias for "untyped only") is normalized to a non-nil
// empty-string filter on the store call.
func TestHandleListMentionedBy_TypeFilter_NullOnly(t *testing.T) {
	_, ms, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by?type=mentions", nil)
	requireStatus(t, rec, http.StatusOK)

	if ms.lastMentionedByRelType == nil || *ms.lastMentionedByRelType != "" {
		t.Fatalf("store received relType=%v, want non-nil empty-string", ms.lastMentionedByRelType)
	}
}

// TestHandleListMentionedBy_NoTypeQuery_NoFilter ensures the absence of the
// query param produces a nil filter pointer (i.e. no narrowing).
func TestHandleListMentionedBy_NoTypeQuery_NoFilter(t *testing.T) {
	_, ms, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-target00001/mentioned-by", nil)
	requireStatus(t, rec, http.StatusOK)

	if ms.lastMentionedByRelType != nil {
		t.Fatalf("store received relType=%v, want nil (no filter)", *ms.lastMentionedByRelType)
	}
}

// TestHandleListMentions_TypeFilter_Verb mirrors the verb test for the
// outgoing direction (?type=parent on /mentions).
func TestHandleListMentions_TypeFilter_Verb(t *testing.T) {
	_, ms, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/beads/gd-ldkpaoah1s/mentions?type=blocks", nil)
	requireStatus(t, rec, http.StatusOK)

	if ms.lastMentionsRelType == nil || *ms.lastMentionsRelType != "blocks" {
		t.Fatalf("store received relType=%v, want pointer to 'blocks'", ms.lastMentionsRelType)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
