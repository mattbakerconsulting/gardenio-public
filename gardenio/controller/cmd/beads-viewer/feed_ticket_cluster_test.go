package main

import (
	"testing"
	"time"
)

// feed_ticket_cluster_test.go — ka-0000001658. ONE upstream tracker ticket
// (DEVOPS-775) fans out into N distinct task beads filed by N DIFFERENT monorepo
// gardeners. None share a bead id, none share an author, and they are all the
// same TYPE, so neither collapseGlobalDuplicates / collapseBeadLifecycle (key on
// bead id) nor collapseSquawkClusters (key on author+topic) fold them.
// collapseExternalTicketClusters keys on the normalized ticket token parsed from
// each title and folds the fan-out to one evolving "×N" card. Different tickets
// and non-ticket cards stay distinct.

func TestFeedExternalTicketRef(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"colon form", "DEVOPS-775: Read deployed ACME version from telemetry/UI not Terraform HCL", "DEVOPS-775"},
		{"bracket form", "[DEVOPS-775] scan-vm: read deployed ACME version from telemetry/UI not Terraform HCL", "DEVOPS-775"},
		{"PE project", "PE-1422 wire up the new dashboard", "PE-1422"},
		{"lowercase project is not a ticket", "devops-775 lowercase prose", ""},
		{"bare kd bead id is not a ticket", "crew reporting in (ka-0000003233)", ""},
		{"single-letter project is not a ticket", "X-1 stub", ""},
		{"no ticket", "release-owner: cut 2026.169.11, merged MR !4625", ""},
		{"first ticket wins", "DEVOPS-775 relates to DEVOPS-786", "DEVOPS-775"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedExternalTicketRef(c.in); got != c.want {
				t.Errorf("feedExternalTicketRef(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestCollapseExternalTicketClusters_Devops775 uses the VERBATIM titles + ids +
// authors of the five DEVOPS-775 beads from the live noise measurement
// (ka-0000001658): ka-0000003233, ka-0000007737, ka-0000004031, ka-0000007828,
// ka-0000000620. They MUST fold to a single card with count 5, keeping the
// newest as exemplar.
func TestCollapseExternalTicketClusters_Devops775(t *testing.T) {
	t0 := mustTime(t, "2026-06-19T05:00:00Z")
	task := func(id, author, title string, ts time.Time) feedCardItem {
		return feedCardItem{
			kind:  feedKindCreated,
			bead:  &feedEventBead{ID: id, Type: "task", Title: title, Status: "open", CreatedBy: author},
			ts:    ts,
			count: 1,
		}
	}

	// Verbatim from the five live beads — different ids, different gardeners
	// (monorepo-gardener-for-gd-feajz3ctyu-<pod>), all naming DEVOPS-775.
	in := []feedCardItem{
		task("ka-0000003233", "monorepo-gardener-for-gd-feajz3ctyu-eagf",
			"DEVOPS-775: Read deployed ACME version from telemetry/UI not Terraform HCL", t0),
		task("ka-0000007737", "monorepo-gardener-for-gd-feajz3ctyu-5sf2",
			"[DEVOPS-775] scan-vm: read deployed ACME version from telemetry/UI not Terraform HCL (MR !1446 green, review-ready)", t0.Add(1*time.Minute)),
		task("ka-0000004031", "monorepo-gardener-for-gd-feajz3ctyu-slwz",
			"[DEVOPS-775] scan-vm: read deployed ACME version from telemetry/UI, not Terraform HCL", t0.Add(2*time.Minute)),
		task("ka-0000007828", "monorepo-gardener-for-gd-feajz3ctyu-ust9",
			"DEVOPS-775: Fix version detection: read ACME version from telemetry/UI not Terraform HCL", t0.Add(3*time.Minute)),
		task("ka-0000000620", "monorepo-gardener-for-gd-feajz3ctyu-53tv",
			"DEVOPS-775: read ACME version from telemetry/UI not Terraform", t0.Add(4*time.Minute)),
	}

	out := collapseExternalTicketClusters(in)

	if len(out) != 1 {
		var got []string
		for _, it := range out {
			got = append(got, it.bead.ID)
		}
		t.Fatalf("DEVOPS-775 fan-out should fold to 1 card, got %d: %v", len(out), got)
	}
	if out[0].count != 5 {
		t.Errorf("folded card count = %d, want 5", out[0].count)
	}
	// Exemplar is the newest (last-arriving) card.
	if out[0].bead.ID != "ka-0000000620" {
		t.Errorf("exemplar = %q, want ka-0000000620 (the newest)", out[0].bead.ID)
	}
}

// TestCollapseExternalTicketClusters_DistinctStaysDistinct proves the pass is
// narrow: distinct tickets each keep a card, and a card naming no ticket renders
// on its own — so genuine signal is preserved.
func TestCollapseExternalTicketClusters_DistinctStaysDistinct(t *testing.T) {
	t0 := mustTime(t, "2026-06-19T05:00:00Z")
	task := func(id, title string, ts time.Time) feedCardItem {
		return feedCardItem{
			kind:  feedKindCreated,
			bead:  &feedEventBead{ID: id, Type: "task", Title: title, Status: "open"},
			ts:    ts,
			count: 1,
		}
	}

	in := []feedCardItem{
		task("gd-a1", "DEVOPS-775: read ACME version from telemetry", t0),
		task("gd-a2", "[DEVOPS-775] scan-vm version detection", t0.Add(1*time.Minute)),
		task("gd-b1", "DEVOPS-786: rotate the perf-bastion key", t0.Add(2*time.Minute)),
		task("gd-b2", "DEVOPS-786 follow-up: document the rotation", t0.Add(3*time.Minute)),
		task("gd-c1", "PE-1422: dashboard polish", t0.Add(4*time.Minute)),
		task("gd-noticket", "release-owner: cut 2026.169.11, merged MR !4625", t0.Add(5*time.Minute)),
	}

	out := collapseExternalTicketClusters(in)

	// DEVOPS-775 (x2 -> 1), DEVOPS-786 (x2 -> 1), PE-1422 (1), no-ticket (1) = 4.
	if len(out) != 4 {
		var got []string
		for _, it := range out {
			got = append(got, it.bead.ID)
		}
		t.Fatalf("expected 4 cards (3 tickets + 1 non-ticket), got %d: %v", len(out), got)
	}

	byTicket := map[string]int{}
	noTicket := 0
	for _, it := range out {
		if tk := feedExternalTicketRef(it.bead.Title); tk != "" {
			byTicket[tk] = it.count
		} else {
			noTicket++
			if it.count != 1 {
				t.Errorf("non-ticket card count = %d, want 1", it.count)
			}
		}
	}
	if byTicket["DEVOPS-775"] != 2 {
		t.Errorf("DEVOPS-775 count = %d, want 2", byTicket["DEVOPS-775"])
	}
	if byTicket["DEVOPS-786"] != 2 {
		t.Errorf("DEVOPS-786 count = %d, want 2", byTicket["DEVOPS-786"])
	}
	if byTicket["PE-1422"] != 1 {
		t.Errorf("PE-1422 count = %d, want 1", byTicket["PE-1422"])
	}
	if noTicket != 1 {
		t.Errorf("non-ticket cards = %d, want 1", noTicket)
	}
}
