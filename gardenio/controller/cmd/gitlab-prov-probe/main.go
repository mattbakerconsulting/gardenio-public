// Command gitlab-prov-probe is a TEST-ONLY driver that exercises the gardenio
// E-PROV forward-provenance stack end-to-end, hermetically, so the e2e suite can
// assert host-scoped MR routing, the upstream webhook merge GATE, the scoped
// backstop poller, the reverse-provenance acceptance record, and the peel
// backlink false-attach guard WITHOUT a live gitlab-bridge, a live GitLab, or
// real agent pods.
//
// ── Why a probe, not a live-bridge module ─────────────────────────────────
//
// The gitlab-bridge (and the second, gitlab-dark-pointed bridge) are OFF in the
// e2e namespace, so there is no live
// /webhook to POST to and no SSE-driven sync running. The E-PROV logic under
// test — GitLabSync host scoping, GitLabWebhookHandlerWithConfig's merge path,
// UpstreamAcceptanceReconciler, GitLabUpstreamPoller, and the MRBacklinkReconciler
// peel guard — is all in-process Go with no product image. So this probe follows
// the EXACT precedent of cmd/jira-conv-probe / cmd/trigger-probe (the blessed
// "real bridge components + stub GitLab + mock daemon, no product image"
// pattern): it wires the REAL bridge code through the REAL SSE stream + the REAL
// webhook handler, against
//
//   - STUB GitLab instances (httptest.Server per host) that serve MR JSON and
//     RECORD every request path (so cross-host isolation can be asserted by
//     which IID each host's stub was — or was not — asked about); and
//   - an in-process mock daemon (mockdaemon.go) that RECORDS UpsertMergeRequestBead
//     / AddDependency / UpdateBeadFields / AddComment / CloseBead — so provenance
//     writes are observed as recorded calls and NO real bead (hence NO pod) is
//     ever created.
//
// Each leg prints a single line "LEG <name>: PASS|FAIL <detail>" and the probe
// exits non-zero if any leg fails, so the wrapping shell module (the
// `gitlab-dark-provenance` e2e module) surfaces a per-leg breakdown and fails the
// shard on any leg. It is NOT part of any release Makefile/Dockerfile target — it
// is built from source on demand inside the e2e shard, exactly like trigger-probe
// / jira-conv-probe, and never ships.
//
// Legs (task ka-0000005448; E-PROV epic; commits 9754ec0/ac7b40d/668fc9c/0de8ee7):
//
//	1  hostscope       Two host-scoped GitLabSyncs (dark + upstream) share ONE
//	                   SSE stream + ONE beads daemon. Each mints a produces edge +
//	                   type=merge-request bead ONLY for its own host's mr_url, and
//	                   never GETs / PUTs the foreign host's MR (FW-1).
//	2  webhook-gate    A real merge_request=merged webhook through the real handler
//	                   closes the upstream-mirror bead (mr_outcome=merged) AND the
//	                   wired acceptance reconciler stamps the producing peel-request
//	                   bead (upstream_accepted_at/upstream_mr_url/upstream_merged_by)
//	                   WITHOUT closing the peel-request or the dark work bead
//	                   (FW-2 gate + FW-3 acceptance).
//	3  backstop-poll   The scoped GitLabUpstreamPoller lists ONLY upstream:true
//	                   mirror beads, closes a merged one (mr_outcome=merged), skips
//	                   a foreign-host mirror bead, and (backstop→acceptance) stamps
//	                   the peel-request with an EMPTY merged_by that never clobbers a
//	                   webhook value (FW-2 backstop + FW-3 idempotency).
//	4  peel-backlink   The MRBacklinkReconciler links a peel MR to its UPSTREAM
//	                   peel-request bead and NEVER false-attaches to the DARK anchor
//	                   buried in the §7 provenance block (FW-6).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

const (
	darkHost     = "gitlab-dark.gardenio-e2e.e2e.example.com"
	upstreamHost = "gitlab.com"
	webhookToken = "prov-probe-webhook-secret"
)

func darkMRURL(iid string) string {
	return "https://" + darkHost + "/monorepo-dark/monorepo/-/merge_requests/" + iid
}

func upstreamMRURL(iid string) string {
	return "https://" + upstreamHost + "/example-org/monorepo/-/merge_requests/" + iid
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	legs := []struct {
		name string
		run  func(*slog.Logger) legResult
	}{
		{"hostscope", runHostScopeLeg},
		{"webhook-gate", runWebhookGateLeg},
		{"backstop-poll", runBackstopPollLeg},
		{"peel-backlink", runPeelBacklinkLeg},
	}

	failures := 0
	for _, leg := range legs {
		res := leg.run(logger)
		status := "PASS"
		if !res.ok {
			status = "FAIL"
			failures++
		}
		fmt.Printf("LEG %s: %s %s\n", leg.name, status, res.detail)
	}

	if failures > 0 {
		fmt.Printf("gitlab-prov-probe: %d/%d legs FAILED\n", failures, len(legs))
		os.Exit(1)
	}
	fmt.Printf("gitlab-prov-probe: all %d legs passed\n", len(legs))
}

// legResult is the outcome of one provenance leg.
type legResult struct {
	ok     bool
	detail string
}

func pass(format string, args ...any) legResult {
	return legResult{ok: true, detail: fmt.Sprintf(format, args...)}
}

func fail(format string, args ...any) legResult {
	return legResult{ok: false, detail: fmt.Sprintf(format, args...)}
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 1: hostscope — two host-scoped bridges on ONE SSE stream + ONE daemon.
//
// This is the "two-host SSE stream -> each bridge only acts on its own host"
// repro the task asks for (FW-1 / ka-0000006221). A dark-scoped GitLabSync and an
// upstream-scoped GitLabSync both register on the SAME real SSEStream, fed by a
// stub SSE server that emits a bead.updated for a task bead on EACH host. Each
// bridge's own GitLab client points at its own host's stub. The load-bearing
// property: neither bridge ever calls its GitLab client for the OTHER host's IID
// (which would mint a wrong-host MR bead + PUT a bogus description). We assert
// that by recording, per stub, exactly which MR IIDs it was asked about — the
// dark stub must see only the dark IID, the upstream stub only the upstream IID —
// and by asserting the shared daemon got exactly one produces edge per host.
// ─────────────────────────────────────────────────────────────────────────────

func runHostScopeLeg(logger *slog.Logger) legResult {
	const darkIID, upstreamIID = 4821, 210
	darkURL := darkMRURL(strconv.Itoa(darkIID))
	upURL := upstreamMRURL(strconv.Itoa(upstreamIID))

	darkStub := newGitLabStub()
	defer darkStub.Close()
	upStub := newGitLabStub()
	defer upStub.Close()

	// ONE shared beads daemon — the dual-GitLab topology shares a single daemon
	// + SSE stream (R2 artifact §7). Seed one task bead per host.
	daemon := newMockDaemon()
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: "ka-DarkTask", Type: "task", Status: "in_progress",
		Fields: map[string]string{"mr_url": darkURL},
	})
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: "ka-UpstreamTask", Type: "task", Status: "in_progress",
		Fields: map[string]string{"mr_url": upURL},
	})

	darkSync := bridge.NewGitLabSync(bridge.GitLabSyncConfig{
		GitLab: bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: darkStub.URL(), Token: "t", Logger: logger}),
		Daemon: daemon, Logger: logger, Host: darkHost,
	})
	upSync := bridge.NewGitLabSync(bridge.GitLabSyncConfig{
		GitLab: bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: upStub.URL(), Token: "t", Logger: logger}),
		Daemon: daemon, Logger: logger, Host: upstreamHost,
	})

	// Drive the REAL SSE dispatch path: both syncs register on the SAME stream,
	// fed by a stub SSE server that emits both bead.updated events. This is the
	// faithful shared-stream repro (not a direct handleUpdated call).
	sse := newStubSSE(
		beadUpdatedFrame("1", "ka-DarkTask", "task", darkURL),
		beadUpdatedFrame("2", "ka-UpstreamTask", "task", upURL),
	)
	defer sse.Close()
	stream := bridge.NewSSEStream(bridge.SSEStreamConfig{
		BeadsHTTPAddr: sse.URL, Topics: []string{"beads.bead.updated"}, Logger: logger,
	})
	darkSync.RegisterHandlers(stream)
	upSync.RegisterHandlers(stream)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = stream.Start(ctx) }()

	// Both events produce exactly one MR upsert each (one per host). Wait for the
	// shared daemon to record 2 upserts.
	if !waitFor(3*time.Second, func() bool { return len(daemon.getMRUpserts()) >= 2 }) {
		return fail("expected 2 MR upserts across both hosts within 3s, got %d", len(daemon.getMRUpserts()))
	}
	// Give a beat for any (erroneous) cross-host handler to also fire, so the
	// "exactly 2" / isolation assertions are not racing an in-flight wrong write.
	time.Sleep(300 * time.Millisecond)
	cancel()

	// Exactly two upserts — a cross-host leak would make it 3+.
	ups := daemon.getMRUpserts()
	if len(ups) != 2 {
		return fail("expected exactly 2 MR upserts (one per host), got %d: %+v", len(ups), ups)
	}
	// The dark URL upsert and the upstream URL upsert must each exist exactly once.
	darkUps, upUps := 0, 0
	for _, u := range ups {
		switch u.Fields.MRURL {
		case darkURL:
			darkUps++
		case upURL:
			upUps++
		default:
			return fail("unexpected MR upsert url %q", u.Fields.MRURL)
		}
	}
	if darkUps != 1 || upUps != 1 {
		return fail("expected 1 upsert per host, got dark=%d upstream=%d", darkUps, upUps)
	}

	// Produces edge per host: ka-DarkTask→dark-MR bead and ka-UpstreamTask→upstream-MR bead.
	deps := daemon.getDeps()
	if !hasProducesFrom(deps, "ka-DarkTask") {
		return fail("missing produces edge from ka-DarkTask (dark bridge did not link its own MR): %+v", deps)
	}
	if !hasProducesFrom(deps, "ka-UpstreamTask") {
		return fail("missing produces edge from ka-UpstreamTask (upstream bridge did not link its own MR): %+v", deps)
	}

	// The load-bearing isolation assertion: each stub was asked ONLY about its
	// own host's IID. A host-scope regression would make the dark client GET the
	// upstream IID (and vice versa) — corrupting state with a wrong-host MR bead.
	darkIIDs := darkStub.iids()
	upIIDs := upStub.iids()
	if !onlyContains(darkIIDs, darkIID) {
		return fail("dark stub was asked about foreign IIDs %v (want only %d) — host scope leaked", darkIIDs, darkIID)
	}
	if !onlyContains(upIIDs, upstreamIID) {
		return fail("upstream stub was asked about foreign IIDs %v (want only %d) — host scope leaked", upIIDs, upstreamIID)
	}
	// And neither stub ever received a PUT for the other host (description sync is
	// registered by RegisterHandlers, so a cross-host act would PUT a description).
	if darkStub.puts() > 0 && !onlyContains(darkStub.putIIDs(), darkIID) {
		return fail("dark stub received a cross-host description PUT: putIIDs=%v", darkStub.putIIDs())
	}
	if upStub.puts() > 0 && !onlyContains(upStub.putIIDs(), upstreamIID) {
		return fail("upstream stub received a cross-host description PUT: putIIDs=%v", upStub.putIIDs())
	}

	return pass("dark+upstream bridges on ONE stream each acted only on their own host (dark IID %d, upstream IID %d); 2 upserts, 2 produces edges, 0 cross-host GET/PUT",
		darkIID, upstreamIID)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 2: webhook-gate — the upstream merge_request=merged webhook GATE (FW-2)
//        drives the acceptance reconciler (FW-3).
//
// Seed an upstream peel-request task bead + its pre-linked upstream-mirror MR
// bead (label upstream:true, joined by a produces edge). POST a real
// merge_request/action:merge/state:merged webhook through the REAL
// GitLabWebhookHandlerWithConfig, with a wired UpstreamAcceptanceReconciler
// (scope upstream:true). Assert the mirror bead closes with mr_outcome=merged and
// the peel-request is stamped with the reverse-provenance acceptance record —
// while the peel-request and the dark work bead behind it are NEVER closed.
// ─────────────────────────────────────────────────────────────────────────────

func runWebhookGateLeg(logger *slog.Logger) legResult {
	const (
		peelReqID = "ka-PeelReq2"
		darkWork  = "ka-DarkWork2"
		mergedBy  = "matthew.baker"
		iid       = 777
	)
	mrURL := upstreamMRURL("777")

	daemon := newMockDaemon()
	// The peel-request bead carries the upstream mr_url (set at gb mr link time).
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: peelReqID, Type: "task", Status: "in_progress",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"mr_url": mrURL},
	})
	// The pre-existing upstream-mirror MR bead (FW-5 marker upstream:true), still
	// OPEN, keyed by the same mr_url. The webhook's UpsertMergeRequestBead finds
	// this by URL and closes it — preserving its upstream:true label.
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: "mr-mirror-2", Type: "merge-request", Status: "open",
		Labels: []string{"upstream:true", "external-mirror:true"},
		Fields: map[string]string{"mr_url": mrURL, "mr_state": "opened"},
	})
	// The reverse produces edge (peel-request → mirror MR) the acceptance
	// reconciler walks to find the peel-request from the closed mirror bead.
	_ = daemon.AddDependency(context.Background(), peelReqID, "mr-mirror-2", "produces", "seed")
	// A dark work bead the peel slice was extracted from — must be UNTOUCHED.
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: darkWork, Type: "task", Status: "in_progress",
		Fields: map[string]string{},
	})

	acceptance := bridge.NewUpstreamAcceptanceReconciler(bridge.UpstreamAcceptanceReconcilerConfig{
		Daemon: daemon, Logger: logger, ScopeLabel: "upstream:true",
	})
	handler := bridge.GitLabWebhookHandlerWithConfig(bridge.GitLabWebhookConfig{
		Daemon:               daemon,
		WebhookSecret:        webhookToken,
		AcceptanceReconciler: acceptance,
		Logger:               logger,
		// GitLab client intentionally nil: the merge path is payload-driven
		// (host-blind), so no host-pinned GitLab call happens (FW-2 GATE property).
	})

	code, body := postWebhook(handler, mergeRequestMergedPayload(iid, mrURL, mergedBy, "example-org/monorepo"))
	if code != http.StatusOK {
		return fail("merge webhook returned HTTP %d (%s), want 200", code, strings.TrimSpace(body))
	}

	// The mirror MR bead must be closed with mr_outcome=merged (the FW-2 close).
	mrBead, _ := daemon.MRBeadForTask(context.Background(), peelReqID)
	if mrBead == nil {
		return fail("no MR bead linked to peel-request after merge webhook")
	}
	if mrBead.Status != "closed" {
		return fail("mirror MR bead status=%q, want closed", mrBead.Status)
	}
	if got := mrBead.Fields["mr_outcome"]; got != "merged" {
		return fail("mirror MR bead mr_outcome=%q, want merged", got)
	}

	// The peel-request must be stamped with the reverse-provenance acceptance record.
	if got := daemon.beadField(peelReqID, "upstream_accepted_at"); got == "" {
		return fail("peel-request %s missing upstream_accepted_at (FW-3 acceptance not stamped)", peelReqID)
	}
	if got := daemon.beadField(peelReqID, "upstream_mr_url"); got != mrURL {
		return fail("peel-request upstream_mr_url=%q, want %q", got, mrURL)
	}
	if got := daemon.beadField(peelReqID, "upstream_merged_by"); got != "gitlab:"+mergedBy {
		return fail("peel-request upstream_merged_by=%q, want gitlab:%s (webhook actor)", got, mergedBy)
	}
	if got := daemon.beadField(peelReqID, "upstream_rejected_at"); got != "" {
		return fail("peel-request wrongly stamped upstream_rejected_at=%q on a MERGE", got)
	}
	if n := len(daemon.commentsFor(peelReqID)); n != 1 {
		return fail("expected exactly 1 acceptance comment on peel-request, got %d", n)
	}

	// INVARIANT: the reconciler NEVER closes the peel-request or the dark work bead.
	if daemon.wasClosed(peelReqID) {
		return fail("peel-request %s was CLOSED — acceptance must never close the provenance anchor", peelReqID)
	}
	if daemon.wasClosed(darkWork) {
		return fail("dark work bead %s was CLOSED — dark lives ahead of the peeled slice", darkWork)
	}
	if n := len(daemon.updatesFor(darkWork)); n != 0 {
		return fail("dark work bead %s was written (%d field updates) — it must be untouched", darkWork, n)
	}

	return pass("merge webhook closed mirror bead (mr_outcome=merged) + stamped peel-request %s (accepted_at, mr_url, merged_by=gitlab:%s); peel-request + dark work untouched",
		peelReqID, mergedBy)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 3: backstop-poll — the scoped GitLabUpstreamPoller (FW-2 backstop) +
//        backstop→acceptance idempotency (FW-3).
//
// Seed two OPEN mirror beads: an upstream one (label upstream:true, upstream MR
// merged on the stub) and a foreign-host (dark) one. Run one poll. Assert the
// poller (a) listed ONLY the upstream:true scope, (b) closed the upstream mirror
// with mr_outcome=merged, (c) NEVER GETs the foreign-host mirror (host guard),
// and (d) drove acceptance with an EMPTY merged_by that does not clobber a value
// already stamped by a webhook (idempotency).
// ─────────────────────────────────────────────────────────────────────────────

func runBackstopPollLeg(logger *slog.Logger) legResult {
	const peelReqID = "ka-PeelReq3"
	upURL := upstreamMRURL("778")
	darkURL := darkMRURL("999")

	// Stub upstream GitLab: the peel MR is merged (the webhook missed it).
	stub := newGitLabStubMR(bridge.GitLabMR{
		IID: 778, State: "merged", Title: "peel: PE-778 slice",
		WebURL: upURL, SourceBranch: "peel/PE-778", TargetBranch: "main",
		ProjectID: 7, Author: &bridge.GitLabUser{Username: "matthew.baker"},
	})
	defer stub.Close()

	daemon := newMockDaemon()
	// Peel-request already carries a webhook-stamped merged_by — the backstop's
	// empty merged_by must NOT clobber it (idempotency).
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: peelReqID, Type: "task", Status: "in_progress",
		Fields: map[string]string{"mr_url": upURL, "upstream_merged_by": "slack:U07WEBHOOK"},
	})
	// The OPEN upstream-mirror MR bead (upstream:true), pre-linked to peel-request.
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: "mr-mirror-3", Type: "merge-request", Status: "open",
		Labels: []string{"upstream:true"},
		Fields: map[string]string{"mr_url": upURL, "mr_state": "opened"},
	})
	_ = daemon.AddDependency(context.Background(), peelReqID, "mr-mirror-3", "produces", "seed")
	// A FOREIGN-host (dark) mirror bead also carrying upstream:true — the poller
	// must skip it on the host guard, never GET it against the upstream API.
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: "mr-mirror-3-dark", Type: "merge-request", Status: "open",
		Labels: []string{"upstream:true"},
		Fields: map[string]string{"mr_url": darkURL, "mr_state": "opened"},
	})

	acceptance := bridge.NewUpstreamAcceptanceReconciler(bridge.UpstreamAcceptanceReconcilerConfig{
		Daemon: daemon, Logger: logger, ScopeLabel: "upstream:true",
	})
	poller := bridge.NewGitLabUpstreamPoller(bridge.GitLabUpstreamPollerConfig{
		GitLab:       bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: stub.URL(), Token: "t", Logger: logger}),
		Daemon:       daemon,
		Logger:       logger,
		PollInterval: time.Hour, // one poll only; we cancel right after
		ScopeLabel:   "upstream:true",
		Host:         upstreamHost,
		Acceptance:   acceptance,
	})

	// Run drives an immediate poll, then blocks on the ticker; cancel after the
	// mirror closes so we stop cleanly.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = poller.Run(ctx) }()

	if !waitFor(3*time.Second, func() bool { return daemon.beadStatus("mr-mirror-3") == "closed" }) {
		return fail("backstop poll did not close the merged upstream mirror bead within 3s")
	}
	time.Sleep(200 * time.Millisecond)
	cancel()

	// (a) Scope: every ListOpenMergeRequestBeads call passed the upstream:true label.
	scopes := daemon.listOpenMRScopes()
	if len(scopes) == 0 {
		return fail("poller never listed mirror beads")
	}
	for _, s := range scopes {
		if s != "upstream:true" {
			return fail("poller listed with scope %q, want upstream:true (backstop must be label-scoped)", s)
		}
	}

	// (b) Closed with mr_outcome=merged.
	if got := daemon.beadField("mr-mirror-3", "mr_outcome"); got != "merged" {
		return fail("upstream mirror mr_outcome=%q, want merged", got)
	}

	// (c) The foreign-host mirror must be untouched (host guard skipped its GET).
	if daemon.beadStatus("mr-mirror-3-dark") == "closed" {
		return fail("foreign-host (dark) mirror bead was closed — the host guard leaked")
	}
	if stub.contains(999) {
		return fail("upstream stub was asked about the dark IID 999 — host guard leaked")
	}

	// (d) acceptance stamped the peel-request; empty merged_by did NOT clobber.
	if got := daemon.beadField(peelReqID, "upstream_accepted_at"); got == "" {
		return fail("backstop did not drive acceptance stamping on the peel-request")
	}
	if got := daemon.beadField(peelReqID, "upstream_merged_by"); got != "slack:U07WEBHOOK" {
		return fail("backstop clobbered upstream_merged_by=%q, want the webhook value slack:U07WEBHOOK (idempotency)", got)
	}

	return pass("backstop closed upstream mirror (mr_outcome=merged, scope=upstream:true), skipped dark IID 999, stamped acceptance without clobbering webhook merged_by")
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 4: peel-backlink — the MRBacklinkReconciler peel false-attach guard (FW-6).
//
// A peel-on-demand MR lives on the UPSTREAM host; its ONLY explicit ka-ref is a
// DARK anchor buried in the §7 provenance block, while its legitimate attachment
// key is the upstream jira PE key on branch/title. The reconciler must link the
// peel-request bead and must NEVER set mr_url on the dark anchor (cross-host
// false-attach). We drive the REAL reconciler via Run() (immediate scan) against
// a fake MR lister + the mock daemon.
// ─────────────────────────────────────────────────────────────────────────────

func runPeelBacklinkLeg(logger *slog.Logger) legResult {
	const (
		darkAnchor    = "ka-DarkAnchor4"
		peelReqID     = "ka-PeelReq4"
		jiraKey       = "PE-40004"
		upstreamPeel  = "https://gitlab.com/example-org/monorepo/-/merge_requests/404"
		reconcilerGrp = 42
	)

	daemon := newMockDaemon()
	// The dark anchor EXISTS as a real actionable bead — so if the reconciler DID
	// extract it, GetBead would succeed and it would be (wrongly) linked.
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: darkAnchor, Type: "task", Kind: "issue", Status: "in_progress",
		Fields: map[string]string{},
	})
	// The legitimate upstream peel-request bead, resolvable via jira:PE-40004.
	daemon.seedBead(&beadsapi.BeadDetail{
		ID: peelReqID, Type: "task", Kind: "issue", Status: "in_progress",
		Labels: []string{"jira:" + jiraKey},
		Fields: map[string]string{},
	})

	// The peel MR: branch/title carry the upstream PE key; description carries the
	// §7 dark-provenance block whose ONLY ka-ref is the dark anchor.
	lister := &fakeMRLister{opened: []bridge.GitLabMR{{
		WebURL:       upstreamPeel,
		SourceBranch: "peel/" + jiraKey + "-slice",
		Title:        "peel: " + jiraKey,
		Description:  peelMRDescription(jiraKey, darkAnchor),
	}}}

	reconciler := bridge.NewMRBacklinkReconciler(bridge.MRBacklinkConfig{
		GitLab: lister, Daemon: daemon, Logger: logger,
		GroupID: reconcilerGrp, Interval: time.Hour,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = reconciler.Run(ctx) }()

	// The reconciler runs one scan immediately; wait for the peel-request link.
	if !waitFor(3*time.Second, func() bool { return daemon.beadField(peelReqID, "mr_url") == upstreamPeel }) {
		return fail("peel MR was not back-linked to peel-request %s within 3s (mr_url=%q)", peelReqID, daemon.beadField(peelReqID, "mr_url"))
	}
	time.Sleep(200 * time.Millisecond)
	cancel()

	// GATE: the dark anchor must NEVER receive an mr_url (cross-host false-attach).
	if got := daemon.beadField(darkAnchor, "mr_url"); got != "" {
		return fail("FW-6 GATE VIOLATED: dark anchor %s false-attached to upstream peel MR (mr_url=%q)", darkAnchor, got)
	}
	if n := len(daemon.updatesFor(darkAnchor)); n != 0 {
		return fail("FW-6 GATE VIOLATED: dark anchor %s was written (%d updates) — it must never be touched", darkAnchor, n)
	}
	// And the peel-request was linked to the upstream peel MR.
	if got := daemon.beadField(peelReqID, "mr_url"); got != upstreamPeel {
		return fail("peel-request %s mr_url=%q, want %q", peelReqID, got, upstreamPeel)
	}

	return pass("peel MR linked to upstream peel-request %s via jira:%s; dark anchor %s never false-attached", peelReqID, jiraKey, darkAnchor)
}

// peelMRDescription renders a realistic peel-MR body with the §7 dark provenance
// trailer block (peel-trailer.sh emit_block shape). The dark anchor appears ONLY
// inside the Dark-Bead: trailer, which stripDarkProvenanceAnchors must remove
// before key extraction.
func peelMRDescription(jiraKey, darkBead string) string {
	return strings.Join([]string{
		"Peel-on-demand slice extracted from dark integration.",
		"",
		"Bead: " + jiraKey,
		"Operator: matthew.baker@acme.example",
		"# peel provenance (lab-internal — dark beads/MRs live on gitlab-dark)",
		"Dark-Bead: " + darkBead,
		"Dark-MR: 42",
		"Provenance-Hash: sha256:deadbeefcafef00dba5eba11deadbeefcafef00dba5eba11deadbeefcafef00d",
	}, "\n")
}

// ─────────────────────────────────────────────────────────────────────────────
// fakeMRLister satisfies the (package-private) mrLister interface the
// MRBacklinkReconciler consumes. Its method is exported, so a package-main type
// structurally satisfies the interface — the same cross-package pattern
// cmd/gitlab-bridge/main.go uses to pass beadsapi.Client.
// ─────────────────────────────────────────────────────────────────────────────

type fakeMRLister struct {
	opened []bridge.GitLabMR
	merged []bridge.GitLabMR
}

func (f *fakeMRLister) ListGroupMergeRequests(_ context.Context, _ int, state string, _ time.Time) ([]bridge.GitLabMR, error) {
	switch state {
	case "opened":
		return f.opened, nil
	case "merged":
		return f.merged, nil
	}
	return nil, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Stub GitLab: an httptest server that serves a canned MR JSON for any
// GET .../merge_requests/:iid and records every IID it is asked about (GET) and
// every description PUT, so cross-host isolation can be asserted.
// ─────────────────────────────────────────────────────────────────────────────

type gitlabStub struct {
	srv *httptest.Server
	mu  sync.Mutex
	// getIIDList / putIIDList record the MR IIDs the stub was asked about, per method.
	getIIDList []int
	putIIDList []int
	mr         *bridge.GitLabMR // canned MR to return; nil = a generic opened MR
}

func newGitLabStub() *gitlabStub { return newGitLabStubMR(bridge.GitLabMR{}) }

func newGitLabStubMR(mr bridge.GitLabMR) *gitlabStub {
	s := &gitlabStub{}
	if mr.IID != 0 || mr.State != "" {
		m := mr
		s.mr = &m
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *gitlabStub) URL() string { return s.srv.URL }
func (s *gitlabStub) Close()      { s.srv.Close() }

func (s *gitlabStub) handle(w http.ResponseWriter, r *http.Request) {
	iid := iidFromMRPath(r.URL.Path)
	s.mu.Lock()
	if iid > 0 {
		switch r.Method {
		case http.MethodPut:
			s.putIIDList = append(s.putIIDList, iid)
		default:
			s.getIIDList = append(s.getIIDList, iid)
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	mr := bridge.GitLabMR{
		IID: iid, State: "opened", Title: "stub MR",
		SourceBranch: "peel/stub", TargetBranch: "main", ProjectID: 7,
		Author: &bridge.GitLabUser{Username: "matthew.baker"},
	}
	if s.mr != nil {
		mr = *s.mr
		if mr.IID == 0 {
			mr.IID = iid
		}
	}
	_ = json.NewEncoder(w).Encode(mr)
}

// iids returns the distinct GET IIDs the stub was asked about.
func (s *gitlabStub) iids() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return distinctInts(s.getIIDList)
}

func (s *gitlabStub) putIIDs() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return distinctInts(s.putIIDList)
}

func (s *gitlabStub) puts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.putIIDList)
}

// contains reports whether the stub was ever asked (GET or PUT) about iid.
func (s *gitlabStub) contains(iid int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range append(append([]int{}, s.getIIDList...), s.putIIDList...) {
		if v == iid {
			return true
		}
	}
	return false
}

// iidFromMRPath extracts :iid from /api/v4/projects/<enc>/merge_requests/:iid[/...].
func iidFromMRPath(path string) int {
	const marker = "/merge_requests/"
	idx := strings.Index(path, marker)
	if idx < 0 {
		return 0
	}
	rest := path[idx+len(marker):]
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	if rest == "" {
		return 0
	}
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// Stub SSE server: emits the given event frames once, then holds the connection
// open so the SSEStream does not reconnect-spam (mirrors cmd/jira-conv-probe).
// ─────────────────────────────────────────────────────────────────────────────

type stubSSE struct {
	*httptest.Server
}

func newStubSSE(frames ...string) *stubSSE {
	s := &stubSSE{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for _, f := range frames {
			fmt.Fprint(w, f)
			flusher.Flush()
		}
		time.Sleep(3 * time.Second)
	}))
	return s
}

// beadUpdatedFrame builds one SSE frame for a beads.bead.updated event carrying a
// task bead with the given mr_url — the exact effect of `gb mr link`.
func beadUpdatedFrame(id, beadID, btype, mrURL string) string {
	inner, _ := json.Marshal(map[string]any{
		"id":     beadID,
		"type":   btype,
		"status": "in_progress",
		"fields": map[string]string{"mr_url": mrURL},
	})
	data, _ := json.Marshal(map[string]json.RawMessage{"bead": inner})
	return fmt.Sprintf("id:%s\nevent:beads.bead.updated\ndata:%s\n\n", id, data)
}

// mergeRequestMergedPayload builds a GitLab merge_request webhook body for a
// merged MR (object_attributes.action=merge, state=merged).
func mergeRequestMergedPayload(iid int, mrURL, authorUsername, projectPath string) []byte {
	payload := map[string]any{
		"object_kind": "merge_request",
		"event_type":  "merge_request",
		"user":        map[string]any{"username": authorUsername},
		"project":     map[string]any{"path_with_namespace": projectPath},
		"object_attributes": map[string]any{
			"iid":           iid,
			"state":         "merged",
			"action":        "merge",
			"url":           mrURL,
			"title":         "peel: merged slice",
			"source_branch": "peel/slice",
			"target_branch": "main",
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// postWebhook POSTs a body to the webhook handler with the shared-secret header
// and returns the status + body.
func postWebhook(handler http.Handler, body []byte) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", webhookToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// small helpers
// ─────────────────────────────────────────────────────────────────────────────

// waitFor polls cond every 10ms until true or the deadline elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// hasProducesFrom reports whether deps contains a produces edge out of beadID.
func hasProducesFrom(deps []depRecord, beadID string) bool {
	for _, d := range deps {
		if d.BeadID == beadID && d.DepType == "produces" {
			return true
		}
	}
	return false
}

// onlyContains reports whether xs is non-empty and every element equals want.
func onlyContains(xs []int, want int) bool {
	if len(xs) == 0 {
		return false
	}
	for _, x := range xs {
		if x != want {
			return false
		}
	}
	return true
}

// distinctInts returns the de-duplicated ints in xs, order-preserving.
func distinctInts(xs []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
