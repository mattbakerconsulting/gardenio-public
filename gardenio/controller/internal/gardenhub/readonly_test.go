package gardenhub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A read-only viewer (a project-scoped human, gd-GuZ3tbnTFI) is offered no write on any surface, whatever the Can* hooks
// say: a scoped human holding the operator role passes CanAdopt, CanTend, CanEdit and CanOperateGardeners, yet every
// /garden write route stays admin-only for them. The header says "Read only". The same handler for anyone else is
// unchanged.
func TestReadOnlyViewerIsOfferedNoWrite(t *testing.T) {
	yes := func(*http.Request) bool { return true }
	handler := func(ro bool) *Handler {
		now := time.Now()
		return &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Beads: beadFixture(),
			Queues: fixtureQueues(now), Plans: fixturePlans(now), Projects: fixtureProjects(now), Gardeners: fixtureGardenerPanels(now),
			CanApproveEscalation: yes, CanAdopt: yes, CanOperateGardeners: yes, CanTend: yes, CanEdit: yes, CanWake: yes,
			MaySpawn: func(*http.Request, string) bool { return true },
			ReadOnly: func(*http.Request) bool { return ro }}
	}
	page := func(ro bool) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		handler(ro).Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		return rec
	}
	rw, ro := page(false), page(true)
	flags := []string{` data-esc="1"`, ` data-adopt="1"`, ` data-tend="1"`, ` data-wake="1"`, ` data-edit="1"`}
	for _, f := range flags {
		if !strings.Contains(rw.Body.String(), f) {
			t.Errorf("the writer's page lacks %s", f)
		}
		if strings.Contains(ro.Body.String(), f) {
			t.Errorf("the read-only page carries %s", f)
		}
	}
	const marker = `<span class="chip ro" `
	if !strings.Contains(ro.Body.String(), ` data-ro="1"`) || !strings.Contains(ro.Body.String(), marker) {
		t.Error("the read-only page lacks data-ro or its header marker")
	}
	if strings.Contains(rw.Body.String(), ` data-ro="1"`) || strings.Contains(rw.Body.String(), marker) {
		t.Error("the writer's page carries data-ro or the read-only marker")
	}
	if rw.Header().Get("ETag") == ro.Header().Get("ETag") {
		t.Error("the writer's and the read-only page share an ETag")
	}

	// every panel's document: the write flags the writer gets, the read-only viewer does not
	for _, c := range []struct{ path, key string }{
		{"/garden/queue/escalations", "approve"},
		{"/garden/queue/decisions", "edit"},
		{"/garden/plan/gd-planA", "edit"},
		{"/garden/plan/gd-planA", "approve"},
		{"/garden/project/gardenio", "approve"},
		{"/garden/project/gardenio", "can_edit"},
		{"/garden/project/gardenio", "can_create_gardener"},
		{"/garden/gardener/gd-GardTrig01", "can_operate"},
		{"/garden/bead/gd-task", "spawn"},
	} {
		for _, readOnly := range []bool{false, true} {
			code, doc := getQueue(t, handler(readOnly), c.path)
			if code != http.StatusOK {
				t.Fatalf("%s: %d", c.path, code)
			}
			if got := doc[c.key] == true; got == readOnly {
				t.Errorf("%s read-only=%v: %s = %v", c.path, readOnly, c.key, doc[c.key])
			}
		}
	}
	if code, _ := getQueue(t, handler(true), "/garden/plan/gd-planA/pour"); code != http.StatusForbidden {
		t.Errorf("a read-only viewer's pour fold: %d, want 403", code)
	}
}
