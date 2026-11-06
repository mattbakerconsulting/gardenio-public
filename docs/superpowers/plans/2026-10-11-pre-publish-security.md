# Pre-publish Security Close-out (P7) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the holes a technical reviewer would find before the repository goes public: agents holding the trust-anchor token, the `X-Operated-By` relay granting authority, the open Slack bridge web API, Secret values left in `last-applied-configuration`, pod access to IMDS, and the escalation verbs that any agent can drive; then publish a SECURITY.md that says what is closed and what is not.

**Architecture:** Phase 1 gives agents their own credential (per-agent fleet tokens resolved to the agent's name) so the daemon can tell an agent from a bridge, a service, and a human, then narrows the relay and the bridge web API on top of that, and removes cleartext Secret copies from annotations. Phase 2 denies IMDS with one allow-all-but-one NetworkPolicy template and proves it from a CI runner pod. Phase 4 adds caller classes to the escalation verbs, makes the daemon the only writer of decision fields, and gives every approved escalation exactly one executor chosen and claimed inside the approve handlers. Phase 3's SECURITY.md is written last.

**Tech Stack:** Go 1.25 (modules `kbeads` and `gardenio/controller`), kbeads HTTP server (`kbeads/internal/server`), Postgres store (`kbeads/internal/store/postgres`), Slack bridge (`gardenio/controller/internal/bridge`, `cmd/slack-bridge`), Helm chart `gardenio/helm/gardenio` with Go render tests in `gardenio/helm/gardenio/tests`, raw manifests under `gardenio/helm/{gardenio-ziti,beads,ziti}`, bash and Python guards under `scripts/`, GitHub Actions `checks.yml`, e2e scripts under `gardenio/tests/e2e/scripts`.

**Spec:** `docs/superpowers/specs/2026-10-11-pre-publish-security-design.md` (§0 binding; §1 evidence with anchors; §2 overlap with the identity lane; §3 to §5 design per phase; §6 what stays open; §7 owner questions and their defaults).

**Plan bead:** P7 `gd-5f5bJPKLhC` under umbrella epic `gd-CGO0GT7Wp6`. Phases: 1 `gd-VkxF0GYXkq`, 2 `gd-Iu9JOTe8Qu`, 3 `gd-mu3cEUKoAV`, 4 `gd-pTmaK6Uuem`.

## Global Constraints

- Spec: findings bundle gd-I9dFR2el5V (read with `gb bundle doc <doc-id>`). Program and sequencing: bundle doc gd-H90Z1Rug2E. Viewer: https://gardenio.clonk.tv/bundle/gd-I9dFR2el5V
- Owner goal is career visibility (acqui-hire or contracting), not a product company. Prefer what a reviewer sees over what a tenant needs.
- Slack is the headline. Any user-visible change must read well in a Slack thread first.
- No live-cluster helm/kubectl from crews; chart edits via PR only (standing rule gd-oI2AIaSzjF).
- Branch per task: agent/launch/<task-id>; PR against main; crews self-merge on green per the drop-gate policy.
- plan_state stays draft/ready until the owner clicks Approve & build in the cockpit.
- Before Approve & build, write the task-level plan doc under docs/superpowers/plans/ and mint verbatim task bodies onto the step beads (house minting recipe).
- Escalation wording on every public surface (PR descriptions, SECURITY.md, Slack text, commit messages): "designed, exercised, enforcement in progress". Never "enforced".
- Steps marked **OPERATOR** are run by the owner or the laptop operator session, never by a crew or captain. A crew finishes its PR and stops at the first OPERATOR step, commenting on its bead that the PR is merged and the operator step is next.
- Every "cannot" test is paired with a positive control: the same request from an allowed identity must succeed in the same test (memory `ci-guards-that-cannot-fail`).
- Never print a Secret value, a token, or a `last-applied-configuration` body in logs, test output, PR text, or Slack.
- Go: `gofumpt -l`, `go vet ./...`, `staticcheck ./...` clean on the touched module; `go test` on the touched packages passes. Gate every commit on the test command's exit code (`go test ./... && git commit ...`), never on `tail` output.
- Bash: `set -euo pipefail`; `shellcheck -x` clean; no producer piped into `grep -q` (`scripts/check-sigpipe-producers.py`).
- Touching `kbeads/internal/server/*authz*`, `interceptors.go`, `edge_auth.go`, `deploy-values/**`, `gardenio/helm/**`, or `.github/workflows/**` makes the PR a merge-desk PR (working rule (d)); say so in the PR description.
- Bridge fleet subjects env: `BEADS_BRIDGE_SUBJECTS`, default `slack-bridge,gitlab-bridge,jira-bridge,github-bridge`.
- Phase 4 release rule (OPERATOR): Tasks 9 to 15 land in dependency order and no release containing Task 9 is deployed until Task 13 has merged.

## Review Focus

1. **A long-running crew that started before the per-agent-token rollout.** Its pod still holds the shared token. It must keep working until it exits, and a respawn must come back with a per-agent token, never the shared one. Pinned: Task 1 step 6 (`TestProvisionAgentToken_FailureRefusesSpawn`) and Task 2 step 6 (live check on a fresh crew only).
2. **The owner rejecting from the beads viewer after Task 9.** Slack has no escalation buttons ("Approval happens in the web UI", `gardenio/controller/internal/bridge/bot_escalations.go:767`); the viewer's reject today rides the static token, which Task 9 refuses. Task 9 is blocked by gd-0ORn0Mhr4I so the viewer reaches the daemon as the human first. Pinned: Task 9 step 2 (`TestRejectResult_CallerClasses`: operator 200, static 403) and Task 10 step 1 (`TestReject_StampsApproverIdentity`).
3. **Re-approving an escalation whose server replay failed.** Today the replay claim is released on failure so a second approve retries. The new dispatch claim must not wedge that. Pinned: Task 12 step 1 (`TestApprove_ServerReplayFailureCanBeRetried`).
4. **The reaper closing a verdict record out-of-band.** After the `fields` allowlist, the reaper's close must still render "Complete" in Slack and still land its `reaped*` and MR witness keys. Pinned: Task 10 step 1 (`TestResult_BridgeRenderAllowlist`) and Task 10 step 6 (reaper test: every key passes the allowlist).
5. **A Secret recreated from a runbook after the sweep.** The guard must catch the classic `kubectl create secret ... --dry-run=client -o yaml | kubectl apply -f -` pipe, not only `kubectl apply -f secret.yaml`. Pinned: Task 5 step 1 (planted pipe in the lint test).

## Task to step map and dependencies

| Task | Step bead | Phase |
|---|---|---|
| 1 Agent identity for per-agent tokens | gd-ik2wf4ncGp | 1 |
| 2 Roll out per-agent tokens and split the coopmux token | NEW (minted with this plan) | 1 |
| 3 Relay is attribution, never authority; approver identity | NEW (minted with this plan) | 1 |
| 4 Slack bridge web API fails closed | gd-3tOmdG4moG | 1 |
| 5 Strip `last-applied-configuration` from Secrets; guard; runbooks | NEW (minted with this plan) | 1 |
| 6 IMDS deny NetworkPolicy | gd-fetk49kFlz | 2 |
| 7 Prove IMDS is unreachable from a runner and a crew | gd-2yCgKBKaOs | 2 |
| 8 SECURITY.md | gd-0aRM4lYkfY | 3 |
| 9 Caller classes on escalation verbs | gd-nPfSNHTkQQ | 4 |
| 10 Server-stamped result and reject | gd-Cg0ykgRoyD | 4 |
| 11 Escalation decision fields immutable | gd-tZYAufHCdo | 4 |
| 12 One dispatch, one claim | gd-88AQJozzvX | 4 |
| 13 Named local target; service_account runs a privileged role | gd-46OpRA7jHg | 4 |
| 14 Four-eyes on every verb; replay-unfit refused at create | gd-D2ygHeriDy | 4 |
| 15 Mutation history, non-empty operation_spec, derived approval_method | gd-kEce0G9n1a | 4 |

Dependency order (P7-internal): 1 → 2 → 3; 1 → 9; 2 → 9; 9 → 10 → 12 → 13; 13 → 11; 4 → 11; 13 → 14; 13 → 15; 6 → 7; every other task → 8.

Cross-lane blockers (edges on the P7 step only): 3 ← gd-bFFquoScHy (PR #249), gd-0ORn0Mhr4I; 9 ← gd-l1Jh9aFEXY, gd-MBsUUXpJZB, gd-bFFquoScHy, gd-0ORn0Mhr4I. Tasks 4, 5, and 6 have no blockers and can start at once. PR #250 (gd-7CDM9gdog0) is not a blocker: Slack has no escalation approve or reject buttons.

## Shared test helper (created by Task 1, used by Tasks 3, 9 to 15)

`kbeads/internal/server/classed_test_server_test.go` (Task 1 step 1) provides:

- `newClassedTestServer(t) (*BeadsServer, *mockStore, http.Handler)`: primary static token `testStaticToken`, per-agent fleet token `testAgentToken` (fleet `agent-gd-a1`, subject `crew-a`, project `gardenio`), a second per-agent token `testAgentToken2` (fleet `agent-gd-a2`, subject `crew-b`, project `gardenio`), bridge token `testBridgeToken` (fleet `slack-bridge`), a non-bridge non-agent fleet token `testOtherFleetToken` (fleet `qa-fleet`), and `bearerSubjectVerifier`, so any other bearer is an OIDC admin whose email is the bearer value.
- `doBearerJSON(t, h, method, path, bearer string, body any, headers map[string]string) *httptest.ResponseRecorder`.

---

## Phase 1: token path

### Task 1: Per-agent tokens become a first-class agent identity: subject is the agent name, agent-pod headers honoured, provisioning fails closed

**Step bead:** gd-ik2wf4ncGp. Spec §0 D1, §1.1, §3 "Agent identity". This is the precondition that makes gd-opMHKHiy9I closable: while agents hold the primary static token, they can mint any fleet token (`kbeads/internal/server/http_fleet_tokens.go:332-342` (`isAdminRequest`)), so no allowlist of trusted callers holds.

**Files:**
- Create: `kbeads/internal/server/agent_identity.go`, `kbeads/internal/server/agent_identity_test.go`, `kbeads/internal/server/classed_test_server_test.go`
- Modify: `kbeads/internal/server/interceptors.go:907-922` (`if id.Type == "fleet"`) (HTTP fleet branch); the gRPC twin in `AuthInterceptor` (find with `grep -n 'Fleet-token bridge attribution' kbeads/internal/server/interceptors.go`; the first hit is the gRPC branch)
- Modify: `kbeads/internal/server/escalation_requesting_agent.go:35-41` (`applyEscalationRequestingAgentGuard`)
- Modify: `gardenio/controller/cmd/controller/agenttoken.go:50-60` (`agentTokenSubject`), `:84-102` (`provisionAgentToken`)
- Modify: `gardenio/controller/cmd/controller/main.go:2622-2631` and `:2990-2999` (fallback to shared token)
- Modify: `gardenio/controller/cmd/controller/microvm_credentials.go:55-70` (`resolveMicrovmBeadsToken`)
- Test: `gardenio/controller/cmd/controller/agenttoken_test.go`

**Interfaces:**
- Consumes: `FleetFromContext(ctx) string` (`kbeads/internal/server/fleet.go:496`), `AuthIdentityFromContext`, `sanitizeTriggerBeadHeader`, `WithTriggerBead`, `triggerBeadFromContext` (`kbeads/internal/server/audit.go:406-412`), `parseNoAutoParentHeader`, `WithNoAutoParent`, `noAutoParentFromContext` (`kbeads/internal/server/audit.go:338-344`), `FleetConfig.RegisterTokenFull(token, fleet, project string, allowedTypes, allowedOps []string, expiresAt time.Time, subject string, privileged bool)`.
- Produces:
  - `const agentFleetPrefix = "agent-"`
  - `func isAgentFleet(fleet string) bool`
  - `func agentNameFromContext(ctx context.Context) (string, bool)`: returns `(Subject, true)` when the identity is type `fleet`, `FleetFromContext` has the agent prefix, and Subject is non-empty.
  - Controller: `func agentTokenSubject(agentName string) string` (returns the agent name; the bead id is no longer the subject), `provisionAgentToken(ctx, logger, daemon, k8sClient, beadID, agentName, project, namespace string, tm *tokenMetrics) (string, error)`, `resolveMicrovmBeadsToken(ctx, logger, daemon, cfg, beadID, agentName, project string) (access, refresh string)`.
  - Test helper `newClassedTestServer` and `doBearerJSON` (see "Shared test helper" above).

- [ ] **Step 1: Write the shared test helper**

Create `kbeads/internal/server/classed_test_server_test.go`:

```go
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
)

const (
	testStaticToken     = "static-tok"
	testAgentToken      = "agent-tok-a"
	testAgentToken2     = "agent-tok-b"
	testBridgeToken     = "bridge-tok"
	testOtherFleetToken = "other-fleet-tok"
)

// newClassedTestServer wires one server with every caller class the P7 tasks
// distinguish: the primary static token, two per-agent fleet tokens, a bridge
// fleet token, an unlisted fleet token, and bearerSubjectVerifier so any other
// bearer authenticates as an OIDC admin whose email is the bearer value.
func newClassedTestServer(t *testing.T) (*BeadsServer, *mockStore, http.Handler) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull(testAgentToken, "agent-gd-a1", "gardenio", nil, nil, time.Time{}, "crew-a", false)
	fc.RegisterTokenFull(testAgentToken2, "agent-gd-a2", "gardenio", nil, nil, time.Time{}, "crew-b", false)
	fc.RegisterTokenFull(testBridgeToken, "slack-bridge", "", nil, nil, time.Time{}, "", false)
	fc.RegisterTokenFull(testOtherFleetToken, "qa-fleet", "", nil, nil, time.Time{}, "", false)
	s.SetFleetAuth(fc, testStaticToken)
	return s, ms, s.NewHTTPHandler(testStaticToken, &bearerSubjectVerifier{}, fc)
}

// doBearerJSON sends body as JSON with Authorization: Bearer <bearer> and any
// extra headers.
func doBearerJSON(t *testing.T, h http.Handler, method, path, bearer string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		buf = bytes.NewReader(b)
	} else {
		buf = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
```

- [ ] **Step 2: Write the failing tests**

Create `kbeads/internal/server/agent_identity_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The agent token's created_by is the agent name from the token subject; an
// X-Actor header (honoured only for the primary static token) cannot change it.
// Positive control: the static token's X-Actor still sets created_by.
func TestAgentToken_CreatedByIsAgentNameNotXActor(t *testing.T) {
	_, _, h := newClassedTestServer(t)
	body := map[string]any{"title": "t", "type": "task", "labels": []string{"project:gardenio"}}

	rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, map[string]string{"X-Actor": "impostor"})
	requireStatus(t, rec, http.StatusCreated)
	var got map[string]any
	decodeJSON(t, rec, &got)
	if got["created_by"] != "crew-a" {
		t.Fatalf("agent token created_by = %v, want crew-a", got["created_by"])
	}

	rec = doBearerJSON(t, h, "POST", "/v1/beads", testStaticToken, body, map[string]string{"X-Actor": "static-actor"})
	requireStatus(t, rec, http.StatusCreated)
	decodeJSON(t, rec, &got)
	if got["created_by"] != "static-actor" {
		t.Fatalf("positive control: static X-Actor created_by = %v, want static-actor", got["created_by"])
	}
}

// The agent branch honours X-Trigger-Bead and X-No-Auto-Parent (agent-pod
// headers) exactly as the static branch does; an unlisted fleet token does not.
func TestAgentToken_HonoursAgentPodHeaders(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	var trigger string
	var noParent bool
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trigger = triggerBeadFromContext(r.Context())
		noParent = noAutoParentFromContext(r.Context())
	})
	mw := AuthMiddleware(testStaticToken, s.fleetCfg, &bearerSubjectVerifier{}, capture)

	for _, tc := range []struct {
		bearer       string
		wantHonoured bool
	}{
		{testAgentToken, true},
		{testStaticToken, true},       // positive control: the pre-existing static behaviour
		{testOtherFleetToken, false},  // a non-agent fleet token does not get agent headers
	} {
		trigger, noParent = "", false
		req := httptest.NewRequest("GET", "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer "+tc.bearer)
		req.Header.Set("X-Trigger-Bead", "gd-trigger1")
		req.Header.Set("X-No-Auto-Parent", "1")
		mw.ServeHTTP(httptest.NewRecorder(), req)
		if (trigger == "gd-trigger1") != tc.wantHonoured || noParent != tc.wantHonoured {
			t.Errorf("bearer %s: trigger=%q noParent=%v, want honoured=%v", tc.bearer, trigger, noParent, tc.wantHonoured)
		}
	}
}

// agentNameFromContext recognises only agent-prefixed fleet identities.
func TestAgentNameFromContext(t *testing.T) {
	ctx := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"}), "agent-gd-a1")
	if name, ok := agentNameFromContext(ctx); !ok || name != "crew-a" {
		t.Fatalf("agent fleet: got (%q,%v)", name, ok)
	}
	bridge := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"}), "slack-bridge")
	if _, ok := agentNameFromContext(bridge); ok {
		t.Fatal("a bridge fleet identity is not an agent")
	}
	static := WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "crew-a"})
	if _, ok := agentNameFromContext(static); ok {
		t.Fatal("the static token is not an agent identity, even with an agent-looking X-Actor")
	}
}

// An escalation filed with an agent token gets requesting_agent stamped from
// the token, whatever the body says.
func TestEscalationRequestingAgent_StampedForAgentToken(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, map[string]any{
		"title": "esc", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}, "requesting_agent": "victim"},
	}, nil)
	requireStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	var fields map[string]any
	_ = json.Unmarshal(ms.beads[created["id"].(string)].Fields, &fields)
	if fields["requesting_agent"] != "crew-a" {
		t.Fatalf("requesting_agent = %v, want crew-a", fields["requesting_agent"])
	}
}

// Pins existing behaviour that matters once agents hold fleet tokens: only the
// primary token can mint a bridge/fleet token. Positive control included.
func TestAgentToken_CannotMintBridgeToken(t *testing.T) {
	_, _, h := newClassedTestServer(t)
	body := map[string]any{"token": "minted-" + time.Now().Format("150405.000"), "bridge": "slack-bridge"}
	if rec := doBearerJSON(t, h, "POST", "/v1/bridge-tokens", testAgentToken, body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("agent token minting a bridge token: status %d, want 403", rec.Code)
	}
	body["token"] = "minted2-" + time.Now().Format("150405.000")
	if rec := doBearerJSON(t, h, "POST", "/v1/bridge-tokens", testStaticToken, body, nil); rec.Code != http.StatusCreated {
		t.Fatalf("positive control: primary token minting: status %d, want 201; body %s", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd kbeads && go test ./internal/server -run 'TestAgentToken_|TestAgentNameFromContext|TestEscalationRequestingAgent_StampedForAgentToken' -count=1`
Expected: FAIL. `agentNameFromContext` is undefined (compile error). After Step 4 creates it, `TestAgentToken_HonoursAgentPodHeaders` and `TestEscalationRequestingAgent_StampedForAgentToken` still fail until Step 5. `TestAgentToken_CreatedByIsAgentNameNotXActor` and `TestAgentToken_CannotMintBridgeToken` pass once the file compiles: they pin existing behaviour (`created_by` is already the fleet subject, `kbeads/internal/server/beads.go:265`) that the rollout depends on.

- [ ] **Step 4: Implement the agent identity helpers**

Create `kbeads/internal/server/agent_identity.go`:

```go
package server

import (
	"context"
	"strings"
)

// agentFleetPrefix is the fleet-name prefix the controller gives every
// per-agent token (agentTokenBridgeName in gardenio/controller/cmd/controller/
// agenttoken.go). A fleet identity with this prefix is an AGENT: its subject is
// the agent's own name, issued by the controller, and it is the only beads
// credential an agent pod holds once agents.perAgentTokens is on (P7 Task 2).
const agentFleetPrefix = "agent-"

func isAgentFleet(fleet string) bool {
	return strings.HasPrefix(fleet, agentFleetPrefix) && len(fleet) > len(agentFleetPrefix)
}

// agentNameFromContext returns the authenticated agent name for a per-agent
// fleet token. The primary static token is never an agent identity here, even
// when its X-Actor names an agent: X-Actor is a claim, the per-agent token is
// a credential.
func agentNameFromContext(ctx context.Context) (string, bool) {
	id := AuthIdentityFromContext(ctx)
	if id.Type != "fleet" || id.Subject == "" || !isAgentFleet(FleetFromContext(ctx)) {
		return "", false
	}
	return id.Subject, true
}
```

- [ ] **Step 5: Honour agent-pod headers on the agent branch and stamp requesting_agent**

In `kbeads/internal/server/interceptors.go`, immediately after the closing brace of the `if id.Type == "fleet" { ... }` block at `:907-922`, insert:

```go
			// Per-agent tokens (P7 Task 1): an agent pod that authenticates with
			// its own fleet token keeps the agent-pod headers the static branch
			// honours for it today. X-Actor, X-Fleet, X-System-Operator and
			// X-Spawn-Privileged-Override stay static-only: the agent's name
			// comes from its credential, not from a header.
			if id.Type == "fleet" && isAgentFleet(FleetFromContext(ctx)) {
				if raw := r.Header.Get("X-Trigger-Bead"); raw != "" {
					if v, ok := sanitizeTriggerBeadHeader(raw); ok {
						ctx = WithTriggerBead(ctx, v)
					}
				}
				if parseNoAutoParentHeader(r.Header.Get("X-No-Auto-Parent")) {
					ctx = WithNoAutoParent(ctx)
				}
			}
```

Apply the same block to the gRPC `AuthInterceptor` fleet branch (first hit of `grep -n 'Fleet-token bridge attribution' kbeads/internal/server/interceptors.go`), reading the gRPC metadata keys `x-trigger-bead` and `x-no-auto-parent` the way that function's static branch reads them.

Replace `applyEscalationRequestingAgentGuard` at `kbeads/internal/server/escalation_requesting_agent.go:35-41` with:

```go
func (s *BeadsServer) applyEscalationRequestingAgentGuard(ctx context.Context, bead *model.Bead) {
	if name, ok := agentNameFromContext(ctx); ok {
		bead.Fields = setField(bead.Fields, "requesting_agent", name)
		return
	}
	id := AuthIdentityFromContext(ctx)
	if id.Type != "static" || id.Subject == "" || id.Subject == "static" {
		return
	}
	bead.Fields = setField(bead.Fields, "requesting_agent", id.Subject)
}
```

Run: `cd kbeads && go test ./internal/server -run 'TestAgentToken_|TestAgentNameFromContext|TestEscalationRequestingAgent' -count=1`
Expected: PASS.

- [ ] **Step 6: Controller: subject is the agent name; provisioning failure refuses the spawn**

Write the failing test first, appended to `gardenio/controller/cmd/controller/agenttoken_test.go`:

```go
func TestAgentTokenSubject_IsAgentName(t *testing.T) {
	if got := agentTokenSubject("gardenio-crew-x1"); got != "gardenio-crew-x1" {
		t.Fatalf("agentTokenSubject = %q, want the agent name", got)
	}
	if got := agentTokenSubject(""); got != "" {
		t.Fatalf("empty agent name must stay empty, got %q", got)
	}
}
```

Find the existing test that pins the old `agent:<beadID>` subject (`grep -n 'agent:' gardenio/controller/cmd/controller/agenttoken_test.go`) and update it to expect the agent name. Then replace `agentTokenSubject` at `gardenio/controller/cmd/controller/agenttoken.go:50-60` with:

```go
// agentTokenSubject returns the audit subject the daemon attributes to
// requests authenticated by a per-agent token: the agent's own name, the same
// value agents send today as X-Actor, so every ownership gate that compares the
// actor with a bead's assignee keeps working (P7 Task 1). Empty stays empty.
func agentTokenSubject(agentName string) string {
	return agentName
}
```

Add `agentName string` after `beadID` in `provisionAgentToken` and pass `agentTokenSubject(agentName)` at `:100`. Do the same for `resolveMicrovmBeadsToken` (`microvm_credentials.go:55-70`) and its callers (`grep -n 'resolveMicrovmBeadsToken(' gardenio/controller/cmd/controller/*.go`), passing the agent name each caller already has.

Replace the fallback at `gardenio/controller/cmd/controller/main.go:2622-2631` with:

```go
		// Per-agent tokens: generate a project-scoped token for this agent.
		// Fail closed (P7 Task 1): falling back to the shared token would hand
		// this agent the primary credential the flag exists to keep from it.
		if cfg.PerAgentTokens && agentBeadID != "" {
			secretName, err := provisionAgentToken(ctx, logger, daemon, k8sClient, agentBeadID, event.AgentName, event.Project, spec.Namespace, tokMetrics)
			if err != nil {
				logger.Error("refusing spawn: per-agent token provisioning failed",
					"agent", event.AgentName, "bead", agentBeadID, "error", err)
				return fmt.Errorf("per-agent token provisioning failed for %s: %w", event.AgentName, err)
			}
			spec.DaemonTokenSecret = secretName
		}
```

and the twin at `:2990-2999` the same way (message `"refusing stuck-restart recreate: per-agent token provisioning failed"`). In `resolveMicrovmBeadsToken`, replace each `return strings.TrimSpace(os.Getenv("BEADS_DAEMON_TOKEN")), ""` fallback inside the `cfg.PerAgentTokens` branch with `return "", ""` and make its caller refuse the launch when `access == ""` and `cfg.PerAgentTokens` is true (find the caller with the grep above; log `"refusing microvm launch: per-agent token provisioning failed"`).

Add a test that the refusal happens (`TestProvisionAgentToken_FailureRefusesSpawn`) next to the existing spawn tests that exercise `cfg.PerAgentTokens` (find with `grep -n 'PerAgentTokens' gardenio/controller/cmd/controller/*_test.go`): make the fake daemon's `RegisterBridgeTokenWithRefresh` return an error and assert the spawn handler returns a non-nil error and creates no pod.

Run: `cd gardenio/controller && go test ./cmd/controller -run 'AgentToken|PerAgent|Microvm' -count=1`
Expected: PASS.

- [ ] **Step 7: Format, vet, full package tests**

Run: `cd kbeads && gofumpt -l internal/server && go vet ./internal/server && staticcheck ./internal/server && go test ./internal/server -count=1`
Run: `cd gardenio/controller && gofumpt -l cmd/controller && go vet ./cmd/controller && staticcheck ./cmd/controller && go test ./cmd/controller -count=1`
Expected: no gofumpt output; vet and staticcheck silent; tests PASS.

- [ ] **Step 8: Commit**

```bash
cd kbeads && go test ./internal/server -count=1 && cd ../gardenio/controller && go test ./cmd/controller -count=1 && cd ../.. && \
git add kbeads/internal/server/agent_identity.go kbeads/internal/server/agent_identity_test.go kbeads/internal/server/classed_test_server_test.go kbeads/internal/server/interceptors.go kbeads/internal/server/escalation_requesting_agent.go gardenio/controller/cmd/controller/ && \
git commit -m "feat(kbeads,controller): per-agent tokens are an agent identity named after the agent; provisioning fails closed (gd-ik2wf4ncGp)"
```

### Task 2: Roll out per-agent tokens and split the coopmux agent token from kd-beads-token

**Step bead:** NEW, Phase 1 (minted with this plan). Spec §0 D1, §1.1, §3 "Rollout". Blocked by Task 1.

**Files:**
- Modify: `gardenio/helm/gardenio/templates/coopmux/deployment.yaml:194-198` (`BEADS_TOKEN`)
- Modify: `deploy-values/gardenio-k3s.yaml:710-711` (`coopmux:`) and `:877-878` (`agents:`)
- Create: `gardenio/helm/gardenio/tests/per_agent_tokens_overlay_test.go`
- Create: `gardenio/tests/e2e/scripts/test-per-agent-token-identity.sh`
- Modify: `gardenio/tests/e2e/scripts/run-shard.sh:2977` (register the module next to `bridge-fleet-token-rotation`)

**Interfaces:**
- Consumes: Task 1's agent identity (`agent-` fleet prefix, subject = agent name).
- Produces: a Secret named `coopmux-agent-token` (key `token`) referenced by `coopmux.coopmuxTokenSecret`; overlay values `agents.perAgentTokens: true`.

- [ ] **Step 1: Write the failing chart test**

Create `gardenio/helm/gardenio/tests/per_agent_tokens_overlay_test.go`:

```go
package tests

import (
	"strings"
	"testing"
)

// P7 Task 2: the live overlay turns per-agent tokens on and gives agents a
// coopmux token that is not kd-beads-token; coopmux's own daemon bearer stays
// the beads token so coopmux keeps reaching the daemon.
func TestOverlay_PerAgentTokensOn(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t, "templates/controller/deployment.yaml",
		[]string{"../../../deploy-values/gardenio-k3s.yaml"})
	if !strings.Contains(out, "name: PER_AGENT_TOKENS") {
		t.Fatal("deploy-values/gardenio-k3s.yaml must set agents.perAgentTokens=true (controller env PER_AGENT_TOKENS)")
	}
	if !strings.Contains(out, "value: coopmux-agent-token") {
		t.Fatal("controller COOPMUX_TOKEN_SECRET must name coopmux-agent-token, not the beads token secret")
	}
}

func TestCoopmux_DaemonBearerStaysBeadsToken(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t, "templates/coopmux/deployment.yaml",
		[]string{"../../../deploy-values/gardenio-k3s.yaml"})
	i := strings.Index(out, "- name: BEADS_TOKEN")
	if i < 0 {
		t.Fatal("coopmux must still carry BEADS_TOKEN")
	}
	block := out[i:min(len(out), i+200)]
	if strings.Contains(block, "coopmux-agent-token") {
		t.Fatal("coopmux BEADS_TOKEN must come from the beads token secret, not the agent-facing coopmux token")
	}
	// Positive control: the agent-facing auth token IS the split secret.
	j := strings.Index(out, "- name: COOP_MUX_AUTH_TOKEN")
	if j < 0 || !strings.Contains(out[j:min(len(out), j+200)], "coopmux-agent-token") {
		t.Fatal("coopmux COOP_MUX_AUTH_TOKEN must come from coopmux-agent-token")
	}
}
```

Run: `cd gardenio/helm/gardenio/tests && go test -run 'TestOverlay_PerAgentTokensOn|TestCoopmux_DaemonBearerStaysBeadsToken' -count=1`
Expected: FAIL (overlay does not set either value).

- [ ] **Step 2: Make coopmux's daemon bearer independent of its agent-facing token**

In `gardenio/helm/gardenio/templates/coopmux/deployment.yaml`, replace lines `:194-198`:

```yaml
            - name: BEADS_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "gardenio.coopmux.authTokenSecretName" . }}
                  key: token
```

with:

```yaml
            {{- /* P7 Task 2: coopmux's own daemon bearer is the beads token.
                   coopmux.coopmuxTokenSecret is the AGENT-FACING token (agents
                   register with it); splitting it from kd-beads-token is what
                   keeps the primary daemon token out of agent pods. */}}
            - name: BEADS_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "gardenio.beads.tokenSecretName" . }}
                  key: token
```

- [ ] **Step 3: Set the overlay values**

In `deploy-values/gardenio-k3s.yaml`, directly under `coopmux:` (`deploy-values/gardenio-k3s.yaml:710`) add:

```yaml
  # P7 Task 2: agent-facing coopmux token, NOT kd-beads-token. Created by the
  # operator (no ExternalSecret): kubectl create secret generic coopmux-agent-token
  # --from-literal=token=<random 32 bytes hex>. Agents get it as COOP_MUX_TOKEN /
  # COOP_BROKER_TOKEN; coopmux's own daemon bearer stays the beads token.
  coopmuxTokenSecret: "coopmux-agent-token"
```

and directly under `agents:` (`deploy-values/gardenio-k3s.yaml:877`) add:

```yaml
  # P7 Task 2 (spec D1): every agent pod gets its own project-scoped beads
  # token named after the agent; the primary token leaves agent pods.
  perAgentTokens: true
```

Run: `cd gardenio/helm/gardenio/tests && go test -run 'TestOverlay_PerAgentTokensOn|TestCoopmux_DaemonBearerStaysBeadsToken' -count=1 && helm lint ../ -f ../../../../deploy-values/gardenio-k3s.yaml`
Expected: PASS; lint `0 chart(s) failed`.

- [ ] **Step 4: e2e module for the agent identity**

Create `gardenio/tests/e2e/scripts/test-per-agent-token-identity.sh`, copying the config, helper, port-forward, and admin-token discovery sections verbatim from `gardenio/tests/e2e/scripts/test-bridge-fleet-token-rotation.sh` (its `# ── Config`, `# ── Helpers`, `setup_portforward`, `setup_admin_token` sections), then:

```bash
# ── Scenarios ─────────────────────────────────────────────────────
AGENT_NAME="e2e-agent-${TEST_TAG}"
AGENT_TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"

register_agent_token() {
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BEADS_HTTP_URL}/v1/bridge-tokens" \
    -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"token\":\"${AGENT_TOKEN}\",\"bridge\":\"agent-gd-e2e${TEST_TAG//[^a-z0-9]/}\",\"project\":\"gardenio\",\"subject\":\"${AGENT_NAME}\",\"ttl\":\"10m\"}")
  [ "$code" = "201" ] && pass "registered an agent-class token" || fail "register agent token" "status $code"
}

scenario_created_by_is_agent() {
  local out
  out=$(curl -s -X POST "${BEADS_HTTP_URL}/v1/beads" -H "Authorization: Bearer ${AGENT_TOKEN}" \
    -H 'Content-Type: application/json' -H 'X-Actor: impostor' \
    -d "{\"title\":\"e2e ${TEST_TAG}\",\"type\":\"task\",\"labels\":[\"project:gardenio\",\"e2e:${TEST_TAG}\"]}")
  if [ "$(jq -r '.created_by' <<<"$out")" = "$AGENT_NAME" ]; then pass "created_by is the agent name, X-Actor ignored"; else fail "created_by" "$(jq -c '{created_by}' <<<"$out")"; fi
}

scenario_cannot_mint() {
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BEADS_HTTP_URL}/v1/bridge-tokens" \
    -H "Authorization: Bearer ${AGENT_TOKEN}" -H 'Content-Type: application/json' \
    -d '{"token":"should-not-exist-'"$TEST_TAG"'","bridge":"slack-bridge"}')
  [ "$code" = "403" ] && pass "agent token cannot mint a bridge token" || fail "agent mint" "status $code (want 403)"
}

setup_portforward
setup_admin_token || { dim "no admin token reachable; skipping"; exit 0; }
register_agent_token
scenario_created_by_is_agent
scenario_cannot_mint
curl -s -o /dev/null -X DELETE "${BEADS_HTTP_URL}/v1/bridge-tokens/agent-gd-e2e${TEST_TAG//[^a-z0-9]/}" -H "Authorization: Bearer ${BEADS_ADMIN_TOKEN}" || true
[ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
```

The positive control for `scenario_cannot_mint` is `register_agent_token`, the same request with the admin token, which must return 201.

Register it in `gardenio/tests/e2e/scripts/run-shard.sh` on the line after `:2977`:

```bash
  run_module "per-agent-token-identity" "./gardenio/tests/e2e/scripts/test-per-agent-token-identity.sh" || MODULE_RC=1
```

Run: `shellcheck -x gardenio/tests/e2e/scripts/test-per-agent-token-identity.sh`
Expected: silent.

- [ ] **Step 5: Commit and open the PR**

```bash
cd gardenio/helm/gardenio/tests && go test -run 'TestOverlay_PerAgentTokensOn|TestCoopmux_DaemonBearerStaysBeadsToken' -count=1 && cd - && \
git add gardenio/helm/gardenio/templates/coopmux/deployment.yaml deploy-values/gardenio-k3s.yaml gardenio/helm/gardenio/tests/per_agent_tokens_overlay_test.go gardenio/tests/e2e/scripts/test-per-agent-token-identity.sh gardenio/tests/e2e/scripts/run-shard.sh && \
git commit -m "feat(chart): per-agent tokens on; coopmux agent token split from kd-beads-token (P7 Task 2)"
```

The PR description says: merge-desk PR (touches `deploy-values/**` and `gardenio/helm/**`); merging changes nothing live until the operator steps below run.

- [ ] **Step 6: OPERATOR: create the Secret, roll out, verify on a fresh crew**

1. `kubectl -n gardenio create secret generic coopmux-agent-token --from-literal=token="$(openssl rand -hex 32)"` (create, never apply; see Task 5).
2. Roll out through the normal release (merged is not deployed; memory `gardenio-calver-releases`).
3. Spawn one throwaway crew, then list its secretKeyRef names only:
   `kubectl -n gardenio get pod <crew-pod> -o json | jq -r '.spec.containers[].env[]? | select(.valueFrom.secretKeyRef) | "\(.name) <- \(.valueFrom.secretKeyRef.name)"'`
   Expected: `BEADS_*` from `agent-token-<bead-id>`, `COOP_MUX_TOKEN`/`COOP_BROKER_TOKEN` from `coopmux-agent-token`, and **no line naming `kd-beads-token`**.
4. Have the crew create, update, and close one task bead; confirm `created_by` is its agent name.
5. Comment the results on this step bead. Rollback: set `agents.perAgentTokens: false` and release; agent pods return to the shared token on respawn.

### Task 3: X-Operated-By is attribution, never authority; approver identity is OIDC from an allowed client or verified v2 edge

**Step bead:** NEW, Phase 1 (minted with this plan). Spec §0 D2, D3, §1.3, §3 "Relay". Closes gd-opMHKHiy9I together with Tasks 1 and 2. Blocked by Task 2, gd-bFFquoScHy (PR #249 edits `escalationApprover`; rebase on it, do not re-implement its `azp` allowlist), and gd-0ORn0Mhr4I (the viewer must write with the user's bearer before its static-token relay loses authority; spec Q5).

**Files:**
- Create: `kbeads/internal/server/relay_trust.go`, `kbeads/internal/server/relay_trust_test.go`
- Modify: `kbeads/internal/server/server.go:27-110` (`type BeadsServer struct`) (field `bridgeSubjects map[string]bool`), `kbeads/internal/server/server.go:122-134` (`NewBeadsServer`) (default)
- Modify: `kbeads/internal/config/config.go:56` and `:148` (new `BridgeSubjects []string // BEADS_BRIDGE_SUBJECTS`), `kbeads/cmd/kd/serve.go:301` (call `SetBridgeSubjects`)
- Modify: `kbeads/internal/server/team_membership.go:123-130` (`teamsOf`), `kbeads/internal/server/taint_read_gate.go:83-90` (`grantsOfOperator`), `kbeads/internal/server/http_taint_grants.go:197-206` (`granterOperatorID`), `kbeads/internal/server/http_decisions.go:330-348` (`decisionAttribution`)
- Modify: `kbeads/internal/server/escalation_approval_authz.go:70-76` (`escalationApprover`) as it stands after PR #249
- Modify: the Helm beads deployment env (find with `grep -n 'BEADS_SERVICE_FIELD_FLEETS' gardenio/helm/gardenio/templates/beads/deployment.yaml`) to pass `BEADS_BRIDGE_SUBJECTS` from a new value `beads.bridgeSubjects` (default the four bridge names)

**Interfaces:**
- Consumes: `operatedByHeaderFromContext(ctx) string` (`kbeads/internal/server/audit.go:324`), `authMethodFromContext`, `AuthIdentityFromContext`, test helpers from Task 1.
- Produces:
  - `func (s *BeadsServer) SetBridgeSubjects(subjects []string)` (blank entries dropped; nil keeps the default list)
  - `func (s *BeadsServer) isBridgeSubject(subject string) bool`
  - `func (s *BeadsServer) relayTrusted(ctx context.Context) bool`
  - `func (s *BeadsServer) trustedRelayOperator(ctx context.Context) string` ("" unless `relayTrusted`)
  - `escalationApprover` accepts `edge` only when `authMethodFromContext(ctx) == authMethodOIDC` (verified v2).

- [ ] **Step 1: Write the failing tests**

Create `kbeads/internal/server/relay_trust_test.go`:

```go
package server

import (
	"context"
	"testing"
)

func relayCtx(idType, subject, fleet, relayed string) context.Context {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: idType, Subject: subject})
	if fleet != "" {
		ctx = WithFleet(ctx, fleet)
	}
	if relayed != "" {
		ctx = WithOperatedByHeader(ctx, relayed)
	}
	return ctx
}

// Only a bridge fleet subject's relay counts for access control. The static
// token (held by the viewer, controller, coopmux and, before Task 2, every
// agent) and an agent token relay attribution only.
func TestTrustedRelayOperator(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	const owner = "oidc:owner@example.com"
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"bridge relays", relayCtx("fleet", "slack-bridge", "slack-bridge", owner), owner},
		{"static does not", relayCtx("static", "static", "", owner), ""},
		{"agent does not", relayCtx("fleet", "crew-a", "agent-gd-a1", owner), ""},
		{"unlisted fleet does not", relayCtx("fleet", "qa-fleet", "qa-fleet", owner), ""},
	}
	for _, c := range cases {
		if got := s.trustedRelayOperator(c.ctx); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// The relay no longer resolves teams for a static caller; positive control:
// the same relay from the bridge does.
func TestTeamsOf_RelayOnlyFromBridge(t *testing.T) {
	s, ms, _ := newClassedTestServer(t)
	seedOperatorInTeam(t, ms, "oidc", "owner@example.com", "gd-team1")
	static := relayCtx("static", "static", "", "oidc:owner@example.com")
	if teams, _ := s.TeamsOf(static); len(teams) != 0 {
		t.Fatalf("static relay resolved teams %v; want none", teams)
	}
	bridge := relayCtx("fleet", "slack-bridge", "slack-bridge", "oidc:owner@example.com")
	if teams, _ := s.TeamsOf(bridge); len(teams) != 1 || teams[0] != "gd-team1" {
		t.Fatalf("positive control: bridge relay teams = %v, want [gd-team1]", teams)
	}
}

// decisionAttribution keeps the static caller's relayed human out of
// responded_by; the bridge's relay is kept.
func TestDecisionAttribution_RelayOnlyFromBridge(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	if got := s.decisionAttribution(relayCtx("static", "static", "", "oidc:owner@example.com"), ""); got != "static" {
		t.Fatalf("static relay: responded_by = %q, want static", got)
	}
	if got := s.decisionAttribution(relayCtx("fleet", "slack-bridge", "slack-bridge", "oidc:owner@example.com"), ""); got != "oidc:owner@example.com" {
		t.Fatalf("positive control: bridge relay responded_by = %q", got)
	}
}

// Verified v1 edge (authMethodEdge) is not an approver; verified v2 edge
// (authMethodOIDC with roles) is.
func TestEscalationApprover_EdgeV1Refused_V2Allowed(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	v1 := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "owner@example.com"}), authMethodEdge)
	if _, ok := s.escalationApprover(v1); ok {
		t.Fatal("verified v1 edge must not approve")
	}
	v2 := WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "owner@example.com"}), authMethodOIDC), []string{RoleOperator})
	if got, ok := s.escalationApprover(v2); !ok || got != "owner@example.com" {
		t.Fatalf("positive control: verified v2 edge approver = (%q,%v)", got, ok)
	}
}
```

`seedOperatorInTeam` does not exist yet. Write it in the same file, modelled on the team fixtures in the existing team tests (find with `grep -ln 'TeamsOfDirect' kbeads/internal/server/*_test.go`; copy the fixture that creates an `operator_aliases` row and a `type:team` bead with `members`, parameterised by kind, subject, and team bead id).

Run: `cd kbeads && go test ./internal/server -run 'TestTrustedRelayOperator|TestTeamsOf_RelayOnlyFromBridge|TestDecisionAttribution_RelayOnlyFromBridge|TestEscalationApprover_EdgeV1Refused_V2Allowed' -count=1`
Expected: FAIL (`trustedRelayOperator` undefined).

- [ ] **Step 2: Implement the relay trust helpers**

Create `kbeads/internal/server/relay_trust.go`:

```go
package server

import (
	"context"
	"strings"
)

// defaultBridgeSubjects are the fleet subjects whose X-Operated-By relay may
// stand in for a human in an access-control decision (spec D2). Every other
// caller's relay is attribution only: it lands in operated_by, it never
// resolves teams, grants, or a decision's responder. Overridable with
// BEADS_BRIDGE_SUBJECTS.
var defaultBridgeSubjects = []string{"slack-bridge", "gitlab-bridge", "jira-bridge", "github-bridge"}

func bridgeSubjectSet(subjects []string) map[string]bool {
	m := make(map[string]bool, len(subjects))
	for _, s := range subjects {
		if s = strings.TrimSpace(s); s != "" {
			m[s] = true
		}
	}
	return m
}

// SetBridgeSubjects replaces the bridge subject list. nil keeps the default.
func (s *BeadsServer) SetBridgeSubjects(subjects []string) {
	if subjects == nil {
		return
	}
	s.bridgeSubjects = bridgeSubjectSet(subjects)
}

func (s *BeadsServer) isBridgeSubject(subject string) bool {
	set := s.bridgeSubjects
	if set == nil {
		set = bridgeSubjectSet(defaultBridgeSubjects)
	}
	return set[subject]
}

// relayTrusted reports whether the caller's X-Operated-By may be used for an
// access-control decision: only a fleet identity whose subject is a bridge.
func (s *BeadsServer) relayTrusted(ctx context.Context) bool {
	id := AuthIdentityFromContext(ctx)
	return id.Type == "fleet" && s.isBridgeSubject(id.Subject)
}

// trustedRelayOperator returns the relayed human only for a trusted relay.
func (s *BeadsServer) trustedRelayOperator(ctx context.Context) string {
	if !s.relayTrusted(ctx) {
		return ""
	}
	return operatedByHeaderFromContext(ctx)
}
```

Add `bridgeSubjects map[string]bool` to `type BeadsServer struct` (`server.go`) and `bridgeSubjects: bridgeSubjectSet(defaultBridgeSubjects),` to the `NewBeadsServer` literal. In `kbeads/internal/config/config.go` add the field `BridgeSubjects []string // BEADS_BRIDGE_SUBJECTS` next to `ServiceFieldFleets` (`:56`) and `BridgeSubjects: parseCommaSeparated(os.Getenv("BEADS_BRIDGE_SUBJECTS")),` next to `:148`; in `kbeads/cmd/kd/serve.go` after `:301` add `beadsServer.SetBridgeSubjects(cfg.BridgeSubjects)` (an unset env yields nil from `parseCommaSeparated`, which keeps the default; confirm with `grep -n 'func parseCommaSeparated' -A12 kbeads/internal/config/config.go`).

- [ ] **Step 3: Switch the four consumers to the trusted relay**

- `team_membership.go:126-130`: replace `if op := operatedByHeaderFromContext(ctx); op != "" {` with `if op := s.trustedRelayOperator(ctx); op != "" {`.
- `taint_read_gate.go:87`: same replacement.
- `http_taint_grants.go:202`: same replacement.
- `http_decisions.go:337`: replace `if op, _ := s.resolveOperatedByPure(ctx); op != "" && isHumanOperator(op) {` with `if op := s.trustedRelayOperator(ctx); op != "" && isHumanOperator(op) {`.

Update each function's doc comment: the viewer-relay sentence becomes "the relay counts only from a bridge fleet subject (relayTrusted, P7 Task 3); the static token's relay is attribution only."

- [ ] **Step 4: Refuse verified v1 edge in escalationApprover**

In `escalation_approval_authz.go`, in the `case "edge":` arm PR #249 introduced (or, if #249 has not changed this function, in the `id.Type == "edge"` condition), require `authMethodFromContext(ctx) == authMethodOIDC`:

```go
	case "edge":
		// Verified v2 edge envelopes are stamped authMethodOIDC with roles
		// (edge_auth.go); a verified v1 envelope is authMethodEdge, carries
		// no roles, and is signed with a shared HMAC key. Only v2 approves
		// (spec D3, owner question Q4).
		if authMethodFromContext(ctx) == authMethodOIDC {
			return id.Subject, true
		}
```

Run: `cd kbeads && go test ./internal/server -run 'TestTrustedRelayOperator|TestTeamsOf_RelayOnlyFromBridge|TestDecisionAttribution_RelayOnlyFromBridge|TestEscalationApprover' -count=1`
Expected: PASS.

- [ ] **Step 5: Fix the tests that pinned the old relay behaviour**

Run: `cd kbeads && go test ./internal/server -count=1 2>&1 | grep -E '^(--- FAIL|FAIL|ok)'`
Expected: a handful of failures in tests that send `X-Operated-By` with the static token and expect team, grant, or responder resolution (search `grep -ln 'X-Operated-By' kbeads/internal/server/*_test.go`). For each, switch the request to `testBridgeToken` via `newClassedTestServer` when the test's intent is "a bridge relays a human", or flip the assertion to "not resolved" when the intent is "the static token". Do not delete a failing test; each one changes to the new rule with a one-line comment citing P7 Task 3.

- [ ] **Step 6: Chart value and env**

In `gardenio/helm/gardenio/values.yaml` under `beads:` (find `grep -n '^beads:' gardenio/helm/gardenio/values.yaml`), add:

```yaml
  # P7 Task 3 (spec D2): fleet subjects whose X-Operated-By relay may stand in
  # for a human in access-control checks. Everyone else's relay is attribution
  # only. Comma-joined into BEADS_BRIDGE_SUBJECTS.
  bridgeSubjects: ["slack-bridge", "gitlab-bridge", "jira-bridge", "github-bridge"]
```

and in the beads deployment template next to `BEADS_SERVICE_FIELD_FLEETS`:

```yaml
            - name: BEADS_BRIDGE_SUBJECTS
              value: {{ join "," .Values.beads.bridgeSubjects | quote }}
```

Run: `helm lint gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml && helm template t gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml --show-only templates/beads/deployment.yaml | grep -n -A1 BEADS_BRIDGE_SUBJECTS`
Expected: lint clean; `value: "slack-bridge,gitlab-bridge,jira-bridge,github-bridge"`.

- [ ] **Step 7: Format, vet, test, commit**

```bash
cd kbeads && gofumpt -l internal cmd && go vet ./... && staticcheck ./internal/server ./internal/config ./cmd/kd && go test ./internal/server ./internal/config ./cmd/kd -count=1 && cd .. && \
git add kbeads/internal/server kbeads/internal/config/config.go kbeads/cmd/kd/serve.go gardenio/helm/gardenio/values.yaml gardenio/helm/gardenio/templates/beads/deployment.yaml && \
git commit -m "fix(kbeads): X-Operated-By is attribution, not authority, except from bridge subjects; v1 edge no longer approves (closes gd-opMHKHiy9I with P7 Tasks 1-2)"
```

The PR description lists the four consumers changed and states: escalations remain "designed, exercised, enforcement in progress".

### Task 4: Slack bridge web API fails closed without OIDC; web approve and reject removed; document the bridge auth model

**Step bead:** gd-3tOmdG4moG. Spec §0 D4, §1.2, owner question Q7. No blockers.

**Files:**
- Modify: `gardenio/controller/cmd/slack-bridge/main.go:1363-1433` (`func bridgeRBAC`), `gardenio/controller/cmd/slack-bridge/main.go:1135-1138` (config read: add `insecureOpenAPI`), `gardenio/controller/cmd/slack-bridge/helpers.go:309-316` (`logAuthMode`)
- Modify: `gardenio/controller/internal/bridge/api_escalations.go:154-165` (`switch action`)
- Test: `gardenio/controller/cmd/slack-bridge/rbac_test.go` (helper `newTestHandler` at `:16-23` calls `bridgeRBAC`), `gardenio/controller/internal/bridge/api_escalations_test.go`
- Modify: `gardenio/tests/e2e/scripts/test-escalation-approval.sh` (scenarios 4 to 7), `gardenio/tests/e2e/scripts/test-escalation-execution.sh:332-333`, `:546-547`, `:618-619`
- Create: `gardenio/docs/slack-bridge-auth.md`

**Interfaces:**
- Consumes: nothing new.
- Produces: `bridgeRBAC(next, oidcMW, agentToken string, oidcConfigured, insecureOpenAPI bool, logger)` (new parameter); env `SLACK_BRIDGE_INSECURE_OPEN_API` (`"1"` enables the dev-only open mode; anything else is off).

- [ ] **Step 1: Write the failing tests**

Append to `gardenio/controller/cmd/slack-bridge/rbac_test.go`:

```go
// P7 Task 4: with no OIDC and no explicit insecure flag, regular /api/* routes
// fail closed, even with a forged X-Auth-Request-Email. Positive control: the
// agent-facing route with the agent token still works, and the explicit dev
// flag restores the old open behaviour.
func TestBridgeRBAC_NoOIDC_FailsClosed(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	noop := func(h http.Handler) http.Handler { return h }
	h := bridgeRBAC(ok, noop, "agent-secret", false, false, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/api/escalations/gd-x/approve", nil)
	req.Header.Set("X-Auth-Request-Email", "owner@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forged-header approve without OIDC: %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/slack/channels", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("positive control: agent route with agent token: %d, want 200", rec.Code)
	}

	dev := bridgeRBAC(ok, noop, "", false, true, slog.Default())
	req = httptest.NewRequest(http.MethodGet, "/api/escalations", nil)
	rec = httptest.NewRecorder()
	dev.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("positive control: explicit insecure dev mode: %d, want 200", rec.Code)
	}
}
```

Append to `gardenio/controller/internal/bridge/api_escalations_test.go`:

```go
// P7 Task 4: the bridge no longer approves or rejects escalations by PATCHing
// approved_by; Slack buttons and the beads viewer are the approval surfaces.
func TestEscalationAPI_ApproveRejectGone(t *testing.T) {
	a := NewEscalationAPI(nil, nil, slog.Default())
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	for _, action := range []string{"approve", "reject"} {
		req := httptest.NewRequest(http.MethodPost, "/api/escalations/gd-x/"+action, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusGone {
			t.Fatalf("%s: %d, want 410", action, rec.Code)
		}
	}
}
```

Run: `cd gardenio/controller && go test ./cmd/slack-bridge ./internal/bridge -run 'TestBridgeRBAC_NoOIDC_FailsClosed|TestEscalationAPI_ApproveRejectGone' -count=1`
Expected: FAIL (`bridgeRBAC` has five parameters; approve is not 410).

- [ ] **Step 2: Implement fail-closed RBAC**

Change the signature to `func bridgeRBAC(next http.Handler, oidcMW func(http.Handler) http.Handler, agentToken string, oidcConfigured, insecureOpenAPI bool, logger *slog.Logger) http.Handler`. In the agent-facing branch replace the condition `if !oidcConfigured && agentToken == "" {` (`gardenio/controller/cmd/slack-bridge/main.go:1397`) with `if insecureOpenAPI && !oidcConfigured && agentToken == "" {`. In the regular branch replace:

```go
			if len(roles) == 0 {
				if !oidcConfigured {
					// No OIDC at all — insecure dev fallback.
					next.ServeHTTP(w, r)
					return
				}
```

with:

```go
			if len(roles) == 0 {
				if !oidcConfigured && insecureOpenAPI {
					// Explicit dev-only open mode (SLACK_BRIDGE_INSECURE_OPEN_API=1).
					next.ServeHTTP(w, r)
					return
				}
				if !oidcConfigured {
					// P7 Task 4: no OIDC means no identity, so fail closed.
					// X-Auth-Request-Email is a header any pod can send.
					http.Error(w, "bridge web API requires OIDC (set OIDC_ISSUER_URL)", http.StatusForbidden)
					return
				}
```

Add `insecureOpenAPI bool` to the config struct near `agentToken` (`gardenio/controller/cmd/slack-bridge/main.go:830`) read as `insecureOpenAPI: os.Getenv("SLACK_BRIDGE_INSECURE_OPEN_API") == "1",` (near `gardenio/controller/cmd/slack-bridge/main.go:1138`), pass it at `gardenio/controller/cmd/slack-bridge/main.go:301`, and change `logAuthMode` so the no-OIDC message reads `"slack-bridge has no OIDC: /api/* web routes are CLOSED (403); set OIDC_ISSUER_URL, or SLACK_BRIDGE_INSECURE_OPEN_API=1 for local dev only"` and logs at Error level when `insecureOpenAPI` is set. Give `newTestHandler` in `rbac_test.go` (`:16-23`) an `insecureOpenAPI bool` parameter passed through to `bridgeRBAC`; existing callers pass `false`, except tests whose intent is the old open-without-OIDC path, which pass `true`.

- [ ] **Step 3: Remove web approve and reject**

In `api_escalations.go`, replace the two cases at `gardenio/controller/internal/bridge/api_escalations.go:157-160`:

```go
	case "approve":
		a.handleApprove(w, r, id)
	case "reject":
		a.handleReject(w, r, id)
```

with:

```go
	case "approve", "reject":
		// P7 Task 4: the bridge web API used to approve by PATCHing
		// approved_by, which the bridge then executed. Approval happens in
		// Slack (as the mapped person) or in the beads viewer.
		writeJSONError(w, http.StatusGone, "approve and reject moved: use the Slack card or the beads viewer")
```

Delete `handleApprove`, `handleReject`, `approveRequest`, `rejectRequest`, and the now-unused `RejectEscalation` and `UpdateBeadFields` methods from the `EscalationAPIClient` interface if nothing else in the file uses them (`go vet` reports unused imports; `staticcheck` reports unused functions). Delete the tests that exercised the removed handlers (`grep -n 'handleApprove\|handleReject\|/approve\|/reject' gardenio/controller/internal/bridge/api_escalations_test.go`).

Run: `cd gardenio/controller && go test ./cmd/slack-bridge ./internal/bridge -count=1`
Expected: PASS.

- [ ] **Step 4: e2e scenarios become negative controls**

In `gardenio/tests/e2e/scripts/test-escalation-approval.sh`, scenarios 4 to 7 (`POST /api/escalations/{id}/approve|reject`) change their expectation to the HTTP status the bridge now returns: 403 when the e2e bridge has no OIDC, 410 when it does. Rewrite the header comment lines `:22-25` to say so. Keep scenarios 1 to 3 and 8 as they are only when the e2e install sets OIDC or `SLACK_BRIDGE_INSECURE_OPEN_API=1`; otherwise assert 403 for them too. In `test-escalation-execution.sh`, the three `bridge_curl POST "/api/escalations/$id/approve"` calls (`:332`, `:546`, `:618`) assert the forged header is refused (403 or 410), and the approval itself moves to `"$KD_BIN" escalation approve "$id"` the way `test-escalation-crud.sh` Scenario 4 does (`gardenio/tests/e2e/scripts/test-escalation-crud.sh:296-345`). If that approve is refused because the e2e daemon has no OIDC approver (the approve verb has been OIDC/edge-only since gd-ZjiSd9Kzio), mark the execution scenarios SKIP with the reason printed, and say so in the PR; do not re-open the forged path to make them pass.

Run: `shellcheck -x gardenio/tests/e2e/scripts/test-escalation-approval.sh gardenio/tests/e2e/scripts/test-escalation-execution.sh`
Expected: silent.

- [ ] **Step 5: Document the bridge auth model**

Create `gardenio/docs/slack-bridge-auth.md`:

```markdown
# Slack bridge: who can call what

| Surface | Who | How it authenticates |
|---|---|---|
| Slack events, interactions, slash commands | Slack | Socket Mode connection with the bot and app tokens |
| `/api/slack/*`, `/api/permission/*` | agents | shared bearer `SLACK_BRIDGE_AGENT_TOKEN` (every agent pod holds it as `SLACK_BRIDGE_TOKEN`), or an OIDC admin |
| `/api/escalations`, `/api/decisions`, `/api/unreleased` | humans | OIDC bearer verified by the bridge (`OIDC_ISSUER_URL`). **Without OIDC these routes return 403.** `SLACK_BRIDGE_INSECURE_OPEN_API=1` opens them for local development only. |
| `/api/escalations/{id}/approve`, `/reject` | nobody | removed (410). Approve in the Slack card or the beads viewer. |
| Bridge to daemon | the bridge | fleet token `slack-bridge`; `X-Operated-By` relays the Slack user for attribution and, because `slack-bridge` is a bridge subject, for team and decision attribution |

Known residual: any agent can use `/api/slack/*` to post, upload, and delete bot
messages, because the agent token is shared. See SECURITY.md.
```

- [ ] **Step 6: Format, vet, commit**

```bash
cd gardenio/controller && gofumpt -l cmd/slack-bridge internal/bridge && go vet ./cmd/slack-bridge ./internal/bridge && staticcheck ./cmd/slack-bridge ./internal/bridge && go test ./cmd/slack-bridge ./internal/bridge -count=1 && cd ../.. && \
git add gardenio/controller/cmd/slack-bridge gardenio/controller/internal/bridge/api_escalations.go gardenio/controller/internal/bridge/api_escalations_test.go gardenio/tests/e2e/scripts/test-escalation-approval.sh gardenio/tests/e2e/scripts/test-escalation-execution.sh gardenio/docs/slack-bridge-auth.md && \
git commit -m "fix(slack-bridge): web API fails closed without OIDC; remove PATCH-based web approve/reject (gd-3tOmdG4moG)"
```

### Task 5: Strip kubectl last-applied-configuration from every Secret; guard it from coming back; runbooks use create, never apply

**Step bead:** NEW, Phase 1 (minted with this plan; operator add-on 2026-10-11). Spec §0 D14, §1.6. No blockers. The live sweep (Step 7) is an OPERATOR step; the crew never runs kubectl.

Live today (names only, read 2026-10-11): `gardenio/aws-cost-auditor`, `gardenio/gardenio-forwardauth-secrets`, `ziti/router-aws-1-jwt` carry `kubectl.kubernetes.io/last-applied-configuration`, which holds the Secret's full cleartext. fimba-c6 stripped `gardenio-grafana-admin` and `gardenio-owner-initial-password` the same day. 3 of 129 Secrets.

**Files:**
- Create: `scripts/security/check-secret-last-applied.sh`, `scripts/security/check-secret-last-applied_test.sh`
- Create: `scripts/security/lint-kubectl-apply-secret.py`, `scripts/security/lint-kubectl-apply-secret_test.py`
- Modify: `.github/workflows/checks.yml:431` (shellcheck list), `:515` (self-test list), and a new step in `script-selftests` (`:409`)
- Modify: `gardenio/docs/runbooks/websocket-auth-revocation.md:132`
- Create: `gardenio/docs/runbooks/secrets-create-not-apply.md`

**Interfaces:**
- Produces:
  - `scripts/security/check-secret-last-applied.sh [--from-file FILE] [--strip]`: reads `kubectl get secrets -A -o json` (or FILE), prints `namespace/name` for each Secret carrying the annotation and a count line `secrets-with-last-applied: N of M`; exit 0 when N is 0, 1 otherwise, 2 on usage error. `--strip` (OPERATOR only, live cluster only, refused with `--from-file`) runs `kubectl -n NS annotate secret NAME kubectl.kubernetes.io/last-applied-configuration-` for each, then re-counts. Never prints annotation values.
  - `scripts/security/lint-kubectl-apply-secret.py [ROOT]`: scans tracked `*.sh` files and fenced code blocks in tracked `*.md` files (excluding `docs/archive/`, `.worktrees/`) for a `kubectl ... apply` line that mentions `secret` or is followed within 20 lines by `kind: Secret`; prints `path:line: text`; exit 1 on any finding.

- [ ] **Step 1: Write the failing tests (planted positive controls)**

Create `scripts/security/check-secret-last-applied_test.sh`:

```bash
#!/usr/bin/env bash
# Planted positive control for check-secret-last-applied.sh: a fixture with one
# annotated Secret must fail and name it without printing the value; a clean
# fixture must pass.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fail=0

cat >"$tmp/dirty.json" <<'JSON'
{"items":[
 {"metadata":{"namespace":"gardenio","name":"clean-one","annotations":{}}},
 {"metadata":{"namespace":"ziti","name":"planted","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"token\":\"UExBTlRFRC1TRUNSRVQ=\"}}"}}}
]}
JSON
cat >"$tmp/clean.json" <<'JSON'
{"items":[{"metadata":{"namespace":"gardenio","name":"clean-one"}}]}
JSON

set +e
out="$(bash "$here/check-secret-last-applied.sh" --from-file "$tmp/dirty.json")"; rc=$?
set -e
if [ "$rc" -eq 1 ] && [[ "$out" == *"ziti/planted"* ]] && [[ "$out" == *"secrets-with-last-applied: 1 of 2"* ]]; then
  echo "PASS dirty fixture fails and names the Secret"
else
  echo "FAIL dirty fixture: rc=$rc out=$out"; fail=1
fi
if [[ "$out" == *"UExBTlRFRC1TRUNSRVQ"* ]]; then
  echo "FAIL the annotation value was printed"; fail=1
else
  echo "PASS no value printed"
fi

set +e
out="$(bash "$here/check-secret-last-applied.sh" --from-file "$tmp/clean.json")"; rc=$?
set -e
if [ "$rc" -eq 0 ] && [[ "$out" == *"secrets-with-last-applied: 0 of 1"* ]]; then
  echo "PASS clean fixture passes"
else
  echo "FAIL clean fixture: rc=$rc out=$out"; fail=1
fi

set +e
bash "$here/check-secret-last-applied.sh" --from-file "$tmp/clean.json" --strip >/dev/null 2>&1; rc=$?
set -e
[ "$rc" -eq 2 ] && echo "PASS --strip refused with --from-file" || { echo "FAIL --strip with --from-file rc=$rc"; fail=1; }

exit "$fail"
```

Create `scripts/security/lint-kubectl-apply-secret_test.py`:

```python
#!/usr/bin/env python3
"""Planted positive controls for lint-kubectl-apply-secret.py."""
import pathlib
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
LINT = HERE / "lint-kubectl-apply-secret.py"


def run(root):
    p = subprocess.run([sys.executable, "-I", str(LINT), str(root)], capture_output=True, text=True)
    return p.returncode, p.stdout


def make_repo(files):
    d = tempfile.mkdtemp()
    root = pathlib.Path(d)
    subprocess.run(["git", "init", "-q", str(root)], check=True)
    for rel, text in files.items():
        f = root / rel
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_text(text)
    subprocess.run(["git", "-C", str(root), "add", "-A"], check=True)
    return root


failures = 0
cases = {
    "pipe into apply (the classic)": ({"docs/a.md": "```bash\nkubectl create secret generic x --from-literal=a=b --dry-run=client -o yaml | kubectl apply -f -\n```\n"}, 1),
    "apply a secret file": ({"scripts/b.sh": "#!/bin/sh\nkubectl -n gardenio apply -f my-secret.yaml\n"}, 1),
    "heredoc with kind: Secret": ({"docs/c.md": "```\nkubectl apply -f - <<EOF\napiVersion: v1\nkind: Secret\nmetadata:\n  name: x\nEOF\n```\n"}, 1),
    "prose mention is fine": ({"docs/d.md": "Never use `kubectl apply` for a Secret.\n"}, 0),
    "create is fine": ({"docs/e.md": "```\nkubectl create secret generic x --from-literal=a=b\n```\n"}, 0),
    "replace pipe is fine": ({"docs/f.md": "```\nkubectl create secret generic x --from-literal=a=b --dry-run=client -o yaml | kubectl replace -f -\n```\n"}, 0),
    "apply of a non-secret is fine": ({"docs/g.md": "```\nkubectl apply -f deployment.yaml\n```\n"}, 0),
    "archive is exempt": ({"docs/archive/h.md": "```\nkubectl apply -f my-secret.yaml\n```\n"}, 0),
}
for name, (files, want) in cases.items():
    rc, out = run(make_repo(files))
    status = "PASS" if rc == want else "FAIL"
    if status == "FAIL":
        failures += 1
    print(f"{status} {name}: rc={rc} want={want} {out.strip()[:120]}")
sys.exit(1 if failures else 0)
```

Run: `bash scripts/security/check-secret-last-applied_test.sh; python3 scripts/security/lint-kubectl-apply-secret_test.py`
Expected: FAIL (neither script exists).

- [ ] **Step 2: Implement the live check**

Create `scripts/security/check-secret-last-applied.sh`:

```bash
#!/usr/bin/env bash
# check-secret-last-applied.sh: list Secrets whose metadata still carries
# kubectl.kubernetes.io/last-applied-configuration (a full cleartext copy of the
# Secret written by `kubectl apply`). Prints namespace/name only, never values.
#
# Usage: check-secret-last-applied.sh [--from-file FILE] [--strip]
#   --from-file FILE  read `kubectl get secrets -A -o json` output from FILE (tests)
#   --strip           OPERATOR ONLY: remove the annotation from every hit on the
#                     live cluster, then re-count. Refused with --from-file.
# Exit: 0 none, 1 some remain, 2 usage error.
set -euo pipefail

file=""; strip=0
while [ $# -gt 0 ]; do
  case "$1" in
    --from-file) file="${2:-}"; [ -n "$file" ] || { echo "usage: --from-file FILE" >&2; exit 2; }; shift 2;;
    --strip) strip=1; shift;;
    *) echo "unknown argument: $1" >&2; exit 2;;
  esac
done
if [ "$strip" -eq 1 ] && [ -n "$file" ]; then
  echo "--strip acts on the live cluster and cannot be combined with --from-file" >&2; exit 2
fi

ANN='kubectl.kubernetes.io/last-applied-configuration'
json="$(mktemp)"; trap 'rm -f "$json"' EXIT

load() {
  if [ -n "$file" ]; then cat "$file" >"$json"; else kubectl get secrets -A -o json >"$json"; fi
}
hits() {
  jq -r --arg a "$ANN" '.items[] | select(.metadata.annotations[$a] != null) | "\(.metadata.namespace)/\(.metadata.name)"' "$json"
}
count() {
  local n m
  n="$(hits | wc -l | tr -d ' ')"
  m="$(jq '.items | length' "$json")"
  echo "secrets-with-last-applied: $n of $m"
  [ "$n" -eq 0 ]
}

load
hits
if [ "$strip" -eq 1 ]; then
  echo "before: $(count || true)"
  while IFS=/ read -r ns name; do
    [ -n "$ns" ] || continue
    kubectl -n "$ns" annotate secret "$name" "${ANN}-" >/dev/null
    echo "stripped $ns/$name"
  done < <(hits)
  load
  echo "after:"
fi
count
```

Run: `chmod +x scripts/security/check-secret-last-applied.sh && bash scripts/security/check-secret-last-applied_test.sh`
Expected: four `PASS` lines, exit 0.

- [ ] **Step 3: Implement the repo lint**

Create `scripts/security/lint-kubectl-apply-secret.py`:

```python
#!/usr/bin/env python3
"""Fail when a tracked script or a fenced code block in tracked markdown runs
`kubectl apply` on a Secret. `kubectl apply` stores the whole object, data
included, in the kubectl.kubernetes.io/last-applied-configuration annotation.
Use `kubectl create secret ...` for a new Secret and
`kubectl create secret ... --dry-run=client -o yaml | kubectl replace -f -`
to rotate an existing one (gardenio/docs/runbooks/secrets-create-not-apply.md).

Usage: lint-kubectl-apply-secret.py [ROOT]   exit 0 clean, 1 findings.
"""
import pathlib
import re
import subprocess
import sys

APPLY = re.compile(r"\bkubectl\b[^#\n]*\bapply\b")
SECRET_WORD = re.compile(r"(?i)\bsecrets?\b")
KIND_SECRET = re.compile(r"^\s*kind:\s*Secret\s*$", re.M)
EXEMPT = ("docs/archive/", ".worktrees/")


def tracked(root):
    out = subprocess.run(["git", "-C", str(root), "ls-files", "*.md", "*.sh"],
                         capture_output=True, text=True, check=True).stdout
    return [p for p in out.splitlines() if p and not p.startswith(EXEMPT)]


def scan(root, rel):
    lines = (root / rel).read_text(encoding="utf-8", errors="replace").split("\n")
    md = rel.endswith(".md")
    in_code = not md
    found = []
    for i, line in enumerate(lines):
        if md and line.strip().startswith("```"):
            in_code = not in_code
            continue
        if not in_code or not APPLY.search(line):
            continue
        window = "\n".join(lines[i:i + 20])
        if SECRET_WORD.search(line) or KIND_SECRET.search(window):
            found.append(f"{rel}:{i + 1}: {line.strip()[:160]}")
    return found


def main():
    root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
    findings = [f for rel in tracked(root) for f in scan(root, rel)]
    for f in findings:
        print(f)
    if findings:
        print(f"lint-kubectl-apply-secret: {len(findings)} finding(s); use kubectl create / replace for Secrets", file=sys.stderr)
        return 1
    print("lint-kubectl-apply-secret: clean")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

Run: `python3 scripts/security/lint-kubectl-apply-secret_test.py && python3 scripts/security/lint-kubectl-apply-secret.py .`
Expected: eight `PASS` lines; then exactly one finding on the real tree, `gardenio/docs/runbooks/websocket-auth-revocation.md:132`, exit 1 (fixed in Step 4).

- [ ] **Step 4: Runbooks say create or replace**

Create `gardenio/docs/runbooks/secrets-create-not-apply.md`:

```markdown
# Creating and rotating Secrets: create or replace, never apply

`kubectl apply` writes the whole object, Secret data included, into the
`kubectl.kubernetes.io/last-applied-configuration` annotation. That copy then
shows up in `kubectl get -o yaml`, backups, and support bundles, and a later
`apply` can quietly restore an old value from it.

| Task | Command |
|---|---|
| New Secret | `kubectl -n NS create secret generic NAME --from-literal=key=VALUE` (or `--from-file=key=PATH`) |
| New Secret from a manifest you generated | `kubectl create secret generic NAME ... --dry-run=client -o yaml \| kubectl -n NS create -f -` |
| Rotate an existing Secret | `kubectl create secret generic NAME ... --dry-run=client -o yaml \| kubectl -n NS replace -f -` |
| Check for leftovers | `scripts/security/check-secret-last-applied.sh` (prints names only) |
| Remove leftovers (operator) | `scripts/security/check-secret-last-applied.sh --strip` |

Helm-managed Secrets are unaffected: Helm does not write this annotation.
CI fails a script or runbook code block that pipes or points `kubectl apply`
at a Secret (`scripts/security/lint-kubectl-apply-secret.py`).
```

In `gardenio/docs/runbooks/websocket-auth-revocation.md`, replace line `:132`:

```
   kubectl -n "$NS" edit secret <secret-name>   # or: kubectl create secret ... --dry-run=client -o yaml | kubectl apply -f -
```

with:

```
   kubectl -n "$NS" edit secret <secret-name>   # or: kubectl create secret ... --dry-run=client -o yaml | kubectl replace -f -  (never apply: see secrets-create-not-apply.md)
```

Run: `python3 scripts/security/lint-kubectl-apply-secret.py .`
Expected: `lint-kubectl-apply-secret: clean`, exit 0.

- [ ] **Step 5: Wire into CI**

In `.github/workflows/checks.yml`: append `scripts/security/check-secret-last-applied.sh scripts/security/check-secret-last-applied_test.sh` to the end of the `shellcheck -x scripts/check-large-tracked-files.sh ...` line (`:431`); append `scripts/security/check-secret-last-applied_test.sh scripts/security/lint-kubectl-apply-secret_test.py` to the `for t in ...; do` list (`:515`, before `; do`); and add a step to `script-selftests` after the step that contains `:515`:

```yaml
      - name: no kubectl apply of Secrets in scripts or runbooks
        run: python3 scripts/security/lint-kubectl-apply-secret.py .
```

P1 Task 7 edits the same two lines; whichever lands second rebases and keeps both sets of additions.

Run: `shellcheck -x scripts/security/check-secret-last-applied.sh scripts/security/check-secret-last-applied_test.sh && python3 scripts/check-sigpipe-producers.py && python3 scripts/workflow-changed-jobs.py --help >/dev/null`
Expected: silent; exit 0.

- [ ] **Step 6: Commit**

```bash
bash scripts/security/check-secret-last-applied_test.sh && python3 scripts/security/lint-kubectl-apply-secret_test.py && python3 scripts/security/lint-kubectl-apply-secret.py . && \
git add scripts/security/check-secret-last-applied.sh scripts/security/check-secret-last-applied_test.sh scripts/security/lint-kubectl-apply-secret.py scripts/security/lint-kubectl-apply-secret_test.py .github/workflows/checks.yml gardenio/docs/runbooks/secrets-create-not-apply.md gardenio/docs/runbooks/websocket-auth-revocation.md && \
git commit -m "security: guard and runbook against kubectl apply of Secrets; names-only last-applied check with planted controls (P7 Task 5)"
```

- [ ] **Step 7: OPERATOR: sweep the live cluster**

Not for crews. On the laptop with the cluster kubeconfig:

```bash
scripts/security/check-secret-last-applied.sh            # expect 3 names, "secrets-with-last-applied: 3 of N", exit 1
scripts/security/check-secret-last-applied.sh --strip    # prints before/after counts and stripped names only
scripts/security/check-secret-last-applied.sh            # expect "secrets-with-last-applied: 0 of N", exit 0
```

Then confirm each stripped Secret's consumers still work (the values are unchanged; only the annotation went away). Comment the before and after counts on this step bead. Never paste `-o yaml` of a Secret anywhere.

---

## Phase 2: IMDS

### Task 6: Deny pod access to IMDS with one allow-all-but-IMDS NetworkPolicy per namespace

**Step bead:** gd-fetk49kFlz. Spec §0 D5, §1.4, §4. Re-homes the IMDS half of gd-9ip3Fsen5c and the ecr-token-refresh label work in gd-fTCYDqMYeH. No blockers.

**Files:**
- Create: `gardenio/helm/gardenio/templates/networkpolicies/imds-deny.yaml`
- Create: `gardenio/helm/gardenio/tests/imds_deny_test.go`
- Modify: `gardenio/helm/gardenio/values.yaml:15535` (`networkPolicies:`): add a sibling top-level `imdsDeny:` block directly above it
- Modify: `deploy-values/gardenio-k3s.yaml:1145` (the overlay's `networkPolicies:` block; add `imdsDeny:` directly above it)
- Modify: `gardenio/helm/gardenio/templates/postgres/s3-backup-cronjob.yaml:39-43` (pod labels)
- Modify: `gardenio/helm/gardenio-ziti/ecr-token-refresh.yaml:151-157` (`jobTemplate:`) (add pod metadata and label)
- Modify: `gardenio/helm/beads/dolt-backup-cronjob.yaml:23-26` (pod labels)
- Modify: `gardenio/helm/ziti/controller.yaml:168-171` (pod labels)

**Interfaces:**
- Produces: values `imdsDeny.enabled` (bool, default false; true in the k3s overlay), `imdsDeny.namespaces` (list), `imdsDeny.exemptLabelNamespaces` (list); pod label `gardenio.io/imds: allow` (honoured only in an exempt-label namespace, never on an agent pod); policies named `imds-deny` and `imds-deny-agents` in each listed namespace.

- [ ] **Step 1: Write the failing chart test**

Create `gardenio/helm/gardenio/tests/imds_deny_test.go`:

```go
package tests

import (
	"strings"
	"testing"
)

const imdsTpl = "templates/networkpolicies/imds-deny.yaml"

func imdsDocs(t *testing.T, setFlags ...string) []string {
	t.Helper()
	out := helmTemplate(t, imdsTpl, setFlags...)
	var docs []string
	for doc := range strings.SplitSeq(out, "\n---\n") {
		if strings.Contains(doc, "kind: NetworkPolicy") {
			docs = append(docs, doc)
		}
	}
	return docs
}

func docFor(docs []string, ns, name string) string {
	for _, d := range docs {
		if strings.Contains(d, "namespace: "+ns+"\n") && strings.Contains(d, "name: "+name+"\n") {
			return d
		}
	}
	return ""
}

// Off by default: nothing renders. Renders the whole chart, because
// `helm template --show-only` errors on a template that renders nothing.
func TestIMDSDeny_OffByDefault(t *testing.T) {
	if out := helmTemplate(t, ""); strings.Contains(out, "gardenio.io/policy: imds-deny") {
		t.Fatal("imdsDeny is off by default, but an imds-deny policy rendered")
	}
}

// Every listed namespace gets a policy that allows all egress except the IMDS
// addresses, so turning it on cannot break anything but IMDS.
func TestIMDSDeny_AllowsEverythingButIMDS(t *testing.T) {
	docs := imdsDocs(t, "imdsDeny.enabled=true",
		"imdsDeny.namespaces={gardenio,arc-runners,gc-dogfood}",
		"imdsDeny.exemptLabelNamespaces={gardenio}")
	for _, ns := range []string{"gardenio", "arc-runners", "gc-dogfood"} {
		d := docFor(docs, ns, "imds-deny")
		if d == "" {
			t.Fatalf("no imds-deny policy in %s", ns)
		}
		for _, want := range []string{"- Egress", "cidr: 0.0.0.0/0", "- 169.254.169.254/32", "cidr: ::/0", "- fd00:ec2::254/128", "namespaceSelector: {}"} {
			if !strings.Contains(d, want) {
				t.Errorf("%s imds-deny missing %q:\n%s", ns, want, d)
			}
		}
		if strings.Contains(d, "- Ingress") {
			t.Errorf("%s imds-deny must not restrict ingress", ns)
		}
	}
}

// The exemption label is honoured only in an exempt-label namespace, and the
// agents policy keeps agent pods denied there even if they carry the label.
// gc-dogfood (where k8s-live can create pods) never honours it.
func TestIMDSDeny_ExemptionScope(t *testing.T) {
	docs := imdsDocs(t, "imdsDeny.enabled=true",
		"imdsDeny.namespaces={gardenio,gc-dogfood}",
		"imdsDeny.exemptLabelNamespaces={gardenio}")
	g := docFor(docs, "gardenio", "imds-deny")
	if !strings.Contains(g, "key: gardenio.io/imds") || !strings.Contains(g, "operator: NotIn") {
		t.Fatalf("gardenio imds-deny must exempt gardenio.io/imds=allow:\n%s", g)
	}
	a := docFor(docs, "gardenio", "imds-deny-agents")
	if !strings.Contains(a, "key: gardenio.io/agent") || !strings.Contains(a, "operator: Exists") {
		t.Fatalf("gardenio must have imds-deny-agents selecting gardenio.io/agent:\n%s", a)
	}
	d := docFor(docs, "gc-dogfood", "imds-deny")
	if strings.Contains(d, "gardenio.io/imds") || !strings.Contains(d, "podSelector: {}") {
		t.Fatalf("gc-dogfood must select every pod and ignore the exemption label:\n%s", d)
	}
	if docFor(docs, "gc-dogfood", "imds-deny-agents") != "" {
		t.Fatal("gc-dogfood needs no agents policy: its imds-deny already selects every pod")
	}
}

// The live overlay turns it on for the namespaces in spec §4 and never lists
// kube-system (cluster-autoscaler and the cloud controller need IMDS).
func TestIMDSDeny_OverlayNamespaces(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t, imdsTpl, []string{"../../../deploy-values/gardenio-k3s.yaml"})
	for _, ns := range []string{"gardenio", "arc-runners", "arc-systems", "beads", "gc-dogfood", "operators", "default", "headlamp", "public-edge", "cert-manager", "ziti"} {
		if !strings.Contains(out, "namespace: "+ns+"\n") {
			t.Errorf("overlay must deny IMDS in %s", ns)
		}
	}
	if strings.Contains(out, "namespace: kube-system\n") {
		t.Fatal("kube-system must not get an IMDS deny")
	}
}
```

Run: `cd gardenio/helm/gardenio/tests && go test -run TestIMDSDeny -count=1`
Expected: FAIL (`helm template --show-only` errors because the template does not exist; `TestIMDSDeny_OffByDefault` passes, which is correct).

- [ ] **Step 2: Add the values**

In `gardenio/helm/gardenio/values.yaml`, directly above `networkPolicies:` (`:15535`), add:

```yaml
# P7 Task 6 (spec D5): deny pod-network access to the EC2 instance metadata
# service (IMDS) without touching any other egress. Independent of
# networkPolicies.enabled. Every pod in a listed namespace may reach anything
# except 169.254.169.254 and fd00:ec2::254. Pods labelled gardenio.io/imds:
# allow are exempt ONLY in exemptLabelNamespaces, and agent pods
# (gardenio.io/agent) are never exempt. kube-system is deliberately absent
# (cluster-autoscaler, cloud controller). hostNetwork pods are not affected by
# NetworkPolicy at all.
imdsDeny:
  enabled: false
  namespaces: []
  exemptLabelNamespaces: []
```

In `deploy-values/gardenio-k3s.yaml`, directly above the overlay's `networkPolicies:` (`:1145`), add:

```yaml
# P7 Task 6: IMDS deny. Label the four legitimate IMDS users first
# (ecr-token-refresh, postgres-s3-backup, dolt-backup, ziti-controller) and
# confirm the label on a live pod before this ships (operator step).
imdsDeny:
  enabled: true
  namespaces: [gardenio, arc-runners, arc-systems, beads, gc-dogfood, operators, default, headlamp, public-edge, cert-manager, ziti]
  exemptLabelNamespaces: [gardenio, beads, ziti]
```

- [ ] **Step 3: Write the template**

Create `gardenio/helm/gardenio/templates/networkpolicies/imds-deny.yaml`:

```yaml
{{- /* P7 Task 6 (spec D5, §4). One allow-all-but-IMDS egress policy per listed
       namespace. Policies are additive: a pod selected by any policy here may
       reach everything except the IMDS addresses, because none of them allows
       those addresses. An agent pod is selected by imds-deny-agents even when
       it carries the exemption label. */}}
{{- if .Values.imdsDeny.enabled }}
{{- $exempt := .Values.imdsDeny.exemptLabelNamespaces | default list }}
{{- range $ns := .Values.imdsDeny.namespaces }}
{{- $honour := has $ns $exempt }}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: imds-deny
  namespace: {{ $ns }}
  labels:
    app.kubernetes.io/part-of: gardenio
    gardenio.io/policy: imds-deny
spec:
  {{- if $honour }}
  podSelector:
    matchExpressions:
      - key: gardenio.io/imds
        operator: NotIn
        values: ["allow"]
  {{- else }}
  podSelector: {}
  {{- end }}
  policyTypes:
    - Egress
  egress:
    - to:
        - namespaceSelector: {}
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
              - 169.254.169.254/32
        - ipBlock:
            cidr: ::/0
            except:
              - fd00:ec2::254/128
{{- if $honour }}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: imds-deny-agents
  namespace: {{ $ns }}
  labels:
    app.kubernetes.io/part-of: gardenio
    gardenio.io/policy: imds-deny
spec:
  podSelector:
    matchExpressions:
      - key: gardenio.io/agent
        operator: Exists
  policyTypes:
    - Egress
  egress:
    - to:
        - namespaceSelector: {}
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
              - 169.254.169.254/32
        - ipBlock:
            cidr: ::/0
            except:
              - fd00:ec2::254/128
{{- end }}
{{- end }}
{{- end }}
```

Run: `cd gardenio/helm/gardenio/tests && go test -run TestIMDSDeny -count=1`
Expected: PASS.

- [ ] **Step 4: Label the four legitimate IMDS users**

`gardenio/helm/gardenio/templates/postgres/s3-backup-cronjob.yaml`, replace lines `:40-43`:

```yaml
        metadata:
          labels:
            {{- include "gardenio.postgres.selectorLabels" . | nindent 12 }}
            app.kubernetes.io/component: postgres-s3-backup
```

with:

```yaml
        metadata:
          labels:
            {{- include "gardenio.postgres.selectorLabels" . | nindent 12 }}
            app.kubernetes.io/component: postgres-s3-backup
            # P7 Task 6: uploads to S3 with the node role over IMDS.
            gardenio.io/imds: allow
```

`gardenio/helm/gardenio-ziti/ecr-token-refresh.yaml`, replace lines `:155-157`:

```yaml
      template:
        spec:
          serviceAccountName: ecr-token-refresh
```

with:

```yaml
      template:
        metadata:
          labels:
            app: ecr-token-refresh
            # P7 Task 6: mints the ECR pull token with the node role over IMDS.
            gardenio.io/imds: allow
        spec:
          serviceAccountName: ecr-token-refresh
```

`gardenio/helm/beads/dolt-backup-cronjob.yaml`, replace lines `:24-26`:

```yaml
        metadata:
          labels:
            app: dolt-backup
```

with:

```yaml
        metadata:
          labels:
            app: dolt-backup
            # P7 Task 6: uploads to S3 with the node role over IMDS.
            gardenio.io/imds: allow
```

`gardenio/helm/ziti/controller.yaml`, replace lines `:169-171`:

```yaml
    metadata:
      labels:
        app: ziti-controller
```

with:

```yaml
    metadata:
      labels:
        app: ziti-controller
        # P7 Task 6: the s3-backup sidecar uses the node role over IMDS.
        gardenio.io/imds: allow
```

Run: `helm lint gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml && helm template t gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml --show-only templates/postgres/s3-backup-cronjob.yaml | grep -c 'gardenio.io/imds: allow'`
Expected: lint `0 chart(s) failed`; count `1`.

- [ ] **Step 5: Commit**

```bash
cd gardenio/helm/gardenio/tests && go test -run 'TestIMDSDeny' -count=1 && go test ./... -count=1 && cd - && \
git add gardenio/helm/gardenio/templates/networkpolicies/imds-deny.yaml gardenio/helm/gardenio/tests/imds_deny_test.go gardenio/helm/gardenio/values.yaml deploy-values/gardenio-k3s.yaml gardenio/helm/gardenio/templates/postgres/s3-backup-cronjob.yaml gardenio/helm/gardenio-ziti/ecr-token-refresh.yaml gardenio/helm/beads/dolt-backup-cronjob.yaml gardenio/helm/ziti/controller.yaml && \
git commit -m "feat(chart): deny pod access to IMDS per namespace; label the four legitimate IMDS users (gd-fetk49kFlz)"
```

The PR description says: merge-desk PR; the raw manifests (`gardenio-ziti/`, `beads/`, `ziti/`) are applied by the operator, not by the release.

- [ ] **Step 6: OPERATOR: labels first, then the policy**

1. Apply the three raw manifests with their usual commands (each file's header says how; none of them is a Secret). Wait for the next run of each CronJob, or trigger one with `kubectl -n <ns> create job --from=cronjob/<name> <name>-imds-check`, and confirm the pod has the label: `kubectl -n <ns> get pod -l app=<app> -o jsonpath='{.items[*].metadata.labels.gardenio\.io/imds}'` prints `allow`. For ziti-controller the Deployment rollout restarts the controller; do it in a quiet window (memory `ziti-fabric`).
2. Release the chart (policy goes live).
3. Run the Task 7 probe from a crew pod and confirm the next ecr-token-refresh, postgres-s3-backup, and dolt-backup runs succeed.
4. Rollback: `imdsDeny.enabled: false` and release.
5. Comment results on this bead. Task 7's crew waits for this comment.

### Task 7: Prove IMDS is unreachable from a CI runner pod on every PR, with a positive control

**Step bead:** gd-2yCgKBKaOs. Spec §0 D6. Blocked by Task 6. The crew starts only after Task 6's bead carries the operator's "policy live" comment; until then the CI step would fail every PR.

**Files:**
- Create: `scripts/security/probe-imds-unreachable.sh`, `scripts/security/probe-imds-unreachable_test.sh`
- Modify: `.github/workflows/checks.yml:431` (shellcheck list), `:515` (self-test list), new step in `script-selftests` (`:409`)

**Interfaces:**
- Produces: `scripts/security/probe-imds-unreachable.sh` (no args; env `IMDS_PROBE_CONTROL_URL`, default `https://api.github.com/zen`). Exit 0 when the IMDS token PUT fails AND the control URL answers 200; exit 1 when IMDS answers; exit 2 when the control fails (no network, so the probe proves nothing).

- [ ] **Step 1: Write the failing test (fake curl, planted positive control)**

Create `scripts/security/probe-imds-unreachable_test.sh`:

```bash
#!/usr/bin/env bash
# Drives probe-imds-unreachable.sh with a fake curl on PATH so all three
# outcomes are exercised without touching the network.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fail=0

make_curl() { # $1 = imds behaviour (open|blocked), $2 = control code
  cat >"$tmp/curl" <<EOF
#!/usr/bin/env bash
for a in "\$@"; do
  case "\$a" in
    *169.254.169.254*) if [ "$1" = open ]; then printf 'AQAEAFAKE-TOKEN'; exit 0; else exit 28; fi;;
    *fd00:ec2::254*) exit 7;;
    http*) printf '%s' "$2"; exit 0;;
  esac
done
exit 2
EOF
  chmod +x "$tmp/curl"
}

run() { set +e; PATH="$tmp:$PATH" bash "$here/probe-imds-unreachable.sh" >/dev/null 2>&1; echo $?; set -e; }

make_curl blocked 200; rc=$(run)
[ "$rc" = 0 ] && echo "PASS blocked IMDS + live control passes" || { echo "FAIL blocked: rc=$rc"; fail=1; }
make_curl open 200; rc=$(run)
[ "$rc" = 1 ] && echo "PASS planted reachable IMDS fails" || { echo "FAIL open: rc=$rc"; fail=1; }
make_curl blocked 000; rc=$(run)
[ "$rc" = 2 ] && echo "PASS dead network fails the control" || { echo "FAIL dead network: rc=$rc"; fail=1; }
exit "$fail"
```

Run: `bash scripts/security/probe-imds-unreachable_test.sh`
Expected: FAIL (script missing).

- [ ] **Step 2: Implement the probe**

Create `scripts/security/probe-imds-unreachable.sh`:

```bash
#!/usr/bin/env bash
# probe-imds-unreachable.sh: prove this pod cannot reach the EC2 instance
# metadata service (P7 Task 7, spec D6). The positive control proves the pod
# has working egress, so "IMDS unreachable" is not just "no network".
# Exit: 0 IMDS blocked + control OK; 1 IMDS reachable; 2 control failed.
set -euo pipefail
control="${IMDS_PROBE_CONTROL_URL:-https://api.github.com/zen}"

code="$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$control" 2>/dev/null || true)"
if [ "$code" != "200" ]; then
  echo "control: $control answered '$code', want 200; the probe proves nothing without egress" >&2
  exit 2
fi
echo "control: $control answered 200"

if curl -sS -m 3 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' \
     -o /dev/null http://169.254.169.254/latest/api/token 2>/dev/null; then
  echo "IMDS (169.254.169.254) answered the token request: pod network can reach the node role" >&2
  exit 1
fi
if curl -sS -m 3 -g -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' \
     -o /dev/null 'http://[fd00:ec2::254]/latest/api/token' 2>/dev/null; then
  echo "IMDS (fd00:ec2::254) answered the token request" >&2
  exit 1
fi
echo "IMDS unreachable from this pod (IPv4 and IPv6)"
```

Run: `chmod +x scripts/security/probe-imds-unreachable.sh && bash scripts/security/probe-imds-unreachable_test.sh && shellcheck -x scripts/security/probe-imds-unreachable.sh scripts/security/probe-imds-unreachable_test.sh`
Expected: three `PASS` lines; shellcheck silent.

- [ ] **Step 3: Wire into CI**

In `.github/workflows/checks.yml`: append the two script paths to the shellcheck line (`:431`); append `scripts/security/probe-imds-unreachable_test.sh` to the self-test `for t in` list (`:515`); and add a step to `script-selftests` (`runs-on: gardenio-k3s-light`, a pod in `arc-runners` on the cluster's AWS nodes):

```yaml
      - name: IMDS is unreachable from a runner pod (P7 Task 7)
        run: bash scripts/security/probe-imds-unreachable.sh
```

Task 5 edits the same two lines; whichever lands second rebases and keeps both sets of additions.

- [ ] **Step 4: Commit**

```bash
bash scripts/security/probe-imds-unreachable_test.sh && \
git add scripts/security/probe-imds-unreachable.sh scripts/security/probe-imds-unreachable_test.sh .github/workflows/checks.yml && \
git commit -m "ci: prove IMDS is unreachable from a runner pod, with an egress positive control (gd-2yCgKBKaOs)"
```

The PR's own `script-selftests` run is the first live proof; link that job in the PR description.

- [ ] **Step 5: OPERATOR: probe from a crew pod**

From the laptop: `kubectl -n gardenio exec <any crew pod> -- bash -c "$(cat scripts/security/probe-imds-unreachable.sh)"`. Expected: exit 0 and "IMDS unreachable". Comment the output on this bead.

---

## Phase 3: SECURITY.md

### Task 8: SECURITY.md: threat model, what is closed, what stays open, how to report

**Step bead:** gd-0aRM4lYkfY. Spec §0 D13, §6, owner question Q9. Blocked by every other P7 task (it describes their merged state). Gates P1's publish step.

**Files:**
- Create: `SECURITY.md`
- Create: `scripts/security/check-security-md.py`, `scripts/security/check-security-md_test.py`
- Modify: `.github/workflows/checks.yml:515` (self-test list) and one step in `script-selftests`

**Interfaces:**
- Produces: `SECURITY.md` at the repo root; `scripts/security/check-security-md.py [PATH]` exits 1 if the file says escalations are "enforced" without the house qualifier, lacks any required heading, or has a `PR #?` placeholder.

- [ ] **Step 1: Write the failing wording test**

Create `scripts/security/check-security-md_test.py`:

```python
#!/usr/bin/env python3
"""Planted controls for check-security-md.py."""
import pathlib
import subprocess
import sys
import tempfile

LINT = pathlib.Path(__file__).resolve().parent / "check-security-md.py"
GOOD = """# Security
## Reporting a vulnerability
## Threat model
## What is closed
## What stays open
Escalations: designed, exercised, enforcement in progress.
"""
cases = {
    "good file": (GOOD, 0),
    "says enforced": (GOOD + "\nEscalations are enforced.\n", 1),
    "missing heading": (GOOD.replace("## What stays open\n", ""), 1),
    "placeholder PR": (GOOD + "\n| x | PR #? |\n", 1),
}
fails = 0
for name, (text, want) in cases.items():
    with tempfile.NamedTemporaryFile("w", suffix=".md", delete=False) as f:
        f.write(text)
    rc = subprocess.run([sys.executable, "-I", str(LINT), f.name], capture_output=True).returncode
    ok = rc == want
    fails += not ok
    print(("PASS" if ok else "FAIL"), name, rc, want)
sys.exit(1 if fails else 0)
```

Run: `python3 scripts/security/check-security-md_test.py`
Expected: FAIL (lint missing).

- [ ] **Step 2: Implement the wording check**

Create `scripts/security/check-security-md.py`:

```python
#!/usr/bin/env python3
"""SECURITY.md guard (P7 Task 8). Fails when the file calls escalations
enforced, drops a required section, or still has a PR placeholder.
Usage: check-security-md.py [PATH]   default SECURITY.md"""
import pathlib
import re
import sys

REQUIRED = ["## Reporting a vulnerability", "## Threat model", "## What is closed", "## What stays open"]
QUALIFIER = "designed, exercised, enforcement in progress"


def main():
    path = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "SECURITY.md")
    text = path.read_text(encoding="utf-8")
    errors = [f"missing section: {h}" for h in REQUIRED if h not in text]
    if QUALIFIER not in text:
        errors.append(f"escalations must be described as '{QUALIFIER}'")
    for i, line in enumerate(text.splitlines(), 1):
        if re.search(r"(?i)\benforced\b", line) and QUALIFIER not in line and "not enforced" not in line.lower():
            errors.append(f"{path}:{i}: says 'enforced': {line.strip()[:120]}")
        if "PR #?" in line:
            errors.append(f"{path}:{i}: PR placeholder left in")
    for e in errors:
        print(e)
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
```

Run: `python3 scripts/security/check-security-md_test.py`
Expected: four `PASS` lines.

- [ ] **Step 3: Write SECURITY.md**

Create `SECURITY.md` with exactly this content, then replace each `PR #?` with the PR that merged that task (`GH_CONFIG_DIR=~/.config/gh-ja gh pr list -R mattbakerconsulting/gardenio --state merged --search <step-bead-id> --json number,title`). Leave a row's PR cell as `not merged` only if that task is genuinely unmerged, and then move the row to "What stays open".

```markdown
# Security

gardenio runs autonomous coding agents on a Kubernetes cluster and gives
humans a work tracker (beads), a Slack bridge, and an approval path for the
privileged things agents may not do themselves. This file says what we defend
against, what is closed and how we know, and what is still open.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting on this repository
("Security" tab, "Report a vulnerability"). Please do not open a public issue.
We aim to acknowledge within three working days.

## Threat model

| Actor | Assumed able to | Must not be able to |
|---|---|---|
| An agent (including a prompt-injected one) | read and write beads in its own project, run code in its own pod, call the Slack bridge's agent endpoints | approve, reject, or resolve its own escalation; mint credentials; act as a human; read the node's cloud role |
| Another pod in the cluster | open TCP connections to cluster services | use the Slack bridge's web API, reach the EC2 metadata service |
| A human with a Slack account in the workspace | press buttons on cards addressed to them | approve escalations (approval happens in the beads viewer) |
| The repository reader | read all code, history, and docs | find credentials, internal hostnames that grant access, or Secret values |

Out of scope: a compromised cluster administrator, a compromised AWS account,
and the shared Slack agent token (see "What stays open").

## What is closed

| Area | Change | Test that proves it | PR |
|---|---|---|---|
| Agent credentials | Agents hold a per-agent beads token named after the agent; the primary token left agent pods; a provisioning failure refuses the spawn | `TestAgentToken_CannotMintBridgeToken`, `TestProvisionAgentToken_FailureRefusesSpawn`, e2e `per-agent-token-identity` | PR #? |
| Coopmux token | Agents' coopmux token is separate from the beads token | `TestCoopmux_DaemonBearerStaysBeadsToken` | PR #? |
| Relay header | `X-Operated-By` is attribution only, except from a bridge identity | `TestTrustedRelayOperator`, `TestTeamsOf_RelayOnlyFromBridge` | PR #? |
| Edge envelopes | Only verified v2 envelopes (with roles) count as a human approver | `TestEscalationApprover_EdgeV1Refused_V2Allowed` | PR #? |
| Slack bridge web API | Fails closed without OIDC; web approve/reject removed | `TestBridgeRBAC_NoOIDC_FailsClosed` | PR #? |
| Secret annotations | No Secret carries `last-applied-configuration`; CI rejects `kubectl apply` of a Secret in scripts and runbooks | `check-secret-last-applied_test.sh`, `lint-kubectl-apply-secret_test.py` | PR #? |
| EC2 metadata (IMDS) | NetworkPolicy denies pod access; checked from a CI runner pod on every PR | `probe-imds-unreachable.sh` in `script-selftests` | PR #? |
| Viewer admin mode | The viewer fails closed for roleless sessions | (identity lane) | PR #251 |
| Alias self-link | Linking an operator alias is admin-only | (identity lane) | PR #246 |

**Escalations: designed, exercised, enforcement in progress.** The approve,
reject, and result verbs check who is calling (operator, bridge, the dispatched
agent); the daemon is the only writer of approval and result fields; each
approved escalation has one executor chosen and claimed by the daemon;
four-eyes applies to every verb when policy asks for it; approvals and results
write mutation history. The tests are `TestRejectResult_CallerClasses`,
`TestResult_ExecutorIsServerStamped`, `TestEscalationFields_ServerOnlyRefused`,
`TestApprove_ClaimsOneDispatch`, `TestFourEyes_RejectAndResult`, and
`TestApprove_RecordsMutationHistory` (PR #?). What is not yet in place is listed
below.

## What stays open

- **Escalation enforcement for humans** depends on the daemon verifying OIDC and
  on `kd login`; until both are live in a deployment, humans approve only
  through the beads viewer.
- **Local execution** of an escalation names a local agent bead today; runner
  identities replace that later.
- **The shared Slack agent token**: every agent can post, upload, and delete bot
  messages through the bridge's agent endpoints.
- **The primary beads token** is still held by the controller, the beads viewer,
  coopmux, the release deployer, backup jobs, and operator laptops. Whoever
  holds it can mint bridge tokens.
- **Per-agent tokens are project-scoped**; an agent that must read another
  project needs a different credential (not built).
- **The node's cloud role is broad.** Pods can no longer reach it; anything on
  the host network still can.
- **Field-edit endpoints** (`/v1/beads/{id}/fields/...`) apply the escalation
  and user-identity guards but not the scrub-request guard.
- **Audit attribution from the primary token** is the caller's claim, not proof.
```

Run: `python3 scripts/security/check-security-md.py SECURITY.md`
Expected: exit 0 after every `PR #?` is replaced.

- [ ] **Step 4: Wire the check into CI**

Append `scripts/security/check-security-md_test.py` to the `for t in` list (`.github/workflows/checks.yml:515`) and add a `script-selftests` step:

```yaml
      - name: SECURITY.md wording (P7 Task 8)
        run: python3 scripts/security/check-security-md.py SECURITY.md
```

- [ ] **Step 5: Commit**

```bash
python3 scripts/security/check-security-md_test.py && python3 scripts/security/check-security-md.py SECURITY.md && \
git add SECURITY.md scripts/security/check-security-md.py scripts/security/check-security-md_test.py .github/workflows/checks.yml && \
git commit -m "docs: SECURITY.md with threat model, closed and open items, private reporting (gd-0aRM4lYkfY)"
```

---

## Phase 4: escalation authorization

Release rule for Phase 4 (OPERATOR): Tasks 9 to 15 land on main in dependency order, and the operator does not deploy a release that contains Task 9 until Task 13 has merged. In between, local and service_account execution have no executor (Task 12 deletes the bridge's, Task 13 adds the daemon's). Every Phase 4 PR description repeats this line.

### Task 9: Caller classes on the escalation verbs: reject is operator-only, result is operator, bridge, or agent

**Step bead:** gd-nPfSNHTkQQ. Spec §0 D7, §5.1, doc 11 defect 1. Blocked by Tasks 1 and 2 (the agent class needs per-agent tokens), and cross-lane by gd-l1Jh9aFEXY (daemon OIDC verifier), gd-MBsUUXpJZB (owner is an admin), gd-bFFquoScHy (PR #249, `escalationApprover`), and gd-0ORn0Mhr4I (the viewer's reject must reach the daemon as the human, not the static token).

**Release rule (OPERATOR):** do not deploy a release that contains Task 9 until Task 13 has merged (in between, local and service_account escalations have no executor). State this in the PR description.

**Files:**
- Create: `kbeads/internal/server/caller_class.go`, `kbeads/internal/server/caller_class_test.go`, `kbeads/internal/server/escalation_seed_test.go`
- Modify: `kbeads/internal/server/http.go:221-222` (routes for `/reject` and `/result`)

**Interfaces:**
- Consumes: `agentNameFromContext` (Task 1), `isBridgeSubject` (Task 3), `authMethodFromContext`, `hasMinimumRole`, `RolesFromContext`, test helpers `newClassedTestServer`, `doBearerJSON` (Task 1).
- Produces:
  - `type callerClass string` with constants `classOperator`, `classBridge`, `classAgent`, `classRunner` (reserved; P4 Task 8 assigns it), `classService`, `classOther`, `classNone`
  - `func (s *BeadsServer) callerClassFromContext(ctx context.Context) callerClass`
  - `func (s *BeadsServer) requireEscalationResolver(verb string, handler http.HandlerFunc) http.HandlerFunc` (`verb` is `"reject"` or `"result"`)
  - Test helpers `seedEscalation(t, ms, id, operatedBy string, fields map[string]any)` and `escFields(t, ms, id) map[string]any`

- [ ] **Step 1: Write the seed helpers**

Create `kbeads/internal/server/escalation_seed_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// seedEscalation stores an open escalation in project gardenio. operatedBy is
// the requester's human (four-eyes compares it byte-for-byte first).
func seedEscalation(t *testing.T, ms *mockStore, id, operatedBy string, fields map[string]any) {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	b := &model.Bead{
		ID: id, Title: "esc " + id, Type: model.TypeEscalation, Kind: model.KindData,
		Status: model.StatusOpen, Labels: []string{"project:gardenio"}, Fields: raw, OperatedBy: operatedBy,
	}
	if err := ms.CreateBead(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

func escFields(t *testing.T, ms *mockStore, id string) map[string]any {
	t.Helper()
	b, err := ms.GetBead(context.Background(), id)
	if err != nil || b == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	m := map[string]any{}
	_ = json.Unmarshal(b.Fields, &m)
	return m
}
```

- [ ] **Step 2: Write the failing tests**

Create `kbeads/internal/server/caller_class_test.go`:

```go
package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestCallerClassFromContext(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	oidcOp := WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "owner@example.com"}), authMethodOIDC), []string{RoleOperator})
	oidcViewer := WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "v@example.com"}), authMethodOIDC), []string{RoleViewer})
	edgeV1 := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "owner@example.com"}), authMethodEdge)
	static := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "crew-a"}), authMethodStatic)
	agent := WithFleet(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"}), authMethodStatic), "agent-gd-a1")
	bridge := WithFleet(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"}), authMethodStatic), "slack-bridge")
	other := WithFleet(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "qa-fleet"}), authMethodStatic), "qa-fleet")
	none := withAuthMethod(context.Background(), authMethodNone)
	for name, c := range map[string]struct {
		ctx  context.Context
		want callerClass
	}{
		"oidc operator": {oidcOp, classOperator},
		"oidc viewer":   {oidcViewer, classOther},
		"edge v1":       {edgeV1, classOther},
		"static":        {static, classService},
		"agent":         {agent, classAgent},
		"bridge":        {bridge, classBridge},
		"other fleet":   {other, classOther},
		"auth disabled": {none, classNone},
	} {
		if got := s.callerClassFromContext(c.ctx); got != c.want {
			t.Errorf("%s: class %q, want %q", name, got, c.want)
		}
	}
}

// Doc 11 defect 1: /reject and /result were open to every static-token pod.
// Positive controls: an operator rejects, a bridge posts a result.
func TestRejectResult_CallerClasses(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	cases := []struct {
		name, bearer, verb string
		want               int
	}{
		{"static reject", testStaticToken, "reject", http.StatusForbidden},
		{"agent reject", testAgentToken, "reject", http.StatusForbidden},
		{"bridge reject", testBridgeToken, "reject", http.StatusForbidden},
		{"static result", testStaticToken, "result", http.StatusForbidden},
		{"unlisted fleet result", testOtherFleetToken, "result", http.StatusForbidden},
		{"operator reject", "owner@example.com", "reject", http.StatusOK},
		{"bridge result", testBridgeToken, "result", http.StatusOK},
	}
	for i, c := range cases {
		id := fmt.Sprintf("gd-esc%d", i)
		seedEscalation(t, ms, id, "", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}})
		body := map[string]any{"reason": "no"}
		if c.verb == "result" {
			body = map[string]any{"result_status": "success"}
		}
		rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/"+c.verb, c.bearer, body, nil)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d; body %s", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}
```

Run: `cd kbeads && go test ./internal/server -run 'TestCallerClassFromContext|TestRejectResult_CallerClasses' -count=1`
Expected: FAIL (`callerClassFromContext` undefined).

- [ ] **Step 3: Implement caller classes and the route gate**

Create `kbeads/internal/server/caller_class.go`:

```go
package server

import (
	"context"
	"net/http"
)

// callerClass is WHO is calling, for the escalation verbs (spec D7). It is not
// a role rank: requireRole(RoleOperator) admits every non-OIDC caller to
// operator routes, which is correct for bead CRUD (agents need it) and wrong
// for resolving an escalation.
type callerClass string

const (
	classOperator callerClass = "operator" // OIDC (or verified v2 edge) human with RoleOperator+
	classBridge   callerClass = "bridge"   // fleet token whose subject is on BEADS_BRIDGE_SUBJECTS
	classAgent    callerClass = "agent"    // per-agent fleet token (Task 1)
	classRunner   callerClass = "runner"   // reserved: P4 Task 8 (gr_ runner tokens)
	classService  callerClass = "service"  // the primary static token (controller, viewer, coopmux, cronjobs)
	classOther    callerClass = "other"    // authenticated, no escalation standing (OIDC below operator, v1 edge, other fleets)
	classNone     callerClass = "none"     // auth disabled (dev, unit tests)
)

func (s *BeadsServer) callerClassFromContext(ctx context.Context) callerClass {
	method := authMethodFromContext(ctx)
	id := AuthIdentityFromContext(ctx)
	switch {
	case method == authMethodNone:
		return classNone
	case method == authMethodOIDC && (id.Type == "oidc" || id.Type == "edge") && id.Subject != "" &&
		hasMinimumRole(RolesFromContext(ctx), RoleOperator):
		return classOperator
	case id.Type == "fleet" && s.isBridgeSubject(id.Subject):
		return classBridge
	}
	if _, ok := agentNameFromContext(ctx); ok {
		return classAgent
	}
	if id.Type == "static" {
		return classService
	}
	return classOther
}

// requireEscalationResolver gates /v1/escalations/{id}/reject (operator only)
// and /result (operator, bridge, agent; the handler narrows which agent).
func (s *BeadsServer) requireEscalationResolver(verb string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := s.callerClassFromContext(r.Context())
		allowed := c == classNone || c == classOperator ||
			(verb == "result" && (c == classBridge || c == classAgent))
		if allowed {
			handler(w, r)
			return
		}
		msg := "escalation reject requires an operator identity (OIDC or verified v2 edge with gardenio-operator)"
		if verb == "result" {
			msg = "escalation result requires an operator, a bridge, or the dispatched agent"
		}
		writeError(w, http.StatusForbidden, msg+"; caller class: "+string(c))
	}
}
```

In `kbeads/internal/server/http.go`, replace lines `:221-222`:

```go
	mux.HandleFunc("POST /v1/escalations/{id}/reject", requireRole(RoleOperator, s.handleRejectEscalation))
	mux.HandleFunc("POST /v1/escalations/{id}/result", requireRole(RoleOperator, s.handleCaptureEscalationResult))
```

with:

```go
	// P7 Task 9 (spec D7): caller classes, not role rank. requireRole still
	// runs first so an OIDC caller below operator is refused the usual way.
	mux.HandleFunc("POST /v1/escalations/{id}/reject", requireRole(RoleOperator, s.requireEscalationResolver("reject", s.handleRejectEscalation)))
	mux.HandleFunc("POST /v1/escalations/{id}/result", requireRole(RoleOperator, s.requireEscalationResolver("result", s.handleCaptureEscalationResult)))
```

Run: `cd kbeads && go test ./internal/server -run 'TestCallerClassFromContext|TestRejectResult_CallerClasses' -count=1`
Expected: PASS.

- [ ] **Step 4: Fix tests that relied on static-token resolution**

Run: `cd kbeads && go test ./internal/server -count=1 2>&1 | grep -E '^(--- FAIL|FAIL|ok)'`
Tests that POST `/reject` or `/result` through `newTestServer` pass (its default bearer is an OIDC admin). Tests that use a static token (`grep -ln 'escalations/.*/\(reject\|result\)' kbeads/internal/server/*_test.go`) either switch to an operator bearer or a bridge token, matching the test's intent, with a comment citing P7 Task 9. Then update the e2e scripts that resolve escalations with the static token (`grep -ln 'escalation \(reject\|close\)\|/reject\|/result' gardenio/tests/e2e/scripts/*.sh`): each such call either runs as the bridge fleet token the script already registers, or is marked SKIP with the reason printed, the same treatment Task 4 gives approve.

- [ ] **Step 5: Format, vet, commit**

```bash
cd kbeads && gofumpt -l internal/server && go vet ./internal/server && staticcheck ./internal/server && go test ./internal/server -count=1 && cd .. && \
git add kbeads/internal/server/caller_class.go kbeads/internal/server/caller_class_test.go kbeads/internal/server/escalation_seed_test.go kbeads/internal/server/http.go kbeads/internal/server/*_test.go gardenio/tests/e2e/scripts && \
git commit -m "fix(kbeads): escalation reject/result admit callers by class, not role rank (gd-nPfSNHTkQQ)"
```

### Task 10: Server-stamped result and reject: executor, rejected_by, and closed_by come from the caller; result fields allowlisted; agents resolve only what they may

**Step bead:** gd-Cg0ykgRoyD. Spec §0 D7, D9, §5.2, doc 11 defects 1, 2. Blocked by Task 9.

**Release rule (OPERATOR):** do not deploy a release that contains Task 9 until Task 13 has merged (in between, local and service_account escalations have no executor). State this in the PR description.

**Files:**
- Create: `kbeads/internal/server/escalation_op_classes.go`, `kbeads/internal/server/escalation_resolver.go`, `kbeads/internal/server/escalation_resolver_test.go`
- Modify: `kbeads/internal/server/http_escalations.go:626-676` (`handleRejectEscalation`), `:697-780` (`handleCaptureEscalationResult`)
- Modify: `kbeads/internal/server/config.go:1575-1638` (`"type:escalation"`): add `{"name": "rejected_by", "type": "string"}` after `approved_by`
- Modify: `kbeads/internal/server/escalation_test.go:544-604` (`TestCaptureEscalationResult_FieldsPassthrough`): replace
- Modify: `gardenio/controller/internal/bridge/escalation_reaper.go:813-856` (close fields), `:591-596` (approvals witness key)
- Modify: `gardenio/controller/cmd/gb/escalation_lifecycle.go` (`escalationCloseCmd`'s `--executor` flag: deprecate)

**Interfaces:**
- Consumes: `callerClassFromContext`, `agentNameFromContext`, `escalationApprover`, `resolveAgentBeadBySubject` (`kbeads/internal/server/audit.go:567`).
- Produces:
  - `func isVerdictOperation(op string) bool` (github_pr_merge, gitlab_mr_merge, github_pr_review, doc_review; case-insensitive)
  - `func isServerReplayOperation(op string) bool` (create, update, close, grant)
  - `func isReplayUnfitOperation(op string) bool` (prefix `terraform_` or `aws_`, or `coopmux_account_sync`, `git_push_public`)
  - `func escalationStringFields(b *model.Bead) map[string]string`
  - `func resultRenderField(key string) bool` (`reaped`, `reaped_*`, `mr_*`, `approvals_*`)
  - `func (s *BeadsServer) resultResolver(ctx context.Context, bead *model.Bead) (identity, refusal string)`

- [ ] **Step 1: Write the failing tests**

Create `kbeads/internal/server/escalation_resolver_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// rejected_by, closed_by and the audit actor are the operator's identity, not
// the body's. Bridges cannot reject (Task 9), so the Slack bridge's old web
// reject could not forge this either.
func TestReject_StampsApproverIdentity(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-rej1", "", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}})
	rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-rej1/reject", "owner@example.com",
		map[string]any{"rejected_by": "someone-else", "reason": "no"}, nil)
	requireStatus(t, rec, http.StatusOK)
	b, _ := ms.GetBead(context.Background(), "gd-rej1")
	if b.ClosedBy != "owner@example.com" {
		t.Fatalf("closed_by = %q, want owner@example.com", b.ClosedBy)
	}
	if f := escFields(t, ms, "gd-rej1"); f["rejected_by"] != "owner@example.com" {
		t.Fatalf("rejected_by = %v", f["rejected_by"])
	}
}

// executor is server-stamped; a forged body executor/execution_target is ignored.
func TestResult_ExecutorIsServerStamped(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-res1", "", map[string]any{"operation_type": "github_pr_merge", "execution_target": "local"})
	rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-res1/result", testBridgeToken,
		map[string]any{"result_status": "success", "executor": "forged", "execution_target": "service_account"}, nil)
	requireStatus(t, rec, http.StatusOK)
	f := escFields(t, ms, "gd-res1")
	if f["executor"] != "slack-bridge" || f["execution_target"] != "local" {
		t.Fatalf("executor=%v execution_target=%v; want slack-bridge, local", f["executor"], f["execution_target"])
	}
	b, _ := ms.GetBead(context.Background(), "gd-res1")
	if b.ClosedBy != "slack-bridge" {
		t.Fatalf("closed_by = %q", b.ClosedBy)
	}
}

// The reaper's render keys land from a bridge; anything else in fields is a
// 400, and only a bridge may send fields at all.
func TestResult_BridgeRenderAllowlist(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-res2", "", map[string]any{"operation_type": "github_pr_merge"})
	ok := map[string]any{"result_status": "success", "fields": map[string]string{"reaped": "true", "reaped_by": "escalation-reaper", "mr_state": "merged", "approvals_left": "0"}}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-res2/result", testBridgeToken, ok, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-res2"); f["mr_state"] != "merged" || f["reaped"] != "true" {
		t.Fatalf("render keys did not land: %v", f)
	}
	for i, c := range []struct {
		name, bearer string
		fields       map[string]string
	}{
		{"bridge sends approved_by", testBridgeToken, map[string]string{"approved_by": "x"}},
		{"bridge sends executor", testBridgeToken, map[string]string{"executor": "x"}},
		{"agent sends fields", testAgentToken, map[string]string{"reaped": "true"}},
	} {
		id := fmt.Sprintf("gd-res2x%d", i)
		seedEscalation(t, ms, id, "", map[string]any{"operation_type": "github_pr_merge", "requesting_agent": "crew-b"})
		rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/result", c.bearer,
			map[string]any{"result_status": "success", "fields": c.fields}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", c.name, rec.Code)
		}
	}
}

// Agents resolve a verdict record they did not request; for executable
// operations only the server-recorded executor may post the result.
func TestResult_AgentAdmission(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	post := func(id, bearer string) int {
		return doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/result", bearer, map[string]any{"result_status": "success"}, nil).Code
	}
	seedEscalation(t, ms, "gd-v1", "", map[string]any{"operation_type": "github_pr_merge", "requesting_agent": "crew-b"})
	if c := post("gd-v1", testAgentToken2); c != http.StatusForbidden {
		t.Errorf("requester resolving its own verdict: %d, want 403", c)
	}
	if c := post("gd-v1", testAgentToken); c != http.StatusOK {
		t.Errorf("positive control: non-requester verdict: %d, want 200", c)
	}
	seedEscalation(t, ms, "gd-x1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "executor": "crew-a"})
	if c := post("gd-x1", testAgentToken2); c != http.StatusForbidden {
		t.Errorf("non-executor agent on executable op: %d, want 403", c)
	}
	if c := post("gd-x1", testAgentToken); c != http.StatusOK {
		t.Errorf("positive control: recorded executor: %d, want 200", c)
	}
	seedEscalation(t, ms, "gd-x2", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}})
	if c := post("gd-x2", testAgentToken); c != http.StatusForbidden {
		t.Errorf("agent on undispatched executable op: %d, want 403", c)
	}
}

// An agent spawned by the requester counts as the requester (spec Q8).
func TestResult_AgentSpawnedByRequesterRefused(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	raw, _ := json.Marshal(map[string]string{"agent": "crew-a", "spawned_by": "crew-b"})
	if err := ms.CreateBead(context.Background(), &model.Bead{ID: "crew-a", Title: "crew-a", Type: "agent", Kind: model.KindData, Status: model.StatusOpen, Fields: raw}); err != nil {
		t.Fatal(err)
	}
	seedEscalation(t, ms, "gd-v2", "", map[string]any{"operation_type": "doc_review", "requesting_agent": "crew-b"})
	if rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-v2/result", testAgentToken, map[string]any{"result_status": "success"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("agent spawned by the requester: %d, want 403", rec.Code)
	}
	seedEscalation(t, ms, "gd-v3", "", map[string]any{"operation_type": "doc_review", "requesting_agent": "crew-z"})
	if rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-v3/result", testAgentToken, map[string]any{"result_status": "success"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("positive control: same agent, unrelated requester: %d, want 200", rec.Code)
	}
}
```

Replace `TestCaptureEscalationResult_FieldsPassthrough` (`kbeads/internal/server/escalation_test.go:544-604`) by deleting it; `TestResult_BridgeRenderAllowlist` above is its successor (name it in the commit message).

Run: `cd kbeads && go test ./internal/server -run 'TestReject_StampsApproverIdentity|TestResult_' -count=1`
Expected: FAIL (`rejected_by` not written; executor taken from body; allowlist absent).

- [ ] **Step 2: Operation classes and field helpers**

Create `kbeads/internal/server/escalation_op_classes.go`:

```go
package server

import (
	"encoding/json"
	"strings"

	"gardenio/kbeads/internal/model"
)

func normOp(op string) string { return strings.ToLower(strings.TrimSpace(op)) }

// Verdict records (spec D9, owner question Q1): a reviewer's verdict on a PR,
// MR, or document. Never executed, never dispatched, never cascade.
var verdictOperationTypes = map[string]bool{
	"github_pr_merge": true, "gitlab_mr_merge": true, "github_pr_review": true, "doc_review": true,
}

func isVerdictOperation(op string) bool { return verdictOperationTypes[normOp(op)] }

// Operation types the daemon itself replays on approval (replayEscalation).
var serverReplayOperationTypes = map[string]bool{"create": true, "update": true, "close": true, "grant": true}

func isServerReplayOperation(op string) bool { return serverReplayOperationTypes[normOp(op)] }

// Replay-unfit types (decision gd-UQTnDZATTU, owner question Q6): no executor
// may run them from an escalation; a human runs them outside gardenio.
func isReplayUnfitOperation(op string) bool {
	op = normOp(op)
	return strings.HasPrefix(op, "terraform_") || strings.HasPrefix(op, "aws_") ||
		op == "coopmux_account_sync" || op == "git_push_public"
}

// escalationStringFields returns the string-valued fields of b.
func escalationStringFields(b *model.Bead) map[string]string {
	out := map[string]string{}
	if b == nil || len(b.Fields) == 0 {
		return out
	}
	var m map[string]any
	if json.Unmarshal(b.Fields, &m) != nil {
		return out
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// resultRenderField: the only keys a bridge may land through /result's
// "fields" (the escalation reaper's render and witness trail).
func resultRenderField(key string) bool {
	return key == "reaped" || strings.HasPrefix(key, "reaped_") ||
		strings.HasPrefix(key, "mr_") || strings.HasPrefix(key, "approvals_")
}
```

- [ ] **Step 3: The result resolver**

Create `kbeads/internal/server/escalation_resolver.go`:

```go
package server

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// resultResolver returns the server-derived identity allowed to post bead's
// result, or a refusal. Spec §5.2.
func (s *BeadsServer) resultResolver(ctx context.Context, bead *model.Bead) (string, string) {
	switch s.callerClassFromContext(ctx) {
	case classNone:
		if a := actorFromContext(ctx); a != "" {
			return a, ""
		}
		return "anonymous", ""
	case classOperator:
		id, _ := s.escalationApprover(ctx)
		return id, ""
	case classBridge:
		return AuthIdentityFromContext(ctx).Subject, ""
	case classAgent:
		name, _ := agentNameFromContext(ctx)
		f := escalationStringFields(bead)
		if f["executor"] != "" && f["executor"] == name {
			return name, ""
		}
		if !isVerdictOperation(f["operation_type"]) {
			return "", "only the dispatched executor may post this escalation's result"
		}
		if s.isRequesterOrSpawnedByRequester(ctx, name, f["requesting_agent"]) {
			return "", "the requesting agent (or an agent it spawned) cannot resolve its own escalation"
		}
		return name, ""
	}
	return "", "escalation result requires an operator, a bridge, or the dispatched agent"
}

// isRequesterOrSpawnedByRequester: requester may be an agent name or an agent
// bead id (both shapes occur in requesting_agent).
func (s *BeadsServer) isRequesterOrSpawnedByRequester(ctx context.Context, caller, requester string) bool {
	if requester == "" {
		return false
	}
	if caller == requester {
		return true
	}
	cb := s.resolveAgentBeadBySubject(ctx, caller)
	if cb == nil {
		return false
	}
	if cb.ID == requester {
		return true
	}
	spawnedBy := escalationStringFields(cb)["spawned_by"]
	if spawnedBy == "" {
		return false
	}
	if spawnedBy == requester {
		return true
	}
	if rb := s.resolveAgentBeadBySubject(ctx, requester); rb != nil && spawnedBy == rb.ID {
		return true
	}
	return false
}
```

- [ ] **Step 4: Rewrite reject**

In `handleRejectEscalation`, replace lines `kbeads/internal/server/http_escalations.go:638-656`:

```go
	// Enforce cross-project write-authz before mutating (ka-0000002666).
	if _, ok := s.authorizeResolutionTarget(w, r, id, "escalation", "handleRejectEscalation"); !ok {
		return
	}

	extra := map[string]any{
		"result_status": "failure",
	}
	if req.Reason != "" {
		extra["result"] = map[string]string{"error": "rejected", "reason": req.Reason}
	}

	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		slog.Warn("failed to merge escalation reject fields", "bead", id, "error", err)
	}

	s.auditEscalationRejection(r.Context(), id, req.RejectedBy, req.Reason)

	closedBy := req.RejectedBy
```

with:

```go
	// Enforce cross-project write-authz before mutating (ka-0000002666).
	if _, ok := s.authorizeResolutionTarget(w, r, id, "escalation", "handleRejectEscalation"); !ok {
		return
	}

	// P7 Task 10: the rejecter is the authenticated operator, never the body.
	rejectedBy, ok := s.escalationApprover(r.Context())
	if !ok {
		if s.callerClassFromContext(r.Context()) != classNone {
			writeError(w, http.StatusForbidden, "escalation rejection requires an operator identity")
			return
		}
		rejectedBy = req.RejectedBy // auth disabled (dev): keep the old behaviour
	}
	if req.RejectedBy != "" && req.RejectedBy != rejectedBy {
		slog.Warn("handleRejectEscalation: ignoring body rejected_by, using derived identity",
			"bead", id, "body_rejected_by", req.RejectedBy, "derived", rejectedBy)
	}

	extra := map[string]any{
		"result_status": "failure",
		"rejected_by":   rejectedBy,
	}
	if req.Reason != "" {
		extra["result"] = map[string]string{"error": "rejected", "reason": req.Reason}
	}

	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		slog.Warn("failed to merge escalation reject fields", "bead", id, "error", err)
	}

	s.auditEscalationRejection(r.Context(), id, rejectedBy, req.Reason)

	closedBy := rejectedBy
```

- [ ] **Step 5: Rewrite result**

In `handleCaptureEscalationResult`, replace lines `kbeads/internal/server/http_escalations.go:722-751`:

```go
	// Enforce cross-project write-authz before mutating (ka-0000002666).
	if _, ok := s.authorizeResolutionTarget(w, r, id, "escalation", "handleCaptureEscalationResult"); !ok {
		return
	}

	extra := map[string]any{}
	// Caller-supplied audit/render fields land FIRST so the typed result fields
	// below always win on a key collision — result_status/executor/… stay
	// authoritative regardless of what the caller put in fields.
	for k, v := range req.Fields {
		if v != "" {
			extra[k] = v
		}
	}
	extra["result_status"] = req.ResultStatus
	if req.Executor != "" {
		extra["executor"] = req.Executor
	}
	if req.ExecutionTarget != "" {
		extra["execution_target"] = req.ExecutionTarget
	}
	if req.Result != nil {
		extra["result"] = req.Result
	}

	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		slog.Warn("failed to merge escalation result fields", "bead", id, "error", err)
	}

	closedBy := req.Executor
```

with:

```go
	// Enforce cross-project write-authz before mutating (ka-0000002666).
	target, ok := s.authorizeResolutionTarget(w, r, id, "escalation", "handleCaptureEscalationResult")
	if !ok {
		return
	}

	// P7 Task 10 (spec §5.2): who resolved is the authenticated caller.
	resolver, refusal := s.resultResolver(r.Context(), target)
	if refusal != "" {
		writeError(w, http.StatusForbidden, refusal)
		return
	}
	if req.Executor != "" && req.Executor != resolver {
		slog.Warn("handleCaptureEscalationResult: ignoring body executor", "bead", id, "body_executor", req.Executor, "resolver", resolver)
	}
	if req.ExecutionTarget != "" {
		slog.Warn("handleCaptureEscalationResult: ignoring body execution_target (frozen at filing)", "bead", id, "body", req.ExecutionTarget)
	}

	extra := map[string]any{}
	if len(req.Fields) > 0 {
		if s.callerClassFromContext(r.Context()) != classBridge && s.callerClassFromContext(r.Context()) != classNone {
			writeError(w, http.StatusBadRequest, "result fields are accepted only from a bridge")
			return
		}
		for k, v := range req.Fields {
			if !resultRenderField(k) {
				writeError(w, http.StatusBadRequest, "result field "+k+" is not allowed (reaped*, mr_*, approvals_* only)")
				return
			}
			if v != "" {
				extra[k] = v
			}
		}
	}
	extra["result_status"] = req.ResultStatus
	if escalationStringFields(target)["executor"] == "" {
		extra["executor"] = resolver
	}
	if req.Result != nil {
		extra["result"] = req.Result
	}

	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		slog.Warn("failed to merge escalation result fields", "bead", id, "error", err)
	}

	closedBy := resolver
```

Add `{"name": "rejected_by", "type": "string"},` after the `approved_by` line in the `type:escalation` config (`kbeads/internal/server/config.go:1597`).

Run: `cd kbeads && go test ./internal/server -run 'TestReject_|TestResult_|TestRejectResult_' -count=1`
Expected: PASS.

- [ ] **Step 6: The reaper stops sending decision fields**

In `gardenio/controller/internal/bridge/escalation_reaper.go`, replace lines `:813-824`:

```go
	closeFields := map[string]string{
		"result_status": "success",
		"executor":      reapActor,
		// approved_by → the out-of-band satisfier. Non-empty routes the SSE
		// handleClosed to the "Complete" (green) Slack render instead of the
		// "Rejected" banner it shows for a never-approved close.
		"approved_by":   actor,
		"reaped":        "true",
		"reaped_by":     reapActor,
		"reaped_at":     nowStr,
		"reaped_reason": audit.reason,
	}
```

with:

```go
	// P7 Task 10: only render/witness keys ride along (the daemon refuses
	// anything else). result_status=success already routes handleClosed to the
	// "Complete" render; the daemon stamps executor and closed_by itself.
	closeFields := map[string]string{
		"reaped":            "true",
		"reaped_by":         reapActor,
		"reaped_at":         nowStr,
		"reaped_reason":     audit.reason,
		"reaped_satisfier":  actor,
	}
```

At `gardenio/controller/internal/bridge/escalation_reaper.go:848-852` drop `Executor: reapActor,` from the request. At `gardenio/controller/internal/bridge/escalation_reaper.go:595` rename the approvals witness key `"approved_by": approvers,` to `"mr_approved_by": approvers,`. Update the reaper's tests (`grep -n 'approved_by\|closeFields\|Executor' gardenio/controller/internal/bridge/escalation_reaper_test.go`): every assertion on `approved_by` becomes `reaped_satisfier` (or `mr_approved_by` for the approvals witness), and add one assertion that every key in the captured `Fields` passes `key == "reaped" || strings.HasPrefix(key, "reaped_") || strings.HasPrefix(key, "mr_") || strings.HasPrefix(key, "approvals_")`. Check the Slack result renderer does not read `approved_by` for reaped escalations (`grep -n 'approved_by' gardenio/controller/internal/bridge/bot_escalations.go`); where it shows "Approved by", fall back to `reaped_satisfier`.

In `gardenio/controller/cmd/gb/escalation_lifecycle.go`, after the `escalationCloseCmd` flags are registered (`grep -n '"executor"' gardenio/controller/cmd/gb/escalation_lifecycle.go`), add `_ = escalationCloseCmd.Flags().MarkDeprecated("executor", "the daemon records the authenticated caller as executor")`.

Run: `cd gardenio/controller && go test ./internal/bridge ./cmd/gb -count=1`
Expected: PASS.

- [ ] **Step 7: Format, vet, commit**

```bash
cd kbeads && gofumpt -l internal/server && go vet ./internal/server && staticcheck ./internal/server && go test ./internal/server -count=1 && \
cd ../gardenio/controller && gofumpt -l internal/bridge cmd/gb && go vet ./internal/bridge ./cmd/gb && go test ./internal/bridge ./cmd/gb -count=1 && cd ../.. && \
git add kbeads/internal/server gardenio/controller/internal/bridge/escalation_reaper.go gardenio/controller/internal/bridge/escalation_reaper_test.go gardenio/controller/internal/bridge/bot_escalations.go gardenio/controller/cmd/gb/escalation_lifecycle.go && \
git commit -m "fix(kbeads): server-stamped escalation result/reject; allowlisted render fields replace the passthrough (TestCaptureEscalationResult_FieldsPassthrough -> TestResult_BridgeRenderAllowlist) (gd-Cg0ykgRoyD)"
```

### Task 11: The daemon is the only writer of escalation decision fields; filing fields are frozen; create, PATCH, and field-edit endpoints all enforce it

**Step bead:** gd-tZYAufHCdo. Spec §0 D8, §1.5, doc 11 defects 1, 3, 11. Blocked by Task 13 (which removes the watcher's and bridge's `executor` PATCHes) and Task 4 (which removes the bridge's `approved_by` PATCH).

**Release rule (OPERATOR):** do not deploy a release that contains Task 9 until Task 13 has merged (in between, local and service_account escalations have no executor). State this in the PR description.

**Files:**
- Create: `kbeads/internal/server/escalation_field_guard.go`, `kbeads/internal/server/escalation_field_guard_test.go`
- Modify: `kbeads/internal/server/escalation_requesting_agent.go:43-77` (`checkEscalationRequestingAgentImmutable`): delete; its successor is `checkEscalationFieldsImmutable`
- Modify: `kbeads/internal/server/beads.go:1414` (PATCH call site), `kbeads/internal/server/beads.go:390` (create: add the create check just before the requesting-agent guard)
- Modify: `kbeads/internal/server/http_beads.go:929-933` (`tokenAuthorizedForBead`, the chokepoint for append/reorder/edit/increment)

**Interfaces:**
- Consumes: `isMachineWrite(ctx)` (`kbeads/internal/server/write_authz.go:105`), `inputError`.
- Produces:
  - `var escalationServerOnlyFields map[string]bool` (requesting_agent, approved_by, approved_by_email, approved_at, approval_method, policy_rule, executor, execution_dispatch, replay_in_progress, result_status, result, rejected_by)
  - `var escalationFrozenFields map[string]bool` (operation_type, operation_spec, target_bead_id, execution_target, target_runner, spawn_role, execution_script, requires_four_eyes, approvers, sensitivity)
  - `func checkEscalationFieldsImmutable(bead *model.Bead, priorFields json.RawMessage) error`
  - `func escalationFieldWriteDenied(bead *model.Bead, keys ...string) string`
  - `func checkEscalationCreateFields(ctx context.Context, bead *model.Bead) error`

- [ ] **Step 1: Write the failing tests**

Create `kbeads/internal/server/escalation_field_guard_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// PATCH cannot write a server-only field or change a frozen one. Positive
// control: an ordinary field (notified_at, stamped by the bridge) still patches.
func TestEscalationFields_PatchGuard(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-f1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}})
	for _, f := range []map[string]any{
		{"approved_by": "x"}, {"executor": "x"}, {"result_status": "success"}, {"replay_in_progress": true},
		{"execution_dispatch": "local"}, {"operation_spec": map[string]any{"m": 2}}, {"execution_target": "local"},
	} {
		rec := doBearerJSON(t, h, "PATCH", "/v1/beads/gd-f1", "owner@example.com", map[string]any{"fields": f}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %v: status %d, want 400", f, rec.Code)
		}
	}
	rec := doBearerJSON(t, h, "PATCH", "/v1/beads/gd-f1", "owner@example.com", map[string]any{"fields": map[string]any{"notified_at": "2026-10-11T00:00:00Z"}}, nil)
	requireStatus(t, rec, http.StatusOK)
}

// The field-edit endpoints share tokenAuthorizedForBead; they bypassed every
// per-type update guard (spec §1.5). Positive control: the same edit on a task.
func TestEscalationFields_SubFieldEndpointsGuarded(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-f2", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}})
	rec := doBearerJSON(t, h, "POST", "/v1/beads/gd-f2/fields/operation_spec/edit", testStaticToken,
		map[string]any{"op": "set", "key": "m", "value": "2"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("edit operation_spec: %d, want 400", rec.Code)
	}
	raw, _ := json.Marshal(map[string]any{"meta": map[string]any{"m": "1"}})
	if err := ms.CreateBead(context.Background(), &model.Bead{ID: "gd-t1", Title: "t", Type: "task", Kind: model.KindIssue,
		Status: model.StatusOpen, Labels: []string{"project:gardenio"}, Fields: raw}); err != nil {
		t.Fatal(err)
	}
	rec = doBearerJSON(t, h, "POST", "/v1/beads/gd-t1/fields/meta/edit", testStaticToken,
		map[string]any{"op": "set", "key": "m", "value": "2"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("positive control: edit on a task: %d, want 200; %s", rec.Code, rec.Body.String())
	}
}

// Create refuses client-supplied server-only fields (requesting_agent stays
// allowed: the create guard stamps or accepts it). Frozen fields are set here.
func TestEscalationFields_CreateRefusesServerOnly(t *testing.T) {
	_, _, h := newClassedTestServer(t)
	base := func(extra map[string]any) map[string]any {
		f := map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}}
		for k, v := range extra {
			f[k] = v
		}
		return map[string]any{"title": "e", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"}, "fields": f}
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", base(map[string]any{"approved_by": "me"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("create with approved_by: %d, want 400", rec.Code)
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", base(map[string]any{"requesting_agent": "crew-x", "sensitivity": "internal"}), nil); rec.Code != http.StatusCreated {
		t.Fatalf("positive control: create with requesting_agent + frozen field: %d, want 201; %s", rec.Code, rec.Body.String())
	}
}
```

Run: `cd kbeads && go test ./internal/server -run 'TestEscalationFields_' -count=1`
Expected: FAIL (PATCH of `approved_by` returns 200; sub-field edit returns 200; create with `approved_by` returns 201).

- [ ] **Step 2: Implement the guard**

Create `kbeads/internal/server/escalation_field_guard.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"

	"gardenio/kbeads/internal/model"
)

// Spec D8. Written only by the daemon's own verbs (approve, reject, result,
// the create guards, the replay and dispatch claims).
var escalationServerOnlyFields = map[string]bool{
	"requesting_agent": true, "approved_by": true, "approved_by_email": true, "approved_at": true,
	"approval_method": true, "policy_rule": true, "executor": true, "execution_dispatch": true,
	"replay_in_progress": true, "result_status": true, "result": true, "rejected_by": true,
}

// What the approver saw. Set at filing, never changed afterwards.
var escalationFrozenFields = map[string]bool{
	"operation_type": true, "operation_spec": true, "target_bead_id": true, "execution_target": true,
	"target_runner": true, "spawn_role": true, "execution_script": true, "requires_four_eyes": true,
	"approvers": true, "sensitivity": true,
}

func escalationGuardedReason(key string) string {
	if escalationServerOnlyFields[key] {
		return key + " is written only by the daemon's escalation verbs"
	}
	if escalationFrozenFields[key] {
		return key + " is frozen once the escalation is filed (it is what the approver saw)"
	}
	return ""
}

// checkEscalationFieldsImmutable replaces checkEscalationRequestingAgentImmutable
// on the PATCH path: no guarded key may change. bead.Fields is post-merge.
func checkEscalationFieldsImmutable(bead *model.Bead, priorFields json.RawMessage) error {
	if bead.Type != model.TypeEscalation {
		return nil
	}
	var prior, current map[string]any
	if len(priorFields) > 0 {
		_ = json.Unmarshal(priorFields, &prior)
	}
	if len(bead.Fields) > 0 {
		_ = json.Unmarshal(bead.Fields, &current)
	}
	keys := make([]string, 0, len(current)+len(prior))
	for k := range current {
		keys = append(keys, k)
	}
	for k := range prior {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		reason := escalationGuardedReason(k)
		if reason == "" {
			continue
		}
		pv, pok := prior[k]
		cv, cok := current[k]
		if pok != cok || !reflect.DeepEqual(pv, cv) {
			return inputError(reason)
		}
	}
	return nil
}

// escalationFieldWriteDenied is the field-edit endpoints' check (append,
// reorder, edit, increment all name the field they write).
func escalationFieldWriteDenied(bead *model.Bead, keys ...string) string {
	if bead == nil || bead.Type != model.TypeEscalation {
		return ""
	}
	for _, k := range keys {
		if reason := escalationGuardedReason(k); reason != "" {
			return reason
		}
	}
	return ""
}

// checkEscalationCreateFields refuses client-supplied server-only keys at
// create. requesting_agent is the exception: the create guard stamps it for
// agents and accepts it from everyone else (unchanged behaviour). Machine
// writes (replay, the daemon's own spawns) are exempt.
func checkEscalationCreateFields(ctx context.Context, bead *model.Bead) error {
	if bead.Type != model.TypeEscalation || isMachineWrite(ctx) || len(bead.Fields) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(bead.Fields, &m) != nil {
		return nil // validateFields reports the malformed blob
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "requesting_agent" && escalationServerOnlyFields[k] {
			return inputError(k + " is written only by the daemon's escalation verbs; omit it when filing")
		}
	}
	return nil
}
```

- [ ] **Step 3: Wire it into the three paths**

1. PATCH: at `kbeads/internal/server/beads.go:1414` replace `s.checkEscalationRequestingAgentImmutable(bead, json.RawMessage(oldValues["fields"].(string)))` with `checkEscalationFieldsImmutable(bead, json.RawMessage(oldValues["fields"].(string)))`, update the comment above it (`:1401-1413`) to name P7 Task 11, and delete `checkEscalationRequestingAgentImmutable` from `escalation_requesting_agent.go`. Rename its tests' calls (`grep -ln checkEscalationRequestingAgentImmutable kbeads/internal/server/*_test.go`) to the new function; they keep passing because `requesting_agent` is in the server-only set.
2. Create: in `beads.go`, immediately before `s.applyEscalationRequestingAgentGuard(ctx, bead)` (`:390`), add:

```go
		if err := checkEscalationCreateFields(ctx, bead); err != nil {
			return nil, err
		}
```

(Match the surrounding function's return shape; `grep -n 'applyEscalationRequestingAgentGuard' -B8 kbeads/internal/server/beads.go` shows it.)
3. Field-edit endpoints: in `tokenAuthorizedForBead`, after the `authorizeUserBeadFieldWrite` block (`kbeads/internal/server/http_beads.go:929-933`), add:

```go
	// P7 Task 11: these endpoints write straight to the store and skip the
	// PATCH path's per-type guards; escalation decision and filing fields
	// are refused here so they cannot be edited around the verbs.
	if reason := escalationFieldWriteDenied(existing, fieldKeys...); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return false
	}
```

Run: `cd kbeads && go test ./internal/server -run 'TestEscalationFields_|RequestingAgent' -count=1`
Expected: PASS.

- [ ] **Step 4: Find and fix every client that PATCHes a guarded key**

Run: `grep -rn --include=*.go -E 'UpdateBeadFields(Typed)?\(' gardenio/controller kbeads/cmd | grep -v _test.go` and, for each hit whose map contains a key from either set on an escalation bead, remove the write (the daemon now owns it) or move it into the verb that owns it. After Tasks 4, 12, and 13 there should be none; record the grep output in the PR description. Then update agent-facing instructions that tell an agent to PATCH a result: `grep -rn "result_status=success" --include=*.go --include=*.md gardenio kbeads docs | grep -v _test`; each becomes `gb escalation close <id> --status success --result '<json or text>'`.

- [ ] **Step 5: Full suite, format, commit**

```bash
cd kbeads && gofumpt -l internal/server && go vet ./internal/server && staticcheck ./internal/server && go test ./... -count=1 && cd .. && \
git add kbeads/internal/server gardenio docs && \
git commit -m "fix(kbeads): escalation decision fields server-only, filing fields frozen, on create/PATCH/field-edit endpoints (gd-tZYAufHCdo)"
```

### Task 12: One dispatch, one claim: the approve handlers choose the executor and claim it; the bridge's approved_by trigger is deleted

**Step bead:** gd-88AQJozzvX. Spec §0 D10, §5.3, doc 11 defects 3, 4, 5. Blocked by Task 10.

**Release rule (OPERATOR):** do not deploy a release that contains Task 9 until Task 13 has merged (in between, local and service_account escalations have no executor). State this in the PR description.

**Files:**
- Modify: `kbeads/internal/store/store.go:1930-1944` (escalation replay claim interface block): add `ClaimEscalationExecution`
- Create: `kbeads/internal/store/postgres/escalation_execution_claim.go`, `kbeads/internal/store/postgres/escalation_execution_claim_test.go`
- Modify: mocks `kbeads/internal/server/http_test.go:2817-2835` (`ClaimEscalationReplay`; add the new method after it) and `kbeads/internal/sync/mock_store_test.go` (add the method next to its `ClaimEscalationReplay`)
- Create: `kbeads/internal/server/escalation_dispatch.go`, `kbeads/internal/server/escalation_dispatch_test.go`
- Modify: `kbeads/internal/server/http_escalations.go:98-108` (`approveEscalationRequest`), `:205-245` (approve: merge, transition, replay)
- Modify: `kbeads/internal/server/spawn_policy.go:515-553` (`autoApproveSpawnEscalation`)
- Modify: `kbeads/internal/server/config.go:1602` (add `execution_dispatch` enum field after `execution_target`)
- Modify: `gardenio/controller/internal/bridge/escalations.go:192-221` (trigger), `:380-569` (`triggerExecution` to `triggerLocal`: delete)
- Modify: `gardenio/controller/cmd/gb/agent_start_escalation.go:250-258` (watcher filter)
- Modify: `gardenio/controller/cmd/gb/escalation_lifecycle.go:56-68` (`--target` PATCH moves into the approve request), `gardenio/controller/internal/beadsapi/client_escalations.go:83-86` and `kbeads/internal/client/escalations.go:70-80` (`ApproveEscalationRequest` gains `ExecutionTarget`)

**Interfaces:**
- Consumes: `isVerdictOperation`, `isServerReplayOperation`, `escalationStringFields` (Task 10).
- Produces:
  - store: `ClaimEscalationExecution(ctx context.Context, beadID, dispatch string) (claimed bool, current string, err error)`
  - `const dispatchServer, dispatchLocal, dispatchServiceAccount, dispatchNone = "server", "local", "service_account", "none"`
  - `func escalationDispatchFor(f map[string]string) string`
  - `func (s *BeadsServer) claimEscalationDispatch(ctx context.Context, bead *model.Bead) (dispatch string, proceed bool, err error)`
  - approve body: `execution_target` (optional; `local` or `service_account`; operator override recorded with the approval)

- [ ] **Step 1: Write the failing tests**

Create `kbeads/internal/server/escalation_dispatch_test.go`:

```go
package server

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

func TestEscalationDispatchFor(t *testing.T) {
	for _, c := range []struct {
		f    map[string]string
		want string
	}{
		{map[string]string{"operation_type": "github_pr_merge", "execution_target": "local"}, dispatchNone},
		{map[string]string{"operation_type": "create", "operation_spec": `{"title":"x"}`}, dispatchServer},
		{map[string]string{"operation_type": "k8s_apply", "execution_target": "local"}, dispatchLocal},
		{map[string]string{"operation_type": "k8s_apply", "execution_target": "service_account"}, dispatchServiceAccount},
		{map[string]string{"operation_type": "manual_operator_action"}, dispatchNone},
	} {
		if got := escalationDispatchFor(c.f); got != c.want {
			t.Errorf("%v: %q, want %q", c.f, got, c.want)
		}
	}
}

// Approve records execution_dispatch exactly once; two concurrent approves of a
// local escalation produce one claim and one 409.
func TestApprove_ClaimsOneDispatch(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-d1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "local"})
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doBearerJSON(t, h, "POST", "/v1/escalations/gd-d1/approve", "owner@example.com", map[string]any{}, nil).Code
		}(i)
	}
	wg.Wait()
	if !((codes[0] == http.StatusOK && codes[1] == http.StatusConflict) || (codes[0] == http.StatusConflict && codes[1] == http.StatusOK)) {
		t.Fatalf("two approves: %v, want one 200 and one 409", codes)
	}
	if f := escFields(t, ms, "gd-d1"); f["execution_dispatch"] != "local" {
		t.Fatalf("execution_dispatch = %v", f["execution_dispatch"])
	}
}

// Review Focus 3: a server replay that failed can be approved again; the
// existing replay claim, not the dispatch claim, serialises the retry.
func TestApprove_ServerReplayFailureCanBeRetried(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-d2", "", map[string]any{"operation_type": "update", "operation_spec": map[string]any{"title": "x"}, "target_bead_id": "gd-missing"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-d2/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-d2"); f["execution_dispatch"] != "server" || f["result_status"] == "success" {
		t.Fatalf("first approve: %v", f)
	}
	if _, proceed, err := s.claimEscalationDispatch(context.Background(), ms.beads["gd-d2"]); err != nil || !proceed {
		t.Fatalf("re-approve of a server dispatch must proceed to replay: proceed=%v err=%v", proceed, err)
	}
}
```

Add a postgres test in `kbeads/internal/store/postgres/escalation_execution_claim_test.go` modelled on `escalation_replay_claim_test.go` (same DB setup and skip rule): first claim `local` returns `(true, "local")`; second claim `server` returns `(false, "local")`; a missing bead returns `(false, "")`.

Run: `cd kbeads && go test ./internal/server -run 'TestEscalationDispatchFor|TestApprove_ClaimsOneDispatch|TestApprove_ServerReplayFailureCanBeRetried' -count=1`
Expected: FAIL (undefined `escalationDispatchFor`).

- [ ] **Step 2: Store method**

Add to the interface in `kbeads/internal/store/store.go`, after `ReleaseEscalationReplayClaim` (`:1944`):

```go
	// ClaimEscalationExecution atomically records which executor runs an
	// approved escalation (P7 Task 12, spec D10). The guard (only when
	// fields.execution_dispatch is unset or empty) and the write are one
	// statement. Returns claimed=false and the value already there when
	// another approve won; ("", false, nil) when the bead is missing.
	ClaimEscalationExecution(ctx context.Context, beadID, dispatch string) (claimed bool, current string, err error)
```

Create `kbeads/internal/store/postgres/escalation_execution_claim.go`:

```go
package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// ClaimEscalationExecution: see store.Store. One guarded UPDATE, then a read
// of the winning value when this caller lost.
func (s *Store) ClaimEscalationExecution(ctx context.Context, beadID, dispatch string) (bool, string, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE beads
		   SET fields = jsonb_set(COALESCE(fields, '{}'::jsonb), '{execution_dispatch}', to_jsonb($2::text), true),
		       updated_at = NOW()
		 WHERE id = $1
		   AND deleted_at IS NULL
		   AND COALESCE(fields ->> 'execution_dispatch', '') = ''`,
		beadID, dispatch)
	if err != nil {
		return false, "", err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, "", err
	} else if n == 1 {
		return true, dispatch, nil
	}
	var current sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT fields ->> 'execution_dispatch' FROM beads WHERE id = $1 AND deleted_at IS NULL`, beadID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return false, current.String, err
}
```

(Match the receiver and DB handle names used in `escalation_replay_claim.go`; `grep -n 'func (s \*Store) ClaimEscalationReplay' -A5 kbeads/internal/store/postgres/escalation_replay_claim.go` shows them.)

Mock, in `kbeads/internal/server/http_test.go` after `ClaimEscalationReplay` (`:2835`), and the same body in `kbeads/internal/sync/mock_store_test.go` (adapt the field-map helpers to that file's mock):

```go
func (m *mockStore) ClaimEscalationExecution(_ context.Context, beadID, dispatch string) (bool, string, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[beadID]
	if !ok {
		return false, "", nil
	}
	fields := mockFieldMap(b)
	if raw, ok := fields["execution_dispatch"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err == nil && v != "" {
			return false, v, nil
		}
	}
	encoded, _ := json.Marshal(dispatch)
	fields["execution_dispatch"] = encoded
	mockWriteFields(b, fields)
	return true, dispatch, nil
}
```

Add `{"name": "execution_dispatch", "type": "enum", "values": ["server", "local", "service_account", "none"]},` after the `execution_target` line in the `type:escalation` config (`kbeads/internal/server/config.go:1602`).

- [ ] **Step 3: Dispatch selection and claim**

Create `kbeads/internal/server/escalation_dispatch.go`:

```go
package server

import (
	"context"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
)

const (
	dispatchServer         = "server"
	dispatchLocal          = "local"
	dispatchServiceAccount = "service_account"
	dispatchNone           = "none"
)

// escalationDispatchFor picks the single executor for an approved
// escalation (spec D10). Verdict records and escalations with no executor
// are "none": the daemon dispatches nothing and a human or reviewer posts
// the result.
func escalationDispatchFor(f map[string]string) string {
	op := f["operation_type"]
	switch {
	case isVerdictOperation(op):
		return dispatchNone
	case isServerReplayOperation(op) && strings.TrimSpace(f["operation_spec"]) != "":
		return dispatchServer
	case f["execution_target"] == "service_account":
		return dispatchServiceAccount
	case f["execution_target"] == "local":
		return dispatchLocal
	}
	return dispatchNone
}

// claimEscalationDispatch claims bead's dispatch. proceed=false means another
// approve already dispatched it to a different executor (caller returns 409).
// A repeated server dispatch proceeds: replayEscalation's own claim
// serialises the retry of a failed replay.
func (s *BeadsServer) claimEscalationDispatch(ctx context.Context, bead *model.Bead) (string, bool, error) {
	f := escalationStringFields(bead)
	if raw := fieldRaw(bead, "operation_spec"); raw != "" {
		f["operation_spec"] = raw
	}
	want := escalationDispatchFor(f)
	claimed, current, err := s.store.ClaimEscalationExecution(ctx, bead.ID, want)
	if err != nil {
		return "", false, fmt.Errorf("claim escalation dispatch: %w", err)
	}
	if claimed {
		return want, true, nil
	}
	if current == dispatchServer && want == dispatchServer {
		return want, true, nil
	}
	return current, false, nil
}
```

`operation_spec` is a JSON object, so `escalationStringFields` skips it; add the small helper `fieldRaw(b *model.Bead, key string) string` in the same file, returning the raw JSON text of that key when it is a non-empty object, array, or string, and `""` for absent, `null`, `{}`, `[]`, or `""`.

- [ ] **Step 4: Wire the approve handlers**

Add `ExecutionTarget string `json:"execution_target,omitempty"`` to `approveEscalationRequest` (`kbeads/internal/server/http_escalations.go:98-108`). In `handleApproveEscalation`, at `:205-211` (building `extra`), add, before the merge:

```go
	// P7 Task 12: the operator may choose the executor at approval (gb
	// escalation approve --target). Written with the approval, by the daemon.
	switch req.ExecutionTarget {
	case "":
	case "local", "service_account":
		if escalationStringFields(targetBead)["execution_dispatch"] == "" {
			extra["execution_target"] = req.ExecutionTarget
		}
	default:
		writeError(w, http.StatusBadRequest, "execution_target must be local or service_account")
		return
	}
```

Then replace `kbeads/internal/server/http_escalations.go:244-245`:

```go
	// Attempt auto-replay of the denied operation.
	replayResult := s.replayEscalation(r.Context(), bead, approvedBy, req.GrantUntil)
```

with:

```go
	// P7 Task 12 (spec D10): exactly one executor, claimed before anything runs.
	dispatch, proceed, err := s.claimEscalationDispatch(r.Context(), bead)
	if err != nil {
		writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to claim escalation dispatch")
		return
	}
	if !proceed {
		writeError(w, http.StatusConflict, "escalation already dispatched to "+dispatch)
		return
	}
	var replayResult map[string]any
	if dispatch == dispatchServer {
		replayResult = s.replayEscalation(r.Context(), bead, approvedBy, req.GrantUntil)
	}
```

In `autoApproveSpawnEscalation` (`kbeads/internal/server/spawn_policy.go:550`), replace `result := s.replayEscalation(ctx, refreshed, spawnPolicyActor, nil)` with:

```go
	dispatch, proceed, err := s.claimEscalationDispatch(ctx, refreshed)
	if err != nil || !proceed || dispatch != dispatchServer {
		slog.Warn("spawn policy: not replaying", "escalation", bead.ID, "dispatch", dispatch, "proceed", proceed, "error", err)
		return
	}
	result := s.replayEscalation(ctx, refreshed, spawnPolicyActor, nil)
```

Run: `cd kbeads && go test ./internal/server -run 'TestEscalationDispatchFor|TestApprove_|SpawnPolicy|Escalation' -count=1`
Expected: PASS.

- [ ] **Step 5: Delete the bridge's trigger; the watcher requires a local dispatch**

In `gardenio/controller/internal/bridge/escalations.go`, replace lines `:192-221` (from `// Dedup: don't trigger execution twice` through `e.triggerExecution(ctx, *bead)`) with:

```go
	// P7 Task 12: the bridge no longer executes anything on approval. The
	// daemon's approve handler chooses and claims the one executor
	// (execution_dispatch). Only the Slack update and pingback bookkeeping stay.
	e.triggeredMu.Lock()
	if _, seen := e.triggered[bead.ID]; seen {
		e.triggeredMu.Unlock()
		return
	}
	e.triggered[bead.ID] = time.Now()
	e.triggeredMu.Unlock()

	if e.escalationNotifier != nil {
		if err := e.escalationNotifier.UpdateEscalationApproved(ctx, *bead); err != nil {
			e.logger.Error("failed to notify escalation approval", "id", bead.ID, "error", err)
		}
	}
	if agent := bead.Fields["requesting_agent"]; agent != "" {
		e.pendingMu.Lock()
		e.pending[bead.ID] = agent
		e.pendingMu.Unlock()
	}
```

Delete `triggerExecution`, `triggerServiceAccount`, `spawnLocalAgent`, `spawnExecutionAgent`, and `triggerLocal` (`gardenio/controller/internal/bridge/escalations.go:380-569`), then delete whatever `staticcheck` reports as newly unused (expected: `recordResult`, `buildEscalationPrompt`, `buildEscalationExecutionPlan`, `mustMarshalString` if no other caller). Delete their tests (`grep -ln 'triggerExecution\|triggerLocal\|triggerServiceAccount\|spawnLocalAgent' gardenio/controller/internal/bridge/*_test.go`) and add one test, `TestHandleUpdated_ApprovalDoesNotSpawn`, which feeds an approved escalation event to `handleUpdated` with a fake daemon and asserts no `SpawnAgent`, `NudgeAgent`, or `UpdateBeadFields` call was made and the Slack notifier was called once.

In `gardenio/controller/cmd/gb/agent_start_escalation.go`, replace lines `:255-258`:

```go
	// Check that it was actually approved (has approved_by set).
	if bead.fieldString("approved_by") == "" {
		return
	}
```

with:

```go
	// P7 Task 12: act only on escalations the daemon dispatched to a local
	// executor. approved_by alone is not a dispatch.
	if bead.fieldString("execution_dispatch") != "local" {
		return
	}
```

and add a watcher test case (next to the existing `handleEvent` tests, `grep -ln 'handleEvent' gardenio/controller/cmd/gb/*_test.go`): an event with `approved_by` set and no `execution_dispatch` is ignored; the same event with `execution_dispatch=local` is processed.

- [ ] **Step 6: `gb escalation approve --target` uses the approve request**

Add `ExecutionTarget string `json:"execution_target,omitempty"`` to both `ApproveEscalationRequest` structs (`gardenio/controller/internal/beadsapi/client_escalations.go:83-86`, `kbeads/internal/client/escalations.go:70-80`). In `gardenio/controller/cmd/gb/escalation_lifecycle.go`, delete the `UpdateBeadFields(... "execution_target": execTarget ...)` block (`:56-62`) and pass `ExecutionTarget: execTarget` in the `ApproveEscalation` request at `:64-67`.

Run: `cd gardenio/controller && go test ./internal/bridge ./cmd/gb ./internal/beadsapi -count=1 && cd ../../kbeads && go test ./internal/client ./internal/store/... -count=1`
Expected: PASS (postgres tests skip without a database, as they do today).

- [ ] **Step 7: Format, vet, commit**

```bash
cd kbeads && gofumpt -l internal && go vet ./... && staticcheck ./internal/server ./internal/store/... && go test ./internal/server ./internal/sync ./internal/store/... -count=1 && \
cd ../gardenio/controller && gofumpt -l internal/bridge cmd/gb internal/beadsapi && go vet ./internal/bridge ./cmd/gb ./internal/beadsapi && staticcheck ./internal/bridge ./cmd/gb && go test ./internal/bridge ./cmd/gb ./internal/beadsapi -count=1 && cd ../.. && \
git add kbeads gardenio/controller && \
git commit -m "fix(escalations): approve chooses and claims one executor; bridge approved_by trigger and local->spawn fallback deleted (gd-88AQJozzvX)"
```

### Task 13: A local escalation names its runner; service_account runs a privileged role the daemon spawns

**Step bead:** gd-46OpRA7jHg. Spec §0 D10, §5.3, owner questions Q2 and Q3, doc 11 defects 4 to 6. Blocked by Task 12. P4 Task 16 (gd-UAUvpQPOcj) widens `target_runner` to runner beads later and must not re-declare it.

**Release rule (OPERATOR):** do not deploy a release that contains Task 9 until Task 13 has merged (in between, local and service_account escalations have no executor). State this in the PR description.

**Files:**
- Create: `kbeads/internal/server/escalation_targets.go`, `kbeads/internal/server/escalation_targets_test.go`
- Modify: `kbeads/internal/server/beads.go:390` (create: target checks next to Task 11's create check)
- Modify: `kbeads/internal/server/http_escalations.go` (approve: after the Task 12 claim, act on `local` and `service_account`)
- Modify: `kbeads/internal/server/config.go:1602` (add `{"name": "target_runner", "type": "string"}` after `execution_target`)
- Modify: `gardenio/controller/cmd/gb/agent_start_escalation.go:250-263` (filter on `target_runner`), `:319-325` (delete the executor PATCH)

**Interfaces:**
- Consumes: `roleIsPrivileged(ctx, store, role)` (`kbeads/internal/server/spawn_authz.go:176`), `withApprovedSpawnReplay`, `stampSpawnProvenance`, `withMachineWrite`, `WithMachineOperator`, `s.createBead`, dispatch constants (Task 12).
- Produces:
  - `func (s *BeadsServer) checkEscalationTargets(ctx context.Context, bead *model.Bead) error` (create-time)
  - `func (s *BeadsServer) startEscalationExecutor(ctx context.Context, bead *model.Bead, dispatch, approvedBy string) error` (approve-time)
  - Service-account agent beads are named `escalation-sa-<escalation id>`.

- [ ] **Step 1: Write the failing tests**

Create `kbeads/internal/server/escalation_targets_test.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

func seedLocalAgent(t *testing.T, ms *mockStore, id, name string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"agent": name, "execution_target": "local"})
	if err := ms.CreateBead(context.Background(), &model.Bead{ID: id, Title: name, Type: "agent", Kind: model.KindData,
		Status: model.StatusOpen, Labels: []string{"project:gardenio", "execution_target:local"}, Fields: raw}); err != nil {
		t.Fatal(err)
	}
}

func escCreate(fields map[string]any) map[string]any {
	return map[string]any{"title": "e", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"}, "fields": fields}
}

// local requires a named, open, local agent bead (spec Q2).
func TestEscalationTargets_LocalNeedsRunner(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedLocalAgent(t, ms, "gd-la1", "laptop-agent")
	spec := map[string]any{"m": 1}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": "k8s_apply", "operation_spec": spec, "execution_target": "local"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("local without target_runner: %d, want 400", rec.Code)
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": "k8s_apply", "operation_spec": spec, "execution_target": "local", "target_runner": "gd-nope"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("local with unknown runner: %d, want 400", rec.Code)
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": "k8s_apply", "operation_spec": spec, "execution_target": "local", "target_runner": "gd-la1"}), nil); rec.Code != http.StatusCreated {
		t.Fatalf("positive control: local with a real runner: %d; %s", rec.Code, rec.Body.String())
	}
}

// Approving a local escalation records the runner's agent as executor, so
// that agent (and only it) may post the result (Task 10).
func TestApprove_LocalStampsRunnerAsExecutor(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedLocalAgent(t, ms, "gd-la2", "crew-a")
	seedEscalation(t, ms, "gd-l1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "local", "target_runner": "gd-la2"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-l1/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-l1"); f["executor"] != "crew-a" || f["execution_dispatch"] != "local" {
		t.Fatalf("executor=%v dispatch=%v", f["executor"], f["execution_dispatch"])
	}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-l1/result", testAgentToken, map[string]any{"result_status": "success"}, nil), http.StatusOK)
}

// service_account needs a privileged spawn_role and an execution_script; the
// daemon creates the executor agent bead with that role (spec Q3).
func TestApprove_ServiceAccountSpawnsPrivilegedRole(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedPrivilegedRole(t, ms, "k8s-live")
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "service_account", "spawn_role": "crew", "execution_script": "kubectl apply -f x"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("service_account with a non-privileged role: %d, want 400", rec.Code)
	}
	seedEscalation(t, ms, "gd-sa1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "service_account", "spawn_role": "k8s-live", "execution_script": "kubectl apply -f x"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-sa1/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	f := escFields(t, ms, "gd-sa1")
	if f["executor"] != "escalation-sa-gd-sa1" {
		t.Fatalf("executor = %v", f["executor"])
	}
	agent, err := ms.FindAgentByName(context.Background(), "escalation-sa-gd-sa1", nil)
	if err != nil || agent == nil {
		t.Fatalf("no executor agent bead: %v", err)
	}
	af := escalationStringFields(agent)
	if af["role"] != "k8s-live" || af["spawned_by"] != "gd-sa1" || af["spawn_approved_by"] != "owner@example.com" {
		t.Fatalf("agent fields: %v", af)
	}
}
```

`seedPrivilegedRole` seeds whatever `roleIsPrivileged` reads; write it in this file after reading `kbeads/internal/server/spawn_authz.go:176-196` (it looks up a role definition and its `privileged` flag; copy the fixture from the existing `roleIsPrivileged` tests, `grep -ln roleIsPrivileged kbeads/internal/server/*_test.go`).

Run: `cd kbeads && go test ./internal/server -run 'TestEscalationTargets_|TestApprove_LocalStampsRunnerAsExecutor|TestApprove_ServiceAccountSpawnsPrivilegedRole' -count=1`
Expected: FAIL.

- [ ] **Step 2: Create-time target checks**

Create `kbeads/internal/server/escalation_targets.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// checkEscalationTargets enforces spec §5.3 at filing. Skipped for machine
// writes (the daemon's own creates).
func (s *BeadsServer) checkEscalationTargets(ctx context.Context, bead *model.Bead) error {
	if bead.Type != model.TypeEscalation || isMachineWrite(ctx) {
		return nil
	}
	f := escalationStringFields(bead)
	switch f["execution_target"] {
	case "local":
		runner := f["target_runner"]
		if runner == "" {
			return inputError("execution_target=local requires target_runner: the bead id of the local agent that will execute it")
		}
		rb, err := s.store.GetBead(ctx, runner)
		if err != nil || rb == nil || rb.Type != "agent" || (rb.Status != model.StatusOpen && rb.Status != model.StatusInProgress) ||
			!slices.Contains(rb.Labels, "execution_target:local") {
			return inputError(fmt.Sprintf("target_runner %q is not an open local agent bead (type=agent, label execution_target:local)", runner))
		}
	case "service_account":
		role := f["spawn_role"]
		if role == "" || strings.TrimSpace(f["execution_script"]) == "" {
			return inputError("execution_target=service_account requires spawn_role (a privileged role) and a reviewed execution_script")
		}
		ok, err := roleIsPrivileged(ctx, s.store, role)
		if err != nil {
			return fmt.Errorf("check spawn_role %q: %w", role, err)
		}
		if !ok {
			return inputError(fmt.Sprintf("spawn_role %q is not a privileged role; service_account execution needs one", role))
		}
	}
	return nil
}

// startEscalationExecutor runs after the dispatch claim (Task 12).
func (s *BeadsServer) startEscalationExecutor(ctx context.Context, bead *model.Bead, dispatch, approvedBy string) error {
	f := escalationStringFields(bead)
	switch dispatch {
	case dispatchLocal:
		rb, err := s.store.GetBead(ctx, f["target_runner"])
		if err != nil || rb == nil {
			return fmt.Errorf("target_runner %q vanished after filing", f["target_runner"])
		}
		name := escalationStringFields(rb)["agent"]
		if name == "" {
			name = rb.Title
		}
		return s.mergeBeadFields(ctx, bead.ID, map[string]any{"executor": name})
	case dispatchServiceAccount:
		name := "escalation-sa-" + bead.ID
		project := ""
		for _, l := range bead.Labels {
			if p, ok := strings.CutPrefix(l, "project:"); ok {
				project = p
				break
			}
		}
		prompt := fmt.Sprintf("You are the service-account executor for approved escalation %s (operation %s).\n"+
			"Run exactly this reviewed execution_script, nothing else:\n\n%s\n\n"+
			"Then report: gb escalation close %s --status success|failure|partial --result '<what happened>'",
			bead.ID, f["operation_type"], f["execution_script"], bead.ID)
		raw, _ := json.Marshal(map[string]string{
			"agent": name, "mode": "crew", "role": f["spawn_role"], "project": project,
			"prompt": prompt, "escalation_id": bead.ID, "execution_target": "service_account",
		})
		replayCtx := withApprovedSpawnReplay(withMachineWrite(WithMachineOperator(ctx)), bead.ID, approvedBy)
		if _, err := s.createBead(replayCtx, createBeadInput{
			Title: name, Type: "agent", Kind: string(model.KindData), Labels: []string{"project:" + project},
			Fields: stampSpawnProvenance(raw, bead.ID, approvedBy),
		}); err != nil {
			return fmt.Errorf("spawn service-account executor: %w", err)
		}
		return s.mergeBeadFields(ctx, bead.ID, map[string]any{"executor": name})
	}
	return nil
}
```

Check the agent-create field set against what the controller needs to start a pod (`gardenio/controller/internal/beadsapi/client.go:1222-1231`, `SpawnAgentResult`: `agent`, `mode`, `role`, `project`, `prompt`); keep them aligned.

In `beads.go`, next to Task 11's create check (before `applyEscalationRequestingAgentGuard`, `kbeads/internal/server/beads.go:390`), add:

```go
		if err := s.checkEscalationTargets(ctx, bead); err != nil {
			return nil, err
		}
```

In the approve handler, right after Task 12's `if !proceed { ... }` block, add:

```go
	if err := s.startEscalationExecutor(r.Context(), bead, dispatch, approvedBy); err != nil {
		slog.Error("escalation executor did not start", "bead", id, "dispatch", dispatch, "error", err)
		writeError(w, http.StatusBadGateway, "approved, but the executor did not start: "+err.Error())
		return
	}
```

Add `{"name": "target_runner", "type": "string"},` after `execution_target` in the `type:escalation` config.

Run: `cd kbeads && go test ./internal/server -run 'TestEscalationTargets_|TestApprove_' -count=1`
Expected: PASS.

- [ ] **Step 3: The watcher acts only for its own runner and stops writing executor**

In `gardenio/controller/cmd/gb/agent_start_escalation.go`, replace the target filter at `:250-253`:

```go
	// Check execution target is local.
	if bead.fieldString("execution_target") != "local" {
		return
	}
```

with:

```go
	// P7 Task 13 (spec Q2): act only on escalations that name THIS agent's
	// bead as their runner. The project label no longer decides.
	if bead.fieldString("execution_target") != "local" || bead.fieldString("target_runner") != w.agentBead {
		return
	}
```

Delete the executor PATCH at `gardenio/controller/cmd/gb/agent_start_escalation.go:319-325` (the daemon stamped it at approval). Add watcher tests: an event naming another runner is ignored; an event naming `w.agentBead` is processed; `executeEscalation` makes no `UpdateBeadFields` call (assert on the fake daemon). The watcher still posts its result with `CaptureEscalationResult`; that succeeds only when the watcher's credential resolves to the agent the daemon stamped as executor (a per-agent token) or to an operator (`kd login`, PR #249). Say so in the `gb agent start --escalation` help text.

Run: `cd gardenio/controller && go test ./cmd/gb -count=1`
Expected: PASS.

- [ ] **Step 4: Format, vet, commit**

```bash
cd kbeads && gofumpt -l internal/server && go vet ./internal/server && staticcheck ./internal/server && go test ./internal/server -count=1 && \
cd ../gardenio/controller && gofumpt -l cmd/gb && go vet ./cmd/gb && go test ./cmd/gb -count=1 && cd ../.. && \
git add kbeads/internal/server gardenio/controller/cmd/gb && \
git commit -m "fix(escalations): local escalations name their runner; service_account runs a daemon-spawned privileged role (gd-46OpRA7jHg)"
```

### Task 14: Four-eyes on reject and result; policy sets requires_four_eyes; replay-unfit types cannot name an executor; verdicts never cascade

**Step bead:** gd-D2ygHeriDy. Spec §0 D11, §5.4, owner questions Q6 and Q10, doc 11 defects 7, 8. Blocked by Task 13 (its tests reuse Task 13's `seedLocalAgent` and `escCreate`, and the target checks run in the same create path).

**Release rule (OPERATOR):** do not deploy a release that contains Task 9 until Task 13 has merged (in between, local and service_account escalations have no executor). State this in the PR description.

**Files:**
- Create: `kbeads/internal/server/escalation_policy.go`, `kbeads/internal/server/escalation_policy_test.go`
- Modify: `kbeads/internal/server/beads.go:390` (create: apply policy next to Tasks 11 and 13's checks)
- Modify: `kbeads/internal/server/http_escalations.go` (reject and result: four-eyes)
- Modify: `kbeads/internal/server/auto_close.go:614` (never-cascade: add verdict types)

**Interfaces:**
- Consumes: `escalationRequiresFourEyes`, `escalationFourEyesViolation` (`kbeads/internal/server/escalation_approval_authz.go:85`, `:152`), `isReplayUnfitOperation`, `isVerdictOperation`, `isServerReplayOperation` (Task 10), `setFieldValue` (`kbeads/internal/server/scrub_request.go:354`).
- Produces: `func applyEscalationCreatePolicy(bead *model.Bead) error`.

- [ ] **Step 1: Write the failing tests**

Create `kbeads/internal/server/escalation_policy_test.go`:

```go
package server

import (
	"net/http"
	"testing"
)

// Sensitive and secret escalations get requires_four_eyes from policy.
func TestEscalationPolicy_SensitiveGetsFourEyes(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{
		"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}, "sensitivity": "secret"}), nil)
	requireStatus(t, rec, http.StatusCreated)
	var b map[string]any
	decodeJSON(t, rec, &b)
	if f := escFields(t, ms, b["id"].(string)); f["requires_four_eyes"] != true {
		t.Fatalf("requires_four_eyes = %v, want true", f["requires_four_eyes"])
	}
	rec = doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{
		"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}, "sensitivity": "internal"}), nil)
	requireStatus(t, rec, http.StatusCreated)
	decodeJSON(t, rec, &b)
	if f := escFields(t, ms, b["id"].(string)); f["requires_four_eyes"] == true {
		t.Fatal("positive control: internal sensitivity must not force four-eyes")
	}
}

// Replay-unfit types may be filed only as a manual request (no executor).
func TestEscalationPolicy_ReplayUnfitCannotNameExecutor(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedLocalAgent(t, ms, "gd-la3", "laptop")
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{
		"operation_type": "terraform_apply", "operation_spec": map[string]any{"dir": "x"}, "execution_target": "local", "target_runner": "gd-la3"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("terraform_apply with an executor: %d, want 400", rec.Code)
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{
		"operation_type": "terraform_apply", "operation_spec": map[string]any{"dir": "x"}}), nil); rec.Code != http.StatusCreated {
		t.Fatalf("positive control: terraform_apply as a manual request: %d", rec.Code)
	}
}

// Four-eyes applies to reject and result, not only approve.
func TestFourEyes_RejectAndResult(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	for _, verb := range []string{"reject", "result"} {
		id := "gd-4e-" + verb
		seedEscalation(t, ms, id, "owner@example.com", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}, "requires_four_eyes": true})
		body := map[string]any{"reason": "no", "result_status": "success"}
		if rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/"+verb, "owner@example.com", body, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s by the requester: %d, want 403", verb, rec.Code)
		}
		if rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/"+verb, "second@example.com", body, nil); rec.Code != http.StatusOK {
			t.Errorf("positive control: %s by a second operator: %d, want 200", verb, rec.Code)
		}
	}
}
```

Add one never-cascade test next to the existing `github_pr_merge` cascade test (`grep -n 'neverCascade\|github_pr_merge' kbeads/internal/server/auto_close_test.go`), parameterised over `gitlab_mr_merge`, `github_pr_review`, `doc_review`: a success result does not close the requesting task.

Run: `cd kbeads && go test ./internal/server -run 'TestEscalationPolicy_|TestFourEyes_|NeverCascade' -count=1`
Expected: FAIL.

- [ ] **Step 2: Implement**

Create `kbeads/internal/server/escalation_policy.go`:

```go
package server

import (
	"fmt"

	"gardenio/kbeads/internal/model"
)

// applyEscalationCreatePolicy (spec D11): sensitive/secret escalations need a
// second human; replay-unfit operation types may not name an executor.
func applyEscalationCreatePolicy(bead *model.Bead) error {
	if bead.Type != model.TypeEscalation {
		return nil
	}
	f := escalationStringFields(bead)
	op := f["operation_type"]
	if isReplayUnfitOperation(op) && (f["execution_target"] != "" || isServerReplayOperation(op)) {
		return inputError(fmt.Sprintf("operation_type %q may not name an executor: file it without execution_target and a human runs it outside gardenio (decision gd-UQTnDZATTU)", op))
	}
	if s := f["sensitivity"]; s == "sensitive" || s == "secret" {
		bead.Fields = setFieldValue(bead.Fields, "requires_four_eyes", true)
	}
	return nil
}
```

Wire it in `beads.go` next to the Task 11 and Task 13 create checks (apply the policy after the field check, before the target check). In `handleRejectEscalation`, after `rejectedBy` is derived (Task 10), add:

```go
	if target, _ := s.store.GetBead(r.Context(), id); escalationRequiresFourEyes(target) && s.escalationFourEyesViolation(r.Context(), target, rejectedBy) {
		writeError(w, http.StatusForbidden, "four-eyes required: the rejecter must differ from the requester")
		return
	}
```

(If Task 10 kept the bead returned by `authorizeResolutionTarget`, use it instead of re-fetching.) In `handleCaptureEscalationResult`, after `resolver` is derived and only when the caller class is `classOperator`:

```go
	if s.callerClassFromContext(r.Context()) == classOperator && escalationRequiresFourEyes(target) &&
		s.escalationFourEyesViolation(r.Context(), target, resolver) {
		writeError(w, http.StatusForbidden, "four-eyes required: the resolver must differ from the requester")
		return
	}
```

In `kbeads/internal/server/auto_close.go:614` replace `if neverCascadeCloseOperationTypes[strings.ToLower(strings.TrimSpace(escFields["operation_type"]))] {` with `if op := escFields["operation_type"]; neverCascadeCloseOperationTypes[strings.ToLower(strings.TrimSpace(op))] || isVerdictOperation(op) {` and extend the comment above the map (`kbeads/internal/server/auto_close.go:123-132`) to say every verdict type is covered (P7 Task 14).

Run: `cd kbeads && go test ./internal/server -run 'TestEscalationPolicy_|TestFourEyes_|NeverCascade|AutoClose' -count=1`
Expected: PASS.

- [ ] **Step 3: Format, vet, commit**

```bash
cd kbeads && gofumpt -l internal/server && go vet ./internal/server && staticcheck ./internal/server && go test ./internal/server -count=1 && cd .. && \
git add kbeads/internal/server && \
git commit -m "fix(kbeads): four-eyes on reject/result, policy-set for sensitive escalations; replay-unfit types cannot name an executor; verdicts never cascade (gd-D2ygHeriDy)"
```

### Task 15: Approve, result, and reject write mutation history; executable escalations need a real operation_spec; approval_method is derived

**Step bead:** gd-kEce0G9n1a. Spec §0 D12, doc 11 defects 9, 10, 11. Blocked by Task 13 (it uses Task 12's `escalationDispatchFor` and `fieldRaw` and Task 13's test helpers).

**Release rule (OPERATOR):** do not deploy a release that contains Task 9 until Task 13 has merged (in between, local and service_account escalations have no executor). State this in the PR description.

**Files:**
- Create: `kbeads/internal/server/escalation_history.go`, `kbeads/internal/server/escalation_history_test.go`
- Modify: `kbeads/internal/server/http_escalations.go:126-135` (body `approval_method` validation), `:205-215` (approve merge), reject and result merges
- Modify: `kbeads/internal/server/spawn_policy.go:515-525` (`autoApproveSpawnEscalation` merge)
- Modify: `kbeads/internal/server/beads.go:390` (create: operation_spec check next to the other create checks)
- Modify: `kbeads/internal/server/config.go:1600` (`approval_method` enum gains `"edge"`)
- Modify: `gardenio/controller/cmd/gb/escalation_create.go:62-70` (help text only; see Step 4)

**Interfaces:**
- Consumes: `recordMutationNoDibs` (`kbeads/internal/server/mutation_history.go:55`), `escalationDispatchFor`, `fieldRaw` (Task 12).
- Produces:
  - `func (s *BeadsServer) mergeEscalationFieldsWithHistory(ctx context.Context, prior *model.Bead, actor string, extra map[string]any) error`
  - `func approvalMethodFor(ctx context.Context) string`
  - `func checkEscalationOperationSpec(bead *model.Bead) error`

- [ ] **Step 1: Write the failing tests**

Create `kbeads/internal/server/escalation_history_test.go`:

```go
package server

import (
	"net/http"
	"testing"
)

// Each merged field gets a mutation_history row with the old and new value.
func TestApprove_RecordsMutationHistory(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-h1", "", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}})
	before := len(ms.mutations)
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-h1/approve", "owner@example.com", map[string]any{"approval_method": "cli"}, nil), http.StatusOK)
	got := map[string]string{}
	for _, m := range ms.mutations[before:] {
		if m.BeadID == "gd-h1" {
			got[m.Field] = m.NewValue
		}
	}
	for _, k := range []string{"fields.approved_by", "fields.approved_at", "fields.approval_method", "fields.execution_dispatch"} {
		if _, ok := got[k]; !ok {
			t.Errorf("no mutation row for %s (rows: %v)", k, got)
		}
	}
	if got["fields.approval_method"] != `"web_ui"` {
		t.Errorf("approval_method row = %s, want derived \"web_ui\" (body said cli)", got["fields.approval_method"])
	}
}

func TestResultAndReject_RecordMutationHistory(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-h2", "", map[string]any{"operation_type": "github_pr_merge"})
	seedEscalation(t, ms, "gd-h3", "", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}})
	before := len(ms.mutations)
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-h2/result", "owner@example.com", map[string]any{"result_status": "success"}, nil), http.StatusOK)
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-h3/reject", "owner@example.com", map[string]any{"reason": "no"}, nil), http.StatusOK)
	seen := map[string]bool{}
	for _, m := range ms.mutations[before:] {
		seen[m.BeadID+" "+m.Field] = true
	}
	for _, k := range []string{"gd-h2 fields.result_status", "gd-h2 fields.executor", "gd-h3 fields.rejected_by", "gd-h3 fields.result_status"} {
		if !seen[k] {
			t.Errorf("missing mutation row %q", k)
		}
	}
}

// An escalation the daemon would dispatch needs a non-empty operation_spec;
// a verdict record or a manual request does not (gb sends {} by default).
func TestEscalationCreate_OperationSpecMustBeRealWhenExecutable(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedLocalAgent(t, ms, "gd-la4", "laptop")
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{
		"operation_type": "k8s_apply", "operation_spec": map[string]any{}, "execution_target": "local", "target_runner": "gd-la4"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("executable with {} spec: %d, want 400", rec.Code)
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{
		"operation_type": "update", "operation_spec": map[string]any{}, "target_bead_id": "gd-x"}), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("server replay with {} spec: %d, want 400", rec.Code)
	}
	for _, op := range []string{"github_pr_merge", "manual_operator_action"} {
		if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{"operation_type": op, "operation_spec": map[string]any{}}), nil); rec.Code != http.StatusCreated {
			t.Fatalf("positive control: %s with {} spec: %d, want 201", op, rec.Code)
		}
	}
}
```

If the mutation rows are skipped because the mock cannot resolve the operator (`recordMutationNoDibs` logs "operator unresolvable"), seed the operator the way the existing mutation-history tests do (`grep -ln 'ms.mutations' kbeads/internal/server/*_test.go`).

Run: `cd kbeads && go test ./internal/server -run 'TestApprove_RecordsMutationHistory|TestResultAndReject_RecordMutationHistory|TestEscalationCreate_OperationSpec' -count=1`
Expected: FAIL.

- [ ] **Step 2: History-writing merge and derived approval_method**

Create `kbeads/internal/server/escalation_history.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"sort"

	"gardenio/kbeads/internal/model"
)

// mergeEscalationFieldsWithHistory merges extra and writes one
// mutation_history row per key (field "fields.<key>", values JSON-encoded).
// No dibs flip: approving or resolving is not taking ownership.
func (s *BeadsServer) mergeEscalationFieldsWithHistory(ctx context.Context, prior *model.Bead, actor string, extra map[string]any) error {
	if err := s.mergeBeadFields(ctx, prior.ID, extra); err != nil {
		return err
	}
	old := map[string]json.RawMessage{}
	if len(prior.Fields) > 0 {
		_ = json.Unmarshal(prior.Fields, &old)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		nv, _ := json.Marshal(extra[k])
		s.recordMutationNoDibs(ctx, prior.ID, actor, "fields."+k, string(old[k]), string(nv), model.MutationTypeUpdate)
	}
	return nil
}

// approvalMethodFor derives approval_method from the authenticated identity
// (spec D12). The request body's value is logged and ignored.
func approvalMethodFor(ctx context.Context) string {
	if AuthIdentityFromContext(ctx).Type == "edge" {
		return "edge"
	}
	return "web_ui"
}

// checkEscalationOperationSpec: an escalation the daemon would dispatch must
// say what to do.
func checkEscalationOperationSpec(bead *model.Bead) error {
	if bead.Type != model.TypeEscalation {
		return nil
	}
	f := escalationStringFields(bead)
	spec := fieldRaw(bead, "operation_spec")
	f["operation_spec"] = spec
	if d := escalationDispatchFor(f); (d == dispatchNone && !isServerReplayOperation(f["operation_type"])) || spec != "" {
		return nil
	}
	return inputError("operation_spec must be a non-empty object for an escalation that names an executor or is replayed by the daemon")
}
```

- [ ] **Step 3: Use them**

- Approve: delete the body `approval_method` validation block (`kbeads/internal/server/http_escalations.go:126-135`) and replace `:209-215`:

```go
	if req.ApprovalMethod != "" {
		extra["approval_method"] = req.ApprovalMethod
	}

	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		slog.Warn("failed to merge escalation approval fields", "bead", id, "error", err)
	}
```

with:

```go
	extra["approval_method"] = approvalMethodFor(r.Context())
	if req.ApprovalMethod != "" && req.ApprovalMethod != extra["approval_method"] {
		slog.Info("handleApproveEscalation: ignoring body approval_method", "bead", id, "body", req.ApprovalMethod, "derived", extra["approval_method"])
	}

	if err := s.mergeEscalationFieldsWithHistory(r.Context(), targetBead, approvedBy, extra); err != nil {
		slog.Warn("failed to merge escalation approval fields", "bead", id, "error", err)
	}
```

- `ClaimEscalationExecution` writes `execution_dispatch` outside `mergeBeadFields`; after a successful claim in `claimEscalationDispatch` (Task 12) record it: `s.recordMutationNoDibs(ctx, bead.ID, actorFromContext(ctx), "fields.execution_dispatch", "", `"`+want+`"`, model.MutationTypeUpdate)`.
- Reject and result: replace their `s.mergeBeadFields(r.Context(), id, extra)` calls with `s.mergeEscalationFieldsWithHistory(r.Context(), target, rejectedBy /* or resolver */, extra)`, using the bead Task 10 fetched through `authorizeResolutionTarget` (reject: change `if _, ok := s.authorizeResolutionTarget(...)` to keep the bead).
- `autoApproveSpawnEscalation` (`kbeads/internal/server/spawn_policy.go:522`): `s.mergeEscalationFieldsWithHistory(ctx, bead, spawnPolicyActor, extra)`; its `approval_method` stays `"policy"`.
- Create: in `beads.go` with the other create checks add `if err := checkEscalationOperationSpec(bead); err != nil { return nil, err }`.
- Config: `approval_method` enum becomes `["web_ui", "api_token", "cli", "policy", "edge"]`.

Run: `cd kbeads && go test ./internal/server -count=1`
Expected: PASS. Fix older tests that asserted `approval_method` equals the body value: they now expect `web_ui` (OIDC bearer) with a comment citing P7 Task 15.

- [ ] **Step 4: Client help text**

In `gardenio/controller/cmd/gb/escalation_create.go`, the `--operation-spec` flag help gains: "Required (a non-empty JSON object) when the escalation names an execution_target or is a create/update/close/grant replay; optional for verdict records and manual requests." Leave the `{}` default (`:68-70`): `{}` is still valid for the non-executable cases. In `kbeads/cmd/kd/escalation.go` and `gardenio/controller/cmd/gb/escalation_lifecycle.go`, keep sending `approval_method` (older daemons read it) and add a comment that the daemon derives it.

- [ ] **Step 5: Format, vet, commit**

```bash
cd kbeads && gofumpt -l internal/server && go vet ./internal/server && staticcheck ./internal/server && go test ./internal/server -count=1 && \
cd ../gardenio/controller && go test ./cmd/gb -count=1 && cd ../.. && \
git add kbeads/internal/server gardenio/controller/cmd/gb/escalation_create.go gardenio/controller/cmd/gb/escalation_lifecycle.go kbeads/cmd/kd/escalation.go && \
git commit -m "fix(kbeads): escalation verbs write mutation history; executable escalations need a real operation_spec; approval_method derived (gd-kEce0G9n1a)"
```

---
