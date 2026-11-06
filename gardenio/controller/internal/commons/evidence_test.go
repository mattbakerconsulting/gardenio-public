package commons

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
	"gardenio/controller/internal/decisionview"
)

// evidenceDaemon is the fixture daemon with the comment reads evidence needs.
type evidenceDaemon struct {
	*commonstest.Daemon
	comments map[string][]beadsapi.Comment
}

func (e evidenceDaemon) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	return e.comments[id], nil
}

// TestDecisionCardShowsEvidence: an open decision card, and its lightbox,
// show the screenshots on the task it is about under the context
// (gd-Pi7C83262N), and never a picture from a bead of another land.
func TestDecisionCardShowsEvidence(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "cl-EvTask01", Type: "task", Title: "Grade the chart", Status: "in_progress", Labels: []string{"project:claimlayer"}})
	d.Put(&beadsapi.BeadDetail{ID: "gd-EvTask02", Type: "task", Title: "Gardenio secret task", Status: "in_progress", Labels: []string{"project:gardenio"}})
	d.Beads["cl-dec1"].Fields["requesting_task"] = "cl-EvTask01"
	d.Beads["cl-dec1"].Fields["evidence"] = "gd-EvTask02"
	e := evidenceDaemon{d, map[string][]beadsapi.Comment{
		"cl-EvTask01": {{Text: "![chart, phone](https://v.example/image/ui/chart-phone.png)"}},
		"gd-EvTask02": {{Text: "https://v.example/image/ui/secret.png"}},
	}}
	h := commonsServer(t, d, func(h *Handler) { h.Beads, h.Evidence = e, decisionview.NewEvidenceCache(0) })

	_, body := get(t, h, "/test/m/cl-dcard1", "claimlayer")
	mustContain(t, body, `class="dec-ev"`, `src="/base/image/ui/chart-phone.png"`, `alt="chart, phone"`,
		`from <a href="/base/garden?bead=cl-EvTask01">Grade the chart</a>`, `id="dec-ev-cm-cl-dcard1-cl-dec1-0"`)
	mustNotContain(t, body, "secret")

	// An admin's bearer reads every land; the card still borrows only from its own.
	_, body = get(t, h, "/test/m/cl-dcard1", "")
	mustContain(t, body, `src="/base/image/ui/chart-phone.png"`)
	mustNotContain(t, body, "secret")

	_, body = get(t, h, "/garden/c/m/cl-dcard1/lightbox/other", "claimlayer")
	mustContain(t, body, `src="/base/image/ui/chart-phone.png"`, `id="dec-ev-lb-cl-dec1-0"`)

	// A closed decision is answered already: no strip.
	d.Beads["cl-dec1"].Status = "closed"
	_, body = get(t, h, "/test/m/cl-dcard1", "claimlayer")
	mustNotContain(t, body, "dec-ev")
}
