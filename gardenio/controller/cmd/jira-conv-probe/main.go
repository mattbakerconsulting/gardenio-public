// Command jira-conv-probe is a TEST-ONLY driver that exercises the jira-bridge
// CONVERSATIONAL round-trip end-to-end, hermetically, so the e2e suite can
// assert the mention -> spawn -> reply -> squawk -> follow-up loop without
// standing up a live jira-bridge, a live Jira, or (critically) real agent pods.
//
// ── Why a probe, not a live-bridge module ─────────────────────────────────
//
// The jira-bridge is OFF in the e2e namespace, so there is no live /webhook to POST to. More importantly,
// the mention/follow-up paths call daemon.SpawnAgent, which CREATES a
// type=agent bead — and the deployed controller/reconciler mints a REAL pod for
// ANY active agent bead (the image is controller-wide, not project-gated). So
// driving the real spawn path against the SHARED daemon would spawn costly pods
// in the shared namespace — exactly the class of accident an e2e guardrail
// would exist to prevent. `kd serve` needs Postgres, so an isolated daemon is not
// cheap to stand up in a shard either.
//
// This probe follows the EXACT precedent of cmd/trigger-probe (the blessed
// "real bridge + stub Jira, no product image" pattern): it wires the REAL
// bridge components — JiraWebhookHandler, JiraMentionHandler, JiraReplyAPI,
// JiraSquawkForwarder, JiraAgentResolver — through the REAL /webhook HTTP
// handler and the REAL SSE-driven squawk handler, against
//
//   - a STUB Jira (httptest.Server) that RECORDS every outbound comment write
//     (AddComment / AddCommentWithVisibility[AndSourceLink]) and serves comment
//     history, exactly as the sandbox-Jira in the outbox tests records writes;
//   - an in-process mock daemon that records CreateBead / SpawnAgent / AddComment
//     / UpdateBeadFields / CloseBead — so the "spawn" is observed as a recorded
//     call, and NO real bead (and therefore NO pod) is ever created.
//
// The result is a faithful wire-level exercise of the real handler code
// (HMAC-skip-via-DevMode, JSON decode, event routing, mention detection,
// bound-ticket scoping, the F5 project gate, the F3 respawn-of-a-stopped-agent
// path, the F1 squawk->internal-note forwarder) with ZERO pods, ZERO shared
// state, and ZERO sandbox rig — so it is green today and stays a hard gate.
//
// Each leg prints a single line "LEG <name>: PASS|FAIL <detail>" and the probe
// exits non-zero if any leg fails, so the wrapping shell module can surface a
// per-leg breakdown and fail the shard on any leg. It is NOT part of any release
// Makefile/Dockerfile target — it is built from source on demand inside the e2e
// shard, the same way trigger-engine / trigger-probe are, and never ships.
//
// Legs (bead ka-0000003999, epic ka-0000007401; routing added ka-0000002247):
//
//	1  mention        comment_created @gardenio mention -> agent spawn with
//	                  jira_key + jira_comment_id stamped on task AND agent beads.
//	2  routing        multi-project mention routing (MR !5674, G3): with a
//	                  projectMap, a SECOND JIRA prefix routes its mention to a
//	                  DIFFERENT boat project's role/credentials, and a BLOCKED
//	                  prefix is denied (no spawn, no task) even when it carries a
//	                  map entry — the deny gate is keyed on the JIRA prefix, not
//	                  on projectMap presence.
//	3  reply          gb jira reply -> AddCommentWithVisibility(internal) on the
//	                  agent's OWN bound ticket (recorded by the stub Jira).
//	4  squawk         a closed squawk bead from a Jira-bound agent -> internal
//	                  note via the SSE squawk forwarder.
//	5  followup       a follow-up comment webhook -> live agent NUDGED; a stopped
//	                  (terminal agent_state) agent RE-SPAWNED under the same name.
//	6  fsm-negative   an FSM-key comment_created webhook (mention + reply + squawk)
//	                  yields ZERO outbound Jira writes AND ZERO spawn (F5 spine).
//	7  spawn-ack      a @gardenio mention with the JiraSpawnAcknowledger wired onto
//	                  OnSpawn (exactly as cmd/jira-bridge does it) posts ONE
//	                  immediate internal-note "👋 On it" comment to the ticket,
//	                  carrying the spawned agent name, the tracking bead, and the
//	                  operator attribution line — the Jira analog of the Slack
//	                  agent card (ka-0000003551; feature ka-0000001164, plan
//	                  ka-0000007401). The leg embeds its OWN anti-vacuity negative
//	                  control: the SAME mention with OnSpawn left nil (the legacy
//	                  wiring) records ZERO Jira writes, so a green assertion proves
//	                  the ack path — not a latent mention-path write — produced the
//	                  note. Legs 1–6 pass identically with the ack deleted (the
//	                  mention handler holds no *JiraClient), so without this leg the
//	                  ack is structurally unreachable from the e2e.
//	8  crosslink-nowake  GitLab's "<user> mentioned this issue in a merge request/
//	                  commit of <project>" crosslink comment (ka-0000005445,
//	                  jira_webhook.go:447) is BRIDGED to the bead but wakes NOTHING
//	                  — 0 spawns, 0 nudges — even against a CLOSED task whose agent
//	                  is terminal (the F3 respawn shape). Covers the merge-request
//	                  and commit marker phrasings. Embeds the MANDATORY anti-vacuity
//	                  control: the SAME fixture with a HUMAN body that carries the
//	                  verb but not the marker object ("I mentioned this issue in
//	                  standup") respawns exactly once, so a globally-dead respawn
//	                  path cannot pass the suppression asserts vacuously. Drives the
//	                  REAL handler+resolver — the live wiring the mocked-handler
//	                  unit tests cannot reach.
//	9  bridge-nowake   The bridge's OWN Jira comments (ka-0000004998,
//	                  jira_webhook.go:461) — jira_sync's claim / "Automated MR
//	                  created" / "MR merged" notices — are BRIDGED to the bead but
//	                  wake NOTHING (0 spawns, 0 nudges) against the same CLOSED-task
//	                  + terminal-agent F3 shape, or the agent wakes on its own notice
//	                  and loops (observed on DEVOPS-897). The bodies are NOT
//	                  hand-authored: the leg stands up the REAL jira-SYNC producer
//	                  (bridge.NewJiraSync) behind the REAL SSE stream, fires the bead
//	                  events that make it emit each notice, and CAPTURES the exact
//	                  bytes it POSTed — so producer wording and matcher table are
//	                  coupled end-to-end (a drift on EITHER side reds the leg; the
//	                  trap the sibling backfill ka-0000000099 was filed for; advice
//	                  ka-0000001521). Each notice is replayed in BOTH delivery shapes
//	                  — Jira Server (plain string) AND Jira Cloud (raw ADF) — since
//	                  the two flatten through different code (the ka-0000006177 shape
//	                  guardrail). FOUR generated notices are suppressed — claim,
//	                  MR-created, MR-merged, and the bead-closure notice
//	                  (ka-0000004442, landed at jira_webhook.go:740). FOUR MANDATORY
//	                  anti-vacuity controls: a human QUOTING a generated sentence
//	                  (full colon-bearing marker mid-body — a naive Contains would
//	                  suppress it), a human OPENING with "Gardenio agent please take
//	                  another look" (shares the claim PREFIX but not its suffix), a
//	                  plain human follow-up, and a human QUOTING the closure sentence
//	                  back (the closure marker has an EMPTY suffix, so this prefix-vs-
//	                  contains control is load-bearing) — all must still wake exactly
//	                  once.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// mentionTrigger is the @-mention token the probe drives; matches the bridge default.
const mentionTrigger = "@gardenio"

// probeProject is the boat project the mention/spawn path resolves to. A
// spawn-role trigger bead for it is seeded in the mock daemon so
// resolveJiraSpawnRole succeeds (there is no hardcoded fallback).
const probeProject = "gardenio"

// spawnRole is the role the seeded spawn_agent trigger bead grants. crew carries
// the full field allowlist (DefaultFieldAllowlist), so its internal-note replies
// pass the write policy.
const spawnRole = "crew"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	legs := []struct {
		name string
		run  func(*slog.Logger) legResult
	}{
		{"mention", runMentionLeg},
		{"routing", runRoutingLeg},
		{"reply", runReplyLeg},
		{"reply-reclassify", runReplyReclassifyLeg},
		{"reply-codemark", runReplyCodeMarkLeg},
		{"squawk", runSquawkLeg},
		{"followup", runFollowUpLeg},
		{"fsm-negative", runFSMNegativeLeg},
		{"spawn-ack", runSpawnAckLeg},
		{"crosslink-nowake", runCrosslinkNoWakeLeg},
		{"bridge-nowake", runBridgeNoWakeLeg},
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
		fmt.Printf("jira-conv-probe: %d/%d legs FAILED\n", failures, len(legs))
		os.Exit(1)
	}
	fmt.Printf("jira-conv-probe: all %d legs passed\n", len(legs))
}

// legResult is the outcome of one conversational leg.
type legResult struct {
	ok     bool
	detail string
}

// probeMentionUsers is the probe's user directory for the mention gate
// (gd-fBBMiDHb07): every webhook author resolves to a user bead holding the
// "mention" capability, attributed to slack:<slackID> when set, else
// oidc:<accountId>@probe.example. An unmapped author would be refused and the
// mention legs would spawn nothing.
type probeMentionUsers struct{ slackID string }

func (u probeMentionUsers) LookupByJiraUser(_ context.Context, accountID string) *bridge.UserIdentity {
	if accountID == "" {
		return nil
	}
	return &bridge.UserIdentity{
		BeadID:       "probe-user-" + accountID,
		SlackID:      u.slackID,
		Email:        accountID + "@probe.example",
		JiraUser:     accountID,
		Capabilities: []string{bridge.MentionCapability},
	}
}

func (probeMentionUsers) LookupByEmail(context.Context, string) *bridge.UserIdentity {
	return nil
}

func pass(format string, args ...any) legResult {
	return legResult{ok: true, detail: fmt.Sprintf(format, args...)}
}

func fail(format string, args ...any) legResult {
	return legResult{ok: false, detail: fmt.Sprintf(format, args...)}
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 1: comment_created @gardenio mention -> agent spawn, jira_key/comment_id
//        stamped on the task bead AND the agent bead.
// ─────────────────────────────────────────────────────────────────────────────

func runMentionLeg(logger *slog.Logger) legResult {
	const (
		jiraKey   = "PE-51001"
		commentID = "cmt-51001"
	)
	daemon := newMockDaemon()
	daemon.seedSpawnTrigger(probeProject, spawnRole)

	mention := bridge.NewJiraMentionHandler(bridge.JiraMentionHandlerConfig{
		Daemon:      daemon,
		Users:       probeMentionUsers{},
		Project:     probeProject,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})
	handler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:         daemon,
		DevMode:        true, // jiraWebhookDevMode: skip HMAC, the recorded-webhook injection path
		MentionHandler: mention,
		Logger:         logger,
	})

	code, _ := postWebhook(handler, commentCreatedPayload(jiraKey, commentID,
		mentionTrigger+" please investigate the failing build", "Ada Commenter", "acct-ada"))
	if code != http.StatusOK {
		return fail("webhook returned HTTP %d, want 200", code)
	}

	spawns := daemon.spawnCalls()
	if len(spawns) != 1 {
		return fail("expected exactly 1 SpawnAgent call, got %d", len(spawns))
	}
	sp := spawns[0]
	if sp.project != probeProject {
		return fail("spawned into project %q, want %q", sp.project, probeProject)
	}
	if sp.role != spawnRole {
		return fail("spawn role %q, want %q (from the seeded spawn_agent trigger)", sp.role, spawnRole)
	}

	// The task bead the mention created must carry the jira_key + jira_comment_id.
	creates := daemon.createCalls()
	var taskCreate *createRecord
	for i := range creates {
		if contains(creates[i].labels, "jira-mention") {
			taskCreate = &creates[i]
			break
		}
	}
	if taskCreate == nil {
		return fail("no jira-mention task bead was created")
	}
	if got := taskCreate.fields["jira_key"]; got != jiraKey {
		return fail("task bead jira_key=%q, want %q", got, jiraKey)
	}
	if got := taskCreate.fields["jira_comment_id"]; got != commentID {
		return fail("task bead jira_comment_id=%q, want %q", got, commentID)
	}

	// The agent bead's extraFields must carry the same binding so `gb jira reply`
	// can resolve + thread (F3).
	if got := sp.fields["jira_key"]; got != jiraKey {
		return fail("agent bead jira_key=%q, want %q", got, jiraKey)
	}
	if got := sp.fields["jira_comment_id"]; got != commentID {
		return fail("agent bead jira_comment_id=%q, want %q", got, commentID)
	}
	if got := sp.fields["spawn_source"]; got != "jira-mention" {
		return fail("agent bead spawn_source=%q, want jira-mention", got)
	}

	// The spawn prompt (6th SpawnAgent arg -> agent bead fields.prompt) must
	// advertise both re-read affordances so a mention-spawned agent is TOLD it can
	// refresh the stale ticket snapshot (commit 1a8ad32d1). Asserting the
	// INTERPOLATED bound key `gb jira show PE-51001` — not a bare `gb jira show` —
	// is the point: a regression that hardcodes or drops the %s reds this, whereas
	// a bare-substring assert would stay green. The probe formerly DISCARDED this
	// arg (SpawnAgent's 6th param was `_`), so no e2e could see the copy at all.
	if want := "gb jira show " + jiraKey; !strings.Contains(sp.prompt, want) {
		return fail("spawn prompt missing %q (the key-interpolated re-read affordance); got %q", want, sp.prompt)
	}
	if !strings.Contains(sp.prompt, "gb jira thread") {
		return fail("spawn prompt missing `gb jira thread` (the conversation re-read affordance); got %q", sp.prompt)
	}
	return pass("spawn %s into %s/%s; jira_key+jira_comment_id stamped on task %s and agent bead; prompt advertises `gb jira show %s` + `gb jira thread`",
		sp.agentName, sp.project, sp.role, taskCreate.id, jiraKey)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 2: multi-project mention routing (MR !5674, G3 epic ka-0000007401;
//        backfill ka-0000002247). With a projectMap wired (the same
//        BOAT_PROJECTS-derived map the JQL poller gets), the SAME handler routes
//        a mention to the boat project mapped from the ticket's JIRA prefix —
//        task-bead label, spawn project, AND spawn-role all follow the mapping.
//        A SECOND, distinct prefix must route to a DIFFERENT project's role
//        (the auth/routing risk this backfills: a routing regression would spawn
//        a mention-agent under the wrong project's role/credentials). And a
//        BLOCKED prefix (FSM) must be DENIED — no spawn, no task bead — EVEN when
//        it carries a projectMap entry, because the deny gate keys on the JIRA
//        project prefix (allowSpawn/WritePolicy), independent of the map.
//
// The observable routing signals in this hermetic probe are the spawn `project`
// and `role`: the role is resolved from that project's spawn_agent trigger bead,
// and both project + role are exactly what select the per-project credentials at
// pod-creation time downstream. The FSM leg (leg 6) already asserts the deny for
// the SINGLE-project config; this leg proves deny still wins when the blocked
// prefix ALSO appears in a multi-project map (map presence must not open a gate
// the prefix keeps closed).
// ─────────────────────────────────────────────────────────────────────────────

func runRoutingLeg(logger *slog.Logger) legResult {
	const (
		peKey     = "PE-60001"     // writable prefix -> boatpe
		devopsKey = "DEVOPS-60002" // SECOND writable prefix -> boatdevops
		fsmKey    = "FSM-60003"    // BLOCKED prefix — mapped, yet must be denied
	)
	const (
		peProject     = "boatpe"
		devopsProject = "boatdevops"
		fsmProject    = "boatfsm"
		peRole        = "boatpe-crew"
		devopsRole    = "boatdevops-crew"
		fsmRole       = "boatfsm-crew"
	)

	daemon := newMockDaemon()
	// One enabled spawn_agent trigger per MAPPED boat project, each with a
	// DISTINCT role, so a resolved role proves routing ran against the MAPPED
	// project — not a shared default. The FSM->boatfsm trigger is seeded too so
	// the ONLY thing keeping FSM from spawning is the prefix deny gate, not a
	// missing role config (a stronger negative).
	daemon.seedSpawnTrigger(peProject, peRole)
	daemon.seedSpawnTrigger(devopsProject, devopsRole)
	daemon.seedSpawnTrigger(fsmProject, fsmRole)

	// The BOAT_PROJECTS-style prefix->project map, INCLUDING the blocked FSM
	// prefix, so the leg proves the deny gate is keyed on the prefix rather than
	// on map membership.
	projectMap := map[string]string{
		"PE":     peProject,
		"DEVOPS": devopsProject,
		"FSM":    fsmProject,
	}

	// DefaultWritePolicy's writable set is {PE, DEVOPS, GBFSM}; FSM is never
	// writable, so allowSpawn denies the FSM prefix regardless of its map entry.
	wp := bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger)
	mention := bridge.NewJiraMentionHandler(bridge.JiraMentionHandlerConfig{
		Daemon:      daemon,
		Users:       probeMentionUsers{},
		Project:     probeProject, // default (single-project fallback) — should NOT be used here
		ProjectMap:  projectMap,
		WritePolicy: wp,
		Logger:      logger,
	})
	handler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:         daemon,
		DevMode:        true,
		MentionHandler: mention,
		WritePolicy:    wp,
		Logger:         logger,
	})

	// ── Mention 1: PE prefix routes to boatpe/boatpe-crew ──
	if code, _ := postWebhook(handler, commentCreatedPayload(peKey, "cmt-60001",
		mentionTrigger+" investigate the PE regression", "Ada Commenter", "acct-ada")); code != http.StatusOK {
		return fail("[pe] mention webhook HTTP %d, want 200", code)
	}
	// ── Mention 2: SECOND prefix DEVOPS routes to boatdevops/boatdevops-crew ──
	if code, _ := postWebhook(handler, commentCreatedPayload(devopsKey, "cmt-60002",
		mentionTrigger+" rotate the platform certs", "Ben Ops", "acct-ben")); code != http.StatusOK {
		return fail("[devops] mention webhook HTTP %d, want 200", code)
	}
	// ── Mention 3: BLOCKED prefix FSM must be denied (handled, but no spawn) ──
	if code, _ := postWebhook(handler, commentCreatedPayload(fsmKey, "cmt-60003",
		mentionTrigger+" customer wants an update", "Ext Customer", "acct-cust")); code != http.StatusOK {
		return fail("[fsm] blocked mention webhook HTTP %d, want 200 (blocked-but-handled)", code)
	}

	// Exactly TWO spawns (PE + DEVOPS); the FSM prefix must not have spawned.
	spawns := daemon.spawnCalls()
	if len(spawns) != 2 {
		return fail("expected exactly 2 spawns (PE + DEVOPS; FSM denied), got %d: %+v", len(spawns), spawns)
	}
	byProject := make(map[string]spawnRecord, len(spawns))
	for _, sp := range spawns {
		byProject[sp.project] = sp
	}
	pe, ok := byProject[peProject]
	if !ok {
		return fail("[pe] no spawn into %q; PE prefix did not route via the map (spawns=%+v)", peProject, spawns)
	}
	if pe.role != peRole {
		return fail("[pe] spawn role %q, want %q (role resolved against the MAPPED project)", pe.role, peRole)
	}
	dev, ok := byProject[devopsProject]
	if !ok {
		return fail("[devops] no spawn into %q; the SECOND prefix did not route to its own project (spawns=%+v)", devopsProject, spawns)
	}
	if dev.role != devopsRole {
		return fail("[devops] spawn role %q, want %q — routing regression would give the wrong project's role/creds", dev.role, devopsRole)
	}
	// The two mentions must NOT have collapsed onto the default single-project
	// fallback: a distinct role per prefix is the whole routing contract.
	if pe.role == dev.role {
		return fail("both prefixes resolved the same role %q — routing did not diverge by project", pe.role)
	}
	if _, leaked := byProject[fsmProject]; leaked {
		return fail("[fsm] a spawn landed in %q — the blocked prefix's map entry opened the spawn gate", fsmProject)
	}
	if _, leakedDefault := byProject[probeProject]; leakedDefault {
		return fail("a spawn fell back to the default project %q instead of routing via the map", probeProject)
	}

	// Task-bead routing: each spawn's mention task must carry project:<mapped>,
	// and the FSM prefix must have created NO task bead at all (denied before
	// CreateBead).
	creates := daemon.createCalls()
	var peTaskOK, devTaskOK bool
	for _, c := range creates {
		if !contains(c.labels, "jira-mention") {
			continue
		}
		switch c.fields["jira_key"] {
		case peKey:
			peTaskOK = contains(c.labels, "project:"+peProject)
		case devopsKey:
			devTaskOK = contains(c.labels, "project:"+devopsProject)
		case fsmKey:
			return fail("[fsm] a mention task bead was created for the blocked prefix (should be denied before CreateBead)")
		}
	}
	if !peTaskOK {
		return fail("[pe] mention task bead missing label project:%s", peProject)
	}
	if !devTaskOK {
		return fail("[devops] mention task bead missing label project:%s", devopsProject)
	}

	return pass("PE->%s/%s, DEVOPS->%s/%s (distinct role per prefix); blocked FSM prefix denied despite its map entry (no spawn, no task)",
		peProject, peRole, devopsProject, devopsRole)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 3: gb jira reply -> AddCommentWithVisibility(internal) on the agent's own
//        bound ticket, recorded by the stub Jira.
// ─────────────────────────────────────────────────────────────────────────────

func runReplyLeg(logger *slog.Logger) legResult {
	const (
		jiraKey   = "PE-52002"
		agentName = "jira-pe-52002-abcd"
		commentID = "cmt-52002"
	)
	stub := newStubJira()
	defer stub.Close()
	jira := stub.client(logger)

	daemon := newMockDaemon()
	daemon.agents[agentName] = &beadsapi.BeadDetail{
		ID:    "bd-" + agentName,
		Title: agentName,
		Fields: map[string]string{
			"jira_key":        jiraKey,
			"jira_comment_id": commentID,
			"role":            spawnRole,
		},
	}

	replyAPI := bridge.NewJiraReplyAPI(bridge.JiraReplyAPIConfig{
		Daemon:      daemon,
		Jira:        jira,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})
	mux := http.NewServeMux()
	replyAPI.RegisterRoutes(mux)

	// Default (no visibility) must post an INTERNAL note.
	code, body := postJSON(mux, "/api/jira/reply", map[string]any{
		"agent": agentName,
		"text":  "Looked into it — root cause is a stale cache; fix incoming.",
	})
	if code != http.StatusOK {
		return fail("reply returned HTTP %d (%s), want 200", code, strings.TrimSpace(body))
	}

	comments := stub.comments()
	if len(comments) != 1 {
		return fail("expected exactly 1 recorded Jira comment, got %d", len(comments))
	}
	c := comments[0]
	if c.key != jiraKey {
		return fail("reply posted to %q, want the bound ticket %q", c.key, jiraKey)
	}
	if !c.internal {
		return fail("reply was NOT internal (sd.public.comment internal:false) — internal is the default ceiling")
	}
	if !strings.Contains(c.text, agentName) {
		return fail("reply body missing agent attribution %q", agentName)
	}

	// A reply naming a DIFFERENT ticket must be refused (bound-ticket scoping).
	code2, _ := postJSON(mux, "/api/jira/reply", map[string]any{
		"agent":    agentName,
		"jira_key": "PE-99999",
		"text":     "should never post",
	})
	if code2 != http.StatusForbidden {
		return fail("reply to a non-bound ticket returned HTTP %d, want 403", code2)
	}
	if n := len(stub.comments()); n != 1 {
		return fail("a non-bound reply leaked a Jira write: %d comments recorded, want 1", n)
	}
	return pass("internal note recorded on bound ticket %s; cross-ticket reply refused (403, no write)", jiraKey)
}

// replyAPIForStub wires a JiraReplyAPI + mux against the given stub Jira for an
// agent bound to jiraKey, exactly as runReplyLeg does. Shared by the reclassify
// and code-mark legs so each drives the SAME production handler + client the
// happy-path reply leg does — only the stub's fault mode / the reply body vary.
func replyAPIForStub(logger *slog.Logger, stub *stubJira, agentName, jiraKey string) http.Handler {
	daemon := newMockDaemon()
	daemon.agents[agentName] = &beadsapi.BeadDetail{
		ID:    "bd-" + agentName,
		Title: agentName,
		Fields: map[string]string{
			"jira_key": jiraKey,
			"role":     spawnRole,
		},
	}
	replyAPI := bridge.NewJiraReplyAPI(bridge.JiraReplyAPIConfig{
		Daemon:      daemon,
		Jira:        stub.client(logger),
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})
	mux := http.NewServeMux()
	replyAPI.RegisterRoutes(mux)
	return mux
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg reply-reclassify: the reply handler's post-error CLASSIFICATION
//        (classifyReplyPostError, jira_reply.go:305) is only reachable when the
//        stub Jira can FAIL a comment POST — the base stub only ever 201s, so
//        the 422 branch was dead code from the probe's point of view (the exact
//        structural blindness ka-0000005455 was filed for: "the probe wires the
//        handler" is not coverage unless the probe can drive the handler into
//        the branch under test).
//
// A Jira 4xx (except 429) is a CONTENT rejection — the ADF the renderer built
// was invalid — so the reply endpoint must return 422 with an actionable "retry
// as plain text" message, telling the agent to downgrade to plain text rather
// than treat the bridge as down. Everything else — Jira 5xx AND 429 throttling —
// is a transient OUTAGE and must stay 502, so agents back off / retry instead of
// silently dropping to plain text.
//
// MANDATORY anti-vacuity control (per the task): assert 502 for a Jira 500 AND
// for a 429 alongside the 422 for a 400. Without both negatives a blanket-422
// regression (delete classifyReplyPostError, return 422 always) would pass — the
// 500/429 cases are what pin the classifier to the 4xx-only contract.
// ─────────────────────────────────────────────────────────────────────────────

func runReplyReclassifyLeg(logger *slog.Logger) legResult {
	const jiraKey = "PE-52422"

	// The request body deliberately carries a bold-code span (**`kubectl`**) — the
	// very markdown whose ADF Jira 400s in the wild — so the leg reads as the real
	// "unrenderable body" failure, not a synthetic one. The 400 here is injected
	// at the stub (the render itself is well-formed post-fix; see reply-codemark).
	replyText := "Try **`kubectl rollout restart`** — the pod is wedged."

	for _, tc := range []struct {
		name       string
		agentName  string
		failStatus int
		wantCode   int
		wantErrSub string
	}{
		// A Jira 400 (content rejection) -> 422 "retry as plain text".
		{"jira-400", "jira-pe-52422-badreq", http.StatusBadRequest, http.StatusUnprocessableEntity, "retry as plain text"},
		// A Jira 500 (upstream outage) -> 502 (NOT reclassified to 422).
		{"jira-500", "jira-pe-52422-outage", http.StatusInternalServerError, http.StatusBadGateway, ""},
		// A Jira 429 (throttling) -> 502 (429 is explicitly NOT a content rejection).
		{"jira-429", "jira-pe-52422-throttle", http.StatusTooManyRequests, http.StatusBadGateway, ""},
	} {
		if res := func() (r legResult) {
			r = pass("")
			stub := newStubJiraFailing(tc.failStatus)
			defer stub.Close()
			mux := replyAPIForStub(logger, stub, tc.agentName, jiraKey)

			code, body := postJSON(mux, "/api/jira/reply", map[string]any{
				"agent": tc.agentName,
				"text":  replyText,
			})
			if code != tc.wantCode {
				return fail("[%s] Jira %d -> reply HTTP %d, want %d (body: %s)",
					tc.name, tc.failStatus, code, tc.wantCode, strings.TrimSpace(body))
			}
			if tc.wantErrSub != "" && !strings.Contains(body, tc.wantErrSub) {
				return fail("[%s] reply body missing %q; got %q", tc.name, tc.wantErrSub, strings.TrimSpace(body))
			}
			// The rejected POST recorded NO comment — a fault is not a write.
			if n := len(stub.comments()); n != 0 {
				return fail("[%s] a rejected Jira POST recorded %d comment(s), want 0", tc.name, n)
			}
			return r
		}(); !res.ok {
			return res
		}
	}
	return pass("Jira 400 -> 422 \"retry as plain text\"; Jira 500 -> 502; Jira 429 -> 502 (outage NOT reclassified; anti-vacuity 502 controls green)")
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg reply-codemark: ADF code-mark exclusivity (appendCodeMark,
//        jira_adf_markdown.go:378). In the ADF schema the `code` mark excludes
//        every other formatting mark except `link`, so a text node carrying BOTH
//        `strong` and `code` — the ADF a bold code span (**`x`**) naively
//        produces — is what Jira Cloud 400s ("INVALID_INPUT ... marks"), and the
//        reply path surfaced that as a bare 502. The fix drops every mark except
//        `link` alongside `code`; this leg proves it end-to-end.
//
// The whole reply corpus reply body was plain prose, so appendCodeMark never ran
// under any harness (grep appendCodeMark over e2e -> 0; no bold-code fixture
// existed). This leg sends **`kubectl`** — a bold code span — through the REAL
// AddMarkdownCommentWithVisibilityAndSourceLink (the client the reply handler
// posts through) into the stub Jira, then inspects the recorded ADF: the
// backticked text node's marks must be EXACTLY [code] — no `strong`.
//
// Anti-vacuity: the SAME body ALSO carries a plain [text](url) link, and the leg
// asserts a link mark survives SOMEWHERE in the doc — so a regression that
// stripped ALL marks (rather than "all but link") would red here, not pass. And
// a code span nested under a link (a linked inline-code, [`x`](u)) must keep
// BOTH `code` and `link` on the one node — the sole coexistence the schema
// allows — which a naive "code is exclusive, drop everything" would break.
// ─────────────────────────────────────────────────────────────────────────────

func runReplyCodeMarkLeg(logger *slog.Logger) legResult {
	const (
		jiraKey   = "PE-52400"
		agentName = "jira-pe-52400-codemark"
	)
	stub := newStubJira()
	defer stub.Close()
	mux := replyAPIForStub(logger, stub, agentName, jiraKey)

	// A bold code span (must degrade to code-only), a bold word (strong must
	// survive on its OWN node — the fix only touches code nodes), a plain link
	// (link mark must survive), and a linked code span (code+link must coexist).
	replyText := "Run **`kubectl`** now; this is **important**. " +
		"See [the runbook](https://example.invalid/runbook) or [`gb jira reply`](https://example.invalid/gb)."

	code, body := postJSON(mux, "/api/jira/reply", map[string]any{
		"agent": agentName,
		"text":  replyText,
	})
	if code != http.StatusOK {
		return fail("reply returned HTTP %d (%s), want 200", code, strings.TrimSpace(body))
	}

	comments := stub.comments()
	if len(comments) != 1 {
		return fail("expected exactly 1 recorded Jira comment, got %d", len(comments))
	}
	nodes := adfTextNodes(comments[0].raw)
	if len(nodes) == 0 {
		return fail("recorded comment had no ADF text nodes; body=%q", comments[0].text)
	}

	// (1) The bold code span degraded to code-only: the `kubectl` node carries
	// `code` and NOT `strong`. This is the whole fix — a node with both is what
	// Jira 400s.
	var sawKubectl bool
	for _, n := range nodes {
		if n.text != "kubectl" {
			continue
		}
		sawKubectl = true
		if !n.hasMark("code") {
			return fail("the `kubectl` node lost its code mark; marks=%v", n.markTypes())
		}
		if n.hasMark("strong") {
			return fail("the `kubectl` node still carries `strong` alongside `code` — the exclusivity fix did not run (Jira would 400 this); marks=%v", n.markTypes())
		}
		// code coexists ONLY with link; nothing else may ride with it.
		for _, mt := range n.markTypes() {
			if mt != "code" && mt != "link" {
				return fail("the `kubectl` code node carries disallowed mark %q alongside `code`; marks=%v", mt, n.markTypes())
			}
		}
	}
	if !sawKubectl {
		return fail("no ADF text node with text \"kubectl\" was recorded; nodes=%v", summarizeNodes(nodes))
	}

	// (2) Anti-vacuity for "all but link": the plain `strong` word survived on its
	// own node (the fix is scoped to code nodes, it must not strip bold globally),
	// and a `link` mark survives SOMEWHERE (a fix that dropped ALL marks would red).
	var sawStrongOnly, sawLink, sawCodeLink bool
	for _, n := range nodes {
		if n.hasMark("strong") && !n.hasMark("code") {
			sawStrongOnly = true
		}
		if n.hasMark("link") {
			sawLink = true
		}
		if n.hasMark("code") && n.hasMark("link") {
			sawCodeLink = true
		}
	}
	if !sawStrongOnly {
		return fail("no plain `strong` node survived — the fix stripped bold globally, not just off code nodes")
	}
	if !sawLink {
		return fail("no `link` mark survived anywhere — a fix that dropped ALL marks alongside code would do this (the anti-vacuity control)")
	}
	// (3) code+link coexistence: the linked inline-code [`gb jira reply`](url) must
	// keep BOTH marks on the one node — the only pairing the schema allows.
	if !sawCodeLink {
		return fail("the linked code span lost its code+link pairing — appendCodeMark must KEEP link alongside code (the one allowed coexistence)")
	}

	return pass("bold code span **`kubectl`** degraded to code-only (no strong); plain bold + link survive; linked code keeps code+link — ADF code-mark exclusivity enforced")
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 4: a closed squawk bead from a Jira-bound agent -> internal note via the
//        SSE squawk forwarder (F1).
// ─────────────────────────────────────────────────────────────────────────────

func runSquawkLeg(logger *slog.Logger) legResult {
	const (
		jiraKey   = "PE-53003"
		agentName = "jira-pe-53003-wxyz"
	)
	stub := newStubJira()
	defer stub.Close()
	jira := stub.client(logger)

	daemon := newMockDaemon()
	daemon.agents[agentName] = &beadsapi.BeadDetail{
		ID:     "bd-" + agentName,
		Title:  agentName,
		Fields: map[string]string{"jira_key": jiraKey, "role": spawnRole},
	}

	forwarder := bridge.NewJiraSquawkForwarder(bridge.JiraSquawkForwarderConfig{
		Daemon:      daemon,
		Jira:        jira,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})

	// Drive the REAL SSE dispatch path: register on a real SSEStream fed by a
	// stub SSE server that emits one closed squawk bead, exactly as
	// cmd/jira-bridge/main.go wires it.
	sse := newStubSSE(squawkClosedEvent(agentName, "Deployed the fix; pipeline is green."))
	defer sse.Close()
	stream := bridge.NewSSEStream(bridge.SSEStreamConfig{BeadsHTTPAddr: sse.URL, Logger: logger})
	forwarder.RegisterHandlers(stream)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = stream.Start(ctx) }()

	if !waitFor(3*time.Second, func() bool { return len(stub.comments()) > 0 }) {
		return fail("squawk was not forwarded to Jira within 3s (SSE handler not wired / not firing)")
	}
	cancel()

	comments := stub.comments()
	if len(comments) != 1 {
		return fail("expected exactly 1 forwarded squawk comment, got %d", len(comments))
	}
	c := comments[0]
	if c.key != jiraKey {
		return fail("squawk forwarded to %q, want the agent's bound ticket %q", c.key, jiraKey)
	}
	if !c.internal {
		return fail("forwarded squawk was NOT an internal note")
	}
	if !strings.Contains(c.text, "pipeline is green") {
		return fail("forwarded comment missing squawk text; got %q", c.text)
	}
	return pass("closed squawk from %s forwarded as an internal note on %s", agentName, jiraKey)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 5: follow-up comment webhook -> live agent NUDGED; stopped agent RE-SPAWNED
//        under the same name (F3).
// ─────────────────────────────────────────────────────────────────────────────

func runFollowUpLeg(logger *slog.Logger) legResult {
	// 4a: LIVE agent — a follow-up comment nudges it (no respawn).
	liveRes := followUpLive(logger)
	if !liveRes.ok {
		return liveRes
	}
	// 4b: STOPPED agent — a follow-up comment re-spawns it under the same name.
	respawnRes := followUpRespawn(logger)
	if !respawnRes.ok {
		return respawnRes
	}
	return pass("%s; %s", liveRes.detail, respawnRes.detail)
}

func followUpLive(logger *slog.Logger) legResult {
	const (
		jiraKey   = "PE-54004"
		agentName = "jira-pe-54004-live"
		taskID    = "bd-task-54004"
	)
	daemon := newMockDaemon()
	daemon.seedSpawnTrigger(probeProject, spawnRole)
	// An ALIVE agent: active status, a coop_url in notes, no terminal state, on a
	// task bead carrying the jira_key so the webhook matches + delivers.
	daemon.agents[agentName] = &beadsapi.BeadDetail{
		ID:     "bd-" + agentName,
		Title:  agentName,
		Notes:  "coop_url: http://coop.invalid/agent",
		Fields: map[string]string{"jira_key": jiraKey, "agent": agentName, "role": spawnRole},
	}
	daemon.tasks = append(daemon.tasks, &beadsapi.BeadDetail{
		ID:       taskID,
		Type:     "task",
		Status:   "in_progress",
		Assignee: agentName,
		Labels:   []string{"project:" + probeProject, "jira-mention"},
		Fields:   map[string]string{"jira_key": jiraKey, "spawn_source": "jira-mention"},
	})

	var nudged []string
	var nudgeMsgs []string
	var nmu sync.Mutex
	resolver := bridge.NewJiraAgentResolver(bridge.JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: logger,
		Nudge: func(_ context.Context, agent, msg string) error {
			nmu.Lock()
			defer nmu.Unlock()
			nudged = append(nudged, agent)
			nudgeMsgs = append(nudgeMsgs, msg)
			return nil
		},
	})
	handler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:      daemon,
		DevMode:     true,
		Resolver:    resolver,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})

	code, _ := postWebhook(handler, commentCreatedPayload(jiraKey, "cmt-54004",
		"any update on this?", "Ada Commenter", "acct-ada"))
	if code != http.StatusOK {
		return fail("[live] follow-up webhook HTTP %d, want 200", code)
	}
	nmu.Lock()
	gotNudges := append([]string(nil), nudged...)
	gotMsgs := append([]string(nil), nudgeMsgs...)
	nmu.Unlock()
	if len(gotNudges) != 1 || gotNudges[0] != agentName {
		return fail("[live] expected exactly 1 nudge to %s, got %v", agentName, gotNudges)
	}
	if n := len(daemon.spawnCalls()); n != 0 {
		return fail("[live] a live agent was RE-SPAWNED (%d spawns) instead of nudged", n)
	}

	// The nudge MESSAGE (buildJiraCommentNudgeMessage -> delivered to the agent)
	// must advertise both re-read affordances so a nudged agent is TOLD it can
	// refresh the stale ticket snapshot (commit 1a8ad32d1). As in the mention
	// leg, assert the INTERPOLATED bound key `gb jira show PE-54004` (not a bare
	// `gb jira show`) so a regression that drops the %s reds. The probe formerly
	// DISCARDED this message (the Nudge callback's 3rd param was `_`).
	msg := gotMsgs[0]
	if want := "gb jira show " + jiraKey; !strings.Contains(msg, want) {
		return fail("[live] nudge message missing %q (the key-interpolated re-read affordance); got %q", want, msg)
	}
	if !strings.Contains(msg, "gb jira thread") {
		return fail("[live] nudge message missing `gb jira thread` (the conversation re-read affordance); got %q", msg)
	}
	return pass("live agent %s nudged (no respawn); nudge advertises `gb jira show %s` + `gb jira thread`", gotNudges[0], jiraKey)
}

func followUpRespawn(logger *slog.Logger) legResult {
	const (
		jiraKey   = "PE-54005"
		agentName = "jira-pe-54005-stop"
		taskID    = "bd-task-54005"
		commentID = "cmt-54005"
	)
	daemon := newMockDaemon()
	daemon.seedSpawnTrigger(probeProject, spawnRole)
	// A STOPPED agent: terminal agent_state=done, so the resolver must close it
	// and re-spawn under the same name (session resume).
	daemon.agents[agentName] = &beadsapi.BeadDetail{
		ID:     "bd-" + agentName,
		Title:  agentName,
		Fields: map[string]string{"jira_key": jiraKey, "agent": agentName, "agent_state": "done", "role": spawnRole},
	}
	// The follow-up usually lands after the task closed (F3 case).
	daemon.tasks = append(daemon.tasks, &beadsapi.BeadDetail{
		ID:       taskID,
		Type:     "task",
		Status:   "closed",
		Assignee: agentName,
		Labels:   []string{"project:" + probeProject, "jira-mention"},
		Fields:   map[string]string{"jira_key": jiraKey, "spawn_source": "jira-mention", "operated_by": "slack:U07FOLLOWUP"},
	})

	resolver := bridge.NewJiraAgentResolver(bridge.JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: logger,
		Nudge:  func(_ context.Context, _, _ string) error { return nil },
	})
	handler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:      daemon,
		DevMode:     true,
		Resolver:    resolver,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})

	code, _ := postWebhook(handler, commentCreatedPayload(jiraKey, commentID,
		"reopening — the fix regressed", "Ada Commenter", "acct-ada"))
	if code != http.StatusOK {
		return fail("[respawn] follow-up webhook HTTP %d, want 200", code)
	}

	spawns := daemon.spawnCalls()
	if len(spawns) != 1 {
		return fail("[respawn] expected exactly 1 respawn, got %d", len(spawns))
	}
	sp := spawns[0]
	if sp.agentName != agentName {
		return fail("[respawn] respawned as %q, want the SAME name %q (session resume)", sp.agentName, agentName)
	}
	if got := sp.fields["jira_comment_id"]; got != commentID {
		return fail("[respawn] respawned agent jira_comment_id=%q, want %q (thread-back anchor)", got, commentID)
	}
	// The stale terminal bead must be closed before the same-name respawn, or the
	// name-dedup would swallow it and no pod would ever be minted.
	if !daemon.wasClosed("bd-" + agentName) {
		return fail("[respawn] stale terminal agent bead was NOT closed before respawn")
	}
	// The human operator stamped on the task bead must be inherited.
	if got := sp.fields["operated_by"]; got != "slack:U07FOLLOWUP" {
		return fail("[respawn] respawn did not inherit operator; operated_by=%q", got)
	}
	return pass("stopped agent %s re-spawned under same name (operator inherited, stale bead closed)", agentName)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 6: FSM-key webhook (mention + reply + squawk) yields ZERO outbound Jira
//        writes AND ZERO spawn. FSM is not a writable project in any mode.
// ─────────────────────────────────────────────────────────────────────────────

func runFSMNegativeLeg(logger *slog.Logger) legResult {
	const (
		fsmKey    = "FSM-55005"
		agentName = "jira-fsm-55005-zzzz"
	)
	stub := newStubJira()
	defer stub.Close()
	jira := stub.client(logger)
	wp := bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger)

	// 5a: an FSM @gardenio mention must NOT spawn an agent (project gate).
	mentionDaemon := newMockDaemon()
	mentionDaemon.seedSpawnTrigger(probeProject, spawnRole)
	mention := bridge.NewJiraMentionHandler(bridge.JiraMentionHandlerConfig{
		Daemon:      mentionDaemon,
		Users:       probeMentionUsers{},
		Project:     probeProject,
		WritePolicy: wp,
		Logger:      logger,
	})
	mentionHandler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:         mentionDaemon,
		DevMode:        true,
		MentionHandler: mention,
		WritePolicy:    wp,
		Logger:         logger,
	})
	code, _ := postWebhook(mentionHandler, commentCreatedPayload(fsmKey, "cmt-fsm-1",
		mentionTrigger+" customer is asking for an update", "Ext Customer", "acct-cust"))
	if code != http.StatusOK {
		return fail("[fsm] mention webhook HTTP %d, want 200 (blocked-but-handled)", code)
	}
	if n := len(mentionDaemon.spawnCalls()); n != 0 {
		return fail("[fsm] FSM mention SPAWNED an agent (%d spawns) — the project gate is open", n)
	}

	// 5b: `gb jira reply` bound to an FSM ticket must be refused with ZERO write.
	replyDaemon := newMockDaemon()
	replyDaemon.agents[agentName] = &beadsapi.BeadDetail{
		ID:     "bd-" + agentName,
		Title:  agentName,
		Fields: map[string]string{"jira_key": fsmKey, "role": spawnRole},
	}
	replyAPI := bridge.NewJiraReplyAPI(bridge.JiraReplyAPIConfig{
		Daemon: replyDaemon, Jira: jira, WritePolicy: wp, Logger: logger,
	})
	mux := http.NewServeMux()
	replyAPI.RegisterRoutes(mux)
	rcode, _ := postJSON(mux, "/api/jira/reply", map[string]any{
		"agent": agentName, "text": "internal note attempt on FSM",
	})
	if rcode != http.StatusForbidden {
		return fail("[fsm] reply to an FSM ticket returned HTTP %d, want 403", rcode)
	}

	// 5c: a squawk from an FSM-bound agent must NOT be forwarded. Drive it
	// through the REAL SSE dispatch path (same as leg 3) and assert nothing lands.
	forwarder := bridge.NewJiraSquawkForwarder(bridge.JiraSquawkForwarderConfig{
		Daemon: replyDaemon, Jira: jira, WritePolicy: wp, Logger: logger,
	})
	sse := newStubSSE(squawkClosedEvent(agentName, "trying to speak on the FSM ticket"))
	defer sse.Close()
	stream := bridge.NewSSEStream(bridge.SSEStreamConfig{BeadsHTTPAddr: sse.URL, Logger: logger})
	// Register a completion sentinel on the SAME topic so we can tell the event
	// was actually dispatched to handlers before asserting the negative (a squawk
	// that "didn't post" only proves the gate if the event truly reached the
	// forwarder). Handlers run concurrently per dispatch and complete together.
	dispatched := make(chan struct{}, 1)
	stream.On("beads.bead.closed", func(_ context.Context, _ []byte) {
		select {
		case dispatched <- struct{}{}:
		default:
		}
	})
	forwarder.RegisterHandlers(stream)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = stream.Start(ctx) }()
	select {
	case <-dispatched:
	case <-time.After(3 * time.Second):
		return fail("[fsm] squawk event never reached the SSE handlers — negative is vacuous")
	}
	// Give the forwarder's own (concurrent) handler a beat to finish any write it
	// would have made, so the zero-writes assertion is not racing the dispatch.
	time.Sleep(200 * time.Millisecond)
	cancel()

	if n := len(stub.comments()); n != 0 {
		return fail("[fsm] %d outbound Jira write(s) escaped to FSM — the F5 spine is breached", n)
	}
	return pass("FSM mention did not spawn; FSM reply refused (403); FSM squawk not forwarded; 0 outbound writes")
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 7: JiraSpawnAcknowledger — a @gardenio mention with the acknowledger wired
//        onto OnSpawn (exactly as cmd/jira-bridge does it) posts ONE immediate
//        internal-note "👋 On it" comment to the ticket, the Jira analog of the
//        Slack agent card (ka-0000003551; feature ka-0000001164, plan
//        ka-0000007401). Asserts the recorded note is (a) an internal note on the
//        mentioned ticket, (b) naming the spawned agent + tracking bead, and
//        (c) carrying the operator attribution line (the author maps to a
//        Slack user, so operatorAttributionLine is non-empty — otherwise a jira:<acct>
//        author would resolve to a non-human sentinel and the line would be "").
//
// ANTI-VACUITY (bead ka-0000003551): the leg re-drives the SAME mention with
// OnSpawn left nil (the legacy wiring, before feature ka-0000001164) against a
// fresh stub Jira and asserts ZERO recorded writes. This is the in-probe negative
// control the bead asks for: it proves the note in the positive case came from
// the acknowledger wired onto OnSpawn — not from a latent write on the mention
// path — so a green assertion is not vacuous. (The mention handler holds no
// *JiraClient by construction, TestJiraMention_NoJiraWriteSurface; this control
// makes that structural fact observable end-to-end.)
// ─────────────────────────────────────────────────────────────────────────────

func runSpawnAckLeg(logger *slog.Logger) legResult {
	const (
		jiraKey   = "PE-56006"
		commentID = "cmt-56006"
		// A human-shaped Slack operator so operatorAttributionLine stamps a real
		// "Acted by …" line. The webhook author maps (probeMentionUsers) to a
		// user bead with this Slack id and the mention capability —
		// the SAME path production takes for an unmapped mention author.
		authorSlackID  = "U07SPAWNACK"
		authorOperator = "slack:" + authorSlackID
	)

	// ── Positive case: OnSpawn wired to the acknowledger => one internal note ──
	stub := newStubJira()
	defer stub.Close()
	jira := stub.client(logger)

	daemon := newMockDaemon()
	daemon.seedSpawnTrigger(probeProject, spawnRole)

	// Wire the acknowledger EXACTLY as cmd/jira-bridge/main.go does: a separate
	// collaborator holding the *JiraClient, gated by the SAME shared write policy,
	// adapted onto JiraMentionHandlerConfig.OnSpawn via its OnSpawn() method.
	wp := bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger)
	spawnAck := bridge.NewJiraSpawnAcknowledger(bridge.JiraSpawnAcknowledgerConfig{
		Jira:        jira,
		WritePolicy: wp,
		Logger:      logger,
	})
	mention := bridge.NewJiraMentionHandler(bridge.JiraMentionHandlerConfig{
		Daemon:      daemon,
		Project:     probeProject,
		WritePolicy: wp,
		Users:       probeMentionUsers{slackID: authorSlackID},
		OnSpawn:     spawnAck.OnSpawn(),
		Logger:      logger,
	})
	handler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:         daemon,
		DevMode:        true,
		MentionHandler: mention,
		WritePolicy:    wp,
		Logger:         logger,
	})

	code, _ := postWebhook(handler, commentCreatedPayload(jiraKey, commentID,
		mentionTrigger+" please investigate the failing build", "Ada Commenter", "acct-ada"))
	if code != http.StatusOK {
		return fail("webhook returned HTTP %d, want 200", code)
	}

	// The mention must have spawned (the ack fires on a SUCCESSFUL spawn only).
	spawns := daemon.spawnCalls()
	if len(spawns) != 1 {
		return fail("expected exactly 1 spawn before the ack, got %d", len(spawns))
	}
	agentName := spawns[0].agentName

	comments := stub.comments()
	if len(comments) != 1 {
		return fail("expected exactly 1 spawn-ack Jira comment, got %d", len(comments))
	}
	c := comments[0]
	if c.key != jiraKey {
		return fail("ack posted to %q, want the mentioned ticket %q", c.key, jiraKey)
	}
	if !c.internal {
		return fail("spawn-ack was NOT an internal note (sd.public.comment internal:false) — an ack is always internal")
	}
	// (a) it reads as an acknowledgment ("On it").
	if !strings.Contains(c.text, "On it") {
		return fail("ack body missing the \"On it\" acknowledgment; got %q", c.text)
	}
	// (b) it names the spawned agent AND the tracking bead so a human on the
	// ticket can follow the work.
	if !strings.Contains(c.text, agentName) {
		return fail("ack body missing the spawned agent name %q; got %q", agentName, c.text)
	}
	if !strings.Contains(c.text, "Tracking bead:") {
		return fail("ack body missing the tracking-bead reference; got %q", c.text)
	}
	// (c) the operator attribution line — the raw human operated_by id MUST appear
	// verbatim (the audit token the outbox harness greps for). This is the whole
	// reason the author maps to a Slack user: an unmapped author is refused at
	// the mention gate (gd-fBBMiDHb07) and nothing would spawn or post.
	if !strings.Contains(c.text, "Acted by") || !strings.Contains(c.text, authorOperator) {
		return fail("ack body missing operator attribution %q; got %q", authorOperator, c.text)
	}

	// ── Negative control (anti-vacuity): SAME mention, OnSpawn nil => 0 writes ──
	// This is the control the bead requires: it proves the note above came from
	// the acknowledger on OnSpawn, not from a latent mention-path write.
	ctrlStub := newStubJira()
	defer ctrlStub.Close()
	ctrlDaemon := newMockDaemon()
	ctrlDaemon.seedSpawnTrigger(probeProject, spawnRole)
	ctrlMention := bridge.NewJiraMentionHandler(bridge.JiraMentionHandlerConfig{
		Daemon:      ctrlDaemon,
		Project:     probeProject,
		WritePolicy: wp,
		Users:       probeMentionUsers{slackID: authorSlackID},
		// OnSpawn deliberately omitted (nil) — the legacy wiring before the ack.
		Logger: logger,
	})
	ctrlHandler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:         ctrlDaemon,
		DevMode:        true,
		MentionHandler: ctrlMention,
		WritePolicy:    wp,
		Logger:         logger,
	})
	if code, _ := postWebhook(ctrlHandler, commentCreatedPayload(jiraKey, commentID,
		mentionTrigger+" please investigate the failing build", "Ada Commenter", "acct-ada")); code != http.StatusOK {
		return fail("[control] webhook returned HTTP %d, want 200", code)
	}
	if n := len(ctrlDaemon.spawnCalls()); n != 1 {
		return fail("[control] expected the mention to still spawn (1), got %d — control is not exercising the same path", n)
	}
	if n := len(ctrlStub.comments()); n != 0 {
		return fail("[control] OnSpawn nil recorded %d Jira write(s) — the ack is NOT the sole source of the note; positive leg is vacuous", n)
	}

	return pass("spawn-ack posted 1 internal note on %s (agent %s + tracking bead + attribution %q); OnSpawn-nil control recorded 0 writes (spawned but silent)",
		jiraKey, agentName, authorOperator)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 8: GitLab crosslink comment (ka-0000005445; jira_webhook.go:447). The
//        GitLab⇄Jira integration posts an automated "<user> mentioned this issue
//        in a merge request of <project>" notice whenever ANY branch cites the
//        ticket. The bridge MUST bridge that comment to the bead but MUST NOT
//        wake the agent — no nudge, no F3 respawn — or a finished agent
//        resurrects itself off its own MR in a self-sustaining loop (observed on
//        DEVOPS-738: the same agent respawned 3x in 64 minutes off its
//        predecessors' MRs). A real HUMAN follow-up on the same closed ticket
//        still respawns. This leg drives the REAL JiraWebhookHandler + resolver,
//        the live wiring the mocked-handler unit tests (jira_webhook_test.go
//        TestJiraWebhookHandler_GitLabCrosslink_NoWake) cannot reach.
// ─────────────────────────────────────────────────────────────────────────────

func runCrosslinkNoWakeLeg(logger *slog.Logger) legResult {
	// ── Jira Cloud shape (ADF-flattened): adfToPlainText has already stripped
	//    the link marks, so the marker object arrives as bare text. Cover two of
	//    the four phrasings (merge request + commit); each must suppress the wake
	//    while STILL bridging the comment.
	mrRes := crosslinkNoWake(logger, "mr", "PE-57007", "cmt-57007-mr",
		"Pi Health Root mentioned this issue in a merge request of acme/platform/gardenio: See merge request acme/platform/gardenio!6789")
	if !mrRes.ok {
		return mrRes
	}
	commitRes := crosslinkNoWake(logger, "commit", "PE-57008", "cmt-57008-commit",
		"Pi Health Root mentioned this issue in a commit of acme/platform/gardenio: the fix branch cites the ticket")
	if !commitRes.ok {
		return commitRes
	}

	// ── Jira Server shape (WIKI MARKUP — the REAL prod body that HID
	//    ka-0000006177): GitLab's Server integration wraps the object phrase in a
	//    [display|url] link, so "[a merge request|url]" splits the marker phrase
	//    "mentioned this issue in a merge request" and a raw substring scan
	//    misses it — the wake fired even with the guard deployed. The guard MUST
	//    flatten wiki links (flattenJiraWikiLinks) before matching. These two
	//    cases go RED against an UNFLATTENED classifier while the flattened Cloud
	//    cases above stay GREEN; that divergence is the coverage this backfill
	//    adds (mutation proof in the MR). The bodies are the on-the-wire prod
	//    shape: link-wrapped actor, object phrase, and project. ──
	mrWikiRes := crosslinkNoWake(logger, "mr-wiki", "PE-57010", "cmt-57010-mr-wiki",
		"[Gardenio Agent|https://gitlab.com/gardeniobot] mentioned this issue in [a merge request|https://gitlab.com/example-org/gardenio/-/merge_requests/6789] of [Example Org / gardenio|https://gitlab.com/example-org/gardenio]")
	if !mrWikiRes.ok {
		return mrWikiRes
	}
	commitWikiRes := crosslinkNoWake(logger, "commit-wiki", "PE-57011", "cmt-57011-commit-wiki",
		"[Gardenio Agent|https://gitlab.com/gardeniobot] mentioned this issue in [a commit|https://gitlab.com/example-org/gardenio/-/commit/abc1234def] of [Example Org / gardenio|https://gitlab.com/example-org/gardenio]")
	if !commitWikiRes.ok {
		return commitWikiRes
	}

	// Anti-vacuity positive control #1 (MANDATORY): the SAME closed-task +
	// terminal-agent fixture, but a HUMAN body that carries the verb without any
	// marker object, must STILL respawn exactly once. Without it a globally-dead
	// respawn path would pass the suppression asserts vacuously.
	ctrlRes := crosslinkHumanRespawns(logger, "control", "PE-57009",
		"jira-crosslink-human-stop", "bd-task-crosslink-human", "cmt-57009", "Ada Commenter", "acct-ada",
		"I mentioned this issue in standup — the fix regressed, please take another look.")
	if !ctrlRes.ok {
		return ctrlRes
	}

	// Anti-vacuity positive control #2 (MANDATORY for the wiki path): a HUMAN
	// body that CONTAINS a wiki link whose display text is a marker object noun
	// ("a merge request") but carries NO "mentioned this issue in …" verb. After
	// flattening it reads "did a merge request for this land yet?" — not a
	// crosslink. A naive "strip all brackets then look for the object noun"
	// classifier gets this WRONG and would suppress the wake, starving the
	// ticket; the correct guard keys on the FULL marker phrase, so this must
	// respawn exactly once. This is the case that earns the wiki flattening its
	// keep: it separates "flatten then substring the phrase" (correct) from
	// "flatten then hunt for the object noun" (broken).
	ctrlWikiRes := crosslinkHumanRespawns(logger, "control-wiki", "PE-57012",
		"jira-crosslink-humanwiki-stop", "bd-task-crosslink-humanwiki", "cmt-57012", "Bo Reviewer", "acct-bo",
		"did [a merge request|https://gitlab.com/example-org/gardenio/-/merge_requests/6789] for this land yet?")
	if !ctrlWikiRes.ok {
		return ctrlWikiRes
	}

	return pass("%s; %s; %s; %s; %s; %s", mrRes.detail, commitRes.detail,
		mrWikiRes.detail, commitWikiRes.detail, ctrlRes.detail, ctrlWikiRes.detail)
}

// seedCrosslinkFixture builds the F3 shape the crosslink guard must veto: a
// CLOSED task bead carrying jiraKey whose assigned agent is TERMINAL
// (agent_state=done). A human follow-up on THIS fixture would close the stale
// bead and re-spawn under the same name (see the followup leg's respawn case) —
// which is exactly why the crosslink must not.
func seedCrosslinkFixture(daemon *mockDaemon, jiraKey, agentName, taskID string) {
	daemon.seedSpawnTrigger(probeProject, spawnRole)
	daemon.agents[agentName] = &beadsapi.BeadDetail{
		ID:     "bd-" + agentName,
		Title:  agentName,
		Fields: map[string]string{"jira_key": jiraKey, "agent": agentName, "agent_state": "done", "role": spawnRole},
	}
	daemon.tasks = append(daemon.tasks, &beadsapi.BeadDetail{
		ID:       taskID,
		Type:     "task",
		Status:   "closed",
		Assignee: agentName,
		Labels:   []string{"project:" + probeProject, "jira-mention"},
		Fields:   map[string]string{"jira_key": jiraKey, "spawn_source": "jira-mention", "operated_by": "slack:U07CROSSLINK"},
	})
}

// crosslinkNoWake drives one GitLab crosslink body against the F3 fixture and
// asserts the crosslink is BRIDGED (comment count == 1) yet wakes NOTHING (0
// spawns, 0 nudges) — proving the suppression is wake-only, not a drop.
func crosslinkNoWake(logger *slog.Logger, label, jiraKey, commentID, body string) legResult {
	agentName := "jira-crosslink-" + label + "-stop"
	taskID := "bd-task-crosslink-" + label

	daemon := newMockDaemon()
	seedCrosslinkFixture(daemon, jiraKey, agentName, taskID)

	var nudged []string
	var nmu sync.Mutex
	resolver := bridge.NewJiraAgentResolver(bridge.JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: logger,
		Nudge: func(_ context.Context, agent, _ string) error {
			nmu.Lock()
			defer nmu.Unlock()
			nudged = append(nudged, agent)
			return nil
		},
	})
	handler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:      daemon,
		DevMode:     true,
		Resolver:    resolver,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})

	// The crosslink is posted under whichever account owns the GitLab⇄Jira
	// integration (on prod "Pi Health Root", a human-looking shared account), so
	// author identity cannot separate it — the body marker is what classifies it.
	code, _ := postWebhook(handler, commentCreatedPayload(jiraKey, commentID, body,
		"Pi Health Root", "acct-root"))
	if code != http.StatusOK {
		return fail("[%s] crosslink webhook HTTP %d, want 200", label, code)
	}

	// (1) Zero spawns AND zero nudges — the wake is suppressed.
	if n := len(daemon.spawnCalls()); n != 0 {
		return fail("[%s] crosslink RE-SPAWNED a terminal agent (%d spawns) — the wake was not suppressed", label, n)
	}
	nmu.Lock()
	gotNudges := len(nudged)
	nmu.Unlock()
	if gotNudges != 0 {
		return fail("[%s] crosslink NUDGED the agent (%d nudges) — the wake was not suppressed", label, gotNudges)
	}
	// (2) The crosslink IS still bridged — suppression is wake-only, not a drop.
	if n := daemon.commentsFor(taskID); n != 1 {
		return fail("[%s] crosslink comment count on %s = %d, want exactly 1 (must be bridged, not dropped)", label, taskID, n)
	}
	// A respawn would have closed the stale terminal bead first; it must not.
	if daemon.wasClosed("bd-" + agentName) {
		return fail("[%s] the terminal agent bead was closed — a respawn was attempted despite the crosslink guard", label)
	}
	return pass("[%s] crosslink bridged (1 comment) with 0 spawns/0 nudges", label)
}

// crosslinkHumanRespawns drives a HUMAN comment (author + body supplied) against
// the SAME closed-task + terminal-agent F3 fixture and asserts it is NOT
// classified as a crosslink: the F3 respawn fires exactly once, under the same
// agent name (session resume). It backs the mandatory anti-vacuity controls —
// a globally-dead respawn path would make the suppression asserts vacuous. Two
// bodies exercise it: the flattened-verb-no-object control, and the wiki-link
// control whose flattened text carries a marker OBJECT noun but no marker VERB,
// so a classifier that hunts the noun (not the full phrase) wrongly suppresses.
func crosslinkHumanRespawns(logger *slog.Logger, label, jiraKey, agentName, taskID, commentID, author, authorAcct, body string) legResult {
	daemon := newMockDaemon()
	seedCrosslinkFixture(daemon, jiraKey, agentName, taskID)

	resolver := bridge.NewJiraAgentResolver(bridge.JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: logger,
		Nudge:  func(_ context.Context, _, _ string) error { return nil },
	})
	handler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:      daemon,
		DevMode:     true,
		Resolver:    resolver,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})

	code, _ := postWebhook(handler, commentCreatedPayload(jiraKey, commentID, body, author, authorAcct))
	if code != http.StatusOK {
		return fail("[%s] human follow-up webhook HTTP %d, want 200", label, code)
	}
	spawns := daemon.spawnCalls()
	if len(spawns) != 1 {
		return fail("[%s] a HUMAN follow-up produced %d respawns, want exactly 1 — the respawn path is globally dead, so the suppression asserts are vacuous", label, len(spawns))
	}
	if spawns[0].agentName != agentName {
		return fail("[%s] respawned as %q, want the SAME name %q (session resume)", label, spawns[0].agentName, agentName)
	}
	return pass("[%s] human body (not a crosslink) respawned %s exactly once", label, agentName)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 9: bridge-generated comment (ka-0000004998; jira_webhook.go:461). The
//        bridge posts its OWN Jira comments from jira_sync.go's three
//        withAttribution call sites — the claim notice, "Automated MR created",
//        and "MR merged". Each must be BRIDGED to the bead but wake NOTHING — 0
//        spawns, 0 nudges — even against a CLOSED task whose agent is terminal
//        (the exact F3 respawn shape). Otherwise the agent wakes on its OWN
//        notice, acts, posts another notice, and wakes again — the
//        self-sustaining loop observed live on DEVOPS-897 (a re-opened bead's
//        mr_url re-posted "Automated MR created", respawning the assignee 2s
//        later; it could not land protected main, so the sweep re-opened the
//        bead and it repeated).
//
// ── Why this drives the REAL producer, and why that matters ──────────────────
//
// The bodies are NOT hand-authored from the matcher table. This leg stands up
// the REAL jira-SYNC producer (bridge.NewJiraSync) behind the REAL SSE stream
// and fires the bead events that make it emit each of the three comments; a
// capturing stub Jira records the EXACT bytes jira_sync POSTed (generated
// sentence + the withAttribution "Acted by <operator>" line + the ADF
// hardBreak encoding). Those captured bytes are then replayed through the REAL
// webhook. So producer wording and matcher table are COUPLED end-to-end: a
// drift on EITHER side (someone edits the claim sentence in jira_sync.go, or
// narrows bridgeGeneratedMarkers in jira_webhook.go) desynchronizes them and
// reds this leg. A leg that hand-copied the body from the matcher could not
// catch a producer-side drift — the exact trap the sibling backfill
// ka-0000000099 was filed for (advice ka-0000001521: assert against the shape
// prod actually EMITS).
//
// ── Both delivery shapes (the ka-0000000099 / ka-0000006177 shape guardrail) ──
//
// Prod runs Jira SERVER, which delivers a comment_created body as a plain JSON
// STRING (wiki-flavored text). Jira CLOUD delivers the same comment as an ADF
// object. The webhook flattens each via jiraCommentBodyToText, and the two
// flatteners are DIFFERENT code (a bare string vs adfToPlainText, which joins
// ADF paragraphs with a space and strips the blank line withAttribution
// inserts). This leg replays each captured body in BOTH shapes:
//   - Server: the producer's flattened text as a JSON string.
//   - Cloud:  the producer's raw ADF bytes verbatim.
// so the guard is proven against the shape prod ACTUALLY emits, not just the
// one that happens to be convenient — the precise omission that let leg 8's
// sibling ship blind to its own live bug.
//
// ── Mandatory anti-vacuity controls (the assertions that earn their keep) ────
//
//  1. A HUMAN who QUOTES a generated sentence back ("why did \"Automated MR
//     created\" fire twice?") MUST still wake. A naive strings.Contains fails
//     this; the marker is anchored as a PREFIX, so a quote mid-sentence is not
//     suppressed.
//  2. The claim notice is matched as prefix AND suffix because the agent name
//     sits in the middle ("Gardenio agent <name> is working on this issue."). So
//     a human OPENING with "Gardenio agent please take another look" shares the
//     prefix but not the suffix and MUST still wake — silently dropping that is
//     the worst failure this guard can have.
//  3. A plain human follow-up ("any update on this?") respawns exactly once, so
//     a globally-dead respawn path cannot pass the suppression asserts
//     vacuously (the same positive control the crosslink leg carries).
//  4. A HUMAN who QUOTES the CLOSURE sentence back mid-body ("the bot posted
//     \"Task bead closed in beads system (bead …)\" but the ticket is still
//     open") MUST still wake. The closure marker (jira_webhook.go:740) has an
//     EMPTY suffix, so it is matched by PREFIX alone — this control is what
//     distinguishes "closure notice suppressed" from the plausible bad fix "a
//     suffix-less prefix match eats everything downstream that contains the
//     sentence". A mid-body quote does not share the prefix, so HasPrefix lets
//     it wake; a naive Contains would wrongly suppress it.
//
// ── Note on ka-0000004442 (LANDED: closure notice now in the table) ──
//
// jira_sync.go's handleClosed posts a FOURTH generated comment — "Task bead
// closed in beads system (bead <id>)." — and 77e7e0f82 (ka-0000004442) added it
// to bridgeGeneratedMarkers (jira_webhook.go:740), so the bridge now BRIDGES it
// but suppresses the wake, exactly like its three siblings. This leg captures
// the closure body from the same real producer and asserts that suppression in
// BOTH Server+Cloud shapes (bridged, 0 wakes) — the fixed behavior. Because the
// closure marker has an empty suffix, the mandatory control #4 above pins the
// prefix-vs-contains boundary so an over-broad marker table cannot pass.
// ─────────────────────────────────────────────────────────────────────────────

func runBridgeNoWakeLeg(logger *slog.Logger) legResult {
	// Capture each of the FOUR generated bodies from the REAL producer.
	claim, err := produceGeneratedBody(logger, genClaim)
	if err != nil {
		return fail("producing claim notice from the real jira-sync producer: %v", err)
	}
	mrCreated, err := produceGeneratedBody(logger, genMRCreated)
	if err != nil {
		return fail("producing 'Automated MR created' from the real jira-sync producer: %v", err)
	}
	mrMerged, err := produceGeneratedBody(logger, genMRMerged)
	if err != nil {
		return fail("producing 'MR merged' from the real jira-sync producer: %v", err)
	}
	// ka-0000004442 landed: the bead-closure notice is now in
	// bridgeGeneratedMarkers (jira_webhook.go:740), so it is suppressed like its
	// three siblings. Capture it from the SAME real producer and prove the
	// suppression end-to-end (advice ka-0000001521: assert the shape prod emits).
	closure, err := produceGeneratedBody(logger, genClose)
	if err != nil {
		return fail("producing closure notice from the real jira-sync producer: %v", err)
	}

	// Belt-and-suspenders that the capture actually caught the right sentence —
	// a silently-empty capture (e.g. a gate that dropped the write) would make
	// every suppression assertion below vacuously green.
	for _, c := range []struct {
		label, want, got string
	}{
		{"claim", "is working on this issue.", claim.text},
		{"mr-created", "Automated MR created:", mrCreated.text},
		{"mr-merged", "MR merged", mrMerged.text},
		{"closure", "Task bead closed in beads system", closure.text},
	} {
		if !strings.Contains(c.got, c.want) {
			return fail("captured %s body %q does not contain %q — the real producer did not emit the expected notice; suppression asserts would be vacuous", c.label, c.got, c.want)
		}
	}
	// The captured bodies must ALSO carry the withAttribution line, or we are not
	// exercising the real on-the-wire shape (the "Acted by …" line is what the
	// matcher strips before a suffix match — a body without it is a weaker test).
	for _, c := range []struct{ label, got string }{
		{"claim", claim.text}, {"mr-created", mrCreated.text}, {"mr-merged", mrMerged.text}, {"closure", closure.text},
	} {
		if !strings.Contains(c.got, "Acted by") {
			return fail("captured %s body %q lacks the 'Acted by' attribution line — the producer path did not stamp it; the leg is not replaying the real shape", c.label, c.got)
		}
	}

	// Each generated body must be suppressed in BOTH delivery shapes.
	for _, g := range []struct {
		label   string
		jiraKey string
		body    producedBody
	}{
		{"claim", "PE-58001", claim},
		{"mr-created", "PE-58002", mrCreated},
		{"mr-merged", "PE-58003", mrMerged},
		{"closure", "PE-58007", closure},
	} {
		// Jira Server shape: the producer's flattened text, delivered as a JSON string.
		serverRes := bridgeNoWake(logger, g.label+"-server", g.jiraKey+"-srv",
			commentCreatedPayload(g.jiraKey+"-srv", "cmt-"+g.label+"-srv", g.body.text,
				"Automated Gardenio Agent", "acct-bot"))
		if !serverRes.ok {
			return serverRes
		}
		// Jira Cloud shape: the producer's raw ADF bytes, delivered verbatim.
		cloudRes := bridgeNoWake(logger, g.label+"-cloud", g.jiraKey+"-cld",
			commentCreatedRawPayload(g.jiraKey+"-cld", "cmt-"+g.label+"-cld", g.body.raw,
				"Automated Gardenio Agent", "acct-bot"))
		if !cloudRes.ok {
			return cloudRes
		}
	}

	// ── Anti-vacuity control #1: a human QUOTING a generated sentence still wakes.
	//    The quote embeds the FULL colon-bearing marker ("Automated MR created:
	//    <url>") mid-sentence, so a naive strings.Contains matcher WOULD suppress
	//    it — only the prefix-anchoring (HasPrefix) correctly lets it wake. This
	//    is the case the task calls out: "A naive strings.Contains fails this."
	quoteRes := bridgeHumanRespawns(logger, "quote-back", "PE-58004",
		"jira-bridge-quote-stop", "bd-task-bridge-quote", "cmt-58004", "Ada Commenter", "acct-ada",
		`why did the bot note "Automated MR created: `+
			`https://gitlab.com/example-org/gardenio/-/merge_requests/6789" fire twice on this ticket?`)
	if !quoteRes.ok {
		return quoteRes
	}

	// ── Anti-vacuity control #2: a human OPENING by addressing the agent shares
	//    the claim notice's "Gardenio agent " PREFIX but not its suffix, so it must
	//    still wake. This is the worst failure the guard can have — silently
	//    dropping a real request — hence the claim entry matches prefix AND suffix.
	addrRes := bridgeHumanRespawns(logger, "address-agent", "PE-58005",
		"jira-bridge-address-stop", "bd-task-bridge-address", "cmt-58005", "Ada Commenter", "acct-ada",
		"Gardenio agent please take another look — the fix regressed.")
	if !addrRes.ok {
		return addrRes
	}

	// ── Anti-vacuity control #3: a plain human follow-up respawns exactly once,
	//    so the suppression asserts above are not vacuous on a globally-dead path.
	plainRes := bridgeHumanRespawns(logger, "plain-human", "PE-58006",
		"jira-bridge-plain-stop", "bd-task-bridge-plain", "cmt-58006", "Ada Commenter", "acct-ada",
		"any update on this?")
	if !plainRes.ok {
		return plainRes
	}

	// ── Anti-vacuity control #4 (ka-0000004442 negative control): the closure
	//    marker (jira_webhook.go:740) has an EMPTY suffix, so it is matched by
	//    PREFIX alone. That makes this control load-bearing: a human who merely
	//    QUOTES the closure sentence back — mid-body, not as an opening — must
	//    still wake. It distinguishes "the closure notice is suppressed" from the
	//    plausible bad fix "a suffix-less prefix match eats everything downstream
	//    that contains the sentence". A naive strings.Contains would suppress
	//    this; only the prefix-anchoring (HasPrefix) correctly lets it wake.
	closureQuoteRes := bridgeHumanRespawns(logger, "closure-quote-back", "PE-58008",
		"jira-bridge-closure-quote-stop", "bd-task-bridge-closure-quote", "cmt-58008", "Ada Commenter", "acct-ada",
		`the bot posted "Task bead closed in beads system (bead bd-xyz)." but the ticket is still open — please re-check.`)
	if !closureQuoteRes.ok {
		return closureQuoteRes
	}

	return pass("4 generated notices (claim/mr-created/mr-merged/closure) suppressed in BOTH Server+Cloud shapes (bridged, 0 wakes); human quote-back/address-agent/plain/closure-quote-back all still wake")
}

// genKind selects which of jira_sync's generated comments the producer emits.
type genKind int

const (
	genClaim genKind = iota
	genMRCreated
	genMRMerged
	genClose
)

// producedBody is one comment body captured verbatim from the real producer: the
// on-the-wire ADF bytes (raw) and the stub Jira's flattened projection (text).
type producedBody struct {
	raw  json.RawMessage
	text string
}

// produceGeneratedBody stands up the REAL jira-sync producer (bridge.NewJiraSync)
// behind the REAL SSE stream and fires the ONE bead event that makes it emit the
// requested generated comment, returning the exact body it POSTed. This is what
// keeps the leg honest: the fixture body is the producer's OWN output, so a
// producer-side wording change is caught (advice ka-0000001521), not silently
// tolerated by a hand-copied constant.
//
// The producer is wired exactly as cmd/jira-bridge wires it for the sync-back
// path — the config-driven WritePolicy (PE writable, crew role posts internal
// notes), a human operated_by so withAttribution stamps a real "Acted by" line,
// and a jira_key binding so inScope passes. A single-frame SSE stub delivers the
// event; we wait until the capturing stub Jira records the comment.
func produceGeneratedBody(logger *slog.Logger, kind genKind) (producedBody, error) {
	const (
		jiraKey  = "PE-58000"
		beadID   = "bd-bridge-src"
		agent    = "jira-bridge-src-agent"
		operator = "slack:U07BRIDGESRC"
	)

	stub := newStubJira()
	defer stub.Close()

	sync := bridge.NewJiraSync(bridge.JiraSyncConfig{
		Jira:        stub.client(logger),
		Logger:      logger,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
	})

	// Build the SSE bead event that triggers exactly the requested notice. The
	// producer keys each generated comment off distinct bead state, so one event
	// yields one comment. role=crew (Fields["role"]) supplies the acting role the
	// write policy gates on; operated_by makes attribution non-empty.
	fields := map[string]string{
		"jira_key":    jiraKey,
		"role":        spawnRole,
		"operated_by": operator,
	}
	topic := "beads.bead.updated"
	bead := map[string]any{
		"id":     beadID,
		"type":   "task",
		"labels": []string{"project:" + probeProject, "jira-mention"},
	}
	// Isolate exactly ONE generated comment per drive. The claim notice fires on
	// (status==in_progress && assignee!=""); the MR-created / MR-merged / close
	// intents are independent of assignee. So the two MR notices carry an
	// in_progress status (the realistic state) but NO assignee — that skips the
	// claim block while still producing their own notice, whose body is
	// assignee-independent ("Automated MR created: <url>" / "MR merged — …").
	switch kind {
	case genClaim:
		bead["status"] = "in_progress"
		bead["assignee"] = agent // the claim sentence embeds the agent name
	case genMRCreated:
		bead["status"] = "in_progress"
		fields["mr_url"] = "https://gitlab.com/example-org/gardenio/-/merge_requests/6789"
	case genMRMerged:
		bead["status"] = "in_progress"
		fields["mr_merged"] = "true"
	case genClose:
		bead["status"] = "closed"
		bead["assignee"] = agent
		topic = "beads.bead.closed"
	}
	bead["fields"] = fields

	eventData, err := json.Marshal(map[string]any{"bead": bead})
	if err != nil {
		return producedBody{}, fmt.Errorf("marshal bead event: %w", err)
	}

	sse := newStubSSETopic(topic, string(eventData))
	defer sse.Close()
	stream := bridge.NewSSEStream(bridge.SSEStreamConfig{BeadsHTTPAddr: sse.URL, Logger: logger})
	sync.RegisterHandlers(stream)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = stream.Start(ctx) }()

	// Wait for the producer to POST the comment. The claim/MR-created intents also
	// perform a transition / remote-link write to other endpoints, but only the
	// /comment POST is recorded by stub.comments(), so a count of >=1 means the
	// generated comment landed.
	if !waitFor(3*time.Second, func() bool { return len(stub.comments()) >= 1 }) {
		return producedBody{}, fmt.Errorf("producer never posted a comment for kind=%d (gate/scope drift?)", kind)
	}
	cancel()

	got := stub.comments()
	if len(got) != 1 {
		return producedBody{}, fmt.Errorf("producer posted %d comments for kind=%d, want exactly 1 — the drive is ambiguous", len(got), kind)
	}
	c := got[0]
	if c.key != jiraKey {
		return producedBody{}, fmt.Errorf("producer posted to %q, want %q", c.key, jiraKey)
	}
	if !c.internal {
		return producedBody{}, fmt.Errorf("producer comment was not an internal note — a generated notice is always internal")
	}
	// The Jira SERVER wire body is the logical text the producer passed to
	// AddCommentWithVisibility — WITH the blank line withAttribution inserted.
	// Reconstruct it faithfully from the captured ADF (hardBreak → "\n"), NOT via
	// the stub's lossy flattenADF (which drops newlines + adds trailing spaces
	// for the loose "contains" checks the squawk/reply legs use). This
	// reconstruction is how Jira Server renders the very ADF the producer POSTed,
	// so the Server replay is the shape prod actually delivers (advice
	// ka-0000001521) — not a hand-authored string.
	return producedBody{raw: c.raw, text: adfToServerText(c.raw)}, nil
}

// adfToServerText reconstructs the plain-text body Jira Server would store and
// re-deliver for the single-paragraph ADF the bridge posts: text nodes are
// concatenated verbatim and each hardBreak becomes a "\n", so the blank line
// withAttribution inserts (encoded as two hardBreaks around an empty segment)
// round-trips as "\n\n". A bare JSON string (already plain text) is returned
// unchanged. Faithful for the bridge's comment shape (one paragraph, hardBreak
// line splits) — the exact shape produceGeneratedBody captures.
func adfToServerText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var sb strings.Builder
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		switch node["type"] {
		case "text":
			if t, ok := node["text"].(string); ok {
				sb.WriteString(t)
			}
		case "hardBreak":
			sb.WriteByte('\n')
		}
		if content, ok := node["content"].([]any); ok {
			for _, c := range content {
				if cm, ok := c.(map[string]any); ok {
					walk(cm)
				}
			}
		}
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) == nil {
		walk(doc)
	}
	return sb.String()
}

// seedBridgeFixture builds the F3 shape the bridge-generated guard must veto: a
// CLOSED task carrying jiraKey whose assigned agent is TERMINAL — identical in
// spirit to seedCrosslinkFixture. A human follow-up here WOULD close the stale
// bead and re-spawn under the same name (bridgeHumanRespawns proves it), which is
// exactly why a bridge-generated notice must not.
func seedBridgeFixture(daemon *mockDaemon, jiraKey, agentName, taskID string) {
	daemon.seedSpawnTrigger(probeProject, spawnRole)
	daemon.agents[agentName] = &beadsapi.BeadDetail{
		ID:     "bd-" + agentName,
		Title:  agentName,
		Fields: map[string]string{"jira_key": jiraKey, "agent": agentName, "agent_state": "done", "role": spawnRole},
	}
	daemon.tasks = append(daemon.tasks, &beadsapi.BeadDetail{
		ID:       taskID,
		Type:     "task",
		Status:   "closed",
		Assignee: agentName,
		Labels:   []string{"project:" + probeProject, "jira-mention"},
		Fields:   map[string]string{"jira_key": jiraKey, "spawn_source": "jira-mention", "operated_by": "slack:U07BRIDGE"},
	})
}

// bridgeNoWake replays ONE captured bridge-generated body (in whatever delivery
// shape the caller encoded) through the REAL webhook against the F3 fixture and
// asserts it is BRIDGED (comment count == 1) yet wakes NOTHING (0 spawns, 0
// nudges) — proving the suppression is wake-only, not a drop. Mirrors
// crosslinkNoWake, the sibling guard's leg.
func bridgeNoWake(logger *slog.Logger, label, jiraKey string, payload []byte) legResult {
	agentName := "jira-bridge-" + label + "-stop"
	taskID := "bd-task-bridge-" + label

	daemon := newMockDaemon()
	seedBridgeFixture(daemon, jiraKey, agentName, taskID)

	var nudged []string
	var nmu sync.Mutex
	resolver := bridge.NewJiraAgentResolver(bridge.JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: logger,
		Nudge: func(_ context.Context, agent, _ string) error {
			nmu.Lock()
			defer nmu.Unlock()
			nudged = append(nudged, agent)
			return nil
		},
	})
	handler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:      daemon,
		DevMode:     true,
		Resolver:    resolver,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})

	code, _ := postWebhook(handler, payload)
	if code != http.StatusOK {
		return fail("[%s] bridge-generated webhook HTTP %d, want 200", label, code)
	}

	// (1) Zero spawns AND zero nudges — the wake is suppressed.
	if n := len(daemon.spawnCalls()); n != 0 {
		return fail("[%s] bridge-generated notice RE-SPAWNED a terminal agent (%d spawns) — the wake was not suppressed", label, n)
	}
	nmu.Lock()
	gotNudges := len(nudged)
	nmu.Unlock()
	if gotNudges != 0 {
		return fail("[%s] bridge-generated notice NUDGED the agent (%d nudges) — the wake was not suppressed", label, gotNudges)
	}
	// (2) The notice IS still bridged — suppression is wake-only, not a drop.
	if n := daemon.commentsFor(taskID); n != 1 {
		return fail("[%s] bridge-generated comment count on %s = %d, want exactly 1 (must be bridged, not dropped)", label, taskID, n)
	}
	// A respawn would have closed the stale terminal bead first; it must not.
	if daemon.wasClosed("bd-" + agentName) {
		return fail("[%s] the terminal agent bead was closed — a respawn was attempted despite the bridge-generated guard", label)
	}
	return pass("[%s] bridged (1 comment) with 0 spawns/0 nudges", label)
}

// bridgeHumanRespawns drives a HUMAN comment against the SAME F3 fixture and
// asserts it is NOT classified as bridge-generated: the F3 respawn fires exactly
// once, under the same agent name (session resume). It backs the mandatory
// anti-vacuity controls — a globally-dead respawn path would make the
// suppression asserts vacuous. Mirrors crosslinkHumanRespawns.
func bridgeHumanRespawns(logger *slog.Logger, label, jiraKey, agentName, taskID, commentID, author, authorAcct, body string) legResult {
	daemon := newMockDaemon()
	seedBridgeFixture(daemon, jiraKey, agentName, taskID)

	resolver := bridge.NewJiraAgentResolver(bridge.JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: logger,
		Nudge:  func(_ context.Context, _, _ string) error { return nil },
	})
	handler := bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:      daemon,
		DevMode:     true,
		Resolver:    resolver,
		WritePolicy: bridge.NewWritePolicyStore(bridge.DefaultWritePolicy(false), logger),
		Logger:      logger,
	})

	code, _ := postWebhook(handler, commentCreatedPayload(jiraKey, commentID, body, author, authorAcct))
	if code != http.StatusOK {
		return fail("[%s] human follow-up webhook HTTP %d, want 200", label, code)
	}
	spawns := daemon.spawnCalls()
	if len(spawns) != 1 {
		return fail("[%s] a HUMAN follow-up produced %d respawns, want exactly 1 — the respawn path is globally dead, so the suppression asserts are vacuous", label, len(spawns))
	}
	if spawns[0].agentName != agentName {
		return fail("[%s] respawned as %q, want the SAME name %q (session resume)", label, spawns[0].agentName, agentName)
	}
	return pass("[%s] human body (not bridge-generated) respawned %s exactly once", label, agentName)
}

// ─────────────────────────────────────────────────────────────────────────────
// Stub Jira: an httptest server that records outbound comment writes and serves
// comment history, mirroring the sandbox-Jira record pattern in the outbox tests.
// ─────────────────────────────────────────────────────────────────────────────

type recordedComment struct {
	key      string
	text     string
	internal bool
	// raw is the on-the-wire ADF comment body the JiraClient POSTed, kept
	// verbatim so a caller can replay the producer's OWN bytes through the
	// webhook (the Jira Cloud delivery shape). The bridge-nowake leg uses this
	// to avoid hand-authoring a fixture body from the matcher (advice
	// ka-0000001521); flattenADF/text is the lossy human-readable projection.
	raw json.RawMessage
}

type stubJira struct {
	srv *httptest.Server
	mu  sync.Mutex
	rec []recordedComment
	// failStatus, when >= 400, makes the comment-POST endpoint return that HTTP
	// status (with a Jira-style error body) INSTEAD of recording the write and
	// returning 201. The stub otherwise ONLY ever 201s — so the reply handler's
	// classifyReplyPostError branch (a Jira 4xx -> 422, a 5xx/429 -> 502) is dead
	// code from the probe's point of view. This toggle is the single mechanism
	// that lets a leg drive Jira into a 4xx/5xx (ka-0000005455 leg reply-reclassify).
	failStatus int
}

func newStubJira() *stubJira {
	s := &stubJira{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// newStubJiraFailing is newStubJira whose comment-POST endpoint returns `status`
// (a 4xx/5xx) with a Jira-style error body instead of recording the write and
// returning 201. It is what lets the reply-reclassify leg drive the reply
// handler's error-classification branch end-to-end: a Jira 4xx (except 429) must
// surface as 422 "retry as plain text", while a 5xx/429 stays a 502 outage
// (ka-0000000661 / ka-0000005455). The producer path (jira_client.go doJSON)
// wraps the status into a *JiraAPIError the handler unwraps with errors.As.
func newStubJiraFailing(status int) *stubJira {
	s := newStubJira()
	s.mu.Lock()
	s.failStatus = status
	s.mu.Unlock()
	return s
}

func (s *stubJira) Close() { s.srv.Close() }

func (s *stubJira) client(logger *slog.Logger) *bridge.JiraClient {
	return bridge.NewJiraClient(bridge.JiraClientConfig{
		BaseURL: s.srv.URL, Email: "probe@example.com", APIToken: "probe", Logger: logger,
	})
}

func (s *stubJira) comments() []recordedComment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedComment, len(s.rec))
	copy(out, s.rec)
	return out
}

// handle answers the JiraClient comment endpoints: POST .../comment records the
// write (decoding the sd.public.comment internal flag + flattening the ADF body
// to text), GET .../comment returns an empty history.
func (s *stubJira) handle(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.URL.Path, "/comment") {
		writeJSON(w, map[string]any{})
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"comments": []any{}})
		return
	}
	// Fault injection: a stub armed via newStubJiraFailing rejects the comment
	// POST with the configured status and a Jira-style error body, WITHOUT
	// recording the write — exactly how a real Jira 4xx/5xx reaches the reply
	// handler (jira.go doJSON turns any >= 400 into a *JiraAPIError carrying the
	// status). This is the ONLY path that drives classifyReplyPostError.
	s.mu.Lock()
	fs := s.failStatus
	s.mu.Unlock()
	if fs >= 400 {
		w.WriteHeader(fs)
		writeJSON(w, map[string]any{"errorMessages": []string{"INVALID_INPUT: unrenderable comment body"}})
		return
	}
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		Body       json.RawMessage `json:"body"`
		Properties []struct {
			Key   string `json:"key"`
			Value struct {
				Internal bool `json:"internal"`
			} `json:"value"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(body, &payload)
	// Default internal:false; a legacy AddComment posts no property (defaults to
	// a public comment on JSM), while the governed paths always stamp it.
	internal := false
	for _, p := range payload.Properties {
		if p.Key == "sd.public.comment" {
			internal = p.Value.Internal
		}
	}
	s.mu.Lock()
	s.rec = append(s.rec, recordedComment{
		key:      jiraKeyFromCommentPath(r.URL.Path),
		text:     flattenADF(payload.Body),
		internal: internal,
		raw:      append(json.RawMessage(nil), payload.Body...),
	})
	s.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"id": "stub-comment"})
}

// jiraKeyFromCommentPath extracts KEY from /rest/api/3/issue/KEY/comment.
func jiraKeyFromCommentPath(path string) string {
	const prefix = "/rest/api/3/issue/"
	rest := strings.TrimPrefix(path, prefix)
	if idx := strings.Index(rest, "/"); idx > 0 {
		return rest[:idx]
	}
	return rest
}

// flattenADF walks an ADF comment body (or a plain JSON string) and returns its
// concatenated text — enough to assert on the squawk/reply body.
func flattenADF(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var sb strings.Builder
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if t, _ := node["text"].(string); t != "" {
			sb.WriteString(t)
			sb.WriteString(" ")
		}
		if content, ok := node["content"].([]any); ok {
			for _, c := range content {
				if cm, ok := c.(map[string]any); ok {
					walk(cm)
				}
			}
		}
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) == nil {
		walk(doc)
	}
	return sb.String()
}

// adfTextNode is one ADF `text` node projected from a recorded comment body: its
// literal text plus the set of mark types on it. The reply-codemark leg asserts
// on the marks a bold/linked code span produced, so it needs the marks per node
// (flattenADF discards them).
type adfTextNode struct {
	text  string
	marks map[string]bool
}

func (n adfTextNode) hasMark(t string) bool { return n.marks[t] }

func (n adfTextNode) markTypes() []string {
	out := make([]string, 0, len(n.marks))
	for t := range n.marks {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// adfTextNodes walks an ADF comment body (a doc object; a bare JSON string has no
// marked nodes) and returns every `text` node with its mark-type set. This is the
// observable surface for the code-mark exclusivity leg: the marks the converter
// actually stamped on the on-the-wire ADF.
func adfTextNodes(raw json.RawMessage) []adfTextNode {
	if len(raw) == 0 {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	var out []adfTextNode
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["type"] == "text" {
			t, _ := node["text"].(string)
			marks := map[string]bool{}
			if ms, ok := node["marks"].([]any); ok {
				for _, m := range ms {
					if mm, ok := m.(map[string]any); ok {
						if mt, ok := mm["type"].(string); ok {
							marks[mt] = true
						}
					}
				}
			}
			out = append(out, adfTextNode{text: t, marks: marks})
		}
		if content, ok := node["content"].([]any); ok {
			for _, c := range content {
				if cm, ok := c.(map[string]any); ok {
					walk(cm)
				}
			}
		}
	}
	walk(doc)
	return out
}

// summarizeNodes renders text->marks for each node, for a failing-assertion
// message that shows exactly what ADF the converter emitted.
func summarizeNodes(nodes []adfTextNode) string {
	var sb strings.Builder
	for i, n := range nodes {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%q%v", n.text, n.markTypes())
	}
	return sb.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// Stub SSE server: emits a single event frame then holds the connection open so
// the SSEStream does not reconnect-spam (mirrors the wired squawk test).
// ─────────────────────────────────────────────────────────────────────────────

type stubSSE struct {
	*httptest.Server
}

func newStubSSE(eventData string) *stubSSE {
	return newStubSSETopic("beads.bead.closed", eventData)
}

// newStubSSETopic is newStubSSE with a caller-chosen event topic, so a leg can
// drive the jira-SYNC producer (beads.bead.updated / beads.bead.closed) as well
// as the squawk forwarder (beads.bead.closed). Emits one frame then holds the
// connection open so the SSEStream does not reconnect-spam.
func newStubSSETopic(topic, eventData string) *stubSSE {
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
		fmt.Fprintf(w, "id:1\n")
		fmt.Fprintf(w, "event:%s\n", topic)
		fmt.Fprintf(w, "data:%s\n\n", eventData)
		flusher.Flush()
		time.Sleep(2 * time.Second)
	}))
	return s
}

// squawkClosedEvent builds the SSE `data:` payload for a closed squawk bead.
func squawkClosedEvent(agentName, text string) string {
	return string(squawkClosedEventData(agentName, text))
}

// squawkClosedEventData builds the raw JSON of a closed squawk bead event, the
// shape the squawk forwarder's handleClosed parses.
func squawkClosedEventData(agentName, text string) []byte {
	payload := map[string]any{
		"bead": map[string]any{
			"id":     "bd-squawk-" + agentName,
			"type":   "message",
			"labels": []string{"squawk", "from:" + agentName},
			"fields": map[string]string{
				"source_agent": agentName,
				"text":         text,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// ─────────────────────────────────────────────────────────────────────────────
// Webhook payload helpers.
// ─────────────────────────────────────────────────────────────────────────────

// commentCreatedPayload builds a Jira comment_created webhook body (plain-text
// comment body, the Jira Server shape the handlers accept).
func commentCreatedPayload(jiraKey, commentID, body, authorName, accountID string) []byte {
	bodyJSON, _ := json.Marshal(body)
	payload := map[string]any{
		"webhookEvent": "comment_created",
		"issue": map[string]any{
			"key":    jiraKey,
			"fields": map[string]any{"summary": "probe issue " + jiraKey},
		},
		"comment": map[string]any{
			"id":   commentID,
			"body": json.RawMessage(bodyJSON),
			"author": map[string]any{
				"displayName": authorName,
				"accountId":   accountID,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// commentCreatedRawPayload is commentCreatedPayload for a comment body supplied
// as an already-encoded json.RawMessage rather than a plain string. It is the
// Jira CLOUD delivery shape: the webhook's comment.body is a raw ADF object, not
// a JSON string. The bridge-nowake leg feeds it the EXACT ADF bytes the real
// producer (jira_sync.go) POSTed — captured by the stub Jira — so the leg never
// hand-authors a Cloud body from the matcher (advice ka-0000001521).
func commentCreatedRawPayload(jiraKey, commentID string, body json.RawMessage, authorName, accountID string) []byte {
	payload := map[string]any{
		"webhookEvent": "comment_created",
		"issue": map[string]any{
			"key":    jiraKey,
			"fields": map[string]any{"summary": "probe issue " + jiraKey},
		},
		"comment": map[string]any{
			"id":   commentID,
			"body": body,
			"author": map[string]any{
				"displayName": authorName,
				"accountId":   accountID,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// postWebhook POSTs a body to a webhook handler and returns the status + body.
func postWebhook(handler http.Handler, body []byte) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// postJSON POSTs a JSON object to a mux route and returns the status + body.
func postJSON(handler http.Handler, path string, obj map[string]any) (int, string) {
	b, _ := json.Marshal(obj)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// waitFor polls cond every 10ms until it is true or the deadline elapses.
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
