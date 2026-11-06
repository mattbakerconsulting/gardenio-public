package server

import (
	"context"
	"errors"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// The short-title fill exception (gd-JOOXA7UqLh): an agent on its per-agent
// token may set short_title on a bead in its project that it does not own,
// only while short_title is empty, only that field, never on a decision.

const fillTitle = "name the bead"

// groomerCtx is the short-title groomer on its own per-agent token, scoped to
// project as the controller registers it (agenttoken.go).
func groomerCtx(project string) context.Context {
	return WithTokenProject(agentFleetCtx("gd-groomer", "short-title-groomer-a"), project)
}

// seedForeignBead adds a bead filed and held by another crew.
func seedForeignBead(ms *mockStore, id string, typ model.BeadType, project, shortTitle string) {
	ms.beads[id] = &model.Bead{
		ID: id, Kind: model.KindIssue, Type: typ, Title: "Another crew's work",
		ShortTitle: shortTitle, Status: model.StatusOpen,
		CreatedBy: "crew-b", Assignee: "crew-b",
	}
	ms.labels[id] = []string{"project:" + project}
}

func requireFillDenied(t *testing.T, err error) {
	t.Helper()
	var d *AuthzDenial
	if !errors.As(err, &d) {
		t.Fatalf("err = %v, want an *AuthzDenial (403)", err)
	}
}

func TestShortTitleFill_AgentFillsEmptyOnAnotherCrewsTask(t *testing.T) {
	s, ms, _ := newTestServer()
	seedForeignBead(ms, "gd-t1", model.TypeTask, "gardenio", "")

	st := fillTitle
	got, err := s.updateBead(groomerCtx("gardenio"), "gd-t1", updateBeadInput{ShortTitle: &st, OnlyIfShortTitleEmpty: true})
	if err != nil {
		t.Fatalf("fill on an empty short_title must pass: %v", err)
	}
	if got.ShortTitle != fillTitle || ms.beads["gd-t1"].ShortTitle != fillTitle {
		t.Fatalf("short_title = %q (stored %q), want %q", got.ShortTitle, ms.beads["gd-t1"].ShortTitle, fillTitle)
	}
	// Nothing but short_title moved.
	if b := ms.beads["gd-t1"]; b.Title != "Another crew's work" || b.Assignee != "crew-b" || b.Status != model.StatusOpen {
		t.Fatalf("fill changed other columns: %+v", b)
	}
	// Audited like any update.
	found := false
	for _, m := range ms.mutations {
		if m.BeadID == "gd-t1" && m.Field == "short_title" && m.NewValue == fillTitle {
			found = true
		}
	}
	if !found {
		t.Fatalf("no short_title mutation recorded; mutations = %+v", ms.mutations)
	}
}

func TestShortTitleFill_Refused(t *testing.T) {
	newTitle := "Renamed"
	for _, tc := range []struct {
		name    string
		typ     model.BeadType
		project string // the bead's project
		stored  string // the bead's current short_title
		ctx     context.Context
		in      func(st *string) updateBeadInput
	}{
		{"short_title already set", model.TypeTask, "gardenio", "authored name", groomerCtx("gardenio"),
			func(st *string) updateBeadInput { return updateBeadInput{ShortTitle: st} }},
		{"already set, only_if_short_title_empty", model.TypeTask, "gardenio", "authored name", groomerCtx("gardenio"),
			func(st *string) updateBeadInput { return updateBeadInput{ShortTitle: st, OnlyIfShortTitleEmpty: true} }},
		{"short_title and title in one request", model.TypeTask, "gardenio", "", groomerCtx("gardenio"),
			func(st *string) updateBeadInput { return updateBeadInput{ShortTitle: st, Title: &newTitle} }},
		{"short_title and a field", model.TypeTask, "gardenio", "", groomerCtx("gardenio"),
			func(st *string) updateBeadInput {
				return updateBeadInput{ShortTitle: st, Fields: []byte(`{"gate_passed":false}`)}
			}},
		{"short_title and a close", model.TypeTask, "gardenio", "", groomerCtx("gardenio"),
			func(st *string) updateBeadInput { c := "closed"; return updateBeadInput{ShortTitle: st, Status: &c} }},
		{"short_title and heraldry", model.TypeTask, "gardenio", "", groomerCtx("gardenio"),
			func(st *string) updateBeadInput { a := armsRun; return updateBeadInput{ShortTitle: st, Heraldry: &a} }},
		{"cross-project", model.TypeTask, "other", "", groomerCtx("gardenio"),
			func(st *string) updateBeadInput { return updateBeadInput{ShortTitle: st} }},
		{"agent token with no project scope", model.TypeTask, "gardenio", "", agentFleetCtx("gd-groomer", "short-title-groomer-a"),
			func(st *string) updateBeadInput { return updateBeadInput{ShortTitle: st} }},
		{"decision bead", model.TypeDecision, "gardenio", "", groomerCtx("gardenio"),
			func(st *string) updateBeadInput { return updateBeadInput{ShortTitle: st} }},
		{"ordinary fleet token, not an agent", model.TypeTask, "gardenio", "",
			WithTokenProject(WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "jira-bridge"}), "jira-bridge"), "gardenio"),
			func(st *string) updateBeadInput { return updateBeadInput{ShortTitle: st} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedForeignBead(ms, "gd-t1", tc.typ, tc.project, tc.stored)
			st := fillTitle
			_, err := s.updateBead(tc.ctx, "gd-t1", tc.in(&st))
			requireFillDenied(t, err)
			b := ms.beads["gd-t1"]
			if b.ShortTitle != tc.stored || b.Title != "Another crew's work" || b.Status != model.StatusOpen || b.Heraldry != "" {
				t.Fatalf("refused update still wrote: %+v", b)
			}
		})
	}
}

// The owner keeps the plain path: it may replace its own authored value.
func TestShortTitleFill_OwnerUnaffected(t *testing.T) {
	s, ms, _ := newTestServer()
	seedForeignBead(ms, "gd-t1", model.TypeTask, "gardenio", "authored name")
	st := fillTitle
	if _, err := s.updateBead(WithTokenProject(agentFleetCtx("gd-crewb", "crew-b"), "gardenio"), "gd-t1", updateBeadInput{ShortTitle: &st}); err != nil {
		t.Fatalf("owner rename must pass: %v", err)
	}
	if got := ms.beads["gd-t1"].ShortTitle; got != fillTitle {
		t.Fatalf("short_title = %q, want the owner's %q", got, fillTitle)
	}
}

// The shared static token's ownership bypass is gone (gd-ke6PC7bRto): an
// identity still carrying Type "static" is an ordinary non-owner and may not
// replace a set short_title.
func TestShortTitleFill_StaticIdentityNoBypass(t *testing.T) {
	s, ms, _ := newTestServer()
	seedForeignBead(ms, "gd-t1", model.TypeTask, "gardenio", "authored name")
	st := fillTitle
	ctx := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "static"}), authMethodStatic)
	if _, err := s.updateBead(ctx, "gd-t1", updateBeadInput{ShortTitle: &st}); err == nil {
		t.Fatal("a static identity replaced a set short_title; the ownership bypass must be gone")
	}
	if got := ms.beads["gd-t1"].ShortTitle; got != "authored name" {
		t.Fatalf("short_title = %q, want the stored %q", got, "authored name")
	}
}

// staleShortTitleStore serves a read from before an owner set short_title:
// GetBead shows it empty, the row (read by FillShortTitle) has it set.
type staleShortTitleStore struct {
	*mockStore
}

func (r *staleShortTitleStore) GetBead(ctx context.Context, id string) (*model.Bead, error) {
	b, err := r.mockStore.GetBead(ctx, id)
	if b != nil && b.ID == "gd-t1" {
		b.ShortTitle = ""
	}
	return b, err
}

func (r *staleShortTitleStore) RunInTransaction(_ context.Context, fn func(tx store.Store) error) error {
	return fn(r)
}

// A short_title set between updateBead's read and its write is never
// overwritten: the write re-checks it and the fill is refused.
func TestShortTitleFill_SetBetweenReadAndWriteIsKept(t *testing.T) {
	ms := newMockStore()
	seedForeignBead(ms, "gd-t1", model.TypeTask, "gardenio", "authored name")
	s := NewBeadsServer(&staleShortTitleStore{mockStore: ms}, &events.NoopPublisher{})
	s.userProvisioner = nil

	st := fillTitle
	_, err := s.updateBead(groomerCtx("gardenio"), "gd-t1", updateBeadInput{ShortTitle: &st})
	requireFillDenied(t, err)
	if got := ms.beads["gd-t1"].ShortTitle; got != "authored name" {
		t.Fatalf("short_title = %q, want the authored value kept", got)
	}
}
