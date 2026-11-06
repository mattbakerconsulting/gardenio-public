package commons

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/image/font/sfnt"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
	"gardenio/controller/internal/heraldry"
)

const testArms = "blue 🦊 . 🔥 . 📦"

// TestArmsInCardHeader: a card whose subject bears arms shows the picture as
// inline SVG in its header, named in plain words, and never the emoji run.
func TestArmsInCardHeader(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	const name = `aria-label="arms: fox, fire, package"`

	// An epic's card has no face: the picture goes before its link.
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent1", Type: "epic", Title: "Claimlayer plan", Status: "open",
		Labels: []string{"project:claimlayer"}, Heraldry: testArms})
	_, body := get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, `<p class="cm-card"><span class="cm-arms"><svg `, name)
	mustNotContain(t, body, "🦊", "🔥", "📦")

	// A face carries it at the head of its header.
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent1", Type: "agent", Title: "claimlayer-crew-1", Status: "open",
		Labels: []string{"project:claimlayer"}, Heraldry: testArms, Fields: map[string]string{"agent": "claimlayer-crew-1", "agent_state": "working"}})
	_, body = get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, `<span class="cm-arms"><svg `, name)
	if i, j := strings.Index(body, `class="cm-arms"`), strings.Index(body, `class="cm-acard-task"`); i < 0 || j < i {
		t.Fatalf("the picture is not before the agent card's headline:\n%s", body)
	}
	d.Beads["cl-dec1"].Heraldry = testArms
	_, body = get(t, h, "/test/m/cl-dcard1", "claimlayer")
	mustContain(t, body, `<p class="cm-dcard-head"><span class="cm-arms"><svg `)

	// No arms, or a value the renderer does not read: no picture, and the
	// card is otherwise the same.
	for _, v := range []string{"", "teal 🦊 . . . .", "blue 🦄🦄 . . . ."} {
		d.Beads["cl-agent1"].Heraldry = v
		_, body = get(t, h, "/test/m/cl-card1", "claimlayer")
		mustContain(t, body, "cm-acard")
		mustNotContain(t, body, "cm-arms", "<svg xmlns")
	}
}

// TestArmsStayInTheirLand: a faceless card's subject in another land shows
// neither its picture nor anything of it.
func TestArmsStayInTheirLand(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent1", Type: "epic", Title: "gardenio secret plan", Status: "open",
		Labels: []string{"project:gardenio"}, Heraldry: testArms})
	_, body := get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, `href="/base/garden?bead=cl-agent1" data-bead="cl-agent1" title="cl-agent1: not found" data-titles="off">Bead not found <span class="cm-bead-id">cl-agent1</span></a>`)
	mustNotContain(t, body, "cm-arms", "arms:", "secret")
}

// TestArmsDrawNoTextButTheirName guards the font tests' contract for the
// picture (heraldry spec §6.6): the SVG writes no text a font renders but its
// <title>, the plain-words name, and every name the library can make is in
// the Noto Sans subsets the goldens pin, so a tooltip or a reader that shows
// the title never falls back to another face.
func TestArmsDrawNoTextButTheirName(t *testing.T) {
	var faces []*sfnt.Font
	for _, f := range []string{"NotoSans-Regular", "NotoSans-Bold"} {
		b, err := os.ReadFile(filepath.Join("..", "gardenhub", "testdata", "golden", "fonts", f+".ttf"))
		if err != nil {
			t.Fatal(err)
		}
		face, err := sfnt.Parse(b)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		faces = append(faces, face)
	}
	svg := string(armsSVG(testArms))
	title := regexp.MustCompile(`<title>[^<]*</title>`)
	if strings.Count(svg, "<title>") != 1 {
		t.Fatalf("want one <title> in %s", svg)
	}
	if s := strings.TrimSpace(stripTags(title.ReplaceAllString(svg, ""))); s != "" {
		t.Fatalf("the picture draws text outside its title: %q", s)
	}

	var buf sfnt.Buffer
	texts := map[string]bool{"arms: , ": true}
	for _, c := range heraldry.Library() {
		texts[c.Name] = true
	}
	for text := range texts {
		for _, r := range text {
			for _, face := range faces {
				if g, err := face.GlyphIndex(&buf, r); err != nil || g == 0 {
					t.Errorf("%q: %q (U+%04X) is not in the pinned fonts", text, r, r)
				}
			}
		}
	}
}

// TestAgentCardWearsItsEpicsArms: an agent bears no arms of its own (heraldry
// spec R6), so its card shows the arms of the epic its task hangs from, when
// that task and that epic are in the card's land; its own arms still win.
func TestAgentCardWearsItsEpicsArms(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d, func(h *Handler) { h.Nodes = d })
	const name = `aria-label="arms: fox, fire, package"`
	d.Put(&beadsapi.BeadDetail{ID: "cl-epic1", Type: "epic", Title: "Claimlayer plan", Status: "open",
		Labels: []string{"project:claimlayer"}, Heraldry: testArms})
	_, body := get(t, h, "/test/m/cl-card1", "claimlayer")
	mustNotContain(t, body, "cm-arms") // its task hangs from nothing yet

	d.Link("cl-task1", "cl-epic1", "parent-child")
	_, body = get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, `<span class="cm-arms"><svg `, name, "Claimlayer member task")

	d.Beads["cl-agent1"].Heraldry = "blue 🧭 . 🔥 . 📦"
	_, body = get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, "cm-arms")
	mustNotContain(t, body, name)
	d.Beads["cl-agent1"].Heraldry = ""

	// Another land's epic, or a task in another land, shows no arms.
	d.Beads["cl-epic1"].Labels = []string{"project:gardenio"}
	_, body = get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, "cm-acard")
	mustNotContain(t, body, "cm-arms", "arms:")
	d.Beads["cl-epic1"].Labels = []string{"project:claimlayer"}
	d.Beads["cl-task1"].Labels = []string{"project:gardenio"}
	_, body = get(t, h, "/test/m/cl-card1", "claimlayer")
	mustContain(t, body, "cm-acard")
	mustNotContain(t, body, "cm-arms", "arms:", "Claimlayer member task")
}

// TestEpicArmsReadOncePerFragment: the wrap-ups of one plan cost one read of
// its epic, and two cards of one agent one read of its task.
func TestEpicArmsReadOncePerFragment(t *testing.T) {
	d := commonstest.TwoLands(t0)
	cl := []string{"project:claimlayer"}
	d.Put(&beadsapi.BeadDetail{ID: "cl-epic1", Type: "epic", Title: "Claimlayer plan", Status: "open", Labels: cl, Heraldry: testArms})
	var rows []row
	for i, task := range []string{"cl-t1", "cl-t2", "cl-t3", "cl-t1"} {
		agent := "cl-a" + task[len(task)-1:]
		d.Put(&beadsapi.BeadDetail{ID: task, Type: "task", Title: "Step " + task, Status: "open", Labels: cl})
		d.Put(&beadsapi.BeadDetail{ID: agent, Type: "agent", Title: agent, Status: "closed", Labels: cl,
			Fields: map[string]string{"agent": agent, "agent_state": "done", "task_id": task, "wrapup": `{"tldr":"done"}`}})
		if i < 3 {
			d.Link(task, "cl-epic1", "parent-child")
		}
		rows = append(rows, row{M: Message{ID: "cl-c" + string(rune('0'+i)), Land: "claimlayer", Kind: kindCard, SubjectID: agent}})
	}
	reads := &readCounter{Reader: d, n: map[string]int{}}
	h := &Handler{Beads: reads, Nodes: d, Now: func() time.Time { return t0 }}
	h.withCards(context.Background(), "", rows)
	for i, r := range rows {
		if !strings.Contains(string(r.Arms), "<svg") {
			t.Errorf("card %d has no arms", i)
		}
	}
	if reads.n["cl-epic1"] != 1 || reads.n["cl-t1"] != 1 {
		t.Errorf("reads %v: want the epic and each task read once", reads.n)
	}
}

type readCounter struct {
	Reader
	mu sync.Mutex
	n  map[string]int
}

func (c *readCounter) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	c.mu.Lock()
	c.n[id]++
	c.mu.Unlock()
	return c.Reader.GetBead(ctx, id)
}
