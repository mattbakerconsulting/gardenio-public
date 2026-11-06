package gardenhub

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// waitWorld is one plan holding an open decision and escalation in every wait
// state, plus the agent bead an approved escalation names by id.
func waitWorld() *fakeFarmSrc {
	seq = 0
	f := newFakeFarm()
	f.add(planBead("gd-planW", "building", "in_progress", "gd-W1"), fb("gd-W1", "epic", "in_progress", "gd-planW", 40))

	unanswered := fb("gd-Wdu", "decision", "open", "gd-W1", 100)
	answered := fb("gd-Wda", "decision", "open", "gd-W1", 110)
	answered.Fields["chosen"], answered.Fields["responded_at"] = "yes", "2026-10-19T02:00:00Z"
	answered.Fields["requesting_agent_bead_id"] = "gd-agentBeadW"
	pending := fb("gd-Wep", "escalation", "open", "gd-W1", 120)
	pending.Fields["requesting_agent"], pending.Fields["audience"] = "crew-asker", "owner"
	approved := fb("gd-Wea", "escalation", "in_progress", "gd-W1", 130)
	approved.Fields["approved_at"], approved.Fields["approved_by"], approved.Fields["approval_method"] = "2026-10-19T01:56:09Z", "matt@example.com", "web_ui"
	approved.Fields["executor"], approved.Fields["requesting_agent"] = "laptop-claude", "gd-agentBeadW"
	byID := fb("gd-Web", "escalation", "in_progress", "gd-W1", 140)
	byID.Fields["approved_at"], byID.Fields["requesting_agent"] = "2026-10-19T03:00:00Z", "gd-agentBeadW"
	rejected := fb("gd-Wer", "escalation", "open", "gd-W1", 150)
	rejected.Fields["result_status"], rejected.Fields["result"], rejected.Fields["requesting_agent"] = "failure", `{"error":"rejected","reason":"not now"}`, "crew-asker"
	failed := fb("gd-Wef", "escalation", "in_progress", "gd-W1", 160)
	failed.Fields["approved_at"], failed.Fields["result_status"], failed.Fields["result"], failed.Fields["requesting_agent"] = "2026-10-19T03:00:00Z", "failure", `{"output":"helm upgrade timed out"}`, "crew-asker"
	f.add(unanswered, answered, pending, approved, byID, rejected, failed)

	agent := fb("gd-agentBeadW", "agent", "open", "", 1)
	agent.Title, agent.Fields["agent"] = "gardenio-ingarden-bead-view-cd1", "gardenio-ingarden-bead-view-cd1"
	f.add(agent)
	return f
}

func findWait(ws []FarmWait, id string) *FarmWait {
	for i := range ws {
		if ws[i].ID == id {
			return &ws[i]
		}
	}
	return nil
}

// TestFarmWaitStates pins what each open decision and escalation waits on
// (gd-TEl9BfDL2z): only an unanswered decision and an unapproved escalation
// wait on a human; an approved escalation waits on its executor (or the agent
// that asked, resolved from its bead id), and is not "needs you".
func TestFarmWaitStates(t *testing.T) {
	src := waitWorld()
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-planW")
	if p == nil {
		t.Fatal("plan W missing")
	}
	if p.OpenDecisions != 2 || p.OpenEscalations != 5 {
		t.Fatalf("open counters changed meaning: decisions=%d escalations=%d", p.OpenDecisions, p.OpenEscalations)
	}
	if p.NeedsHuman != 2 {
		t.Fatalf("needs_human = %d, want 2 (the unanswered decision and the pending escalation)", p.NeedsHuman)
	}
	at := func(s string) int64 { tm, _ := time.Parse(time.RFC3339, s); return tm.Unix() }
	want := []FarmWait{
		{ID: "gd-Wdu", Kind: "decision", State: WaitUnanswered, Human: true},
		{ID: "gd-Wep", Kind: "escalation", State: WaitPending, Human: true},
		{ID: "gd-Wda", Kind: "decision", State: WaitAnswered, WaitsOn: "gardenio-ingarden-bead-view-cd1", Since: at("2026-10-19T02:00:00Z")},
		{ID: "gd-Wea", Kind: "escalation", State: WaitApproved, WaitsOn: "laptop-claude", Since: at("2026-10-19T01:56:09Z")},
		{ID: "gd-Web", Kind: "escalation", State: WaitApproved, WaitsOn: "gardenio-ingarden-bead-view-cd1", Since: at("2026-10-19T03:00:00Z")},
		{ID: "gd-Wer", Kind: "escalation", State: WaitRejected, WaitsOn: "crew-asker"},
		{ID: "gd-Wef", Kind: "escalation", State: WaitFailed, WaitsOn: "crew-asker"},
	}
	if len(p.Waiting) != len(want) {
		t.Fatalf("waiting has %d items, want %d: %+v", len(p.Waiting), len(want), p.Waiting)
	}
	for _, w := range want {
		got := findWait(p.Waiting, w.ID)
		if got == nil || got.Kind != w.Kind || got.State != w.State || got.Human != w.Human || got.WaitsOn != w.WaitsOn || got.Title != "Title "+w.ID {
			t.Errorf("%s: got %+v, want %+v", w.ID, got, w)
		}
		if got != nil && w.Since != 0 && got.Since != w.Since {
			t.Errorf("%s: since = %d, want %d", w.ID, got.Since, w.Since)
		}
		if got != nil && w.Since == 0 && got.Since <= 0 {
			t.Errorf("%s: since unset", w.ID)
		}
	}
	// Human waits lead the list, so a capped list never drops one before a non-human wait.
	if !p.Waiting[0].Human || !p.Waiting[1].Human || p.Waiting[2].Human {
		t.Fatalf("human waits not first: %+v", p.Waiting)
	}

	// The agent bead is read once: its name is cached across refreshes.
	gets := src.getCalls
	src.mutate("gd-Wdu", func(b *beadsapi.BeadDetail) { b.UpdatedAt = b.UpdatedAt.Add(time.Second) })
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.getCalls != gets {
		t.Fatalf("agent name re-read: %d BatchGetBeads calls, want %d", src.getCalls, gets)
	}
}

// TestFarmApprovedEscalationIsNotNeedsYou is the acceptance shape: a plan whose
// only open item is an approved escalation needs no human and names its agent.
func TestFarmApprovedEscalationIsNotNeedsYou(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-planE", "building", "in_progress"))
	esc := fb("gd-Eea", "escalation", "in_progress", "gd-planE", 100)
	esc.Fields["approved_at"], esc.Fields["requesting_agent"] = "2026-10-19T01:56:09Z", "gardenio-ingarden-bead-view-cd1"
	src.add(esc)
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-planE")
	if p == nil || p.NeedsHuman != 0 || p.OpenEscalations != 1 || len(p.Waiting) != 1 {
		t.Fatalf("plan E wrong: %+v", p)
	}
	if w := p.Waiting[0]; w.State != WaitApproved || w.Human || w.WaitsOn != "gardenio-ingarden-bead-view-cd1" {
		t.Fatalf("approved escalation wrong: %+v", w)
	}
}

// parties is a fake party resolver for waitOf: a ref not listed is a person.
type parties map[string]int

func (ps parties) of(ref string) (string, int) { return ref, ps[ref] }

// TestWaitOfLiveParty pins whom a non-human wait names (gd-g7jfH27efD): a
// local or manual run waits on the operator, a finished party is skipped for
// the next live one, a wait with every party finished falls to the operator
// (and so is a human wait), and a synthetic "escalation-<id>" executor is
// never shown as a person.
func TestWaitOfLiveParty(t *testing.T) {
	const synth = "escalation-gd-2sfhdidw15"
	approvedEsc := func(fields map[string]string, assignee, createdBy string) *beadsapi.BeadDetail {
		b := fb("gd-Lesc01", "escalation", "in_progress", "", 1)
		b.Fields["approved_at"], b.Fields["audience"] = "2026-10-19T03:59:19Z", "owner"
		for k, v := range fields {
			b.Fields[k] = v
		}
		b.Assignee, b.CreatedBy = assignee, createdBy
		return b
	}
	due := time.Date(2026, 10, 19, 21, 30, 0, 0, time.UTC).Unix()
	type waitCase struct {
		name    string
		b       *beadsapi.BeadDetail
		ps      parties
		waitsOn string
		human   bool
		due     int64
	}
	cases := []waitCase{
		{"local run waits on the operator, with its due time",
			approvedEsc(map[string]string{"execution_target": "local", "executor": "crew-live", "due_at": "2026-10-19T21:30:00Z"}, "", "crew-asker"),
			parties{"crew-live": partyLive}, WaitsOnOperator, true, due},
		{"manual operator action waits on the operator",
			approvedEsc(map[string]string{"operation_type": "manual_operator_action", "executor": synth}, "", "crew-asker"),
			parties{synth: partyGone}, WaitsOnOperator, true, 0},
		{"finished executor falls to the assignee",
			approvedEsc(map[string]string{"executor": "crew-exec", "requesting_agent": "crew-asker"}, "crew-assignee", ""),
			parties{"crew-exec": partyGone, "crew-assignee": partyLive, "crew-asker": partyLive}, "crew-assignee", false, 0},
		{"finished executor and assignee fall to the live requesting agent",
			approvedEsc(map[string]string{"executor": "crew-exec", "requesting_agent": "crew-asker"}, "crew-assignee", "crew-filer"),
			parties{"crew-exec": partyGone, "crew-assignee": partyGone, "crew-asker": partyLive}, "crew-asker", false, 0},
		{"every party finished falls to the operator, a human wait",
			approvedEsc(map[string]string{"executor": synth, "requesting_agent": "gd-agentGone1"}, "", "crew-filer"),
			parties{synth: partyGone, "gd-agentGone1": partyGone, "crew-filer": partyGone}, WaitsOnOperator, true, 0},
		{"a live synthetic executor is its executor, not a person",
			approvedEsc(map[string]string{"executor": synth}, "", "crew-filer"),
			parties{synth: partyLive}, WaitsOnExecutor, false, 0},
		{"a finished synthetic executor with nothing better known is its executor (finished)",
			approvedEsc(map[string]string{"executor": synth}, "", "crew-filer"),
			parties{synth: partyDone}, WaitsOnExecutorDone, false, 0},
		{"a finished synthetic executor is skipped for a live party",
			approvedEsc(map[string]string{"executor": synth, "requesting_agent": "crew-asker"}, "", ""),
			parties{synth: partyGone, "crew-asker": partyLive}, "crew-asker", false, 0},
		{"a person is named as is",
			approvedEsc(map[string]string{"executor": "laptop-claude"}, "", ""),
			parties{}, "laptop-claude", false, 0},
		// The desk's and the operators' (gd-bmOiteQi36): a wait that would fall to a human is theirs, not the owner's.
		{"a local run of a desk merge waits on the merge desk",
			approvedEsc(map[string]string{"execution_target": "local", "audience": "desk", "due_at": "2026-10-19T21:30:00Z"}, "", ""),
			parties{}, WaitsOnDesk, false, 0},
		{"a manual action for the operators waits on them",
			approvedEsc(map[string]string{"operation_type": "manual_operator_action", "audience": "operator"}, "", ""),
			parties{}, WaitsOnOperators, false, 0},
		{"one that needs credentials is the owner's whatever it says",
			approvedEsc(map[string]string{"execution_target": "local", "audience": "desk", "credential_requirements": `[{"type":"github_sudo"}]`}, "", ""),
			parties{}, WaitsOnOperator, true, 0},
		// held_by (gd-vFUcVoRCU4): a lane other than the owner's has it, as /up reads it.
		{"a manual action a coordinator holds is the coordinator's, credentials or not",
			approvedEsc(map[string]string{"operation_type": "manual_operator_action", "audience": "owner", "held_by": "coordinator:laptop", "credential_requirements": `[{"type":"k8s_secret"}]`}, "", ""),
			parties{}, "coordinator laptop", false, 0},
		{"a local run the owner's lane holds is still the owner's",
			approvedEsc(map[string]string{"execution_target": "local", "audience": "owner", "held_by": "operator"}, "", ""),
			parties{}, WaitsOnOperator, true, 0},
		{"a local run a land owner holds is a human wait",
			approvedEsc(map[string]string{"execution_target": "local", "audience": "owner", "held_by": "land-owner:acme"}, "", ""),
			parties{}, WaitsOnOperator, true, 0},
	}
	answered := fb("gd-Ldec01", "decision", "open", "", 1)
	answered.Fields["chosen"], answered.Fields["requested_by"] = "yes", "crew-gone"
	failed := fb("gd-Lfail1", "escalation", "in_progress", "", 1)
	failed.Fields["result_status"], failed.Fields["result"], failed.Fields["requesting_agent"] = "failure", `{"output":"x"}`, "crew-gone"
	failed.CreatedBy = "crew-filer"
	pendingDesk := fb("gd-Lpdk01", "escalation", "open", "", 1)
	pendingDesk.Fields["operation_type"] = "gitlab_mr_merge"
	pendingLane := fb("gd-Lpln01", "escalation", "open", "", 1)
	pendingLane.Fields["operation_type"], pendingLane.Fields["sensitivity"], pendingLane.Fields["held_by"] = "k8s_secret_seed", "secret", "coordinator:laptop"
	cases = append(cases,
		waitCase{"an unapproved merge is the desk's, no ask of the owner", pendingDesk, parties{}, WaitsOnDesk, false, 0},
		waitCase{"an unapproved secret a coordinator holds is the coordinator's", pendingLane, parties{}, "coordinator laptop", false, 0},
		waitCase{"an answered decision whose asker finished falls to the operator", answered, parties{"crew-gone": partyGone}, WaitsOnOperator, true, 0},
		waitCase{"a failed run whose asker finished names who filed it", failed, parties{"crew-gone": partyGone, "crew-filer": partyLive}, "crew-filer", false, 0},
	)
	for _, c := range cases {
		w := waitOf(c.b, c.ps.of)
		if w.WaitsOn != c.waitsOn || w.Human != c.human || w.Due != c.due {
			t.Errorf("%s: got waits_on=%q human=%v due=%d, want %q %v %d", c.name, w.WaitsOn, w.Human, w.Due, c.waitsOn, c.human, c.due)
		}
		if strings.HasPrefix(w.WaitsOn, "escalation-") {
			t.Errorf("%s: synthetic executor shown as a person: %q", c.name, w.WaitsOn)
		}
	}
}

// TestFarmWaitNamesLiveParty is the live shape (gd-5f5bJPKLhC on 2026.292.5):
// an approved escalation whose synthetic executor ran and closed. Run locally
// by hand it reads "waiting on the operator"; run in the fleet with every named
// agent finished it falls to the operator too, and an agent that closes after
// the farm first saw it live is noticed on the next roster read.
func TestFarmWaitNamesLiveParty(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-planL", "building", "in_progress"))
	local := fb("gd-Lloc01", "escalation", "in_progress", "gd-planL", 100)
	local.Fields["approved_at"], local.Fields["executor"] = "2026-10-19T03:59:19Z", "escalation-gd-lloc01"
	local.Fields["execution_target"], local.Fields["operation_type"] = "local", "manual_operator_action"
	local.Fields["requesting_agent"], local.CreatedBy = "gd-agentAsk1", "crew-filer"
	local.Fields["audience"] = "owner"
	fleet := fb("gd-Lflt01", "escalation", "in_progress", "gd-planL", 110)
	fleet.Fields["approved_at"], fleet.Fields["executor"] = "2026-10-19T03:59:19Z", "escalation-gd-lflt01"
	fleet.Fields["requesting_agent"], fleet.CreatedBy = "gd-agentAsk1", "crew-filer"
	fleet.Fields["audience"] = "owner"
	live := fb("gd-Llive1", "escalation", "in_progress", "gd-planL", 120)
	live.Fields["approved_at"], live.Fields["executor"] = "2026-10-19T03:59:19Z", "crew-runner"
	live.Fields["audience"] = "owner"
	src.add(local, fleet, live)
	agent := func(id, name, status, state string) *beadsapi.BeadDetail {
		b := fb(id, "agent", status, "", 1)
		b.Title, b.Fields["agent"], b.Fields["agent_state"] = name, name, state
		return b
	}
	src.add(agent("gd-agentExL", "escalation-gd-lloc01", "closed", "done"), agent("gd-agentExF", "escalation-gd-lflt01", "closed", "done"),
		agent("gd-agentAsk1", "crew-asker", "closed", "done"), agent("gd-agentFil", "crew-filer", "open", "done"),
		agent("gd-agentRun", "crew-runner", "open", "working"))
	f := newTestFarm(src)
	now := time.Unix(baseUnix+10_000, 0)
	f.now = func() time.Time { return now }
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-planL")
	if p == nil {
		t.Fatal("plan L missing")
	}
	check := func(p *FarmPlan, id, waitsOn string, human bool) {
		t.Helper()
		w := findWait(p.Waiting, id)
		if w == nil || w.State != WaitApproved || w.WaitsOn != waitsOn || w.Human != human {
			t.Errorf("%s: got %+v, want approved, waiting on %q, human=%v", id, w, waitsOn, human)
		}
	}
	check(p, "gd-Lloc01", WaitsOnOperator, true)
	check(p, "gd-Lflt01", WaitsOnOperator, true)
	check(p, "gd-Llive1", "crew-runner", false)
	if p.NeedsHuman != 2 {
		t.Fatalf("needs_human = %d, want 2: a wait that falls to the operator is a human wait", p.NeedsHuman)
	}

	// crew-runner finishes. The roster is re-read on every refresh (the fast
	// tier's agent read), so the next one sees it gone and the wait falls to
	// the operator.
	src.mutate("gd-agentRun", func(b *beadsapi.BeadDetail) { b.Status, b.Fields["agent_state"] = "closed", "done" })
	src.mutate("gd-Llive1", func(b *beadsapi.BeadDetail) { b.UpdatedAt = b.UpdatedAt.Add(time.Second) })
	now = now.Add(time.Second)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	check(findPlan(decodeSnap(t, f), "gd-planL"), "gd-Llive1", WaitsOnOperator, true)

	// Without a fresh roster nothing is called gone: a finished synthetic
	// executor reads "its executor (finished)", never its raw name.
	f.roster = agentRoster{}
	if name, state := f.party("escalation-gd-lflt01"); state != partyDone || name != "escalation-gd-lflt01" {
		t.Fatalf("stale roster: party = %q, %d; want partyDone", name, state)
	}
	if w := waitOf(fleet, f.party); w.WaitsOn != WaitsOnExecutorDone || w.Human {
		t.Fatalf("stale roster: %+v, want %q and not human", w, WaitsOnExecutorDone)
	}
}

// TestFinishWaitsCap keeps every human wait in a capped list and counts them all.
func TestFinishWaitsCap(t *testing.T) {
	var ws []FarmWait
	for i := range maxWaiting + 5 {
		ws = append(ws, FarmWait{ID: fmt.Sprintf("gd-w%02d", i), Since: int64(i), Human: i >= maxWaiting})
	}
	human, got := finishWaits(ws)
	if human != 5 || len(got) != maxWaiting {
		t.Fatalf("human=%d len=%d, want 5 and %d", human, len(got), maxWaiting)
	}
	for i := range 5 {
		if !got[i].Human {
			t.Fatalf("human wait dropped or not first: %+v", got)
		}
	}
}
