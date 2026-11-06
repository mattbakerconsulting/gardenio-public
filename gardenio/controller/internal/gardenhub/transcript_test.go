package gardenhub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The transcript (gd-MsTDi7JYVs, plan gd-jMl4oF2cJO step 7; bird captions spec 11.1-11.3, 12): each gardener's last 200
// finished calls in memory, served cut per viewer, and live as `call` events only to a stream holding one open.

// rowsOf is gardener id's transcript as an unrestricted viewer reads it, newest first.
func rowsOf(t *testing.T, a *activity, id string) []CallRow {
	t.Helper()
	p, ok := a.transcript(id, nil, transcriptQuery{limit: transcriptRows})
	if !ok {
		t.Fatalf("no transcript for %s", id)
	}
	return p.rows
}

// callRows is every row of the call events queued on ch, and their skipped counts summed.
func callRows(t *testing.T, ch <-chan ActivityMsg) (rows []CallRow, skipped, events int) {
	t.Helper()
	for _, m := range msgs(ch)[CallEvent] {
		var c FarmCalls
		if err := json.Unmarshal(m.Data, &c); err != nil {
			t.Fatal(err)
		}
		rows, skipped, events = append(rows, c.Rows...), skipped+c.Skipped, events+1
	}
	return rows, skipped, events
}

func TestTranscriptRingBound(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	for i := range transcriptRows + 50 {
		id := fmt.Sprintf("c%d", i)
		a.observe(preCap("gd-a", "Read", id, fmt.Sprintf("Read call %d", i), c.t), c.t)
		a.observe(post("gd-a", "Read", id, c.add(time.Second)), c.t)
	}
	rows := rowsOf(t, a, "gd-a")
	if len(rows) != transcriptRows || len(a.agents["gd-a"].ring.rows) != transcriptRows || cap(a.agents["gd-a"].ring.rows) != transcriptRows {
		t.Fatalf("the ring keeps %d rows (len %d, cap %d)", len(rows), len(a.agents["gd-a"].ring.rows), cap(a.agents["gd-a"].ring.rows))
	}
	if rows[0].Cap != "Read call 249" || rows[transcriptRows-1].Cap != "Read call 50" || rows[0].At <= rows[1].At {
		t.Errorf("newest first, the oldest 50 gone: %+v ... %+v", rows[0], rows[transcriptRows-1])
	}

	// limit, since and before: page back from the oldest row a page holds.
	pg, _ := a.transcript("gd-a", nil, transcriptQuery{limit: 10})
	page := pg.rows
	if len(page) != 10 || !pg.more || page[0].Cap != "Read call 249" {
		t.Fatalf("limit 10: %d rows, more %v", len(page), pg.more)
	}
	bk, _ := a.transcript("gd-a", nil, transcriptQuery{before: page[9].At, limit: 10})
	if back := bk.rows; len(back) != 10 || back[0].Cap != "Read call 239" {
		t.Errorf("before the oldest row held: %+v", back[0])
	}
	sn, _ := a.transcript("gd-a", nil, transcriptQuery{since: rows[2].At, limit: transcriptRows})
	if len(sn.rows) != 3 || sn.more {
		t.Errorf("since the third newest: %d rows, more %v", len(sn.rows), sn.more)
	}
}

func TestTranscriptRowAtFinish(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})

	// A start writes nothing; its finish writes the row, at the start, with dur and ok.
	start := c.t
	a.observe(preCap("gd-a", "Bash", "t1", "Run the hub tests", c.t), c.t)
	if rows := rowsOf(t, a, "gd-a"); len(rows) != 0 {
		t.Fatalf("a start writes no row: %+v", rows)
	}
	fin := post("gd-a", "Bash", "t1", c.add(5120*time.Millisecond))
	fin.DurMS = 5120
	a.observe(fin, c.t)
	// A failure, and a finish heard without its start.
	a.observe(preCap("gd-a", "Edit", "t2", "Edit web/birds.js", c.add(time.Second)), c.t)
	failed := post("gd-a", "Edit", "t2", c.add(time.Second))
	failed.Event = HookPostToolUseFailure
	a.observe(failed, c.t)
	orphan := post("gd-a", "Read", "t3", c.add(time.Second))
	orphan.DurMS = 2000
	a.observe(orphan, c.t)

	b, _ := json.Marshal(rowsOf(t, a, "gd-a"))
	want := fmt.Sprintf(`[{"at":%d,"class":"read","tool":"Read","dur":2000,"ok":true},`+
		`{"at":%d,"class":"edit","tool":"Edit","cap":"Edit web/birds.js","dur":1000,"ok":false},`+
		`{"at":%d,"class":"shell","tool":"Bash","cap":"Run the hub tests","dur":5120,"ok":true}]`,
		c.t.Unix()-2, start.Unix()+6, start.Unix())
	if string(b) != want {
		t.Fatalf("rows:\n got %s\nwant %s", b, want)
	}

	// A pool shell (no gardener) keeps no transcript; a thought or a subagent is not a row.
	a.observe(pre("gd-pool", "Read", "p1", c.t), c.t)
	a.observe(post("gd-pool", "Read", "p1", c.t), c.t)
	a.observe(HookActivity{Agent: "gd-a", Event: HookThinking, At: c.t}, c.t)
	a.observe(HookActivity{Agent: "gd-a", Event: HookSubagentStart, CallID: "s1", Session: "s", At: c.t}, c.t)
	a.observe(HookActivity{Agent: "gd-a", Event: HookSubagentStop, CallID: "s1", Session: "s", At: c.t}, c.t)
	if _, ok := a.transcript("gd-pool", nil, transcriptQuery{limit: 1}); ok || len(a.agents["gd-pool"].ring.rows) != 0 {
		t.Error("a pool shell has a transcript")
	}
	if n := len(rowsOf(t, a, "gd-a")); n != 3 {
		t.Errorf("a thought or a subagent wrote a row: %d rows", n)
	}

	// A call that never finishes is written when it is let go: dur 0, no ok.
	a.observe(preCap("gd-a", "Bash", "t4", "Wait on CI", c.t), c.t)
	at := c.t.Unix()
	a.flush(c.add(activityOpenForget))
	if r := rowsOf(t, a, "gd-a")[0]; r.Cap != "Wait on CI" || r.Dur != 0 || r.OK != nil || r.At != at {
		t.Errorf("a call let go: %+v", r)
	}

	// A new session clears it; forgetting the agent drops it.
	next := preCap("gd-a", "Read", "n1", "Read x", c.t)
	next.Session = "s2"
	a.observe(next, c.t)
	if n := len(rowsOf(t, a, "gd-a")); n != 0 {
		t.Errorf("a new session keeps %d rows", n)
	}
	a.observe(post("gd-a", "Read", "n1", c.t), c.t)
	a.setGardeners(nil)
	if _, ok := a.transcript("gd-a", nil, transcriptQuery{limit: 1}); ok {
		t.Error("a gardener that left the set is still served")
	}
	a.flush(c.add(activityForget))
	if a.agents["gd-a"] != nil {
		t.Error("the agent is not forgotten")
	}
}

// What a gardener said (ObserveSay, gd-HHGjxt59Mr) is a row at once: class say, no tool, the text scrubbed and clipped to
// sayMax runes as its caption, dur 0 and no ok. It is said while a call is out too, and the activity event carries it at the
// wire cap. A pool shell's and an empty text are nothing.
func TestTranscriptSayRow(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	f.act.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	_, ch, cancel := f.act.subscribe(nil)
	defer cancel()

	f.act.observe(preCap("gd-a", "Bash", "t1", "Run the hub tests", c.t), c.t)
	c.add(activityMinGap) // the start went at once; quiet since, so the say goes at once too
	drain(ch)
	long := strings.Repeat("word ", 70) // 350 runes: past sayMax, under it on the row
	f.ObserveSay("gd-a", "Pushed the fix to /home/agent/bot/gardenio/work/web/birds.js; token sk-ant-PLANTED0000000000000000000000000000. "+long)
	f.ObserveSay("gd-a", "  \n ")
	f.ObserveSay("gd-pool", "nothing kept")
	rows := rowsOf(t, f.act, "gd-a")
	if len(rows) != 1 {
		t.Fatalf("one say row, got %+v", rows)
	}
	r := rows[0]
	if r.Class != DoingSay || r.Tool != "" || r.Dur != 0 || r.OK != nil || r.At != c.t.Unix() {
		t.Errorf("say row: %+v", r)
	}
	if n := utf8.RuneCountInString(r.Cap); n > sayMax || n <= captionMax || !strings.HasPrefix(r.Cap, "Pushed the fix to ") || !strings.HasSuffix(r.Cap, "…") {
		t.Errorf("say cap (%d runes): %q", n, r.Cap)
	}
	if strings.Contains(r.Cap, "PLANTED") || strings.Contains(r.Cap, "/home/agent") {
		t.Errorf("say cap not scrubbed: %q", r.Cap)
	}
	if a := f.act.agents["gd-pool"]; a != nil && len(a.ring.rows) != 0 {
		t.Error("a pool shell kept what it said")
	}
	// The event: say with the cap at the wire cap, a bird (started 1), and the open call still open.
	got := drain(ch)
	if len(got) != 1 || got[0].Doing != DoingSay {
		t.Fatalf("one say event at once: %+v", got)
	}
	ev := got[0]
	if ev.ID != "gd-a" || ev.Tool != "" || ev.Dur != 0 || ev.started() != 1 || utf8.RuneCountInString(ev.Cap) > wireCaptionMax || !strings.HasPrefix(ev.Cap, "Pushed the fix") {
		t.Errorf("say event: %+v", ev)
	}
	if n := len(f.act.agents["gd-a"].open); n != 1 {
		t.Errorf("a say closed the open call: %d open", n)
	}
	// Masked for a scoped viewer, as a caption is.
	f.ObserveSay("gd-a", "Closed gd-HHGjxt59Mr.")
	if got := rowsOf(t, f.act, "gd-a")[0].masked().Cap; got != "Closed a bead." {
		t.Errorf("masked say: %q", got)
	}
}

// A call whose Haiku caption is out waits for it, up to transcriptHaikuWait, before its row is written.
func TestTranscriptWaitsForHaiku(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a"}})
	asked := func(id string) uint64 {
		ev := pre("gd-a", "Bash", id, c.t)
		ev.haiku = true
		return a.observe(ev, c.t)
	}

	// Finished first, then the caption: the row waits for it.
	n := asked("h1")
	a.observe(post("gd-a", "Bash", "h1", c.add(time.Second)), c.t)
	a.flush(c.t)
	if rows := rowsOf(t, a, "gd-a"); len(rows) != 0 {
		t.Fatalf("written before its caption: %+v", rows)
	}
	a.lateCaption("gd-a", n, "Build the viewer image", "h1", c.add(time.Second))
	if rows := rowsOf(t, a, "gd-a"); len(rows) != 1 || rows[0].Cap != "Build the viewer image" || rows[0].Dur != 1000 {
		t.Fatalf("the late caption writes the row: %+v", rows)
	}

	// The caption first (and a newer call started meanwhile): the finish writes it with the caption.
	n = asked("h2")
	a.observe(pre("gd-a", "Read", "r1", c.t), c.t)
	a.lateCaption("gd-a", n, "Trial-merge main", "h2", c.t)
	a.observe(post("gd-a", "Bash", "h2", c.add(time.Second)), c.t)
	if r := rowsOf(t, a, "gd-a")[0]; r.Cap != "Trial-merge main" {
		t.Fatalf("a late caption on a call no longer the newest still lands in the transcript: %+v", r)
	}

	// Haiku never answers: the row is written without a caption once the wait is up.
	asked("h3")
	a.observe(post("gd-a", "Bash", "h3", c.add(time.Second)), c.t)
	a.flush(c.add(transcriptHaikuWait - 2*time.Second))
	if n := len(rowsOf(t, a, "gd-a")); n != 2 {
		t.Fatalf("written before the wait was up: %d rows", n)
	}
	a.flush(c.add(time.Second))
	if rows := rowsOf(t, a, "gd-a"); len(rows) != 3 || rows[0].Cap != "" || rows[0].Tool != "Bash" {
		t.Fatalf("written without a caption after the wait: %+v", rows)
	}

	// A finish heard after the wait is up is written at once.
	asked("h4")
	a.observe(post("gd-a", "Bash", "h4", c.add(transcriptHaikuWait)), c.t)
	if n := len(rowsOf(t, a, "gd-a")); n != 4 {
		t.Errorf("a finish past the wait: %d rows", n)
	}
}

// The cut: a scoped viewer never gets another project's rows, from either endpoint or the live stream, and bead ids are
// masked out of its captions.
func TestTranscriptCut(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	gs := map[string]FarmGardener{
		"gd-a": {ID: "gd-a", Project: "alpha", Task: "gd-t1", Plot: &FarmPlot{Plan: "gd-p1"}},
		"gd-b": {ID: "gd-b", Project: "alpha"},
		"gd-z": {ID: "gd-z", Project: "beta"},
	}
	f.act.setGardeners(gs)
	alpha := func(g FarmGardener) bool { return g.Project == "alpha" }
	h := &Handler{Farm: f, SeesGardener: func(r *http.Request) func(FarmGardener) bool {
		if r.Header.Get("X-Scoped") != "" {
			return alpha
		}
		return nil
	}}
	srv := httptest.NewServer(farmMux(h))
	defer srv.Close()
	get := func(path string, scoped bool) (int, TranscriptDoc) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if scoped {
			req.Header.Set("X-Scoped", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d TranscriptDoc
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, d
	}

	allSID, all, allCancel := f.act.subscribe(nil)
	defer allCancel()
	sid, scoped, cancel := f.act.subscribe(alpha)
	defer cancel()
	for _, s := range []string{sid, allSID} {
		if ok, err := f.SetTranscript(s, "farm"); !ok || err != nil {
			t.Fatalf("SetTranscript: %v %v", ok, err)
		}
	}
	for i, id := range []string{"gd-a", "gd-b", "gd-z"} {
		call := fmt.Sprintf("%s-1", id)
		f.act.observe(preCap(id, "Bash", call, "Close gd-zQx81Lmn2a and gd-a", c.t), c.t)
		f.act.observe(post(id, "Bash", call, c.add(time.Duration(i+1)*time.Second)), c.t)
	}
	f.act.flush(c.add(transcriptEvery))

	rows, _, _ := callRows(t, scoped)
	if len(rows) != 2 || rows[0].ID != "gd-b" || rows[1].ID != "gd-a" || rows[0].Cap != "Close a bead and gd-a" {
		t.Fatalf("the scoped stream: %+v", rows)
	}
	if rows, _, _ := callRows(t, all); len(rows) != 3 || rows[0].ID != "gd-z" || rows[0].Cap != "Close gd-zQx81Lmn2a and gd-a" {
		t.Fatalf("the unrestricted stream: %+v", rows)
	}

	// One gardener: another project's is a 404, as is one that is no gardener; never a 403.
	if s, d := get("/garden/farm/activity/gd-z", false); s != http.StatusOK || len(d.Rows) != 1 || d.ID != "gd-z" {
		t.Fatalf("unrestricted gd-z: %d %+v", s, d)
	}
	if s, _ := get("/garden/farm/activity/gd-z", true); s != http.StatusNotFound {
		t.Errorf("scoped gd-z: %d, want 404", s)
	}
	if s, _ := get("/garden/farm/activity/gd-nobody", false); s != http.StatusNotFound {
		t.Errorf("no such gardener: %d, want 404", s)
	}
	if s, d := get("/garden/farm/activity/gd-a", true); s != http.StatusOK || len(d.Rows) != 1 || d.Rows[0].Cap != "Close a bead and gd-a" || d.Rows[0].ID != "" {
		t.Errorf("scoped gd-a: %d %+v", s, d)
	}

	// A scope: only the cut's gardeners, merged newest first, each row with its id.
	ids := func(d TranscriptDoc) string {
		var out []string
		for _, r := range d.Rows {
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		q      string
		scoped bool
		want   string
	}{
		{"", false, "gd-z,gd-b,gd-a"},
		{"?scope=farm", true, "gd-b,gd-a"},
		{"?scope=project:beta", true, ""},
		{"?scope=project:beta", false, "gd-z"},
		{"?scope=plot:gd-p1", true, "gd-a"},
		{"?scope=plot:gd-t1", false, "gd-a"},
		{"?scope=farm&limit=1", false, "gd-z"},
	} {
		s, d := get("/garden/farm/activity"+tc.q, tc.scoped)
		if s != http.StatusOK || ids(d) != tc.want {
			t.Errorf("%q scoped=%v: %d %q, want %q", tc.q, tc.scoped, s, ids(d), tc.want)
		}
	}
	for _, q := range []string{"?scope=gd-a", "?scope=project:", "?since=x", "?limit=0", "?cursor=x1.2", "?cursor=o1", "?cursor=n-1.2", "?cursor=o1.x"} {
		if s, _ := get("/garden/farm/activity"+q, false); s != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", q, s)
		}
	}

	// A scoped stream that names another project's gardener hears nothing of it.
	f.SetTranscript(sid, "gd-z")
	f.act.observe(preCap("gd-z", "Read", "z2", "Read x", c.t), c.t)
	f.act.observe(post("gd-z", "Read", "z2", c.add(time.Second)), c.t)
	f.act.flush(c.add(transcriptEvery))
	if rows, _, n := callRows(t, scoped); n != 0 {
		t.Errorf("a scoped stream naming another project's gardener: %+v", rows)
	}
}

// Paging by cursor (gd-LEmM2G7iaa): pages cut inside one second that holds rows of several gardeners lose none and
// repeat none, back by next and forward again by prev, at farm scope and for one gardener; since and before still work.
func TestTranscriptCursorPaging(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	a := f.act
	gs := map[string]FarmGardener{"gd-a": {ID: "gd-a"}, "gd-b": {ID: "gd-b"}, "gd-c": {ID: "gd-c"}}
	a.setGardeners(gs)
	n := 0
	write := func(id string) {
		call := fmt.Sprintf("c%d", n)
		a.observe(preCap(id, "Read", call, fmt.Sprintf("Read %d", n), c.t), c.t)
		a.observe(post(id, "Read", call, c.t), c.t)
		n++
	}
	// A second before, a second of 12 rows across the three gardeners, a second after.
	write("gd-a")
	c.add(time.Second)
	for range 4 {
		for _, id := range []string{"gd-b", "gd-a", "gd-c"} {
			write(id)
		}
	}
	c.add(time.Second)
	write("gd-c")

	caps := func(rows []CallRow) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ID+":"+r.Cap)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		name string
		get  func(transcriptQuery) transcriptPage
	}{
		{"farm", func(q transcriptQuery) transcriptPage { return a.transcriptOf(transcriptScope{kind: "farm"}, nil, q) }},
		{"gd-a", func(q transcriptQuery) transcriptPage { p, _ := a.transcript("gd-a", nil, q); return p }},
	} {
		all := tc.get(transcriptQuery{limit: transcriptRows})
		if all.more || all.next != "" || all.prev == "" {
			t.Fatalf("%s: the whole transcript: more %v next %q prev %q", tc.name, all.more, all.next, all.prev)
		}
		for i := 1; i < len(all.rows); i++ {
			if all.rows[i].At > all.rows[i-1].At || (all.rows[i].At == all.rows[i-1].At && all.rows[i].seq > all.rows[i-1].seq) {
				t.Fatalf("%s: not newest first, written later first: %s", tc.name, caps(all.rows))
			}
		}
		for _, limit := range []int{1, 2, 3, 5, 7} {
			// Back by next, from the newest page to the oldest.
			var back []CallRow
			var pages []transcriptPage
			p := tc.get(transcriptQuery{limit: limit})
			for {
				pages = append(pages, p)
				back = append(back, p.rows...)
				if p.next == "" {
					break
				}
				if !p.more {
					t.Fatalf("%s limit %d: next %q without more", tc.name, limit, p.next)
				}
				cur, ok := parseTranscriptCursor(p.next)
				if !ok || len(pages) > len(all.rows) {
					t.Fatalf("%s limit %d: next %q", tc.name, limit, p.next)
				}
				p = tc.get(transcriptQuery{limit: limit, cursor: &cur})
			}
			if caps(back) != caps(all.rows) {
				t.Errorf("%s limit %d: back by next\n got %s\nwant %s", tc.name, limit, caps(back), caps(all.rows))
			}
			// Forward again by prev, from the oldest page: the same rows, then an empty page that keeps its cursor.
			var fwd []CallRow
			p = pages[len(pages)-1]
			for p.prev != "" {
				fwd = append(p.rows, fwd...)
				cur, ok := parseTranscriptCursor(p.prev)
				if !ok || len(fwd) > len(all.rows) {
					t.Fatalf("%s limit %d: prev %q", tc.name, limit, p.prev)
				}
				if q := tc.get(transcriptQuery{limit: limit, cursor: &cur}); len(q.rows) == 0 {
					if q.prev != p.prev || q.more || q.next != "" {
						t.Errorf("%s limit %d: the newest page asked newer: %+v", tc.name, limit, q)
					}
					break
				} else {
					if q.next == "" || len(q.rows) > limit {
						t.Errorf("%s limit %d: a page asked newer has no next: %+v", tc.name, limit, q)
					}
					p = q
				}
			}
			if caps(fwd) != caps(all.rows) {
				t.Errorf("%s limit %d: forward by prev\n got %s\nwant %s", tc.name, limit, caps(fwd), caps(all.rows))
			}
		}
	}

	// A row written after the newest page is what its prev asks for.
	top, _ := a.transcript("gd-b", nil, transcriptQuery{limit: 3})
	cur, _ := parseTranscriptCursor(top.prev)
	write("gd-b")
	if p, _ := a.transcript("gd-b", nil, transcriptQuery{limit: 3, cursor: &cur}); caps(p.rows) != ":Read 14" || p.more {
		t.Errorf("newer than the newest page: %s more %v", caps(p.rows), p.more)
	}

	// before= still pages by the second (and so skips the rest of a second a page cut into); since= still catches up.
	sec := c.t.Unix() - 1
	if p := a.transcriptOf(transcriptScope{kind: "farm"}, nil, transcriptQuery{before: sec + 1, limit: transcriptRows}); len(p.rows) != 13 {
		t.Errorf("before the last second: %d rows", len(p.rows))
	}
	if p := a.transcriptOf(transcriptScope{kind: "farm"}, nil, transcriptQuery{since: sec, limit: transcriptRows}); len(p.rows) != 14 {
		t.Errorf("since the busy second: %d rows", len(p.rows))
	}

	// Over HTTP: next and prev on the doc, cursor accepted back.
	srv := httptest.NewServer(farmMux(&Handler{Farm: f}))
	defer srv.Close()
	get := func(path string) TranscriptDoc {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d TranscriptDoc
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, base := range []string{"/garden/farm/activity?scope=farm&limit=4", "/garden/farm/activity/gd-a?limit=2"} {
		var got []string
		d := get(base)
		for {
			for _, r := range d.Rows {
				got = append(got, r.ID+":"+r.Cap)
			}
			if d.Next == "" {
				break
			}
			if !d.More || d.Prev == "" {
				t.Fatalf("%s: more %v prev %q", base, d.More, d.Prev)
			}
			d = get(base + "&cursor=" + d.Next)
		}
		whole := get(base[:strings.LastIndex(base, "limit=")] + "limit=100")
		var want []string
		for _, r := range whole.Rows {
			want = append(want, r.ID+":"+r.Cap)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: paged\n got %v\nwant %v", base, got, want)
		}
	}
}

// The bytes a subscribed stream costs: 60 gardeners at 4 calls a second, a farm transcript open, is at most 20 rows a
// second in one event a second (about 3 KB/s), the rest counted as skipped. A stream with no transcript open (the farm
// page) gets no call event at all.
func TestTranscriptCallBytes(t *testing.T) {
	const gardeners, callsPerSec, secs = 60, 4, 30
	captions := []string{
		"Show task, claim it, and read team news", "Poll CI status for three refreshed PRs",
		"Trial-merge main into #1282 in a detached worktree", "Find /w/ handling in beads-viewer on main",
		"Show plan ready frontier", "Read web/birds.js", "Run the gardenhub package tests",
		"Check open MRs on the caption branch", "List closed beads for prior art", "Explore gardenhub transcript endpoints",
	}
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	gs := map[string]FarmGardener{}
	var all []string
	for i := range gardeners {
		id := fmt.Sprintf("gd-gardnr%04d", i)
		gs[id] = FarmGardener{ID: id, Project: fmt.Sprintf("proj%d", i%12)}
		all = append(all, id)
	}
	a.setGardeners(gs)
	subSID, sub, cancelSub := a.subscribe(nil)
	defer cancelSub()
	_, page, cancelPage := a.subscribe(nil)
	defer cancelPage()
	oneSID, one, cancelOne := a.subscribe(nil)
	defer cancelOne()
	if ok, _ := a.setTranscript(subSID, "farm"); !ok {
		t.Fatal("setTranscript")
	}
	a.setTranscript(oneSID, all[0])

	var subBytes, pageCallBytes, pageBytes, oneBytes, oneRows, events, rows, skipped int
	count := func() {
		for _, m := range msgs(sub)[CallEvent] {
			subBytes += m.size()
			var fc FarmCalls
			if err := json.Unmarshal(m.Data, &fc); err != nil {
				t.Fatal(err)
			}
			if len(fc.Rows) > transcriptRate {
				t.Fatalf("a call event with %d rows", len(fc.Rows))
			}
			events, rows, skipped = events+1, rows+len(fc.Rows), skipped+fc.Skipped
		}
		for ev, ms := range msgs(page) {
			for _, m := range ms {
				pageBytes += m.size()
				if ev == CallEvent {
					pageCallBytes += m.size()
				}
			}
		}
		for _, m := range msgs(one)[CallEvent] {
			var fc FarmCalls
			_ = json.Unmarshal(m.Data, &fc)
			oneBytes, oneRows = oneBytes+m.size(), oneRows+len(fc.Rows)
		}
	}
	step := 10 * time.Millisecond
	period := time.Second / callsPerSec
	start := c.t
	written := 0
	for tick := 0; c.t.Sub(start) < secs*time.Second; tick++ {
		el := c.t.Sub(start)
		for i, id := range all {
			off := (el + time.Duration(i)*period/gardeners) % period
			n := (el + time.Duration(i)*period/gardeners) / period
			callID := fmt.Sprintf("%s-%d", id, n)
			switch {
			case off < step:
				a.observe(preCap(id, "Bash", callID, captions[(i+int(n))%len(captions)], c.t), c.t)
			case off >= 50*time.Millisecond && off < 50*time.Millisecond+step:
				a.observe(post(id, "Bash", callID, c.t), c.t)
				written++
			}
		}
		if tick%int(activityTick/step) == 0 {
			a.flush(c.t)
		}
		count()
		c.add(step)
	}
	a.flush(c.add(transcriptEvery))
	count()
	if a.dropped != 0 {
		t.Fatalf("%d events dropped: the measurement is short", a.dropped)
	}

	perSec := float64(subBytes) / secs
	t.Logf("subscribed farm transcript: %d events, %d rows, %d skipped of %d written; %.0f B/s (%.0f B/row)", events, rows, skipped, written, perSec, float64(subBytes)/float64(rows))
	if events > secs+1 || rows > (secs+1)*transcriptRate {
		t.Errorf("more than one event (%d) or %d rows a second (%d)", events, transcriptRate, rows)
	}
	if rows+skipped != written {
		t.Errorf("rows %d + skipped %d != written %d: a row went missing unsaid", rows, skipped, written)
	}
	if perSec > 3*1024 {
		t.Errorf("a subscribed stream costs %.0f B/s, want about 3 KB/s", perSec)
	}
	if pageCallBytes != 0 {
		t.Errorf("a stream with no transcript open got %d call bytes", pageCallBytes)
	}
	if oneRows != written/gardeners {
		t.Errorf("one gardener's transcript: %d rows live, want %d", oneRows, written/gardeners)
	}
	if a.trBytes != uint64(subBytes+oneBytes) || a.trClients != 2 {
		t.Errorf("stats: %d bytes, %d clients", a.trBytes, a.trClients)
	}
	t.Logf("the farm page alongside: %.0f B/s, none of it calls", float64(pageBytes)/secs)

	// Closing the transcript stops the rows; closing the stream forgets it was subscribed.
	a.setTranscript(subSID, "")
	a.observe(preCap(all[0], "Read", "x", "Read x", c.t), c.t)
	a.observe(post(all[0], "Read", "x", c.t), c.t)
	a.flush(c.add(transcriptEvery))
	if _, _, n := callRows(t, sub); n != 0 || a.trClients != 1 {
		t.Errorf("a closed transcript: %d events, %d clients", n, a.trClients)
	}
	cancelOne()
	if a.trClients != 0 {
		t.Errorf("a closed stream still counts: %d", a.trClients)
	}
}

// Over HTTP: the view POST opens a transcript without touching the view, and the stream then carries `event: call`.
func TestTranscriptOverHTTP(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	gs := map[string]FarmGardener{"gd-a": {ID: "gd-a", Project: "alpha"}, "gd-b": {ID: "gd-b", Project: "alpha"}}
	f.mu.Lock()
	f.gardeners = gardenerEntries(gs)
	f.mu.Unlock()
	f.act.setGardeners(gs)
	srv := httptest.NewServer(farmMux(&Handler{Farm: f}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/garden/farm/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 256)
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := resp.Body.Read(buf)
			acc += string(buf[:n])
			for {
				i := strings.IndexByte(acc, '\n')
				if i < 0 {
					break
				}
				lines <- acc[:i]
				acc = acc[i+1:]
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	next := func(prefix string) string {
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended waiting for %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-deadline:
				t.Fatalf("no %q line", prefix)
			}
		}
	}
	next("event: hello")
	var hello struct{ SID string }
	if err := json.Unmarshal([]byte(strings.TrimPrefix(next("data: "), "data: ")), &hello); err != nil || hello.SID == "" {
		t.Fatalf("hello: %v %+v", err, hello)
	}
	post := func(body string) int {
		r, err := http.Post(srv.URL+"/garden/farm/events/view", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return r.StatusCode
	}
	if s := post(`{"sid":"` + hello.SID + `","level":2,"ids":["gd-a"]}`); s != http.StatusNoContent {
		t.Fatalf("view: %d", s)
	}
	if s := post(`{"sid":"` + hello.SID + `","transcript":"gd-b"}`); s != http.StatusNoContent {
		t.Fatalf("transcript: %d", s)
	}
	if s := post(`{"sid":"` + hello.SID + `","transcript":"project:"}`); s != http.StatusBadRequest {
		t.Errorf("a bad transcript: %d, want 400", s)
	}
	if s := post(`{"sid":"nope","transcript":"farm"}`); s != http.StatusNotFound {
		t.Errorf("unknown stream: %d, want 404", s)
	}
	f.act.mu.Lock()
	cl := f.act.clients[f.act.sids[hello.SID]]
	view, tr := cl.view, cl.tr
	f.act.mu.Unlock()
	if view == nil || view.busy || !view.ids["gd-a"] || tr == nil || *tr != (transcriptScope{val: "gd-b"}) {
		t.Fatalf("a transcript alone leaves the view: view %+v, transcript %+v", view, tr)
	}

	// gd-b is out of view: no activity event for it, but its row arrives as a call event.
	f.ObserveHook([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-b","session_id":"s","tool_name":"Read","tool_use_id":"b1","tool_input":{"file_path":"/home/agent/work/web/birds.js"}}`))
	c.add(time.Second)
	f.ObserveHook([]byte(`{"event":"PostToolUse","agent_bead_id":"gd-b","session_id":"s","tool_name":"Read","tool_use_id":"b1","duration_ms":12}`))
	f.act.flush(c.add(transcriptEvery))
	next("event: call")
	want := `data: {"rows":[{"id":"gd-b","at":1792870000,"class":"read","tool":"Read","cap":"Read web/birds.js","dur":12,"ok":true}]}`
	if d := next("data: "); d != want {
		t.Errorf("call event:\n got %s\nwant %s", d, want)
	}
	if st := f.Stats(); st.TranscriptRows != 1 || st.TranscriptClients != 1 || st.TranscriptBytes == 0 {
		t.Errorf("stats: %+v", st)
	}

	r, err := http.Get(srv.URL + "/garden/farm/activity/gd-b?since=1792870000&limit=5")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || r.Header.Get("Cache-Control") != "no-store" ||
		string(b) != `{"id":"gd-b","rows":[{"at":1792870000,"class":"read","tool":"Read","cap":"Read web/birds.js","dur":12,"ok":true}],"prev":"n1792870000.1"}`+"\n" {
		t.Errorf("GET /garden/farm/activity/gd-b: %d %s", r.StatusCode, b)
	}
}

// A requester h.MayTranscript refuses (a read-only verifier, gd-TRdxFvBjW2) cannot open a transcript on its stream; it
// may still close one and steer its view.
func TestFarmEventsViewMayTranscript(t *testing.T) {
	f := NewFarm(nil, nil)
	h := &Handler{Farm: f, MayTranscript: func(*http.Request) bool { return false }}
	post := func(body string) int {
		w := httptest.NewRecorder()
		h.farmEventsView(w, httptest.NewRequest(http.MethodPost, "/garden/farm/events/view", strings.NewReader(body)))
		return w.Code
	}
	for _, tr := range []string{"gd-b", "farm", "project:alpha", "plot:gd-t"} {
		if s := post(`{"sid":"nope","transcript":"` + tr + `"}`); s != http.StatusForbidden {
			t.Errorf("open transcript %q: %d, want 403", tr, s)
		}
	}
	if s := post(`{"sid":"nope","transcript":""}`); s != http.StatusNotFound {
		t.Errorf("close the transcript: %d, want 404 (unknown stream, not refused)", s)
	}
	if s := post(`{"sid":"nope","level":2,"ids":["gd-a"]}`); s != http.StatusNotFound {
		t.Errorf("steer the view: %d, want 404 (unknown stream, not refused)", s)
	}
}
