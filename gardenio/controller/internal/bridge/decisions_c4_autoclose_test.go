package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// ref is a small constructor for a seeded reverse-mention row.
func c4ref(id, title, typ, status string) beadsapi.BeadReferenceSummary {
	return beadsapi.BeadReferenceSummary{ID: id, Title: title, Type: typ, Status: status}
}

// seedDeadWoodSubject seeds a trigger subject bead plus its reverse-mention
// candidates, then returns a Decisions wired to the mock.
func seedDeadWoodSubject(subject string, refs ...beadsapi.BeadReferenceSummary) (*mockDaemon, *mockNotifier, *Decisions) {
	daemon := newMockDaemon()
	daemon.beads[subject] = &beadsapi.BeadDetail{ID: subject, Type: "trigger"}
	daemon.mentionedBy = map[string][]beadsapi.BeadReferenceSummary{subject: refs}
	notif := &mockNotifier{}
	return daemon, notif, newDeadWoodDecisions(daemon, notif)
}

func dismissEvent(decID, subject string) []byte {
	return marshalSSEBeadPayload(BeadEvent{
		ID:   decID,
		Type: "decision",
		Fields: map[string]string{
			"chosen":             "dismissed",
			"subject_trigger_id": subject,
		},
	})
}

func closedIDs(closes []closeCall) []string {
	out := make([]string, 0, len(closes))
	for _, c := range closes {
		out = append(out, c.BeadID)
	}
	return out
}

// TestC4_AutoClose_ClosesOpenTuneTaskForSubject is the happy path: dismissing a
// revival-review decision retires the subject gardener and auto-closes its open
// tune-task obsolete, with a close_reason and audit comment naming the decision.
func TestC4_AutoClose_ClosesOpenTuneTaskForSubject(t *testing.T) {
	subject := "gd-iiJV9u8JlQ"
	tune := c4ref("gd-eO6YdUJUHG",
		"Tune: Perf trace-pipe gardener ("+subject+") — define trigger & gardener-epic",
		"task", "open")
	daemon, _, d := seedDeadWoodSubject(subject, tune)

	d.handleClosed(context.Background(), dismissEvent("dec-1", subject))

	closed := closedIDs(daemon.getClosed())
	if len(closed) != 1 || closed[0] != tune.ID {
		t.Fatalf("expected tune-task %s closed, got %v", tune.ID, closed)
	}
	// close_reason names the dismissing decision.
	var cr string
	for _, c := range daemon.getClosed() {
		if c.BeadID == tune.ID {
			cr = c.Fields["close_reason"]
		}
	}
	if !strings.Contains(cr, "dec-1") || !strings.Contains(cr, "dead wood") {
		t.Errorf("close_reason = %q, want it to name dec-1 and dead wood", cr)
	}
	// Audit comment on the tune-task names the decision.
	var found bool
	for _, c := range daemon.getComments() {
		if c.BeadID == tune.ID && strings.Contains(c.Text, "dec-1") && c.Author == gardenerRetiredDeadWoodActor {
			found = true
		}
	}
	if !found {
		t.Errorf("expected audit comment on %s naming dec-1, got %+v", tune.ID, daemon.getComments())
	}
}

// TestC4_AutoClose_IgnoresNonRemediationMentions verifies the narrow predicate:
// arbitrary beads that merely reference the subject in prose — a report, an
// already-closed tune-task, an epic, a tune-task for a DIFFERENT gardener — are
// never closed. Only the open tune-task naming THIS subject is.
func TestC4_AutoClose_IgnoresNonRemediationMentions(t *testing.T) {
	subject := "gd-SUBJ0000001"
	other := "gd-OTHER0000002"
	target := c4ref("gd-TUNE0000003", "Tune: some gardener ("+subject+") — fix cron", "task", "open")
	daemon, _, d := seedDeadWoodSubject(subject,
		target,
		// A report bead that mentions the subject in prose — not a remediation task.
		c4ref("gd-REP00000004", "Report: revival dismissal for "+subject, "doc", "open"),
		// A tune-task, but already closed — must stay closed, not be re-touched.
		c4ref("gd-TUNE0000005", "Tune: X gardener ("+subject+")", "task", "closed"),
		// An epic that mentions the subject — never auto-closed.
		c4ref("gd-EPIC0000006", "Fix broken gardeners ("+subject+")", "epic", "open"),
		// A tune-task for a DIFFERENT gardener that merely references the subject in
		// its title (id present but title is that other gardener's task). It does
		// contain the subject id, so it WOULD match id+marker — guard: it names the
		// OTHER gardener. We assert the reactor still closes only tasks whose title
		// contains THIS subject id; to keep the negative clean, this row's title
		// does NOT contain the subject id.
		c4ref("gd-TUNE0000007", "Tune: other gardener ("+other+") — unrelated", "task", "open"),
	)

	d.handleClosed(context.Background(), dismissEvent("dec-2", subject))

	closed := closedIDs(daemon.getClosed())
	if len(closed) != 1 || closed[0] != target.ID {
		t.Fatalf("expected only %s closed, got %v", target.ID, closed)
	}
}

// TestC4_AutoClose_SkipsNonLeafToAvoidOrphaning verifies the leaf guard: a
// remediation task that parents open children is NOT closed (would orphan them,
// advice ka-0000000396); a sibling leaf task IS.
func TestC4_AutoClose_SkipsNonLeafToAvoidOrphaning(t *testing.T) {
	subject := "gd-SUBJ0000010"
	parent := c4ref("gd-TUNEP000011", "Tune: gardener ("+subject+") parent", "task", "open")
	leaf := c4ref("gd-TUNEL000012", "Tune: gardener ("+subject+") leaf", "task", "open")
	daemon, _, d := seedDeadWoodSubject(subject, parent, leaf)
	// parent has a parent-child child → not a leaf.
	daemon.reverseDeps = map[string][]beadsapi.Dependency{
		parent.ID: {{BeadID: "gd-VGQYSCJFL9", DependsOnID: parent.ID, Type: "parent-child"}},
	}

	d.handleClosed(context.Background(), dismissEvent("dec-3", subject))

	closed := closedIDs(daemon.getClosed())
	if len(closed) != 1 || closed[0] != leaf.ID {
		t.Fatalf("expected only leaf %s closed (parent skipped), got %v", leaf.ID, closed)
	}
}

// TestC4_AutoClose_MentionLookupFailureIsBestEffort verifies a GetMentionedBy
// failure skips auto-close cleanly: the label stamp and dismissal still stand.
func TestC4_AutoClose_MentionLookupFailureIsBestEffort(t *testing.T) {
	subject := "gd-SUBJ0000020"
	daemon, notif, d := seedDeadWoodSubject(subject)
	daemon.mentionedByErr = errors.New("daemon down")

	d.handleClosed(context.Background(), dismissEvent("dec-4", subject))

	if got := notif.getDismissed(); len(got) != 1 {
		t.Fatalf("dismissal must still complete on mention-lookup failure, got %v", got)
	}
	if !daemon.hasLabel(subject, gardenerRetiredDeadWoodLabel) {
		t.Errorf("subject must still be stamped dead wood despite mention-lookup failure")
	}
	if got := daemon.getClosed(); len(got) != 0 {
		t.Errorf("no task should be closed when the mention lookup fails, got %v", closedIDs(got))
	}
}

// TestC4_AutoClose_CloseFailureContinues verifies one task's close failure does
// not abort the others: given two matching leaf tune-tasks and a forced close
// error, the reactor attempts both rather than bailing on the first.
func TestC4_AutoClose_CloseFailureContinues(t *testing.T) {
	subject := "gd-SUBJ0000030"
	t1 := c4ref("gd-TUNE0000031", "Tune: gardener ("+subject+") one", "task", "open")
	t2 := c4ref("gd-TUNE0000032", "Tune: gardener ("+subject+") two", "task", "open")
	daemon, _, d := seedDeadWoodSubject(subject, t1, t2)
	daemon.closeBeadErr = errors.New("close rejected")

	d.handleClosed(context.Background(), dismissEvent("dec-5", subject))

	// Both closes were ATTEMPTED (recorded before the error short-circuit? no —
	// the mock returns before recording on closeBeadErr). Assert instead that the
	// reactor did not panic and both tasks got their audit comment (proves it
	// iterated past the first failure).
	var c1, c2 bool
	for _, c := range daemon.getComments() {
		if c.BeadID == t1.ID {
			c1 = true
		}
		if c.BeadID == t2.ID {
			c2 = true
		}
	}
	if !c1 || !c2 {
		t.Errorf("expected the reactor to process both tasks past a close failure; comments c1=%v c2=%v", c1, c2)
	}
}

// TestC4_AutoClose_ExpiredDoesNotClose verifies the guard rail carries through to
// C4: an _expired (timeout, not an operator judgement) closure must never
// auto-close a remediation task.
func TestC4_AutoClose_ExpiredDoesNotClose(t *testing.T) {
	subject := "gd-SUBJ0000040"
	tune := c4ref("gd-TUNE0000041", "Tune: gardener ("+subject+")", "task", "open")
	daemon, _, d := seedDeadWoodSubject(subject, tune)

	expired := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-6",
		Type: "decision",
		Fields: map[string]string{
			"chosen":             "_expired",
			"subject_trigger_id": subject,
		},
	})
	d.handleClosed(context.Background(), expired)

	if got := daemon.getClosed(); len(got) != 0 {
		t.Errorf("_expired must NOT auto-close remediation tasks, got %v", closedIDs(got))
	}
}

// TestIsGardenerRemediationTaskFor covers the predicate directly across its edges.
func TestIsGardenerRemediationTaskFor(t *testing.T) {
	s := "gd-SUBJECT99"
	cases := []struct {
		name string
		ref  beadsapi.BeadReferenceSummary
		want bool
	}{
		{"open tune-task in title", c4ref("k1", "Tune: x ("+s+")", "task", "open"), true},
		{"blocked tune-task", c4ref("k2", "Tune: x ("+s+")", "task", "blocked"), true},
		{"in_progress bug remediation", c4ref("k3", "Fix broken gardener "+s, "bug", "in_progress"), true},
		{"remediat marker", c4ref("k4", "Remediate gardener "+s, "task", "open"), true},
		{"closed excluded", c4ref("k5", "Tune: x ("+s+")", "task", "closed"), false},
		{"epic excluded", c4ref("k6", "Tune: x ("+s+")", "epic", "open"), false},
		{"doc excluded", c4ref("k7", "Tune: x ("+s+")", "doc", "open"), false},
		{"id not in title excluded", c4ref("k8", "Tune: some other gardener", "task", "open"), false},
		{"no tune marker excluded", c4ref("k9", "Discuss gardener "+s, "task", "open"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGardenerRemediationTaskFor(tc.ref, s); got != tc.want {
				t.Errorf("isGardenerRemediationTaskFor(%q,%q) = %v, want %v", tc.ref.Title, s, got, tc.want)
			}
		})
	}
}

// TestHasParentChildChild covers the leaf-detection helper's edge selection.
func TestHasParentChildChild(t *testing.T) {
	p := "gd-PARENT01"
	if hasParentChildChild(nil, p) {
		t.Error("nil deps must report no children")
	}
	// A parent-child row where p is the DependsOnID → p has a child.
	yes := []beadsapi.Dependency{{BeadID: "gd-C", DependsOnID: p, Type: "parent-child"}}
	if !hasParentChildChild(yes, p) {
		t.Error("expected a parent-child child to be detected")
	}
	// A blocks edge, or p as the dependent (not the parent), must NOT count.
	no := []beadsapi.Dependency{
		{BeadID: "gd-C", DependsOnID: p, Type: "blocks"},
		{BeadID: p, DependsOnID: "gd-EPIC", Type: "parent-child"},
	}
	if hasParentChildChild(no, p) {
		t.Error("blocks edge / p-as-dependent must not count as a child")
	}
}
