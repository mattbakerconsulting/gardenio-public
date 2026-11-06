package bridge

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"
	"gardenio/controller/internal/decisionview/decisiontest"
)

var updateDecisionAcceptanceGolden = flag.Bool("update-decision-acceptance", false,
	"rewrite testdata/decision_acceptance_calls.golden.json from the acceptance decision's card")

// TestDecisionAcceptanceCardGolden posts the acceptance decision (decisiontest,
// epic gd-hmwSI9lwKe step 6, gd-DiU8SB6TAM: the Gate's plot application, five
// options, --evidence naming a task with two `gb image upload` links) as a
// Slack card and pins its Web API calls. Slack fetches image blocks
// anonymously and the viewer's /image links are private, so the card carries
// the evidence as one line under the context naming the task and its count,
// never a picture. Regenerate with:
// go test ./internal/bridge -run TestDecisionAcceptanceCardGolden -update-decision-acceptance
func TestDecisionAcceptanceCardGolden(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newDecisionCardsBot(t, twin)
	const id, ev = "gd-GateDec001", "gd-GateEvid01"
	dec := decisiontest.Decision(id, ev)
	dec.Assignee = "crew-x"
	dec.Fields["requesting_agent_bead_id"] = "gd-agent-x"
	daemon.beads[id] = dec
	daemon.beads[ev] = decisiontest.Evidence(ev)
	daemon.beadComments = map[string][]beadsapi.Comment{ev: decisiontest.Comments(time.Unix(1_793_000_000, 0))}

	if err := b.NotifyDecision(context.Background(), BeadEvent{ID: id, Type: "decision", Title: dec.Title, Status: "open",
		Priority: dec.Priority, Assignee: dec.Assignee, Labels: dec.Labels, Fields: dec.Fields}); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	calls := twin.Calls()
	if len(calls) != 1 || calls[0].Method != "chat.postMessage" {
		t.Fatalf("want one chat.postMessage, got %+v", calls)
	}
	card := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(calls[0].Params["blocks"]) // Block Kit JSON escapes < and >
	ctxAt := strings.Index(card, "Dismissing this files it again: deny instead.")
	lineAt := strings.Index(card, "2 screenshots on <https://beads.test/"+ev+"|"+decisiontest.EvidenceTitle+">")
	if ctxAt < 0 || lineAt < ctxAt {
		t.Errorf("the evidence line follows the whole context (context %d, line %d):\n%s", ctxAt, lineAt, card)
	}
	for i, s := range decisiontest.OptionShorts {
		if !strings.Contains(card, "*"+strconv.Itoa(i+1)+". "+s+"*") || !strings.Contains(card, decisiontest.OptionDescriptions[i]) {
			t.Errorf("the card has no option %d %q with its description", i+1, s)
		}
	}
	if !strings.Contains(card, `"action_id":"resolve_other_`+id+`"`) {
		t.Error("the card offers Other beside the five options")
	}
	if strings.Contains(card, `"type":"image"`) || strings.Contains(card, "/image/") || strings.Contains(card, "beads.example") {
		t.Errorf("no image block and no private /image link: Slack fetches images anonymously")
	}

	got, err := json.MarshalIndent(calls, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "decision_acceptance_calls.golden.json")
	if *updateDecisionAcceptanceGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (rerun with -update-decision-acceptance)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the acceptance decision's Slack card drifted from %s (rerun with -update-decision-acceptance only if the change is intended):\n%s", path, got)
	}
}
