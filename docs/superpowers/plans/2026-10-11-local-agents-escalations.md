# Local agents as full participants, and escalations across the boundary — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A laptop runs a real gardenio agent that the Slack bridge can reach without dialing it: mail is the only path into any agent (pod or laptop), a registered runner holds a per-runner token over a public HTTPS front door, placement is explicit, a disconnected runner holds its task, and a cluster agent can escalate to a named human machine that only its owner can approve.

**Architecture:** The agent subscribes to its own `type=mail` beads over the existing kbeads SSE stream (new `agent=` scope) and delivers to coop on loopback; the bridge's `NudgeAgent` becomes "create a mail" and the dial code is deleted (spec §3). A `type:runner` bead plus a hashed token record carries machine identity (§4); `RoleRunner` is introduced only after the auth interceptor can tell callers apart. `gb runner` wraps today's `gb agent start --local` (§5). Escalations to a laptop ride the same mail path, emitted by the daemon from inside the approve handlers (§8).

**Tech Stack:** Go (kbeads daemon + `kd`, gardenio controller + `gb`, bridge), Postgres store, Helm chart `gardenio/helm/gardenio` (Traefik IngressRoute), Slack Block Kit, the claudeless mock for e2e.

**Spec:** `docs/superpowers/specs/2026-10-11-local-agents-escalations-design.md` (§0 binding). Owner approval: decision gd-qqnnMlFOSB, 2026-10-11.

## Global Constraints

- Spec: findings bundle gd-I9dFR2el5V (read with `gb bundle doc <doc-id>`). Program and sequencing: bundle doc gd-H90Z1Rug2E. Viewer: https://gardenio.clonk.tv/bundle/gd-I9dFR2el5V
- Owner goal is career visibility (acqui-hire or contracting), not a product company. Prefer what a reviewer sees over what a tenant needs.
- Slack is the headline. Any user-visible change must read well in a Slack thread first.
- No live-cluster helm/kubectl from crews; chart edits via PR only (standing rule gd-oI2AIaSzjF). Tasks that touch `gardenio/helm/**`, `authz`/`auth/` paths, `kbeads/internal/server/interceptors.go`, or `write_authz.go` are never crew self-merged: file the `github_pr_merge` escalation and let the merge desk review them (rule (d) of the self-merge policy).
- Branch per task: `agent/launch/<task-id>`; PR against main; crews self-merge on green per the drop-gate policy.
- plan_state stays draft/ready until the owner clicks Approve & build in the cockpit.
- Verification is scoped: run tests only for the packages you changed, with `GOFLAGS=-p=2`, never repo-wide `go test ./...` or `-race`, never cargo in a pod (advice gd-oI2AIaSzjF, gd-QLhMjEsBsR). Commit and push before any run longer than a minute. CI is the authoritative gate.
- Guards that say "cannot" are paired with a planted positive control: a request that must fail is paired with the same request from an allowed identity that must succeed (spec §11).
- Formatting: `gofumpt`, `go vet`, `staticcheck` on touched Go packages.
- **Ordering constraint from the spec (§4.2, §8.1, §14):** `RoleRunner` (Task 8) waits on the `requireRole` fix, and Tasks 15 and 16 wait on P7 Phase 4 (escalation authz close-out, gd-pTmaK6Uuem; bugs gd-SxFOBp4IOB, gd-Js9psMOccC, gd-5SFLHuilDK, gd-yuC1qkRCm7, gd-EOcXnDkJmQ). Tasks 1 to 6 do not wait on P7.
- No new bead type for the inbox. Mail already is the inbox. One reserved label prefix, `control:`.
- Never dial an agent from the bridge in new code. Delivery is `type=mail` with `assignee=<agent>`.

## Review Focus

1. **The mail loop is the only way in.** After Task 6, `grep -rn 'coop_url\|deriveChannelURL\|nudgeCoop' gardenio/controller/internal/bridge gardenio/controller/cmd/*-bridge` has zero non-test hits. Pinned in Task 6 step 4.
2. **`control:` labels cannot be set by an agent on mail to another agent.** Pinned with a planted positive control in Task 2 steps 1 and 3 (the same request from the bridge identity succeeds).
3. **The `agent=` SSE scope cannot be used to read someone else's mail.** A runner token with `agent=<not-mine>` is a 403, not an empty stream. Pinned in Task 8 step 2.
4. **Replay after reconnect delivers each unread mail once.** `Last-Event-ID` replay plus the inbox drain must not double-deliver. Pinned in Task 3 step 1 (`TestDeliveredSetDedupes`).
5. **A paused local agent is not reaped.** The stale reaper must leave `task_id` assigned and not respawn. Pinned in Task 14 step 1.
6. **A non-owner's click on a cloud-to-local escalation is refused with a message naming whose machine it is, and the owner's click succeeds.** Pinned in Task 17 step 1.

## Unverified at plan time (each task that depends on one settles it first and records the answer as a task comment)

- **U1.** `agent_state=paused` does not exist today (grep of `internal/` finds no such state). Task 14 must enumerate every switch over agent state (`deadAgentStates` in `internal/bridge/nudge.go`, the stale reaper, roster rendering, the Slack card) before adding it, and add a table test over all of them.
- **U2.** Whether `beadsapi.Client.EventStream` can send `Last-Event-ID` and extra query params. Today it takes only `topics` (`internal/beadsapi/client_events.go:133`). Task 3 step 1 adds `EventStreamOpts` and must not change the existing signature's callers.
- **U3.** Whether a pod's agent token passes the new `agent=` scope check. Pods use the shared static token today; Task 1 scopes by query param and Task 8 adds the runner-token ownership check. For pods the scope is an optimization, not a boundary, until per-agent tokens exist (`gd-ZjiSd9Kzio` step 2).
- **U4.** Where `coop_url` is read besides the sites in Task 5: `bot_agent_kill.go:97`, `statusreporter`, and any viewer code. Task 6 step 1 greps the whole tree, including `cmd/beads-viewer`, before deleting.
- **U5.** The chart side of "pods keep a loopback-only coop" is a NetworkPolicy/containerPort change under `gardenio/helm/**`. Task 6 delivers it as a separate chart-only PR through the merge desk.
- **U6.** Whether Slack interactive payloads expose the clicking user's id in the escalation-card handler without an extra lookup (Task 17).

---

## Phase 1: Mail is the inbox

### Task 1: SSE `agent=<bead id>` scope: restrict a stream to one agent's events and the beads assigned to it

**Files:**
- Modify: `kbeads/internal/server/sse.go` (`sseEvent` at `:51`, `sseClient` at `:80`, `sseClientScope` at `:104`, `handleEventStream` at `:325`, `broadcast` at `:122`)
- Test: `kbeads/internal/server/sse_agent_scope_test.go` (create)

**Interfaces:**
- Consumes: the existing `extractFleetFromPayload` pattern (`sse.go:274`) for pulling a field out of the JSON payload.
- Produces: query param `agent=<agent bead id>` on `GET /v1/events/stream`; `sseEvent.Assignee`; `(*sseClient).matchesAgent(evt)`.

Semantics: with `agent=X` set, a client receives an event only if the event's bead id is `X`, or the event's bead `assignee` equals the agent name of bead `X`. The agent name is resolved once at subscribe time with `store.GetBead(X)` and read from the bead's `fields.agent` (that is the field `FindAgentBead` matches on, `gardenio/controller/internal/beadsapi/client.go:2257`). An unknown agent bead is a 404 before the stream opens. Without `agent=`, behaviour is byte-for-byte unchanged.

- [ ] **Step 1: Write the failing tests**

```go
package server

import "testing"

func TestExtractAssigneeFromPayload(t *testing.T) {
	cases := []struct {
		name, payload, want string
	}{
		{"bead.assignee", `{"bead":{"id":"gd-1","assignee":"crew-a"}}`, "crew-a"},
		{"top-level assignee", `{"assignee":"crew-b"}`, "crew-b"},
		{"mail sent To", `{"bead_id":"gd-2","to":"crew-c"}`, "crew-c"},
		{"none", `{"bead":{"id":"gd-3"}}`, ""},
		{"malformed", `{`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractAssigneeFromPayload([]byte(tc.payload)); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestSSEClientMatchesAgent(t *testing.T) {
	c := &sseClient{agentBeadID: "gd-agent", agentName: "crew-a"}
	if !c.matchesAgent(&sseEvent{BeadID: "gd-agent"}) {
		t.Fatal("events on the agent bead itself must match")
	}
	if !c.matchesAgent(&sseEvent{BeadID: "gd-mail", Assignee: "crew-a"}) {
		t.Fatal("events on a bead assigned to the agent must match")
	}
	// planted negatives, each paired with the positives above
	if c.matchesAgent(&sseEvent{BeadID: "gd-mail", Assignee: "crew-b"}) {
		t.Fatal("another agent's mail must not match")
	}
	if c.matchesAgent(&sseEvent{BeadID: "gd-other"}) {
		t.Fatal("unassigned unrelated event must not match")
	}
	un := &sseClient{}
	if !un.matchesAgent(&sseEvent{BeadID: "anything", Assignee: "anyone"}) {
		t.Fatal("a client with no agent scope must match everything (unchanged behaviour)")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd kbeads && GOFLAGS=-p=2 go test ./internal/server/ -run 'TestExtractAssigneeFromPayload|TestSSEClientMatchesAgent' -count=1`
Expected: FAIL (undefined: extractAssigneeFromPayload, matchesAgent, sseClient.agentBeadID).

- [ ] **Step 3: Implement**

In `sse.go` add to `sseEvent`:

```go
	// Assignee is the owning bead's assignee as carried in the payload, used
	// only by the agent= subscriber scope. Empty for events whose payload has none.
	Assignee string
```

Add to `sseClient`: `agentBeadID, agentName string` (immutable after subscribe). Add the extractor next to `extractFleetFromPayload`:

```go
func extractAssigneeFromPayload(data []byte) string {
	var raw struct {
		Assignee string `json:"assignee"`
		To       string `json:"to"`
		Bead     *struct {
			Assignee string `json:"assignee"`
		} `json:"bead"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	switch {
	case raw.Bead != nil && raw.Bead.Assignee != "":
		return raw.Bead.Assignee
	case raw.Assignee != "":
		return raw.Assignee
	default:
		return raw.To
	}
}

// matchesAgent reports whether evt is inside this client's agent= scope. A
// client with no agent scope matches everything.
func (c *sseClient) matchesAgent(evt *sseEvent) bool {
	if c.agentBeadID == "" {
		return true
	}
	if evt.BeadID == c.agentBeadID {
		return true
	}
	return c.agentName != "" && evt.Assignee == c.agentName
}
```

In `broadcast`, set `Assignee: extractAssigneeFromPayload(payload)` on the stored event (the ring buffer copy, so replay sees it) and add `&& c.matchesAgent(evt)` to the fan-out condition at `:147`. In `handleEventStream`: read `agent := r.URL.Query().Get("agent")`; when non-empty, `bead, err := s.store.GetBead(ctx, agent)`, 404 if not found, and pass `agentBeadID`/`agentName` through `sseClientScope` to `subscribe`. Add `client.matchesAgent(evt)` to the replay condition at the `Last-Event-ID` loop so replay cannot exceed live scope.

- [ ] **Step 4: Run to verify they pass**

Run: `cd kbeads && GOFLAGS=-p=2 go test ./internal/server/ -run 'SSE|Assignee' -count=1`
Expected: PASS, including every pre-existing SSE test (they must not need edits).

- [ ] **Step 5: Add the handler-level test with a planted positive control**

Add to the new test file one test that starts the real handler against the existing test store (copy the harness from the nearest existing `handleEventStream` test in `sse_test.go`), publishes a mail assigned to `crew-a` and a mail assigned to `crew-b`, subscribes with `agent=<crew-a bead>`, and asserts the stream yields only the first. The same test subscribes without `agent=` and asserts both arrive (the positive control proving the filter, not the harness, did the excluding).

- [ ] **Step 6: Commit**

```bash
git add kbeads/internal/server/sse.go kbeads/internal/server/sse_agent_scope_test.go
git commit -m "feat(kbeads): SSE agent= scope for per-agent mail subscription"
```

### Task 2: `control:` label guard: only the bridge, controller, an operator, or the runner's owner may set it on mail

**Files:**
- Modify: `kbeads/internal/server/beads.go` (create path, near the `TypeMail` publish at `:684`)
- Create: `kbeads/internal/server/mail_control_labels.go`, `kbeads/internal/server/mail_control_labels_test.go`

**Interfaces:**
- Consumes: the caller identity helpers already used by `write_authz.go` (settle: read `write_authz.go` and `callerFromContext`-style helpers first and record which function yields "is this the bridge/controller/operator").
- Produces: `validateControlLabels(caller callerIdentity, bead *model.Bead) error`, returning an error that maps to HTTP 403 with the message `control: labels may only be set by the bridge, the controller, or an operator`.

- [ ] **Step 1: Write the failing table test**

```go
func TestValidateControlLabels(t *testing.T) {
	mail := func(labels ...string) *model.Bead {
		return &model.Bead{Type: "mail", Assignee: "crew-b", Labels: labels}
	}
	cases := []struct {
		name    string
		caller  callerIdentity
		bead    *model.Bead
		wantErr bool
	}{
		{"agent sets control:kill on peer mail", agentCaller("crew-a"), mail("control:kill"), true},
		{"bridge sets control:kill", bridgeCaller(), mail("control:kill"), false}, // positive control
		{"operator sets control:kill", operatorCaller("oidc:o@example.com"), mail("control:kill"), false},
		{"agent sends ordinary mail", agentCaller("crew-a"), mail("from:crew-a"), false},
		{"agent sets control:-prefixed variant", agentCaller("crew-a"), mail("control:escalation-approved"), true},
		{"non-mail bead with control: label is not this guard's business", agentCaller("crew-a"), &model.Bead{Type: "task", Labels: []string{"control:x"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateControlLabels(tc.caller, tc.bead)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
```

`agentCaller`, `bridgeCaller`, `operatorCaller` are test helpers you add in the same file, built from whatever the identity type is (record its name in the task comment).

- [ ] **Step 2: Run to verify failure**

Run: `cd kbeads && GOFLAGS=-p=2 go test ./internal/server/ -run TestValidateControlLabels -count=1`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement** `validateControlLabels` (any label with prefix `control:` on `Type == "mail"` requires a non-agent caller; the runner-owner case is added in Task 8 once runner callers exist, with its own test) and call it from the create path before the bead is stored. Also call it from the label-add path (`AddLabel`), otherwise an agent could add the label after create. Add a second test, `TestAddLabelControlGuard`, for that path with the same positive control.

- [ ] **Step 4: Verify**

Run: `cd kbeads && GOFLAGS=-p=2 go test ./internal/server/ -run 'ControlLabel|MailSent|CreateBead' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit** `feat(kbeads): reserve control: mail labels for bridge, controller, operators`

### Task 3: `agentinbox`: the agent-side mail subscriber, with `gb mail send --label`

**Files:**
- Create: `gardenio/controller/internal/agentinbox/inbox.go`, `deliver.go`, `inbox_test.go`
- Modify: `gardenio/controller/internal/beadsapi/client_events.go` (add `EventStreamOpts`, keep `EventStream` as a wrapper)
- Modify: `gardenio/controller/cmd/gb/mail.go` (add `--label` repeatable flag to `mail send`, `:48-99`)

**Interfaces:**
- Consumes: Task 1's `agent=` param; `coopapi.Client.Nudge`, `GetAgentState`, `Shutdown` (`internal/coopapi/client.go:127,199,209`); `escalationWaitForReady`/`escalationDeliverNudge` (`cmd/gb/agent_start_escalation.go:41,66`) as the delivery pattern to copy (they are package `main`, so copy, do not import).
- Produces: `agentinbox.Run(ctx, Config) error` which blocks until ctx is canceled and reconnects with capped backoff; `agentinbox.Config{Daemon, Coop, AgentBead, AgentName, Logger, OnKill func(reason string)}`.

Behaviour (spec §3.1, §10):
- Subscribe `topics=beads.bead.created`, `agent=<AgentBead>`, sending `Last-Event-ID` after the first event.
- A `type=mail` bead whose `assignee == AgentName` is delivered. Ordinary mail: nudge text `New mail from <from:…>: <title> — run 'kd show <id>' to read` (identical to the bridge's today, `internal/bridge/mail.go:99`). `delivery:interrupt`: body inline.
- `control:kill`: call `OnKill(reason)` (which asks coop for graceful shutdown), then close the mail.
- Delivery retries: 3 attempts at 2 s, then leave the mail open and log (spec §10).
- On every (re)connect, drain `kd`-equivalent `ListMail(assignee, status=open)` first, deliver in creation order, track delivered mail ids in an in-memory set so replay and drain never double-deliver.
- 401 from the daemon is terminal: return `ErrTokenRejected`; do not retry (spec §10).

- [ ] **Step 1: Write the failing tests**

```go
func TestDeliveredSetDedupes(t *testing.T) {
	d := newDeliveredSet(8)
	if !d.markNew("gd-m1") {
		t.Fatal("first sight must be new")
	}
	if d.markNew("gd-m1") {
		t.Fatal("second sight must not be new")
	}
}

func TestClassifyMail(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   mailKind
	}{
		{"plain", []string{"from:alice"}, kindPlain},
		{"interrupt", []string{"from:alice", "delivery:interrupt"}, kindInterrupt},
		{"kill", []string{"from:bridge", "control:kill"}, kindKill},
		{"unknown control label is plain, never executed", []string{"control:future-thing"}, kindPlain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyMail(tc.labels); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestRunDeliversOnceAcrossReplayAndDrain(t *testing.T) { /* fake daemon serves the same mail via drain and via SSE replay; fake coop counts Nudge calls; assert exactly 1 */ }
func TestRunKillMailCallsOnKillAndClosesMail(t *testing.T)   { /* ... */ }
func TestRunStopsOn401(t *testing.T)                          { /* assert errors.Is(err, ErrTokenRejected) and exactly one connect attempt */ }
func TestRunIgnoresMailForOtherAssignee(t *testing.T)         { /* positive control: same fake stream with assignee == AgentName delivers */ }
```

Write the last four as full tests against `httptest` fakes (daemon SSE + coop `/api/v1/agent/nudge`), copying the fake-coop approach from `agent_start_escalation_test.go`.

- [ ] **Step 2: Run to verify failure**

Run: `cd gardenio/controller && GOFLAGS=-p=2 go test ./internal/agentinbox/ -count=1`
Expected: FAIL (package does not exist).

- [ ] **Step 3: Implement** the package per the behaviour list above. `EventStreamOpts{Topics, Agent, LastEventID string}` in `beadsapi`; the existing `EventStream(ctx, topics)` calls it with only `Topics`. Settle U2 first and record the finding.

- [ ] **Step 4: `gb mail send --label`**

Add `StringArray` flag `label` to `mailSendCmd`; labels are appended after `from:<sender>`. Test: `TestMailSendLabelFlag` asserts `CreateBeadRequest.Labels == ["from:x","control:kill"]` using the existing fake daemon pattern in `mail_coverage_test.go`.

- [ ] **Step 5: Verify**

Run: `cd gardenio/controller && GOFLAGS=-p=2 go test ./internal/agentinbox/ ./internal/beadsapi/ ./cmd/gb/ -run 'Inbox|Mail|EventStream' -count=1`
Expected: PASS.

- [ ] **Step 6: Commit** `feat(gardenio/agentinbox): agent-side mail subscriber and gb mail send --label`

### Task 4: Run the inbox in `gb agent start` for pods and for `--local`

**Files:**
- Modify: `gardenio/controller/cmd/gb/agent_start.go` (`runLocal` at `:206`), `gardenio/controller/cmd/gb/agent_start_k8s.go`
- Test: `gardenio/controller/cmd/gb/agent_start_inbox_test.go` (create)

**Interfaces:**
- Consumes: `agentinbox.Run` (Task 3). The coop loopback URL each path already has (`runLocal` computes `coopAddr` for `localAgentEnv`; the k8s path has its own).
- Produces: both runtimes start the inbox goroutine beside the heartbeat (`agentHeartbeatLoop`, `agent_start.go:724`) and cancel it on shutdown; `OnKill` calls `coop.Shutdown`.

- [ ] **Step 1: Failing test** that `runLocal` with the claudeless mock (`--test-scenario`, see `agent_start_scenario_test.go`) receives a mail created for its agent name and the scenario's nudge log records it. Name: `TestRunLocalDeliversMailViaInbox`.
- [ ] **Step 2: Run to verify failure** (`go test ./cmd/gb/ -run TestRunLocalDeliversMailViaInbox`).
- [ ] **Step 3: Wire it** in both runtimes; gate nothing behind a flag (spec: pods and laptops identical). Keep the existing bridge dial working in this task, so pods get mail twice for one release only if Task 5 has not merged; the `deliveredSet` plus the bridge nudge being a plain "check mail" prompt makes a duplicate harmless. Say so in the PR description.
- [ ] **Step 4: Verify** `go test ./cmd/gb/ -run 'AgentStart|Inbox' -count=1`.
- [ ] **Step 5: Commit** `feat(gardenio/gb): start the mail inbox in agent start for pods and local`

### Task 5: Bridge: `NudgeAgent` creates mail; kill is a `control:kill` mail

**Files:**
- Modify: `gardenio/controller/internal/bridge/nudge.go` (`NudgeAgent` at `:152`, `NudgeAgentWithBootRetry` at `:431` region)
- Modify: `gardenio/controller/internal/bridge/bot_agent_kill.go` (`:90-110`)
- Modify: `gardenio/controller/internal/bridge/mail.go` (delete `nudgeAgent`; the watcher keeps only logging, or is removed if nothing else uses `Mail`)
- Test: `gardenio/controller/internal/bridge/nudge_mail_test.go` (create), update `nudge_test.go`

**Interfaces:**
- Consumes: `NudgeBeadClient`; add `CreateMail(ctx, MailRequest) (string, error)` to the narrow interface (`beadsapi.Client.CreateBead` already exists, wrap it).
- Produces: `NudgeAgent(ctx, daemon, client, logger, agentName, message)` keeps its **signature** (fifteen call sites depend on it: `decisions.go:765`, `escalations.go:558,621`, `bot_thread_forward.go:132,159`, `dm_delivery.go:307`, `bot_decisions_modal.go:776,863`, `advice_nudge.go:165`, `chat.go:222`, `claimed.go:216`, `bot_mentions.go:2590`, `cmd/jira-bridge/main.go:133`, `cmd/gitlab-bridge/main.go:139`); only its body changes.

New body: keep the two early exits (terminal `agent_state`, `stop_requested`), drop the `coop_url` lookup, create a mail `{type: mail, assignee: agentName, title: first line of message, description: message, labels: [from:bridge, delivery:interrupt], priority: 1}` and return nil. `ErrAgentNoCoopURL` is no longer returned; keep the symbol until Task 6 so callers compile.

- [ ] **Step 1: Failing table test** over every call-site shape (one case per file above, passing that file's usual message) asserting exactly one mail is created with the right assignee and the `delivery:interrupt` label and that no HTTP request leaves the process (fake `http.Client` that fails the test on any call).

```go
func TestNudgeAgentCreatesMailAndNeverDials(t *testing.T) {
	d := &fakeNudgeDaemon{bead: agentBead("crew-a", "working")}
	c := &http.Client{Transport: failTransport{t}}
	if err := NudgeAgent(context.Background(), d, c, discardLogger(), "crew-a", "Decision gd-x resolved: go"); err != nil {
		t.Fatal(err)
	}
	if len(d.mails) != 1 || d.mails[0].Assignee != "crew-a" || !hasLabel(d.mails[0].Labels, "delivery:interrupt") {
		t.Fatalf("mails=%+v", d.mails)
	}
	// planted control: a terminal agent is refused and creates nothing
	d2 := &fakeNudgeDaemon{bead: agentBead("crew-a", "done")}
	if err := NudgeAgent(context.Background(), d2, c, discardLogger(), "crew-a", "x"); !errors.Is(err, ErrAgentNotNudgeable) || len(d2.mails) != 0 {
		t.Fatalf("err=%v mails=%d", err, len(d2.mails))
	}
}
```

- [ ] **Step 2: Verify failure** `go test ./internal/bridge/ -run TestNudgeAgentCreatesMail -count=1`.
- [ ] **Step 3: Implement** the new body. Kill: replace the graceful-shutdown dial with a `control:kill` mail, wait up to 30 s for the agent bead to leave `working` (poll the bead, not coop), then `CloseBead` as today. The `--force` path skips the wait.
- [ ] **Step 4: Respawn trigger.** Where the mention path used `nudgeFailureWantsRespawn` on a dial error (`bot_mentions.go` near `:2590`), key the pod respawn off "urgent mail still open after 2 minutes" via a small sweeper in `claimed.go`'s existing ticker; local agents (`execution_target=local`) are exempt (spec D5). Test `TestUnreadUrgentMailRespawnsPodsNotLocal` with a positive control for the pod case.
- [ ] **Step 5: Update existing tests** that asserted a dial (`nudge_test.go`, `mail_test.go`): delete the dial assertions, keep the behavioural ones.
- [ ] **Step 6: Verify** `go test ./internal/bridge/ -run 'Nudge|Mail|Kill|Respawn' -count=1`.
- [ ] **Step 7: Commit** `feat(gardenio/bridge): nudge and kill become mail; bridge stops dialing`

### Task 6: Delete the dial and stop writing `coop_url`

**Files:**
- Modify: `gardenio/controller/internal/bridge/nudge.go` (delete `nudgeChannel`, `nudgeCoop`, `deriveChannelURL`, `channelPort`, `ErrAgentNoCoopURL`)
- Modify: `gardenio/controller/cmd/gb/agent_start.go` (stop writing `coop_url` into notes for new agents)
- Create: `gardenio/helm`-side change as a separate PR (see step 5)

- [ ] **Step 1: Settle U4.** `grep -rn 'coop_url\|ParseNotes' --include=*.go . | grep -v _test.go` over the whole repo, including `cmd/beads-viewer`, `internal/statusreporter`, `internal/subscriber`. List every reader in a task comment. Any reader that is not about to be deleted gets a replacement (the viewer's "open terminal" link, if it uses `coop_url`, keeps working from the mux and is out of scope; record it).
- [ ] **Step 2: Delete** the symbols and fix compile errors until `go build ./internal/bridge/... ./cmd/...` is clean, run per package.
- [ ] **Step 3: Verify** `go vet ./internal/bridge/ ./cmd/gb/` and `go test ./internal/bridge/ ./cmd/gb/ -count=1`.
- [ ] **Step 4: Pin Review Focus 1.** Add `scripts/check-no-agent-dial.sh`, a guard that greps for `deriveChannelURL|nudgeCoop|ErrAgentNoCoopURL` under `gardenio/controller/internal/bridge` and fails if any hit is outside `_test.go`; add `scripts/check-no-agent-dial_test.sh` which plants a file containing `nudgeCoop` in a temp copy and asserts the guard fails, then asserts it passes on the clean tree. Wire both into the existing `script-selftests` job (`.github/workflows/checks.yml`, needs merge-desk review per the self-merge rule).
- [ ] **Step 5: Chart PR (separate, merge desk).** Settle U5: bind coop to loopback for pods and remove the pod-ingress allowance for the coop and channel ports from the chart NetworkPolicy. Chart-only change, `make test-helm` is the local loop.
- [ ] **Step 6: Commit** `refactor(gardenio/bridge): delete the agent dial; mail is the only way in`

---

## Phase 2: Runner identity and front door

> Tasks 7, 9 and 10 do not depend on P7. Task 8 does (see Global Constraints).

### Task 7: Runner bead, token store, `register` and `heartbeat` endpoints

**Files:**
- Modify: `kbeads/internal/model/` (type `runner`, kind `config`), `kbeads/internal/store/` (migration for `runner_tokens`, `runner_invites`), `kbeads/internal/server/` (routes)
- Create: `kbeads/internal/server/runners.go`, `runners_test.go`, `kbeads/internal/store/runner_tokens.go`, a migration file named by the next free number in `kbeads/internal/store/migrations/`

**Interfaces:**
- Produces: `POST /v1/runners/register` (body `{invite, hostname, os_arch, tags}`, returns `{runner_id, token}` once), `POST /v1/runners/{id}/heartbeat`, store methods `CreateRunnerToken(runnerID) (secret string, id int64, error)`, `ResolveRunnerToken(secret) (*Runner, error)`, `RevokeRunnerToken(runnerID)`. Tokens are `gr_` + 32 random bytes base64url; only the SHA-256 is stored. Fields per spec §4.1.

- [ ] **Step 1: Failing tests:** `TestRegisterConsumesInviteOnce` (second use of the same invite is a constant-time 404), `TestRegisterExpiredInvite404`, `TestTokenStoredHashed` (the bead and the table never contain the secret), `TestResolveRunnerTokenRevoked`, `TestHeartbeatUpdatesLastSeen`. Each "must fail" case is paired with the valid-invite success in the same table.
- [ ] **Step 2: Verify failure** `go test ./internal/server/ ./internal/store/ -run 'Runner' -count=1` (sqlmock, no Postgres).
- [ ] **Step 3: Implement.** Constant-time compare for invite codes; 10 registrations per hour per IP is enforced at the ingress (Task 10), not here.
- [ ] **Step 4: Verify** same command, expect PASS.
- [ ] **Step 5: Commit** `feat(kbeads): runner beads, hashed runner tokens, register and heartbeat`

### Task 8: `RoleRunner`: auth matrix, owner attribution, `agent=` ownership check

**Gated on:** the `requireRole` fix in P7 Phase 4 (`interceptors.go:143-147` admits every non-OIDC caller to operator routes today). Do not start before gd-SxFOBp4IOB and gd-Js9psMOccC are closed. This task touches `interceptors.go`: merge-desk path.

**Files:**
- Modify: `kbeads/internal/server/interceptors.go` (`RoleRunner` constant near `:24`, `roleRank`), `kbeads/internal/server/sse.go` (`handleEventStream`), `kbeads/internal/server/mail_control_labels.go` (add the runner-owner case)
- Create: `kbeads/internal/server/runner_authz_test.go`

**Interfaces:**
- Produces: a runner token resolves to `RoleRunner`, with `operated_by` stamped as the runner's `owner`. Allowed: register and heartbeat its own runner, create and update agent beads whose `runtime_id` is its runner, read and claim beads in its allowlisted projects, subscribe to `agent=` for its own agents, write squawks, comments and escalations. Denied: other projects, list runners, config beads, approve anything.

- [ ] **Step 1: Failing matrix test**, one row per endpoint group in spec §4.3 crossed with {own runner token, other runner's token, revoked token, shared `kd-beads-token`, operator}. The own-runner row is the positive control for every denial in its column.
- [ ] **Step 2: Failing SSE test** `TestRunnerStreamRequiresOwnedAgentScope`: runner token with no `agent=` is 403, with `agent=<other runner's agent>` is 403, with its own is 200. (Review Focus 3.)
- [ ] **Step 3: Implement.** Settle U3 and record that pods are unaffected.
- [ ] **Step 4: Verify** `go test ./internal/server/ -run 'Runner|RoleRunner|ControlLabel' -count=1`.
- [ ] **Step 5: Commit** `feat(kbeads): RoleRunner with owner attribution and owned-agent SSE scope`

### Task 9: Slack: `/runner invite` and `/runner revoke`

**Files:**
- Modify: `gardenio/controller/internal/bridge/` (slash-command registry; find it with `grep -rn '"/spawn"\|"/roster"' internal/bridge`, record the file)
- Create: `gardenio/controller/internal/bridge/bot_runner.go`, `bot_runner_test.go`

**Interfaces:**
- Consumes: Task 7 (`runner_invites`, revoke), Slack-user to operator resolution (`gb identity` mapping, `internal/identity` or equivalent: settle and record).
- Produces: `/runner invite @person project-a project-b` (fleet operators only) creates a single-use 24 h invite and DMs the code; `/runner revoke <hostname>` sets `revoked_at`, `state=revoked`, and sends `control:kill` mail to the runner's active agents; `/runner status` posts the runner card (spec §9).

- [ ] **Step 1: Failing tests:** non-operator invoking `invite` gets an ephemeral refusal and creates no invite (paired with an operator creating one); `revoke` of an unknown hostname says so; revoke kills agents via mail, asserted on the fake daemon.
- [ ] **Step 2 to 4:** run, implement, verify (`go test ./internal/bridge/ -run Runner -count=1`).
- [ ] **Step 5: Commit** `feat(gardenio/bridge): /runner invite, revoke, status`

### Task 10: Public listener for runners (chart): path allowlist, runner-token-only, rate limits

**Merge-desk path** (`gardenio/helm/**`). **Files:**
- Modify: `gardenio/helm/gardenio/values.yaml` (new `runnerFrontDoor.{enabled,hostname,tlsIssuer}`, default off), `gardenio/helm/gardenio/templates/` (new `runner-frontdoor.yaml`: IngressRoute, Middleware stripping nothing and allowing only the five groups of spec §4.3, rate-limit middleware, a ForwardAuth or plugin middleware rejecting any bearer not starting `gr_`)
- Test: `gardenio/helm/gardenio/tests/` (the chart Go module; `make test-helm`)

- [ ] **Step 1: Failing chart tests:** default values render no front-door objects; with `runnerFrontDoor.enabled=true` and a hostname, the IngressRoute exists, the allowlist covers exactly the five path groups, a request for `/v1/config` is matched by no route (so Traefik returns 404), and the token-prefix middleware is attached. Positive control: a render with the flag off still passes the existing 203 tests.
- [ ] **Step 2 to 4:** `make test-helm`, implement, `helm lint gardenio/helm/gardenio/`.
- [ ] **Step 5: Commit** `feat(gardenio/helm): optional public listener for local runners (off by default)`

---

## Phase 3: Local runner and placement

### Task 11: `gb runner register|status|leave` and token storage

**Files:** Create `gardenio/controller/cmd/gb/runner.go`, `runner_token.go`, `runner_test.go`. Modify the command registration in `cmd/gb/main.go` (record the exact line).

- [ ] **Step 1: Failing tests:** `TestRunnerRegisterStoresTokenModeless0600` (token file mode 0600 and never printed to stdout/stderr; the keychain path is behind an interface with a fake and a file fallback), `TestRunnerRegisterBadInvite` (error text exactly `invite not valid; ask for a new one`), `TestRunnerStatusJSON`.
- [ ] **Step 2 to 4:** run, implement (`runner_token.go`: macOS Keychain and Linux secret-service via an optional dependency, else file fallback; settle the library choice and record it), verify `go test ./cmd/gb/ -run Runner -count=1`.
- [ ] **Step 5: Commit** `feat(gardenio/gb): gb runner register, status, leave`

### Task 12: `gb runner join`

**Files:** Modify `gardenio/controller/cmd/gb/runner.go`, `agent_start.go` (extract the body of `runLocal` at `:206` into a function `join` can call without flag plumbing; do not change `gb agent start --local` behaviour).

- [ ] **Step 1: Failing tests:** `TestJoinCreatesLocalAgentBead` asserts the created bead has `execution_target=local`, `runtime_id=<runner bead id>`, `operated_by=<owner>`, `role=local`, the project from the flag; `TestJoinStartsInboxAndHeartbeat`; `TestJoinOnRevokedToken401StopsWithMessage` (message `token revoked or expired; ask for a new invite`, exit non-zero, no retry); `TestJoinRefusesProjectOutsideAllowlist`.
- [ ] **Step 2 to 4:** run, implement, verify `go test ./cmd/gb/ -run 'Join|AgentStart' -count=1`.
- [ ] **Step 5: Commit** `feat(gardenio/gb): gb runner join starts a local agent under the runner token`

### Task 13: Placement resolver

**Files:** Create `gardenio/controller/internal/placement/resolve.go`, `resolve_test.go`. Modify the spawn path in `internal/bridge/bot_mentions.go` and the `/spawn` handler (record both call sites after reading) to call it.

**Interfaces:**
- Produces:

```go
type Decision struct {
	Target  string // "cluster" | "runner"
	Runner  string // runner bead id when Target == "runner"
	Ask     []string // candidate runner ids when the owner has several online
	Reason  string
}

func Resolve(in Input) Decision
```

`Input` carries the task fields (`placement`, `runner_tag`, `runner_id`), the `--runner` flag, the Slack poster's operator id, whether the thread was opened by that operator, and the online runners with owner, tags, projects.

- [ ] **Step 1: Failing table test** with every case in spec §7 and §10: field + tag, field + id, tag with no matching runner (`Target: cluster`, reason says so, **never** another owner's runner), owner thread with 0 / 1 / 2 online runners (cluster+message / runner / `Ask` both), mention by someone who is not the owner (cluster), project outside the allowlist (cluster), flag by hostname, and the invariant case "pool pressure never picks a runner" (a task with no placement signal and a runner online resolves to cluster).
- [ ] **Step 2 to 4:** run `go test ./internal/placement/ -count=1`, implement as a pure function, verify.
- [ ] **Step 5: Wire** the bridge call sites; test `TestSpawnFromOwnerThreadWithOneRunnerPlacesOnRunner` and its control `TestSpawnFromOtherUsersThreadStaysOnCluster`.
- [ ] **Step 6: Commit** `feat(gardenio/bridge): explicit placement resolver for local runners`

### Task 14: Paused, not dead: hold on disconnect, local card, paused message

**Files:** Modify `gardenio/controller/internal/statusreporter/stalereaper.go`, `internal/bridge/nudge.go` (`deadAgentStates` region), the agent card and roster renderers (find with `grep -rn 'agent_state' internal/bridge/*card* internal/bridge/*roster*` and record), `kbeads` roster queries if they enumerate states.

- [ ] **Step 1: Settle U1** and list every consumer of agent state in a task comment. Then write `TestAgentStateConsumersKnowPaused`, a table test over each consumer.
- [ ] **Step 2: Failing tests:** `TestStaleReaperPausesLocalAgentWhenRunnerAway` (sets `agent_state=paused`, leaves `task_id` and assignee, posts exactly one thread message with the text from spec §9, and does **not** respawn), `TestStaleReaperStillReapsPodAgents` (positive control: same staleness on a pod agent is reaped as before), `TestHeartbeatResumeRestoresWorking`, `TestLocalCardShowsRunningAsOwner`.
- [ ] **Step 3 to 5:** run, implement, verify `go test ./internal/statusreporter/ ./internal/bridge/ -run 'Paused|StaleReaper|LocalCard' -count=1`.
- [ ] **Step 6: Release.** A kill reaction or `/kill` on a paused agent closes the bead and returns the task to ready; test `TestKillReleasesPausedTask`.
- [ ] **Step 7: Commit** `feat(gardenio): paused state for local agents, local badge, hold on disconnect`

---

## Phase 4: Escalations across the boundary

> Tasks 15 and 16 gate on P7 Phase 4 closing (see Global Constraints). Task 17 and 18 follow them.

### Task 15: `target_runner` and `requesting_runner` fields with create-time validation

**Files:** Modify `kbeads/internal/server/http_escalations.go`, `kbeads/internal/server/project_role_validation.go` only if it holds escalation field validation (settle), `kbeads/internal/client/escalations.go`; tests in `kbeads/internal/server/escalation_test.go`.

- [ ] **Step 1: Failing tests:** `execution_target=local` without `target_runner` is rejected at create (positive control: with it, accepted); `target_runner` naming a runner whose owner is not in the requester's reachable set is rejected; `target_runner` and `execution_target` are immutable after create (extend the `requesting_agent` immutability pattern, positive control: `reason` text is still editable by the requester while open); `requesting_runner` can only be set by a `RoleRunner` caller and is stamped from the token, never from the body.
- [ ] **Step 2 to 4:** run `go test ./internal/server/ -run Escalation -count=1`, implement, verify.
- [ ] **Step 5: Commit** `feat(kbeads): escalation target_runner and requesting_runner with create-time validation`

### Task 16: The daemon emits `control:escalation-approved`; the watcher drains mail

**Files:** Modify `kbeads/internal/server/http_escalations.go` (approve handlers), `gardenio/controller/cmd/gb/agent_start_escalation.go` (rewrite `stream`/`handleEvent`, `:167`, `:226`), `gardenio/controller/internal/bridge/escalations.go` (delete the `approved_by`-non-empty trigger).

- [ ] **Step 1: Failing tests (daemon):** approving an escalation with `execution_target=local`, `target_runner=R` creates exactly one mail assigned to R's active agent (or to `runner:<R>` inbox name if none: settle by reading how `gb runner join` names the agent, record it) labelled `control:escalation-approved` naming the escalation id, **after** the execution claim is taken; approving twice creates no second mail; positive control: a `server`-target escalation creates no mail and still runs the replay path.
- [ ] **Step 2 (client):** `TestWatcherExecutesOnlyMailForOwnRunner`: mail for another runner is ignored, the same mail for this runner executes; the project-wide SSE watch is gone.
- [ ] **Step 3 to 5:** run both packages scoped, implement, verify.
- [ ] **Step 6: Commit** `feat(escalations): approve emits control mail; local watcher executes only its own runner's`

### Task 17: Owner-only approval card

**Files:** Modify `gardenio/controller/internal/bridge/escalations.go` (card builder and the interactive handler); tests alongside.

- [ ] **Step 1: Settle U6.** Failing tests: the request card is posted to the runner owner's DM or thread, not a channel; a non-owner click returns an ephemeral `This runs on <hostname>, which belongs to <owner>. Only they can approve it.` and changes nothing; the owner's click approves; with `requires_four_eyes` a second operator is needed (positive control for each refusal is the matching success).
- [ ] **Step 2 to 4:** run `go test ./internal/bridge/ -run EscalationCard -count=1`, implement, verify.
- [ ] **Step 5: Commit** `feat(gardenio/bridge): cloud-to-local escalation cards approvable only by the runner owner`

### Task 18: End-to-end with the claudeless mock

**Files:** Create tests in `gardenio/tests/e2e` following the existing harness (record the nearest existing e2e file as the template).

- [ ] **Step 1: Write the six scenarios of spec §11 (a) to (f)**, M1 subset: (a) pod agent receives nudge, decision, kill via mail with the dial deleted; (b) register with invite, join, claim, squawk appears in the thread; (c) cloud-to-local escalation with owner approval **including refusal of a non-owner click**; (e) disconnect, paused, resume, unread mail drains; (f) public listener exposes only the allowlisted paths and rejects the shared token. (d) local-to-cloud is M2.
- [ ] **Step 2: Run** the scenarios through CI (`make preflight` or the e2e job); they are too heavy for the pod.
- [ ] **Step 3: Commit** `test(gardenio/e2e): local runner and cross-boundary escalation scenarios`

---

## Milestone 2 (outline only; tasks written after M1 lands)

Not minted as verbatim tasks now. Existing step beads that map here keep their titles and get a comment pointing at this section: S3 session resume for local agents (gd-bK7PkHkrRy), local-to-cloud escalation (gd-38wYfunSqF, spec §8.3), per-runner policy (gd-yXG2jNohHH, spec §8.4), broker credential mode.

## Mapping onto the existing P4 step beads

| Task | Step bead | Disposition |
|---|---|---|
| (spike) | gd-ptjw0XcN0n, gd-qVFbBLLrpu | Superseded: D2/D3 answer what the spike was for. Close with a comment pointing at this plan. |
| 1, 2, 3, 4 | gd-ZCCmu7nwUp "Define agent-inbound event topics" | Retitle to the mail design; mint Tasks 1 to 4 here (or split into one step per task). |
| 5 | gd-TMZriK9DrR "Slack bridge and controller publish events instead of dialing" | Mint Task 5. |
| 6 | gd-c8yB95OCd3 "Remove the inbound dial path" | Mint Task 6. |
| 7 to 10 | (none) | New phase "Runner identity and front door" with four new steps. |
| 11, 12 | gd-SzPJDNTBXS "type:role local-agent" | Replace: the local role has no capability list (D6); mint Tasks 11 and 12 under a new "gb runner" step. |
| 13 | gd-flFEezwcxf "Explicit placement" | Mint Task 13. |
| 14 | gd-oY7I16zD5I "Agent card and /roster mark local agents", gd-PjlxxDPvaz "Lid-close and sleep policy" | Both fold into Task 14; close the second as superseded by D5. |
| 15, 16 | gd-yNRhwrTKEN "execution_target=local routes to a NAMED runner" | Mint Tasks 15 and 16. |
| 17 | gd-UAUvpQPOcj "Cluster agent escalates to a human's laptop" | Mint Task 17. |
| 18 | gd-jW7KJ7ZdNy "End-to-end e2e" | Mint Task 18. |
| M2 | gd-bK7PkHkrRy, gd-38wYfunSqF, gd-yXG2jNohHH | Comment only. |

Dependency edges to add when minting (blocked bead first): Task 4 blocked by 3; 5 by 3 and 4; 6 by 5; 8 by 7 and by the P7 requireRole fix; 12 by 3, 8, 11; 14 by 12; 15 by 8 and P7 Phase 4; 16 by 15 and 5; 17 by 16; 18 by 6, 14, 17.
