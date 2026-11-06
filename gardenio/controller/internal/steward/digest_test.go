package steward

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// digestDay is the digest's as-of time in these tests; lastDigest is the one
// before it.
var (
	digestDay  = time.Date(2026, 10, 23, 8, 13, 0, 0, time.UTC)
	lastDigest = digestDay.Add(-24 * time.Hour)
)

// digestFarm is the land with its notes bead, the last digest recorded on it
// when mark is set.
func digestFarm(mark bool) *notesFarm {
	f := &notesFarm{fakeDaemon: newFarm()}
	fields := map[string]string{"content": EmptyNotes(land)}
	if mark {
		fields[DigestAtField] = lastDigest.Format(time.RFC3339)
		fields[DigestUsedField] = "12.40"
	}
	f.beads = append(f.beads, &beadsapi.BeadDetail{ID: "tp-notes", Type: "doc", Title: NotesTitle(land), Status: "open",
		Labels: []string{"project:" + land}, Fields: fields})
	return f
}

// digestPurse is a purse with 15 credits used of 40.
func digestPurse() purse.Purse {
	spent, flight, left := 14.0, 1.0, 25.0
	return purse.Purse{Land: land, State: purse.StateOK, GrantedCredits: 40, SpentCredits: &spent, InFlightCredits: &flight, RemainingCredits: &left, MarginCredits: 2}
}

func landBead(b *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	b.Labels = append(b.Labels, "project:"+land)
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	return b
}

// A day when only old things are in the land and nobody did anything since
// the last digest is quiet: post nothing, and no post command is offered.
func TestDigest_QuietDayPostsNothing(t *testing.T) {
	f := digestFarm(true)
	before := lastDigest.Add(-time.Hour)
	f.beads = append(f.beads,
		landBead(&beadsapi.BeadDetail{ID: "tp-old", Type: "task", Title: "Water the beds", Status: "closed", ClosedAt: before.Format(time.RFC3339), CreatedAt: before}),
		landBead(&beadsapi.BeadDetail{ID: "tp-wait", Type: "decision", Title: "Plan tp-plan: run it?", Status: "open", CreatedAt: before,
			Fields: map[string]string{"operated_by": "oidc:ada@example.com"}}),
	)
	d, err := GatherDigest(context.Background(), f, land, digestPurse(), digestDay)
	if err != nil {
		t.Fatalf("GatherDigest: %v", err)
	}
	if !d.Quiet || !d.SinceLastDigest || !d.Since.Equal(lastDigest) {
		t.Fatalf("digest %+v: want quiet since the last digest", d)
	}
	// A decision still waiting from before is not news.
	if len(d.Errands) != 1 || d.Errands[0].New {
		t.Fatalf("errands %+v", d.Errands)
	}
	got := FormatDigest(d)
	if !strings.HasPrefix(got, "Quiet: nothing was done, planned, answered or run in tomato-patch since the last digest, 2026-10-22 08:13Z.\nPost nothing.") ||
		strings.Contains(got, "--post") {
		t.Fatalf("quiet digest:\n%s", got)
	}
}

// A busy day: what happened since the last digest, where the land stands,
// the notes' In flight, the credits spent since, and the post command with
// the as-of and used figures the read saw.
func TestDigest_BusyDay(t *testing.T) {
	f := digestFarm(true)
	for _, b := range f.beads {
		if b.ID == "tp-notes" {
			b.Fields["content"] = strings.Replace(EmptyNotes(land), "## In flight\n", "## In flight\n- tp-plan watering schedule: step 2 of 3, gardener on it\n- PR 14 open, waiting on ada's review\n", 1)
		}
	}
	at := lastDigest.Add(3 * time.Hour)
	before := lastDigest.Add(-time.Hour)
	f.beads = append(f.beads,
		landBead(&beadsapi.BeadDetail{ID: "tp-s1", Type: "task", Title: "Water the beds", Status: "closed", ClosedAt: at.Format(time.RFC3339), CreatedAt: before}),
		landBead(&beadsapi.BeadDetail{ID: "tp-old", Type: "task", Title: "Dig the beds", Status: "closed", ClosedAt: before.Format(time.RFC3339), CreatedAt: before}),
		landBead(&beadsapi.BeadDetail{ID: "tp-s2", Type: "task", Title: "Mulch the paths", Status: "in_progress", Assignee: "tomato-patch-g2", CreatedAt: before}),
		landBead(&beadsapi.BeadDetail{ID: "tp-p2", Type: "epic", Title: "Add a compost log", Status: "open", Labels: []string{"plan"}, CreatedAt: at,
			Fields: map[string]string{"plan_state": "ready"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-d1", Type: "decision", Title: "Plan tp-plan: Add a watering schedule - run it?", Status: "closed", ClosedAt: at.Format(time.RFC3339), CreatedAt: before,
			Fields: map[string]string{"chosen": "approve", "responded_by": "oidc:ada@example.com"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-d2", Type: "decision", Title: "Plan tp-p2: Add a compost log - run it?", Status: "open", CreatedAt: at,
			Fields: map[string]string{"operated_by": "oidc:ada@example.com"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-a1", Type: "agent", Title: "tomato-patch-g2", Status: "open", Labels: []string{"role:" + GardenerRole}, CreatedAt: at}),
		// Another land's step is not this land's news.
		&beadsapi.BeadDetail{ID: "bf-s1", Type: "task", Title: "Plant beans", Status: "closed", ClosedAt: at.Format(time.RFC3339), Labels: []string{"project:bean-field"}},
	)
	d, err := GatherDigest(context.Background(), f, land, digestPurse(), digestDay)
	if err != nil {
		t.Fatalf("GatherDigest: %v", err)
	}
	if d.Quiet {
		t.Fatal("a busy day read as quiet")
	}
	if d.SpentSince == nil || formatCredits(*d.SpentSince) != "2.60" {
		t.Fatalf("spent since = %v, want 2.60", d.SpentSince)
	}
	want := `Digest for tomato-patch, since the last digest, 2026-10-22 08:13Z, as of 2026-10-23 08:13Z.

Done (steps closed: their pull requests merged):
- tp-s1: Water the beds

New plans:
- tp-p2: Add a compost log [ready]

Answered:
- tp-d1: Plan tp-plan: Add a watering schedule - run it? (approve, by ada@example.com)

1 gardener session started.

In progress now:
- tp-s2: Mulch the paths (gardener tomato-patch-g2)

What you must do, by place:
Decisions to answer:
- tp-d2: Plan tp-p2: Add a compost log - run it? (for ada@example.com; asked since the last digest)
  done when: an option is chosen on the card

In flight, from your notes:
- tp-plan watering schedule: step 2 of 3, gardener on it
- PR 14 open, waiting on ada's review

Credits: 2.60 spent since the last digest; 25.00 of 40.00 remaining.

Write it for a person and post it in #general with:
  gb lands digest tomato-patch --post digest.md --as-of 2026-10-23T08:13:00Z --used 15.00
`
	if got := FormatDigest(d); got != want {
		t.Fatalf("digest:\n%s\nwant:\n%s", got, want)
	}
}

// Each kind of news alone makes a day not quiet.
func TestDigest_AnyNewsIsNotQuiet(t *testing.T) {
	at := lastDigest.Add(time.Hour)
	for name, b := range map[string]*beadsapi.BeadDetail{
		"a step closed":     {ID: "x", Type: "task", Status: "closed", ClosedAt: at.Format(time.RFC3339)},
		"a plan written":    {ID: "x", Type: "epic", Status: "open", Labels: []string{"plan"}, CreatedAt: at},
		"a plan dropped":    {ID: "x", Type: "epic", Status: "closed", Labels: []string{"plan"}, ClosedAt: at.Format(time.RFC3339), Fields: map[string]string{"plan_state": "abandoned"}},
		"a decision closed": {ID: "x", Type: "decision", Status: "closed", ClosedAt: at.Format(time.RFC3339)},
		"a decision asked":  {ID: "x", Type: "decision", Status: "open", CreatedAt: at},
		"a gardener ran":    {ID: "x", Type: "agent", Status: "closed", Labels: []string{"role:" + GardenerRole}, CreatedAt: at},
	} {
		f := digestFarm(true)
		f.beads = append(f.beads, landBead(b))
		d, err := GatherDigest(context.Background(), f, land, digestPurse(), digestDay)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d.Quiet {
			t.Errorf("%s: read as quiet", name)
		}
	}
}

// Before the first digest the window is a day back and the spend since is
// not known: the digest says what has been used so far.
func TestDigest_FirstDigest(t *testing.T) {
	f := digestFarm(false)
	f.beads = append(f.beads, landBead(&beadsapi.BeadDetail{ID: "tp-s1", Type: "task", Title: "Water the beds", Status: "closed",
		ClosedAt: digestDay.Add(-2 * time.Hour).Format(time.RFC3339)}))
	d, err := GatherDigest(context.Background(), f, land, digestPurse(), digestDay)
	if err != nil {
		t.Fatalf("GatherDigest: %v", err)
	}
	if d.SinceLastDigest || !d.Since.Equal(digestDay.Add(-24*time.Hour)) || d.SpentSince != nil || d.Quiet {
		t.Fatalf("first digest %+v", d)
	}
	got := FormatDigest(d)
	for _, want := range []string{"since a day back (no digest yet)", "Credits: 15.00 used so far; 25.00 of 40.00 remaining."} {
		if !strings.Contains(got, want) {
			t.Errorf("first digest missing %q:\n%s", want, got)
		}
	}
	// A purse that cannot say what is used leaves --used off the post.
	unknown := purse.Purse{Land: land, State: purse.StateUnknown, GrantedCredits: 40}
	if d, err = GatherDigest(context.Background(), f, land, unknown, digestDay); err != nil {
		t.Fatal(err)
	}
	if got := FormatDigest(d); !strings.Contains(got, "Credits: 40.00 granted, remaining not known.") || strings.Contains(got, "--used") {
		t.Errorf("unknown purse:\n%s", got)
	}
}

// Posting records where the next digest starts; the next read starts there,
// and a retried post of the same digest is refused.
func TestDigest_RecordMovesTheWindowAndRefusesARepost(t *testing.T) {
	f := digestFarm(true)
	ctx := context.Background()
	notes, err := CheckDigestMark(ctx, f, land, digestDay)
	if err != nil {
		t.Fatalf("CheckDigestMark: %v", err)
	}
	used := 15.0
	if err := RecordDigest(ctx, f, notes.ID, digestDay, &used); err != nil {
		t.Fatalf("RecordDigest: %v", err)
	}
	if b := f.bead("tp-notes"); b.Fields[DigestAtField] != "2026-10-23T08:13:00Z" || b.Fields[DigestUsedField] != "15.00" {
		t.Fatalf("notes fields %v", b.Fields)
	}
	if _, err := CheckDigestMark(ctx, f, land, digestDay); !errors.Is(err, ErrDigestPosted) {
		t.Fatalf("repost: err %v, want ErrDigestPosted", err)
	}
	if _, err := CheckDigestMark(ctx, f, land, digestDay.Add(-time.Minute)); !errors.Is(err, ErrDigestPosted) {
		t.Fatalf("older post: err %v, want ErrDigestPosted", err)
	}
	next := digestDay.Add(24 * time.Hour)
	d, err := GatherDigest(ctx, f, land, digestPurse(), next)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Since.Equal(digestDay) || d.SpentSince == nil || *d.SpentSince != 0 || !d.Quiet {
		t.Fatalf("next digest %+v", d)
	}
	// An unknown used figure is recorded as unknown, so the next digest does
	// not difference against a stale one.
	if err := RecordDigest(ctx, f, notes.ID, next, nil); err != nil {
		t.Fatal(err)
	}
	if d, err = GatherDigest(ctx, f, land, digestPurse(), next.Add(time.Hour)); err != nil || d.SpentSince != nil {
		t.Fatalf("after an unknown used: %+v (%v)", d, err)
	}
	// A land without notes has nowhere to record the digest.
	bare := &notesFarm{fakeDaemon: newFarm()}
	if _, err := CheckDigestMark(ctx, bare, land, digestDay); err == nil || !strings.Contains(err.Error(), "no steward-notes:tomato-patch") {
		t.Fatalf("no notes: %v", err)
	}
}

// The owner errands (P9.6): every open owner-only item, an owner-at-device
// step, an owner-audience escalation and a decision, is listed once under the
// place the owner has to be, with its clicks and what closes it. An
// escalation in hand for an operator or the desk is not an errand, an
// escalation filed for a listed step folds into it, the same ask filed twice
// is listed once, and an owner-at-device step is not "in progress".
func TestDigest_OwnerErrandsByPlace(t *testing.T) {
	f := digestFarm(true)
	before := lastDigest.Add(-time.Hour)
	at := lastDigest.Add(time.Hour)
	f.beads = append(f.beads,
		landBead(&beadsapi.BeadDetail{ID: "tp-dev", Type: "task", Title: "Walk the Commons on a phone", Status: "in_progress", Assignee: "ada",
			Labels: []string{"owner-at-device"}, CreatedAt: before, Fields: map[string]string{
				ErrandPlaceField: "iphone", ErrandClicksField: "open /garden,\n tap Commons", ErrandEvidenceField: "two screenshots on the step"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-e-dev", Type: "escalation", Title: "Do the phone walk-through", Status: "open", CreatedAt: before,
			Fields: map[string]string{"audience": "owner", "operation_type": "manual", "requesting_task": "tp-dev"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-e-gh", Type: "escalation", Title: "Grant the reviewer token actions:write", Status: "open", CreatedAt: before,
			Fields: map[string]string{"audience": "owner", "operation_type": "github_token_scope",
				"operation_spec": `{"errand_clicks":"Settings > Developer settings > tokens > reviewer: Actions read and write"}`}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-e-gh2", Type: "escalation", Title: "Grant the reviewer token  actions:write", Status: "open", CreatedAt: at,
			Fields: map[string]string{"audience": "owner", "operation_type": "github_token_scope"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-e-sec", Type: "escalation", Title: "Rotate the bridge tokens", Status: "open", CreatedAt: before,
			Fields: map[string]string{"audience": "operator", "sensitivity": "secret", "operation_type": "credential_rotation", "approved_by": "oidc:ada@example.com"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-e-kc", Type: "escalation", Title: "Add bob to the land-owners group", Status: "open", CreatedAt: before,
			Fields: map[string]string{"operation_type": "keycloak_group_membership"}}),
		// In hand for the desk and an operator: not the owner's errands.
		landBead(&beadsapi.BeadDetail{ID: "tp-e-merge", Type: "escalation", Title: "Merge PR #14", Status: "open", CreatedAt: before,
			Fields: map[string]string{"operation_type": "gitlab_mr_merge"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-e-helm", Type: "escalation", Title: "Roll PR #14 in the next window", Status: "open", CreatedAt: before,
			Fields: map[string]string{"operation_type": "helm_deploy"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-e-done", Type: "escalation", Title: "Rotate an old key", Status: "closed", CreatedAt: before,
			Fields: map[string]string{"operation_type": "credential_rotation"}}),
		landBead(&beadsapi.BeadDetail{ID: "tp-d1", Type: "decision", Title: "Plan tp-p2: run it?", Status: "open", CreatedAt: before,
			Fields: map[string]string{"operated_by": "oidc:ada@example.com"}}),
	)
	d, err := GatherDigest(context.Background(), f, land, digestPurse(), digestDay)
	if err != nil {
		t.Fatalf("GatherDigest: %v", err)
	}
	if len(d.Working) != 0 {
		t.Errorf("working %+v: an owner-at-device step is an errand, not in progress", d.Working)
	}
	// The second token ask was filed since the last digest: news.
	if d.Quiet {
		t.Error("a new owner errand read as quiet")
	}
	got := FormatDigest(d)
	want := `What you must do, by place:
GitHub:
- tp-e-gh: Grant the reviewer token actions:write (escalation; asked since the last digest; also tp-e-gh2)
  clicks: Settings > Developer settings > tokens > reviewer: Actions read and write
  done when: the escalation is closed with what was done
Keycloak:
- tp-e-kc: Add bob to the land-owners group (escalation)
  clicks: not written down; read tp-e-kc
  done when: the escalation is closed with what was done
Phone:
- tp-dev: Walk the Commons on a phone (step; also tp-e-dev)
  clicks: open /garden, tap Commons
  done when: two screenshots on the step
Decisions to answer:
- tp-d1: Plan tp-p2: run it? (for ada@example.com)
  done when: an option is chosen on the card
Elsewhere:
- tp-e-sec: Rotate the bridge tokens (escalation; approved, not yet done)
  clicks: not written down; read tp-e-sec
  done when: the escalation is closed with what was done
`
	if !strings.Contains(got, want) {
		t.Fatalf("errands:\n%s\nwant within:\n%s", got, want)
	}
	if strings.Contains(got, "In progress now") || strings.Contains(got, "tp-e-merge") || strings.Contains(got, "tp-e-helm") || strings.Contains(got, "tp-e-done") {
		t.Fatalf("an item not the owner's is listed:\n%s", got)
	}
}

// A filer's errand_place outside the known places is its own group, after
// the known ones and before Elsewhere; the known places take their aliases.
func TestErrandPlace(t *testing.T) {
	for in, want := range map[string]string{
		"GitHub": "github", "gh": "github", "AWS console": "aws", "Slack admin": "slack",
		"laptop": "device", "Registrar dashboard": "Registrar dashboard", "": "",
	} {
		if got := normalizePlace(in); got != want {
			t.Errorf("normalizePlace(%q) = %q, want %q", in, got, want)
		}
	}
	got := foldErrands([]Errand{{ID: "a", Place: "other"}, {ID: "b", Place: "Registrar dashboard"}, {ID: "c", Place: "phone"}})
	if ids := []string{got[0].ID, got[1].ID, got[2].ID}; strings.Join(ids, ",") != "c,b,a" {
		t.Fatalf("order %v, want c,b,a", ids)
	}
	for title, want := range map[string]string{"Grant the AWS role": "aws", "Approve the draws": "", "Invite to Slack": "slack"} {
		if got := placeFromTitle(title); got != want {
			t.Errorf("placeFromTitle(%q) = %q, want %q", title, got, want)
		}
	}
}
