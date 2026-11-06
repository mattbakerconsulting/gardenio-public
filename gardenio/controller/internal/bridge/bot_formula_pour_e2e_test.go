package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// TestE2E_FormulaPour_SlackSlash_MoleculeHasDepToThreadBead drives the full
// `/formula pour` Slack slash command path from a thread context through to
// instantiateFormulaSteps + AddDependency, and asserts:
//
//  1. The molecule bead has a `triggered-by` dep to the canonical Slack
//     thread bead (D3 contract, auto-link epic ka-0000000933 / formula-pour
//     child epic ka-0000007507 D7).
//  2. Every step bead has a `parent-child` dep to the molecule.
//  3. Step beads do NOT carry a duplicate `triggered-by` to the thread bead
//     — lineage is transitive through molecule (D5 single-parent decision).
//
// Plan gd-T5vMf08jDN §6.1.
//
// Distinct from TestHandleFormulaPour_FromThreadAddsTriggerBeadDep
// (unit-level companion in bot_formula_pour_test.go): this e2e variant
// pins the full ingestion-path lineage shape (D3 + D5 together), and
// lives alongside the gitlab/webhook/jira/slack-mention e2e suites for
// discoverability of the auto-link contract per ingestion path.
func TestE2E_FormulaPour_SlackSlash_MoleculeHasDepToThreadBead(t *testing.T) {
	const (
		channel  = "C-pour-1"
		threadTS = "1779.pour"
	)

	daemon := newFormulaMockDaemon()
	daemon.seedProjectWithChannel("gardenio", channel)

	seedFormula(daemon.mockDaemon, "gd-bug-fix-mr", "Bug Fix MR Quality Gates",
		nil,
		[]formulaStep{
			{ID: "step1", Title: "Reproduce the bug"},
			{ID: "step2", Title: "Write failing test"},
			{ID: "step3", Title: "Land the fix"},
		},
	)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	ctx := WithSlashThreadTS(context.Background(), threadTS)
	bot.handleFormulaPour(ctx,
		slack.SlashCommand{ChannelID: channel, UserID: "U0OPERATOR1"},
		[]string{"gd-bug-fix-mr"}, false)

	// The canonical Slack thread bead is also created as type=task (see
	// getOrCreateThreadBead in thread_bead.go), so step beads must be
	// distinguished from the thread bead by absence of the canonical
	// slack-thread:<chan>:<ts> label.
	threadLabel := threadBeadLabel(channel, threadTS)
	hasThreadLabel := func(b *beadsapi.BeadDetail) bool {
		for _, l := range b.Labels {
			if l == threadLabel {
				return true
			}
		}
		return false
	}

	// Wait for the async pour goroutine to land the molecule + steps.
	assertEventually(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		var hasMol bool
		var stepCount int
		for _, b := range daemon.beads {
			switch b.Type {
			case "molecule":
				hasMol = true
			case "task":
				if !hasThreadLabel(b) {
					stepCount++
				}
			}
		}
		return hasMol && stepCount == 3
	}, "expected 1 molecule + 3 step task beads")

	// Locate the thread, molecule, and step beads.
	var (
		threadBead *beadsapi.BeadDetail
		molBead    *beadsapi.BeadDetail
		stepIDs    []string
	)
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if hasThreadLabel(b) {
			threadBead = b
			continue
		}
		switch b.Type {
		case "molecule":
			molBead = b
		case "task":
			stepIDs = append(stepIDs, b.ID)
		}
	}
	daemon.mu.Unlock()

	if threadBead == nil {
		t.Fatal("expected canonical thread bead with label slack-thread:<chan>:<ts>")
	}
	if molBead == nil {
		t.Fatal("expected molecule bead")
	}
	if len(stepIDs) != 3 {
		t.Fatalf("expected 3 step beads, got %d", len(stepIDs))
	}

	deps := daemon.getDeps()

	// (1) molecule → triggered-by → thread bead.
	var sawMolTriggeredBy bool
	for _, d := range deps {
		if d.BeadID == molBead.ID && d.DependsOnID == threadBead.ID && d.DepType == "triggered-by" {
			sawMolTriggeredBy = true
			if d.CreatedBy != "slack-bridge" {
				t.Errorf("molecule triggered-by CreatedBy = %q, want slack-bridge", d.CreatedBy)
			}
		}
	}
	if !sawMolTriggeredBy {
		t.Errorf("missing triggered-by dep molecule=%s → thread=%s (deps=%+v)",
			molBead.ID, threadBead.ID, deps)
	}

	// (2) each step → parent-child → molecule.
	stepHasParent := make(map[string]bool, len(stepIDs))
	for _, d := range deps {
		if d.DepType == "parent-child" && d.DependsOnID == molBead.ID {
			stepHasParent[d.BeadID] = true
		}
	}
	for _, sid := range stepIDs {
		if !stepHasParent[sid] {
			t.Errorf("step %s missing parent-child dep to molecule %s", sid, molBead.ID)
		}
	}

	// (3) D5 single-parent: no step double-parents to the thread bead.
	for _, d := range deps {
		for _, sid := range stepIDs {
			if d.BeadID == sid && d.DependsOnID == threadBead.ID && d.DepType == "triggered-by" {
				t.Errorf("step %s must not double-parent to thread bead via triggered-by (lineage is transitive through molecule)", sid)
			}
		}
	}
}
