package main

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The herder's verdict on the wish rail (gd-RkkGXAz643): a tag on each open
// wish's /threads row and in its rail on /threads/{id}, in the goat's words.

// goldenHerderPath is the checked-in render of the herded rows and rail.
// Regenerate after an intentional template change with
// `GB_UPDATE_GOLDEN=1 go test ./cmd/beads-viewer/ -run TestThreadsHerder_Golden`.
const goldenHerderPath = "testdata/threads_herder.golden.html"

// seedHerdedWishes is one open wish per verdict, an unjudged one, and a closed
// one whose stale verdict is not shown. Each was checked three hours ago, so
// the humanized time is clock-independent.
func seedHerdedWishes() *fakeDaemon {
	f := newFake()
	now := time.Now()
	checked := now.Add(-3*time.Hour - 5*time.Minute).UTC().Format(time.RFC3339)
	f.beads = map[string]*beadsapi.BeadDetail{}
	add := func(id, title, status string, ago time.Duration, fields map[string]string) {
		f.beads[id] = &beadsapi.BeadDetail{
			ID: id, Title: title, Status: status, Type: "task",
			Labels: []string{threadHeadLabel, "project:gardenio"}, Fields: fields,
			UpdatedAt: now.Add(-ago), CreatedAt: now.Add(-48 * time.Hour),
		}
	}
	add("gd-hFul", "Ship the pen", "open", 1*time.Minute, map[string]string{"herder_verdict": "fulfilled", "herder_checked_at": checked})
	add("gd-hWoke", "Fix the gate", "open", 2*time.Minute, map[string]string{"herder_verdict": "unfulfilled", "herder_checked_at": checked, "herder_woke_at": checked})
	add("gd-hAsk", "Something about goats", "open", 3*time.Minute, map[string]string{"herder_verdict": "unclear", "herder_checked_at": checked, "herder_woke_at": checked})
	add("gd-hProg", "Paint the barn", "in_progress", 4*time.Minute, map[string]string{"herder_verdict": "in-progress", "herder_checked_at": checked})
	add("gd-hNew", "Count the goats", "open", 5*time.Minute, map[string]string{})
	add("gd-hDone", "Mend the fence", "closed", 6*time.Minute, map[string]string{"herder_verdict": "fulfilled", "herder_checked_at": checked, "close_reason": "herder-fulfilled"})
	return f
}

var (
	herderMetaRe = regexp.MustCompile(`(?s)<div class="threads-card-meta">.*?</div>`)
	herderRailRe = regexp.MustCompile(`(?s)<aside class="thread2-rail".*?</aside>`)
	spaceRe      = regexp.MustCompile(`\s+`)
)

// herderRender is each /threads row's meta line and the rail of three wishes,
// whitespace folded, one per line.
func herderRender(t *testing.T) string {
	t.Helper()
	mux := mkServerFromFake(t, seedHerdedWishes())
	var out []string
	body, code := getBody(t, mux, "/threads")
	if code != http.StatusOK {
		t.Fatalf("GET /threads = %d", code)
	}
	for _, m := range herderMetaRe.FindAllString(body, -1) {
		out = append(out, spaceRe.ReplaceAllString(m, " "))
	}
	for _, id := range []string{"gd-hFul", "gd-hWoke", "gd-hNew"} {
		body, code := getBody(t, mux, "/threads/"+id)
		if code != http.StatusOK {
			t.Fatalf("GET /threads/%s = %d", id, code)
		}
		rail := herderRailRe.FindString(body)
		if rail == "" {
			t.Fatalf("/threads/%s has no rail", id)
		}
		out = append(out, spaceRe.ReplaceAllString(rail, " "))
	}
	return strings.Join(out, "\n") + "\n"
}

// TestThreadsHerder_Golden pins the herder's tag on the /threads rows (newest
// activity first: fulfilled, unfulfilled and woken, unclear and asked, in
// progress, none, and a closed wish with only its fulfilled chip) and in the
// rail of a fulfilled, a woken and an unjudged wish.
func TestThreadsHerder_Golden(t *testing.T) {
	got := herderRender(t)
	if os.Getenv("GB_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenHerderPath, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("updated golden %s (%d bytes)", goldenHerderPath, len(got))
		return
	}
	want, err := os.ReadFile(goldenHerderPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with GB_UPDATE_GOLDEN=1)", goldenHerderPath, err)
	}
	if got != string(want) {
		t.Fatalf("herder render drifted from %s (regenerate with GB_UPDATE_GOLDEN=1 if intended)\n--- got ---\n%s\n--- want ---\n%s", goldenHerderPath, got, want)
	}
	for _, s := range []string{
		`herder herder-done">herder: fulfilled, closing</span>`,
		`herder herder-ask" title="the herder checked it 3 hours ago">herder: unfulfilled, agent woken</span>`,
		`herder herder-ask" title="the herder checked it 3 hours ago">herder: unclear, asked</span>`,
		`herder herder-plain" title="the herder checked it 3 hours ago">herder: in progress</span>`,
		`<span class="k">Herder</span><span>checked 3 hours ago</span>`,
	} {
		if !strings.Contains(got, s) {
			t.Errorf("render lacks %q", s)
		}
	}
	if n := strings.Count(got, "herder: "); n != 6 {
		t.Errorf("want 6 herder tags (4 rows, 2 rails; none on the unjudged or the closed wish), got %d", n)
	}
}

// TestThreadsHerder_BothThemes pins each tone's colour in the light skin and
// again under the dark skins, on both pages, so neither theme falls back to
// the plain chip's ink.
func TestThreadsHerder_BothThemes(t *testing.T) {
	for page, chip := range map[string]string{"templates/threads.html": ".threads-chip", "templates/thread_detail.html": ".thread2-chip"} {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		css := string(b)
		for _, tone := range []string{"herder-done", "herder-ask"} {
			light := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(chip+"."+tone) + ` \{[^}]*color: #`)
			dark := regexp.MustCompile(`(?m)^\s*html\[data-skin\^="dark"\] ` + regexp.QuoteMeta(chip+"."+tone) + ` \{ color: #`)
			if !light.MatchString(css) || !dark.MatchString(css) {
				t.Errorf("%s: %s.%s needs a light and a dark colour", page, chip, tone)
			}
		}
	}
}
