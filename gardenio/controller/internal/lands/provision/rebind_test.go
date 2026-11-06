package provision

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// stewardedLand provisions acme and returns its provisioner and steward trigger.
func stewardedLand(t *testing.T, w *world) (*Provisioner, *beadsapi.BeadDetail) {
	t.Helper()
	w.application(StateApproved)
	p := w.provisioner()
	pass(t, p)
	trigs := w.byType("trigger")
	if len(trigs) != 1 {
		t.Fatalf("%d steward triggers after provisioning, want 1", len(trigs))
	}
	return p, trigs[0]
}

// closeSteward closes the steward as the engine (slot, kind) or a person
// (no slot) does, at t.
func closeSteward(b *beadsapi.BeadDetail, at time.Time, slot, kind string) {
	b.Status, b.ClosedAt = "closed", at.Format(time.RFC3339)
	if slot != "" {
		b.Fields["gardener_terminated_slot"], b.Fields["gardener_terminated_kind"] = slot, kind
	}
}

// A land whose steward was closed by hand is bound a fresh one on the next
// pass, with the approver as operator and a comment on the land saying why.
// It is not woken, and the pass after writes nothing.
func TestRebind_ClosedStewardIsBoundAgain(t *testing.T) {
	w := newWorld(t)
	p, old := stewardedLand(t, w)
	closeSteward(old, t0, "", "")

	before := len(w.ops)
	pass(t, p)
	if got := w.ops[before:]; !slices.Equal(got, []string{"steward:bind", "comment"}) {
		t.Fatalf("ops %v, want a bind and a comment", got)
	}
	trigs := w.byType("trigger")
	if len(trigs) != 2 || trigs[1].Status != "open" || trigs[1].Fields["operator"] != "oidc:operator@example.com" {
		t.Fatalf("steward triggers = %+v, want a fresh open one with the approver as operator", trigs)
	}
	proj := w.one("project", "acme")
	want := proj.ID + ": Bound steward " + trigs[1].ID + " to land acme: its steward " + old.ID +
		" was closed at " + t0.Format(time.RFC3339) + " with no gardener stop recorded."
	if got := w.comment[len(w.comment)-1]; got != want {
		t.Fatalf("land comment\n got %q\nwant %q", got, want)
	}
	if len(w.byType("message")) != 1 {
		t.Fatal("the fresh steward was woken; it waits for the land's next event")
	}

	before = len(w.ops)
	pass(t, p)
	if len(w.ops) != before {
		t.Fatalf("a pass with an open steward wrote %v", w.ops[before:])
	}
}

// A steward that closed on a hard_stop crash-looped: for the back-off the
// provisioner files one escalation instead of binding, and binds a fresh one
// on the pass after the operator closes it.
func TestRebind_CrashLoopedStewardWaitsForTheOperator(t *testing.T) {
	w := newWorld(t)
	p, old := stewardedLand(t, w)
	closeSteward(old, t0, "hard_stop", "consecutive_failures")
	p.Now = func() time.Time { return t0.Add(time.Hour) }

	pass(t, p)
	pass(t, p)
	if n := len(w.byType("trigger")); n != 1 {
		t.Fatalf("%d steward triggers: a crash-looped steward was bound again within the back-off", n)
	}
	escs := w.byType("escalation")
	if len(escs) != 1 {
		t.Fatalf("%d escalations after two passes, want 1", len(escs))
	}
	esc := escs[0]
	if w.idem["lands-steward:"+old.ID] != esc.ID || !slices.Equal(esc.Labels, []string{IntakeLabel}) ||
		esc.Fields["operation_type"] != "lands_steward" ||
		!strings.Contains(esc.Fields["context"], "closed at "+t0.Format(time.RFC3339)+" on its hard_stop consecutive_failures") ||
		!strings.Contains(esc.Fields["context"], "bound at "+t0.Add(StewardRebindBackoff).Format(time.RFC3339)+" anyway") {
		t.Fatalf("escalation %+v", esc)
	}

	esc.Status = "closed"
	pass(t, p)
	trigs := w.byType("trigger")
	if len(trigs) != 2 || trigs[1].Status != "open" {
		t.Fatalf("steward triggers = %+v, want a fresh one once the operator closed the escalation", trigs)
	}
	if got := w.comment[len(w.comment)-1]; !strings.HasSuffix(got, ", and the operator closed escalation "+esc.ID+".") {
		t.Fatalf("land comment %q", got)
	}
}

// Past the back-off a crash-looped steward is bound again without waiting
// for the operator.
func TestRebind_CrashLoopedStewardIsBoundAfterTheBackoff(t *testing.T) {
	w := newWorld(t)
	p, old := stewardedLand(t, w)
	closeSteward(old, t0, "hard_stop", "consecutive_failures")
	p.Now = func() time.Time { return t0.Add(StewardRebindBackoff) }

	pass(t, p)
	if n := len(w.byType("trigger")); n != 2 {
		t.Fatalf("%d steward triggers, want a fresh one after the back-off", n)
	}
	if n := len(w.byType("escalation")); n != 0 {
		t.Fatalf("%d escalations, want none after the back-off", n)
	}
}

// A frozen land and a land being closed are never bound a steward.
func TestRebind_FrozenOrClosingLandIsLeftAlone(t *testing.T) {
	for name, set := range map[string]func(w *world){
		"frozen": func(w *world) { w.one("project", "acme").Fields["land_frozen"] = "true" },
		"closing": func(w *world) {
			w.byType(ApplicationType)[0].Fields["close_requested_at"] = t0.Format(time.RFC3339)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			p, old := stewardedLand(t, w)
			closeSteward(old, t0, "", "")
			set(w)
			pass(t, p)
			if n := len(w.byType("trigger")); n != 1 {
				t.Fatalf("%d steward triggers, want the closed one only", n)
			}
		})
	}
}

// A steward that cannot be read is retried on the next pass; the
// application is never touched.
func TestRebind_ReadFailureIsRetried(t *testing.T) {
	w := newWorld(t)
	p, old := stewardedLand(t, w)
	closeSteward(old, t0, "", "")
	w.fail["steward:find"] = errors.New("daemon down")
	before := len(w.ops)
	pass(t, p)
	if len(w.ops) != before {
		t.Fatalf("a failed read wrote %v", w.ops[before:])
	}
	if app := appOf(t, w, w.byType(ApplicationType)[0].ID); app.State != StateProvisioned || app.Error != "" {
		t.Fatalf("application = %+v", app)
	}
	delete(w.fail, "steward:find")
	pass(t, p)
	if n := len(w.byType("trigger")); n != 2 {
		t.Fatalf("%d steward triggers, want a fresh one on the retry", n)
	}
}

// Find lives here rather than with the other fakeStewards methods so
// provision_test.go stays under the quench file-length cap.
func (s fakeStewards) Find(_ context.Context, land string) (open, last *beadsapi.BeadDetail, err error) {
	if err := s.w.read("steward:find"); err != nil {
		return nil, nil, err
	}
	for _, b := range s.w.byType("trigger") {
		if !slices.Contains(b.Labels, "gardener-land:"+land) {
			continue
		}
		if b.Status != "closed" {
			return b, nil, nil
		}
		last = b
	}
	return nil, last, nil
}
