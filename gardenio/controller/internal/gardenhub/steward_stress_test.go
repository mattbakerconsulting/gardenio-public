package gardenhub

import (
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/commons/commonstest"
)

// The steward root under the Commons UX rubric's stress (gd-r7JiBQdZNS; the commons-ux-acceptance shoot's
// commonsstewardstress, /garden/c/s/stewards): a land of its own, so gardenio's steward, #general and every golden stay as
// they are. F1 a 200-char plan title, a 90-char run and crew name and #general lines past the transcript's 240-rune cut;
// F2 a URL, a SHA, a branch and a path in its reason, notes, jobs and threads; F3 thirteen plans (one past the tree's 12)
// and a thread with more replies than it shows; F4 emoji, CJK and Arabic in its notes, plans and threads; F6 a plan, a
// parked step and a decision its words name that the daemon does not have.
const (
	swLand, swGeneral, swRun = "stewards", "gd-chSwGeneral", "gd-swStewRun"
	// swGonePlan, swGoneStep and swGoneAsk are named but never put in the daemon (F6)
	swGonePlan, swGoneStep, swGoneAsk = "gd-swGonePlan", "gd-swGoneStep", "gd-swGoneAsk1"
)

// swRunName is the steward's run at the rubric's 90 chars: <land>-steward-<suffix>, so stewardRun reads it as the steward.
var swRunName = (swLand + "-steward-" + strings.Repeat("a-run-name-long-enough-to-wrap-", 3))[:90]

// stewardStressWorld puts the stress land's beads in the Commons daemon: its plans' epics (not swGonePlan), a gardener on
// the first, its run and the run's decisions, and five #general threads that address it.
func stewardStressWorld(d *commonstest.Daemon, now time.Time) {
	at := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	in := []string{"project:" + swLand}
	for i, p := range stewardStressPlans() {
		if p.ID == swGonePlan {
			continue
		}
		d.Put(&beadsapi.BeadDetail{ID: p.ID, Type: "epic", Status: "open", Title: p.Title, Labels: append([]string{"plan"}, in...), CreatedAt: at(600 - i), UpdatedAt: at(600 - i)})
	}
	d.Put(&beadsapi.BeadDetail{ID: swRun, Type: "agent", Status: "open", Title: swRunName, Labels: in, CreatedAt: at(30), UpdatedAt: at(30),
		Fields: map[string]string{"agent": swRunName, "role": "steward", "agent_state": "working"}})
	d.Put(&beadsapi.BeadDetail{ID: "gd-swGard1", Type: "trigger", Status: "open", Title: uxLong("Plan gardener for ", 120), CreatedAt: at(590), UpdatedAt: at(590),
		Labels: append([]string{commons.GardenerEpicLabel + "gd-swPlanNo01", commons.PlanGardenerLabel}, in...), Fields: map[string]string{"enabled": "true"}})
	for i, dc := range [][2]string{
		{"open", uxLong("Merge the release train now, or hold it for ", 200)},
		{"open", "🧑‍🌾 Ship " + uxBranch + " before 田中花子 and مريم sign off? ⚠️"},
		{"open", "Re-run " + uxURL},
		{"closed", "Keep the band until step 8?"},
	} {
		d.Put(&beadsapi.BeadDetail{ID: fmt.Sprintf("gd-swAsk%d", i+1), Type: "decision", Status: dc[0], Title: dc[1], Labels: in, CreatedBy: swRunName,
			CreatedAt: at(500 - i), UpdatedAt: at(500 - i), Fields: map[string]string{"prompt": dc[1]}})
	}
	long := uxLong("@steward ", 300) + " " + uxF4
	for i, m := range [][4]string{
		// the oldest thread, past the transcript's 4, so it is not shown
		{"gd-swMsg0", "", "oidc:sam@example.com", "@steward an old question nobody sees"},
		{"gd-swMsg1", "", "oidc:田中花子@example.com", long},
		{"gd-swMsg1a", "gd-swMsg1", "agent:" + swRunName, "Waiting on " + swGoneAsk + " (a decision the daemon lost) and gd-swPlanNo01. " + uxTokens},
		{"gd-swMsg1b", "gd-swMsg1", "agent:" + uxAgent, "I can take it: " + uxURL},
		{"gd-swMsg2", "", "oidc:مريم@example.com", "@steward مرحبا، ما الذي يعيق الخطة؟ 🇳🇿 ⚠️ café"},
		{"gd-swMsg2a", "gd-swMsg2", "agent:" + swRunName, "الخطة تنتظر " + uxSHA},
		{"gd-swMsg3", "", "oidc:" + strings.Repeat("a-very-long-local-part-", 4) + "x@example.com", "@steward where is " + uxPath + " used?"},
		{"gd-swMsg4", "", "oidc:matt@example.com", "@steward one thread with more replies than the transcript shows"},
	} {
		putSwMsg(d, m, at(470-i))
	}
	for i := range 6 {
		who := []string{"agent:" + swRunName, "oidc:matt@example.com"}[i%2]
		putSwMsg(d, [4]string{fmt.Sprintf("gd-swMsg4r%d", i+1), "gd-swMsg4", who, fmt.Sprintf("Reply %d: 提交工作台 🚀👩🏽‍💻 %s", i+1, uxLong("", 80))}, at(450-i))
	}
}

// putSwMsg puts one of the stress land's #general messages, m being id, thread, author and text.
func putSwMsg(d *commonstest.Daemon, m [4]string, at time.Time) {
	f := map[string]string{"channel_id": swGeneral, "author": m[2], "text": m[3]}
	if m[1] != "" {
		f["thread_id"] = m[1]
	}
	d.Put(&beadsapi.BeadDetail{ID: m[0], Type: "message", Status: "closed", Title: m[3], Labels: []string{"project:" + swLand}, CreatedAt: at, UpdatedAt: at, Fields: f})
}

// stewardStressPlans are the thirteen plans the stress steward tends: F1 a 200-char title, F2 a URL, F4 emoji, CJK and
// Arabic, two stalled with a long why, and swGonePlan (F6).
func stewardStressPlans() []commons.StewardPlan {
	ps := []commons.StewardPlan{
		{ID: "gd-swPlanNo01", Title: uxLong("Commons v2 under stress: ", 200), State: "building"},
		{ID: "gd-swPlanNo02", Title: "提交工作台 🚀👩🏽‍💻 田中花子のプラン", State: "building"},
		{ID: "gd-swPlanNo03", Title: "خطة مريم للحديقة ⚠️", State: "open", Stalled: true, Why: uxLong("no gardener: nothing moved in 9 days, and ", 160)},
		{ID: "gd-swPlanNo04", Title: uxURL, State: "open"},
		{ID: swGonePlan, Title: "A plan the daemon does not have", State: "open", Stalled: true, Why: "its gardener was retired"},
	}
	for i := len(ps); i < 13; i++ {
		ps = append(ps, commons.StewardPlan{ID: fmt.Sprintf("gd-swPlanNo%02d", i), Title: fmt.Sprintf("Plan %d 🇳🇿", i), State: "open"})
	}
	return ps
}

// stewardStress is the stress land's steward for wbStewards: working on swRun at 117 of 120
// runs, its reason and notes full of tokens and F4, two promises past due, two steps parked.
func stewardStress(now time.Time) *commons.StewardLand {
	day := func(n int) string { return now.AddDate(0, 0, n).UTC().Format(time.DateOnly) }
	return &commons.StewardLand{ID: "gd-swSteward", Name: swLand + "-steward", Land: swLand, General: swGeneral,
		State: commons.StewardWorking, Reason: "Waiting on CI for " + uxURL + " 🧑‍🌾 مريم", Run: swRun, RunsToday: 117, MaxRunsPerDay: 120, Spend: "$1,284.56 today",
		NotesID: "gd-swNotes", Notes: "## Holds\n- " + uxLong("never merge a PR that touches billing, and ", 200) + "\n- no chart merges on " + uxBranch +
			"\n- " + uxF4 + "\n- لا دمج قبل الإصدار\n\n## Answers (newest first)\n- jobs are served from memory, see " + uxURL + " (田中花子)\n\n## Promises\n- " +
			day(-3) + ": " + uxLong("report on ", 200) + "\n- " + day(-1) + ": tidy " + uxPath + " 🚀\n- " + day(5) + ": nothing due yet\n\n## In flight\n- " + uxTokens + "\n",
		Plans: stewardStressPlans(),
		Parked: []commons.StewardStep{{ID: "gd-swStep1", Title: uxLong("A parked step: ", 200), Agent: uxAgent},
			{ID: swGoneStep, Title: "", Agent: swRunName}}}
}
