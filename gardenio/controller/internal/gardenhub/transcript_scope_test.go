package gardenhub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// The plot and farm transcript panel (gd-X5dM2VRgDd, plan gd-jMl4oF2cJO step 9; bird captions spec 11.5) as web/transcript.js
// reads it: its first screen is one GET of the scope route, at most transcriptScopeRows rows merged newest first, and Show older
// walks back by the answer's next cursor. TestTranscriptCallBytes holds the live half to 3 KB/s.

// scopeGet is one request of the scope route, as the panel makes it.
func scopeGet(t *testing.T, srv *httptest.Server, scope, cursor string) (TranscriptDoc, int) {
	t.Helper()
	q := url.Values{"scope": {scope}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	r, err := http.Get(srv.URL + "/garden/farm/activity?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("scope %s: %d %s", scope, r.StatusCode, b)
	}
	var d TranscriptDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d, len(b)
}

// newestFirst fails when rows are not merged newest first by start.
func newestFirst(t *testing.T, what string, rows []CallRow) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		if rows[i].At > rows[i-1].At {
			t.Fatalf("%s: row %d (%s at %d) is newer than row %d (%s at %d)", what, i, rows[i].ID, rows[i].At, i-1, rows[i-1].ID, rows[i-1].At)
		}
	}
}

func TestTranscriptFarmPanel(t *testing.T) {
	const gardeners, callsPerSec, secs = 60, 4, 60
	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	gs := map[string]FarmGardener{}
	var all []string
	for i := range gardeners {
		id := fmt.Sprintf("gd-gardnr%04d", i)
		gs[id] = FarmGardener{ID: id, Project: fmt.Sprintf("proj%d", i%12), Plot: &FarmPlot{Plan: fmt.Sprintf("gd-plan%d", i%6)}}
		all = append(all, id)
	}
	// A quiet plot: three gardeners, a call every 10 s each for ten minutes, the farm's pace (about 14k calls a day).
	quiet := []string{"gd-quiet0", "gd-quiet1", "gd-quiet2"}
	for _, id := range quiet {
		gs[id] = FarmGardener{ID: id, Project: "calm", Plot: &FarmPlot{Plan: "gd-calm"}}
	}
	f.mu.Lock()
	f.gardeners = gardenerEntries(gs)
	f.mu.Unlock()
	f.act.setGardeners(gs)
	srv := httptest.NewServer(farmMux(&Handler{Farm: f}))
	defer srv.Close()

	quietCall := func(tag string) {
		for j, id := range quiet {
			callID := fmt.Sprintf("%s-%s", id, tag)
			at := c.t.Add(time.Duration(j) * time.Second)
			f.act.observe(preCap(id, "Read", callID, "Read web/transcript.js", at), at)
			f.act.observe(post(id, "Read", callID, at.Add(20*time.Millisecond)), at.Add(20*time.Millisecond))
		}
	}
	// The quiet plot's first nine minutes, then the busy minute (60 gardeners at 4 calls a second) with its last.
	for s := 0; s < 600-secs; s += 10 {
		quietCall(fmt.Sprint("q", s))
		c.add(10 * time.Second)
	}
	written := 0
	period := time.Second / callsPerSec
	for tick := 0; tick < secs*callsPerSec; tick++ {
		for i, id := range all {
			callID := fmt.Sprintf("%s-%d", id, tick)
			at := c.t.Add(time.Duration(i) * time.Millisecond)
			f.act.observe(preCap(id, "Bash", callID, fmt.Sprintf("Run step %d of %s", tick, id), c.t), c.t)
			f.act.observe(post(id, "Bash", callID, at), at)
			written++
		}
		if tick%(10*callsPerSec) == 0 {
			quietCall(fmt.Sprint("b", tick))
		}
		c.add(period)
	}

	// The farm's first screen: one request, 100 rows of many gardeners, newest first, about 150 B a row.
	d, n := scopeGet(t, srv, "farm", "")
	if len(d.Rows) != transcriptScopeRows || !d.More || d.Next == "" || d.Prev == "" {
		t.Fatalf("farm first screen: %d rows, more %v, next %q, prev %q; want %d rows and both cursors", len(d.Rows), d.More, d.Next, d.Prev, transcriptScopeRows)
	}
	newestFirst(t, "farm first screen", d.Rows)
	who := map[string]bool{}
	for _, r := range d.Rows {
		if r.ID == "" {
			t.Fatalf("a scope row without its gardener: %+v", r)
		}
		who[r.ID] = true
	}
	if n > transcriptScopeRows*160 {
		t.Errorf("the farm's first screen is %d bytes (%d B a row), want at most 160 B a row", n, n/len(d.Rows))
	}
	t.Logf("farm first screen: %d rows from %d gardeners, %d bytes; %d calls written", len(d.Rows), len(who), n, written)

	// Show older: the next page is the 100 rows just older, none repeated, the merge order kept across the cut.
	older, _ := scopeGet(t, srv, "farm", d.Next)
	if len(older.Rows) != transcriptScopeRows {
		t.Fatalf("farm's second page: %d rows", len(older.Rows))
	}
	both := append(append([]CallRow{}, d.Rows...), older.Rows...)
	newestFirst(t, "farm, two pages", both)
	seen := map[string]bool{}
	for _, r := range both {
		k := fmt.Sprintf("%s|%d|%s", r.ID, r.At, r.Cap)
		if seen[k] {
			t.Fatalf("a row on both pages: %s", k)
		}
		seen[k] = true
	}

	// The quiet plot: what happened on it in the last ten minutes is its first screen and one Show older, every row once,
	// its own gardeners' only.
	var plot []CallRow
	cur, pages := "", 0
	for {
		p, _ := scopeGet(t, srv, "plot:gd-calm", cur)
		plot, pages = append(plot, p.Rows...), pages+1
		if p.Next == "" || pages > 5 {
			break
		}
		cur = p.Next
	}
	newestFirst(t, "the quiet plot", plot)
	want := len(quiet) * 60
	if len(plot) != want || pages != 2 {
		t.Errorf("the quiet plot's ten minutes: %d rows in %d pages, want %d in 2", len(plot), pages, want)
	}
	span := plot[0].At - plot[len(plot)-1].At
	for _, r := range plot {
		if r.ID != quiet[0] && r.ID != quiet[1] && r.ID != quiet[2] {
			t.Fatalf("a row of %s on the quiet plot", r.ID)
		}
	}
	if span < 590 {
		t.Errorf("the quiet plot's rows span %d s, want its ten minutes", span)
	}

	// A project's: only its gardeners.
	pj, _ := scopeGet(t, srv, "project:proj3", "")
	for _, r := range pj.Rows {
		if gs[r.ID].Project != "proj3" {
			t.Fatalf("project:proj3 carries a row of %s (%s)", r.ID, gs[r.ID].Project)
		}
	}
}
