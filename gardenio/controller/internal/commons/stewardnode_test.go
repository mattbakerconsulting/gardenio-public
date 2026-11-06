package commons

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// fakeStewards answers claimlayer's steward, and counts the reads.
type fakeStewards struct {
	reads atomic.Int32
	notes string // in place of the fixture's notes, when set
}

func (f *fakeStewards) Steward(_ context.Context, land string) (*StewardLand, error) {
	f.reads.Add(1)
	if land != "claimlayer" && land != "" {
		return nil, nil
	}
	notes := "## Holds\n- no merges on Friday, see cl-HoldsNote1\n\n## Promises\n- 2026-10-18: report on cl-s3\n- 2026-12-01: tidy the readme\n"
	if f.notes != "" {
		notes = f.notes
	}
	return &StewardLand{ID: "cl-stew", Name: "claimlayer-steward", Land: "claimlayer", General: "cl-chgen",
		State: StewardWorking, Reason: "Building the node model: https://ci.example.com/acme/runs/42", Run: "cl-stewrun", RunsToday: 2, MaxRunsPerDay: 24, Spend: "120 credits left",
		NotesID: "cl-notes", Notes: notes,
		Plans:  []StewardPlan{{ID: commonstest.WorkbenchPlan, Title: "Commons v2", State: "building", Stalled: true, Why: "gardener paused"}},
		Parked: []StewardStep{{ID: "cl-s4", Title: "Desktop workbench", Agent: "cl-crew-s4"}}}, nil
}

// TestStewardFaceCutOpensInPlace: notes past nodeFaceMax show cut, and the face
// carries their whole behind a Read it all button, never a link away
// (gd-Fmq3fgqFTm).
func TestStewardFaceCutOpensInPlace(t *testing.T) {
	d := stewardFixture(t)
	sw := &fakeStewards{notes: "## Holds\n- " + strings.Repeat("hold the line ", nodeFaceMax/10) + "the very end"}
	srv := commonsServer(t, d, func(h *Handler) { h.Nodes, h.Stewards = d, sw })
	code, body := get(t, srv, "/garden/c/s/claimlayer", "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	mustContain(t, body, `<div class="cm-node-face" data-from="cl-notes"><div class="cm-cut"><div class="cm-cut-head">`,
		`<div class="cm-cut-whole" hidden>`, "the very end</li>",
		`</div><button type="button" class="cm-link cm-cut-go" aria-expanded="false">Read it all</button></div></div>`)
	mustNotContain(t, body, `/garden?bead=cl-notes`)
	if head, _, _ := strings.Cut(body[strings.Index(body, "cm-cut-head"):], "cm-cut-whole"); strings.Contains(head, "the very end") {
		t.Error("the cut face shows its end")
	}
}

// stewardFixture adds claimlayer's steward to the workbench fixture: its trigger,
// a #general thread that asks it (an agent answers in it too), a thread it
// answered unasked, decisions its runs filed, and a Commons wish nobody has
// answered beside the fixture's Slack wish (cl-wish1, an agent idle on it).
func stewardFixture(t *testing.T) *commonstest.Daemon {
	d := workbench(t)
	cl := []string{"project:claimlayer"}
	msg := func(id, thread, author, text string, m int) {
		f := map[string]string{"channel_id": "cl-chgen", "author": author, "text": text}
		if thread != "" {
			f["thread_id"] = thread
		}
		d.Put(&beadsapi.BeadDetail{ID: id, Type: "message", Status: "closed", Labels: cl, CreatedAt: t0.Add(time.Duration(m) * time.Minute), Fields: f})
	}
	d.Put(&beadsapi.BeadDetail{ID: "cl-stew", Type: "trigger", Status: "open", Title: "claimlayer steward",
		Labels: []string{"project:claimlayer", "steward", "gardener-land:claimlayer"}, Fields: map[string]string{}})
	msg("cl-ask1", "", "oidc:ann@claimlayer.example", "@steward what is stalled?", 40)
	msg("cl-ans1", "cl-ask1", "agent:claimlayer-steward-ab12", "The plan's gardener is paused, see cl-Abcdefghij; cl-Gone000001 was lost.", 41)
	d.Put(&beadsapi.BeadDetail{ID: "cl-Abcdefghij", Type: "task", Status: "open", Title: "Pause the gardener", Labels: cl})
	msg("cl-ans2", "cl-ask1", "agent:claimlayer-crew-1", "I can take it.", 42)
	msg("cl-hi2", "", "oidc:bo@claimlayer.example", "morning all", 43)
	d.Put(&beadsapi.BeadDetail{ID: "cl-HoldsNote1", Type: "task", Status: "open", Title: "Friday freeze", Labels: cl})
	msg("cl-ans3", "cl-hi2", "agent:claimlayer-steward-ab12", "Morning, Bo. "+strings.Repeat("The long answer goes on. ", 12), 44)
	for _, dc := range [][2]string{{"cl-dec1", "open"}, {"cl-dec2", "closed"}} {
		d.Put(&beadsapi.BeadDetail{ID: dc[0], Type: "decision", Status: dc[1], Title: "Run the plan?", Labels: cl, CreatedBy: "claimlayer-steward-ab12", Fields: map[string]string{}})
	}
	d.Put(&beadsapi.BeadDetail{ID: "cl-dec3", Type: "decision", Status: "open", Title: "Someone else's ask", Labels: cl, CreatedBy: "ann", Fields: map[string]string{}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-wish2", Type: "task", Status: "open", Title: "Dark mode for the porch", Labels: []string{"project:claimlayer", "thread"},
		CreatedAt: t0.Add(45 * time.Minute), CreatedBy: "bo@claimlayer.example",
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:bo@claimlayer.example", "origin": "commons", "text": "Can the porch go dark at night?"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-wish3", Type: "task", Status: "closed", Title: "An answered wish", Labels: []string{"project:claimlayer", "thread"}, Fields: map[string]string{"channel_id": "cl-chgen"}})
	return d
}

// TestStewardNode pins the steward node (steward view S1, gd-FsDEc7ZXGl): its
// route and the trigger's own node render kind steward under the node budget,
// with its head, notes, plans and their gardeners, Tending, the threads that
// address it, Asks for you and the composer's target; one viewer's read is
// kept; another land is a 404, and no Stewards a 503.
func TestStewardNode(t *testing.T) {
	d := stewardFixture(t)
	sw := &fakeStewards{}
	srv := commonsServer(t, d, func(h *Handler) { h.Nodes, h.Stewards = d, sw })
	for _, path := range []string{"/garden/c/s/claimlayer", "/garden/c/s", "/garden/c/n/cl-stew"} {
		code, body := get(t, srv, path, "claimlayer")
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		if len(body) > nodeBudget {
			t.Errorf("%s: %d bytes, over the %d-byte budget", path, len(body), nodeBudget)
		}
		mustContain(t, body, `data-kind="steward"`, `data-id="cl-stew"`,
			`<p class="cm-sw-now" data-state="working" data-agent="claimlayer-steward">Working <span class="cm-sw-why">Building the node model: <a class="cm-link" href="https://ci.example.com/acme/runs/42" title="https://ci.example.com/acme/runs/42" target="_blank" rel="noopener noreferrer">ci.example.com/…/runs/42</a></span></p>`,
			`2 runs today of 24</span> · <span data-slot="spend">120 credits left</span> · <a class="cm-sw-watch" href="/base/garden/c/n/cl-stewrun" data-node="cl-stewrun">Watch live</a>`,
			`data-say="/base/garden/c/claimlayer/cl-chgen/say" data-channel="cl-chgen" data-mention="@steward"`,
			`<div class="cm-node-face" data-from="cl-notes">`, "no merges on Friday",
			// the plan it tends, stalled, with its gardener under it
			`data-id="cl-plan" data-type="epic" data-state="building" data-flag="stalled"`, `data-id="cl-gard" data-type="trigger" data-state="running"`,
			`<section class="cm-sw-tending" data-slot="tending"><h3>Tending</h3>`, `data-state="gardener paused" data-flag="stalled"`, `data-id="cl-s4" data-type="task" data-state="parked" data-flag="parked"`,
			// its asks: its runs' open decision, its promise past due, one answered
			`data-done="1"><h3>Asks for you</h3>`, `data-id="cl-dec1"`, "2026-10-18: report on cl-s3",
			// the threads that address it, an agent's answer labelled, newest last
			`<li class="cm-sw-thread" data-thread="cl-ask1">`, `data-voice="steward"`, `<span class="cm-sw-tag">agent</span>`,
			`data-thread="cl-hi2"`, `<section class="cm-jobs" data-slot="jobs"`,
			// an id in its words links under its title (S2, gd-LisW87zrnf)
			`data-bead="cl-Abcdefghij" title="Pause the gardener (cl-Abcdefghij)">Pause the gardener</a>`,
			// (gd-r7JiBQdZNS) its reason's URL is labelled; an id it lacks is "not found", still linked; its notes' ids are titled; a cut line carries its whole text, opened in place (gd-Fmq3fgqFTm)
			`data-bead="cl-Gone000001" title="cl-Gone000001: not found"`, `title="Friday freeze (cl-HoldsNote1)">Friday freeze</a>`,
			`goes on. T…</span><span class="cm-sw-text cm-cut-whole" hidden>Morning, Bo. The long answer goes on.`,
			`The long answer goes on.</span> <button type="button" class="cm-link cm-cut-go" aria-expanded="false">Read it all</button></span>`,
			// its Wishes under the plans (gd-ak4P2qXMss): each open wish to its bead view, with its messages, last line and agent;
			// the one a person spoke last in with no agent live flagged, and asked for you
			`<li class="cm-tn cm-tn--wishes" data-id="cl-chgen" data-type="wishes" data-state="open">`,
			`data-id="cl-wish1" data-type="wish" data-state="idle" data-thread="cl-wish1"><a class="cm-tn-title" href="/base/garden?bead=cl-wish1"`,
			`<span class="cm-tn-wish" data-voice="person">2 replies · <span class="cm-tn-last"><b>ann</b> <time datetime="2026-10-19T09:04:00Z">Oct 19 09:04</time> legacy reply</span> · <span class="cm-tn-agent" data-state="">claimlayer-thread-helper</span></span>`,
			`data-id="cl-wish2" data-type="wish" data-state="open" data-flag="unanswered" data-thread="cl-wish2"><a class="cm-tn-title" href="/base/garden?bead=cl-wish2"`,
			`no replies · <span class="cm-tn-last"><b>bo</b> <time datetime="2026-10-19T09:45:00Z">Oct 19 09:45</time> Can the porch go dark at night?</span></span>`,
			`<li class="cm-open cm-open--wish" data-id="cl-wish2" data-state="unanswered">`,
			// the wishes' threads in its conversation, a reply forwarded without a channel_id in its head's
			`<li class="cm-sw-thread" data-thread="cl-wish1">`, `data-id="cl-rep2" data-voice="person"`, `<li class="cm-sw-thread" data-thread="cl-wish2">`)
		mustNotContain(t, body, "Claimlayer hello", "cl-dec3", "cl-dec2", "cl-wish3", `data-id="cl-wish1" data-state="unanswered"`, "cm-sw-rest", ">Read it all</a>")
		if asks, _, _ := strings.Cut(body[strings.Index(body, "Asks for you"):], "</section>"); strings.Contains(asks, "tidy the readme") {
			t.Errorf("%s: a promise not yet due is asked: %s", path, asks)
		}
		mustNotContain(t, body, commonstest.Leaks...)
		if strings.Index(body, `data-thread="cl-ask1"`) > strings.Index(body, `data-thread="cl-hi2"`) {
			t.Errorf("%s: the threads are not newest last", path)
		}
	}
	if n := sw.reads.Load(); n != 1 {
		t.Errorf("%d steward reads, want one: the route, the current land and the trigger's node read the same land", n)
	}
	if code, _ := get(t, srv, "/garden/c/s/gardenio", "claimlayer"); code != http.StatusNotFound {
		t.Errorf("another land's steward: %d", code)
	}
	if code, _ := get(t, srv, "/garden/c/s/nosteward", ""); code != http.StatusNotFound {
		t.Errorf("a land with no steward: %d", code)
	}
	if code, _ := get(t, commonsServer(t, d, func(h *Handler) { h.Nodes = d }), "/garden/c/s/claimlayer", "claimlayer"); code != http.StatusServiceUnavailable {
		t.Errorf("no Stewards: %d", code)
	}
}

// TestMentionsOfferSteward: the @ list offers @steward after @gardenio when
// steward nodes are served, to a viewer who may post.
func TestMentionsOfferSteward(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d, func(h *Handler) { h.Compose, h.Stewards = &fakeComposer{}, &fakeStewards{} })
	_, body := get(t, h, "/garden/c/claimlayer/general/mentions", "claimlayer")
	var got []mentionable
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if len(got) < 2 || got[1] != (mentionable{Name: StewardMention, Label: "Ask the land's steward", Kind: "steward"}) {
		t.Errorf("mentions = %+v, want @steward second", got)
	}
}

// TestStewardTranscriptVoices: in the steward's conversation the fixture wish's
// card is a state line, its agent linked to its panel and its state word, never
// its "Agent: ..." words, and a squawk is tagged a squawk (gd-d3irroS7Q1). The
// later replies are dropped so the card is among the last stewardRepliesMax.
func TestStewardTranscriptVoices(t *testing.T) {
	d := stewardFixture(t)
	const head = "cl-wishV"
	commonstest.WishFamily(head, "claimlayer", "cl-chgen", t0.Add(50*time.Minute)).PutIn(d)
	for _, id := range []string{commonstest.WishSquawk2, commonstest.WishReply2, commonstest.WishSquawk3} {
		delete(d.Beads, head+id)
	}
	srv := commonsServer(t, d, func(h *Handler) { h.Nodes, h.Stewards = d, &fakeStewards{} })
	_, body := get(t, srv, "/garden/c/s/claimlayer", "claimlayer")
	i := strings.Index(body, `<li class="cm-sw-thread" data-thread="`+head+`">`)
	if i < 0 {
		t.Fatalf("no thread %s in the transcript:\n%s", head, body)
	}
	thread, _, _ := strings.Cut(body[i:], "</li>\n")
	agent := "claimlayer-one-bead-view-k2p"
	mustContain(t, thread,
		`data-id="`+head+`" data-voice="person" data-via="slack"><span class="cm-sw-who">matt</span>`,
		`data-id="`+head+commonstest.WishCard+`" data-voice="agent" data-via="card"><a class="cm-sw-who cm-bead" href="/base/garden?bead=`+head+commonstest.WishAgent+`" data-bead="`+head+commonstest.WishAgent+`">`+agent+`</a> <span class="cm-sw-tag">card</span> <span class="cm-sw-state">working</span> <time`,
		`data-via="squawk"><span class="cm-sw-who">`+agent+` <span class="cm-sw-tag cm-via--squawk">squawk</span></span>`)
	mustNotContain(t, thread, "Agent: ", "slack-bridge")
}
