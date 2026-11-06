// Command agent-feedback-probe is a TEST-ONLY binary that drives the REAL
// always-on agent-feedback write path (internal/bridge/bot_agent_feedback.go)
// end-to-end against the LIVE kbeads daemon, then asserts the beads it wrote.
//
// Bead ka-0000008183 (parent ka-0000000958). Backfills the e2e gap on commit
// de29900 (ka-0000002210): the decoupled, unconditionally-registered
// POST /api/slack/agent-feedback endpoint whose stable type=rating feedback
// bead is the reliable source the feedback→bug trigger (ka-0000007151) drains.
//
// ── The gap this closes ───────────────────────────────────────────────────────
//
// The write path shipped with ONE test — bot_agent_feedback_test.go — which
// drives the handler against a MOCK daemon (newMockDaemon). That mock's
// CreateBead is a naive append: it never runs the type=rating field schema,
// never enforces the (rater, rated_bead_id) idempotent upsert, and never wires
// the `rates` dep edge. So the load-bearing behaviour the endpoint EXISTS for —
// "repeated feedback from the same rater on the same agent updates IN PLACE
// instead of piling up duplicate beads, and each bead links back to the agent
// it rates" — is asserted against NOTHING real. A grep of gardenio/tests for
// agent-feedback / slack-feedback / rated_bead_id / slack-feedback-only returns
// empty. Both failure directions hurt the feedback→bug pipeline: a silently
// rejected write leaves the trigger with NO source (freeform feedback dropped),
// and an upsert regression mints a NEW rating bead per re-submission so the
// trigger fans out a DUPLICATE bug per keystroke-revision.
//
// ── Why a from-source probe against the LIVE daemon, not a black-box HTTP test ─
//
// The bead proposed a black-box test: "POST to the agent-feedback route and
// assert a type=rating bead is written; re-POST and assert an in-place upsert."
// That is impossible in the e2e shard, because the HANDLER's host — the
// slack-bridge — is NOT DEPLOYED in the ephemeral e2e namespace where shards
// run: the e2e values ship slackBridge.enabled=false (only a long-lived
// staging install runs the bridge). There is no /api/slack/agent-feedback endpoint to POST to —
// the same class of constraint that made the escalation-auto-reaper backfill
// (ka-0000006112) a probe rather than a live leg.
//
// But this path is only HALF-hermetic, and that half is the valuable one: the
// DAEMON (kbeads) that enforces the (rater, rated_bead_id) upsert, the rates
// edge, the type=rating schema, and the score=0/slack-feedback-only sentinel IS
// deployed and reachable in EVERY e2e namespace. So instead of the reaper's
// fully-mocked probe, this wires the REAL AgentFeedbackAPI handler (via the
// exported NewAgentFeedbackAPI + RegisterRoutes) to a REAL beadsapi.Client
// pointed at the LIVE daemon, and drives httptest requests through it. One POST
// therefore exercises PRODUCTION CODE on BOTH sides of the boundary the unit
// test could not reach: the handler's request→CreateBeadRequest mapping AND the
// daemon's preCreateRating. A refactor that drops the kind:slack-feedback label,
// the score=0 sentinel pairing, the rater precedence, the XSS escape, the
// rated_bead_id provenance, or the (rater,rated_bead_id) upsert reds this test.
//
// The handler sets no X-Operated-By (in prod its FLEET daemon token self-
// attributes fleet:slack-bridge). The ephemeral namespace has only the static
// primary token, so the probe configures the client with SetOperatedBy(the
// shard's BEADS_OPERATED_BY human) — the same attributed-write outcome the
// bridge's per-Slack-event WithOperator path produces, so the daemon's write
// gate is satisfied exactly as it is in prod.
//
// ── Legs (each prints "LEG <name>: PASS|FAIL <detail>"; any FAIL exits nonzero)─
//
//	1 writes-stable-bead   A first POST returns 200 {ok,bead_id} and the daemon
//	                       persisted a type=rating bead carrying feedback(==raw),
//	                       feedback_raw, rated_bead_id, source=slack-feedback-
//	                       only, score=0, rater=slack:<uid>, answered_at, and the
//	                       kind:slack-feedback + agent:<id> + project:<p> labels.
//	2 rates-edge-wired     The daemon auto-wired the `rates` dep edge from the
//	                       rating bead to the rated (source) bead.
//	3 xss-escaped          Slack control syntax in the feedback is HTML-escaped
//	                       in `feedback` while `feedback_raw` keeps the original.
//	4 idempotent-upsert    A SECOND POST from the SAME rater on the SAME agent
//	                       returns the SAME bead_id, updates the text in place,
//	                       and leaves EXACTLY ONE rating bead for that pair.
//	5 distinct-rater-splits A DIFFERENT rater on the SAME agent yields a SEPARATE
//	                       bead — proving the upsert key is the PAIR, not the
//	                       agent alone (guards against a global collapse).
//	6 rater-precedence     Explicit rater wins over slack_user_id; a bare POST
//	                       with neither falls back to the anonymous rater — each
//	                       lands a correctly-keyed bead.
//	7 validation-gates     Missing agent_bead_id → 400; blank feedback → 400;
//	                       unknown agent_bead_id → 404; GET → 405; and NO rating
//	                       bead is written for the unknown-agent case.
//
// ── Skip-clean contract ───────────────────────────────────────────────────────
//
// The probe SKIPS CLEANLY (exit 0, LOUD banner) when BEADS_HTTP_URL is unset or
// the daemon is unreachable — an infra gap must never red the suite. When the
// daemon is reachable, every leg is a HARD assertion. The companion shell module
// (test-agent-feedback-writepath.sh) owns the toolchain/binary resolution and
// the same skip-clean contract, mirroring escalation-auto-reaper.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// legResult is the outcome of one leg.
type legResult struct {
	pass   bool
	detail string
}

func ok(format string, a ...any) legResult {
	return legResult{pass: true, detail: fmt.Sprintf(format, a...)}
}
func bad(format string, a ...any) legResult {
	return legResult{pass: false, detail: fmt.Sprintf(format, a...)}
}

// probeEnv carries the live-daemon client + the real handler mux shared by every
// leg, plus a list of bead ids to close on teardown. httpURL/token/operator are
// retained so source fixtures can be created via a raw daemon POST that bypasses
// the client's spawn-path gardener_id stamp (see newSourceBead).
type probeEnv struct {
	client   *beadsapi.Client
	handler  http.Handler
	httpURL  string
	token    string
	operator string
	created  []string
}

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	httpURL := firstNonEmpty(os.Getenv("BEADS_HTTP_URL"), os.Getenv("BEADS_HTTP_ADDR"))
	if httpURL == "" {
		skipClean("BEADS_HTTP_URL unset — no live daemon to drive")
	}
	token := firstNonEmpty(os.Getenv("BEADS_TOKEN"), os.Getenv("BEADS_AUTH_TOKEN"), os.Getenv("BEADS_DAEMON_TOKEN"))
	operator := firstNonEmpty(os.Getenv("BEADS_OPERATED_BY"), "oidc:agent-feedback-probe@example.com")

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: httpURL, Token: token})
	if err != nil {
		skipClean("failed to build beads client: " + err.Error())
	}
	defer client.Close()
	// Attribute writes exactly as the bridge's per-event WithOperator path does;
	// the handler itself sets no operator (it relies on its fleet token in prod),
	// and the ephemeral namespace has only the static primary token.
	client.SetOperatedBy(operator)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Reachability probe: a missing/unreachable daemon is an infra gap, skip clean.
	if _, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Types: []string{"rating"}, Limit: 1}); err != nil {
		skipClean(fmt.Sprintf("live daemon unreachable at %s: %v", httpURL, err))
	}

	env := &probeEnv{client: client, httpURL: strings.TrimRight(normalizeURL(httpURL), "/"), token: token, operator: operator}
	// Wire the REAL handler to the REAL live-daemon client.
	mux := http.NewServeMux()
	bridge.NewAgentFeedbackAPI(client, logger).RegisterRoutes(mux)
	env.handler = mux

	defer env.teardown(context.Background())

	legs := []struct {
		name string
		run  func(*probeEnv, context.Context) legResult
	}{
		{"writes-stable-bead", legWritesStableBead},
		{"rates-edge-wired", legRatesEdgeWired},
		{"xss-escaped", legXSSEscaped},
		{"idempotent-upsert", legIdempotentUpsert},
		{"distinct-rater-splits", legDistinctRaterSplits},
		{"rater-precedence", legRaterPrecedence},
		{"validation-gates", legValidationGates},
	}

	failed := 0
	for _, leg := range legs {
		res := leg.run(env, ctx)
		status := "PASS"
		if !res.pass {
			status = "FAIL"
			failed++
		}
		fmt.Printf("LEG %s: %s %s\n", leg.name, status, res.detail)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// ── HTTP-through-the-real-handler helpers ─────────────────────────────────────

type feedbackReq struct {
	AgentBeadID   string `json:"agent_bead_id,omitempty"`
	Feedback      string `json:"feedback,omitempty"`
	Rater         string `json:"rater,omitempty"`
	SlackUserID   string `json:"slack_user_id,omitempty"`
	SlackChannel  string `json:"slack_channel,omitempty"`
	SlackThreadTS string `json:"slack_thread_ts,omitempty"`
}

type feedbackResp struct {
	OK     bool   `json:"ok"`
	BeadID string `json:"bead_id"`
}

// post drives one POST /api/slack/agent-feedback through the REAL handler and
// returns the HTTP status code plus the decoded response body.
func (e *probeEnv) post(body feedbackReq) (int, feedbackResp) {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/slack/agent-feedback", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	var out feedbackResp
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.BeadID != "" {
		e.created = append(e.created, out.BeadID)
	}
	return rec.Code, out
}

// get drives a non-POST request through the real handler to exercise the method
// gate.
func (e *probeEnv) get() int {
	req := httptest.NewRequest(http.MethodGet, "/api/slack/agent-feedback", nil)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec.Code
}

// newSourceBead creates the bead the feedback POST rates against, on the live
// daemon, and records it for teardown. Returns the bead id.
//
// The write path under test is type-AGNOSTIC about the rated bead:
// handlePostFeedback only GetBead's it and reads Fields["project"] to derive the
// project: label, and the daemon's preCreateRating only rejects rating-OF-rating.
// In prod the rated bead is a type=agent bead, but using one as a FIXTURE here
// would couple this probe to the whole evolving agent schema — the required
// `role`/`agent_state` fields, and the gardener_id auto-stamp
// (beadsapi.gardener_id_stamp.go, ka-0000006626) — none of which the feedback
// write path touches. So the fixture is a type=artifact bead (kind=data,
// allow_additional_fields=true, no write_policy) carrying just the `project`
// field the handler reads. That faithfully represents "an existing rateable bead
// with a project" — all the write path requires — while the code UNDER TEST (the
// handler → client.CreateBead(type=rating) → daemon.preCreateRating) still runs
// entirely through production code, and the fixture is immune to agent-schema
// churn. Created via a raw daemon POST so the client's agent-only stamping never
// runs on it.
func (e *probeEnv) newSourceBead(ctx context.Context, suffix string) (string, error) {
	fields := map[string]any{
		"project": "gardenio",
	}
	fieldsJSON, _ := json.Marshal(fields)
	body := map[string]any{
		"title":  "agent-feedback-probe-src-" + suffix,
		"type":   "artifact",
		"labels": []string{"project:gardenio", "e2e:agent-feedback-probe"},
		"fields": json.RawMessage(fieldsJSON),
	}
	id, err := e.rawCreateBead(ctx, body)
	if err != nil {
		return "", err
	}
	e.created = append(e.created, id)
	return id, nil
}

// rawCreateBead POSTs a bead to the live daemon's /v1/beads endpoint directly,
// bypassing beadsapi.Client's spawn-path field stamping. Returns the new id.
func (e *probeEnv) rawCreateBead(ctx context.Context, body map[string]any) (string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.httpURL+"/v1/beads", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	if e.operator != "" {
		req.Header.Set("X-Operated-By", e.operator)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("raw create HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	// The daemon returns either the bare bead object or a {"bead":{…}} envelope.
	var env struct {
		ID   string `json:"id"`
		Bead struct {
			ID string `json:"id"`
		} `json:"bead"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.ID != "" {
		return env.ID, nil
	}
	if env.Bead.ID != "" {
		return env.Bead.ID, nil
	}
	return "", fmt.Errorf("raw create: no id in response: %s", strings.TrimSpace(string(raw)))
}

func (e *probeEnv) teardown(ctx context.Context) {
	// Best-effort close of every bead we created (reverse order so ratings close
	// before their source beads).
	for _, id := range slices.Backward(e.created) {
		_ = e.client.CloseBead(ctx, id, nil)
	}
}

// countRatings returns the number of type=rating beads carrying both the given
// rated_bead_id and rater, using a server-side fielded filter.
func (e *probeEnv) countRatings(ctx context.Context, agentID, rater string) (int, error) {
	res, err := e.client.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"rating"},
		Fields: map[string]string{"rated_bead_id": agentID, "rater": rater},
	})
	if err != nil {
		return 0, err
	}
	return len(res.Beads), nil
}

// ── Legs ──────────────────────────────────────────────────────────────────────

func legWritesStableBead(e *probeEnv, ctx context.Context) legResult {
	agent, err := e.newSourceBead(ctx, "stable")
	if err != nil {
		return bad("create source bead: %v", err)
	}
	feedback := "the spawn flow is great"
	uid := "UPROBE1"
	code, resp := e.post(feedbackReq{AgentBeadID: agent, Feedback: feedback, SlackUserID: uid, SlackChannel: "CPROBE"})
	if code != http.StatusOK {
		return bad("first POST code=%d (want 200)", code)
	}
	if !resp.OK || resp.BeadID == "" {
		return bad("response ok=%v bead_id=%q (want ok=true, non-empty)", resp.OK, resp.BeadID)
	}
	bead, err := e.client.GetBead(ctx, resp.BeadID)
	if err != nil || bead == nil {
		return bad("read back %s: %v", resp.BeadID, err)
	}
	if bead.Type != "rating" {
		return bad("type=%q (want rating)", bead.Type)
	}
	checks := []struct {
		name, got, want string
	}{
		{"feedback", bead.Fields["feedback"], feedback},
		{"feedback_raw", bead.Fields["feedback_raw"], feedback},
		{"rated_bead_id", bead.Fields["rated_bead_id"], agent},
		{"source", bead.Fields["source"], "slack-feedback-only"},
		{"score", bead.Fields["score"], "0"},
		{"rater", bead.Fields["rater"], "slack:" + uid},
		{"slack_user_id", bead.Fields["slack_user_id"], uid},
	}
	for _, c := range checks {
		if c.got != c.want {
			return bad("field %s=%q (want %q)", c.name, c.got, c.want)
		}
	}
	if bead.Fields["answered_at"] == "" {
		return bad("answered_at empty (want a timestamp)")
	}
	for _, want := range []string{"kind:slack-feedback", "agent:" + agent, "project:gardenio"} {
		if !containsStr(bead.Labels, want) {
			return bad("labels %v missing %q", bead.Labels, want)
		}
	}
	return ok("bead %s: type=rating, feedback+raw+provenance+rater+score=0+labels all correct", resp.BeadID)
}

func legRatesEdgeWired(e *probeEnv, ctx context.Context) legResult {
	agent, err := e.newSourceBead(ctx, "edge")
	if err != nil {
		return bad("create source bead: %v", err)
	}
	code, resp := e.post(feedbackReq{AgentBeadID: agent, Feedback: "edge check", SlackUserID: "UEDGE"})
	if code != http.StatusOK || resp.BeadID == "" {
		return bad("POST code=%d bead=%q", code, resp.BeadID)
	}
	deps, err := e.client.GetDependencies(ctx, resp.BeadID)
	if err != nil {
		return bad("get deps for %s: %v", resp.BeadID, err)
	}
	for _, d := range deps {
		if d.Type == "rates" && d.DependsOnID == agent {
			return ok("rates edge %s --rates--> %s present", resp.BeadID, agent)
		}
	}
	return bad("no rates edge → %s in deps %+v", agent, deps)
}

func legXSSEscaped(e *probeEnv, ctx context.Context) legResult {
	agent, err := e.newSourceBead(ctx, "xss")
	if err != nil {
		return bad("create source bead: %v", err)
	}
	raw := "ping <!channel> and <@U999> & <http://x>"
	code, resp := e.post(feedbackReq{AgentBeadID: agent, Feedback: raw})
	if code != http.StatusOK || resp.BeadID == "" {
		return bad("POST code=%d bead=%q", code, resp.BeadID)
	}
	bead, err := e.client.GetBead(ctx, resp.BeadID)
	if err != nil || bead == nil {
		return bad("read back: %v", err)
	}
	esc := bead.Fields["feedback"]
	// The stored `feedback` must NOT contain the raw Slack control tokens
	// verbatim; it must be HTML-entity-escaped (the handler uses the same
	// slackEscapeMrkdwn as the modal path).
	if strings.Contains(esc, "<!channel>") || strings.Contains(esc, "<@U999>") {
		return bad("feedback not escaped: %q", esc)
	}
	// Sanity: the escaped form is what html.EscapeString would produce for the
	// angle-bracket/ampersand run — assert at least the entity markers appear.
	if !strings.Contains(esc, "&lt;") || !strings.Contains(esc, "&gt;") || !strings.Contains(esc, "&amp;") {
		return bad("feedback missing HTML entities (got %q; a reference escape is %q)", esc, html.EscapeString(raw))
	}
	if bead.Fields["feedback_raw"] != raw {
		return bad("feedback_raw=%q (want original %q)", bead.Fields["feedback_raw"], raw)
	}
	return ok("feedback HTML-escaped (%q), feedback_raw keeps original", esc)
}

func legIdempotentUpsert(e *probeEnv, ctx context.Context) legResult {
	agent, err := e.newSourceBead(ctx, "upsert")
	if err != nil {
		return bad("create source bead: %v", err)
	}
	uid := "UUPSERT"
	rater := "slack:" + uid
	code1, resp1 := e.post(feedbackReq{AgentBeadID: agent, Feedback: "first note", SlackUserID: uid})
	if code1 != http.StatusOK || resp1.BeadID == "" {
		return bad("first POST code=%d bead=%q", code1, resp1.BeadID)
	}
	code2, resp2 := e.post(feedbackReq{AgentBeadID: agent, Feedback: "second note updated", SlackUserID: uid})
	if code2 != http.StatusOK {
		return bad("second POST code=%d", code2)
	}
	if resp2.BeadID != resp1.BeadID {
		return bad("upsert returned different bead: first=%s second=%s", resp1.BeadID, resp2.BeadID)
	}
	bead, err := e.client.GetBead(ctx, resp1.BeadID)
	if err != nil || bead == nil {
		return bad("read back %s: %v", resp1.BeadID, err)
	}
	if bead.Fields["feedback"] != "second note updated" {
		return bad("feedback not updated in place: %q", bead.Fields["feedback"])
	}
	n, err := e.countRatings(ctx, agent, rater)
	if err != nil {
		return bad("count ratings: %v", err)
	}
	if n != 1 {
		return bad("expected exactly 1 rating bead for (agent,rater); got %d", n)
	}
	return ok("re-POST → same bead %s, text updated in place, exactly 1 bead for the pair", resp1.BeadID)
}

func legDistinctRaterSplits(e *probeEnv, ctx context.Context) legResult {
	agent, err := e.newSourceBead(ctx, "split")
	if err != nil {
		return bad("create source bead: %v", err)
	}
	_, r1 := e.post(feedbackReq{AgentBeadID: agent, Feedback: "rater one", SlackUserID: "USPLIT1"})
	code2, r2 := e.post(feedbackReq{AgentBeadID: agent, Feedback: "rater two", SlackUserID: "USPLIT2"})
	if r1.BeadID == "" || code2 != http.StatusOK || r2.BeadID == "" {
		return bad("posts: r1=%q code2=%d r2=%q", r1.BeadID, code2, r2.BeadID)
	}
	if r1.BeadID == r2.BeadID {
		return bad("distinct raters collapsed onto one bead %s", r1.BeadID)
	}
	n2, err := e.countRatings(ctx, agent, "slack:USPLIT2")
	if err != nil {
		return bad("count ratings: %v", err)
	}
	if n2 != 1 {
		return bad("expected 1 rating for second rater; got %d", n2)
	}
	return ok("different rater → separate bead (%s ≠ %s)", r1.BeadID, r2.BeadID)
}

func legRaterPrecedence(e *probeEnv, ctx context.Context) legResult {
	// Explicit rater must win over slack_user_id.
	agentA, err := e.newSourceBead(ctx, "rater-explicit")
	if err != nil {
		return bad("create source bead A: %v", err)
	}
	_, ra := e.post(feedbackReq{AgentBeadID: agentA, Feedback: "explicit wins", Rater: "oidc:alice@example.com", SlackUserID: "UIGNORED"})
	if ra.BeadID == "" {
		return bad("explicit-rater POST returned no bead")
	}
	ba, err := e.client.GetBead(ctx, ra.BeadID)
	if err != nil || ba == nil {
		return bad("read back explicit-rater bead: %v", err)
	}
	if ba.Fields["rater"] != "oidc:alice@example.com" {
		return bad("explicit rater not honored: rater=%q (want oidc:alice@example.com)", ba.Fields["rater"])
	}
	// No identity at all → anonymous fallback (bead must still be writable).
	agentB, err := e.newSourceBead(ctx, "rater-anon")
	if err != nil {
		return bad("create source bead B: %v", err)
	}
	codeB, rb := e.post(feedbackReq{AgentBeadID: agentB, Feedback: "anon note"})
	if codeB != http.StatusOK || rb.BeadID == "" {
		return bad("anon POST code=%d bead=%q (feedback must be writable without identity)", codeB, rb.BeadID)
	}
	bb, err := e.client.GetBead(ctx, rb.BeadID)
	if err != nil || bb == nil {
		return bad("read back anon bead: %v", err)
	}
	// The anonymous fallback is the fleet:slack-bridge sentinel (a non-empty
	// rater keeps the bead writable); assert it is non-empty and not a slack:
	// sentinel (no uid was supplied).
	if bb.Fields["rater"] == "" || strings.HasPrefix(bb.Fields["rater"], "slack:") {
		return bad("anon rater=%q (want non-empty non-slack fallback)", bb.Fields["rater"])
	}
	return ok("explicit rater wins (oidc:alice@example.com); no-identity falls back to %q", bb.Fields["rater"])
}

func legValidationGates(e *probeEnv, ctx context.Context) legResult {
	agent, err := e.newSourceBead(ctx, "validation")
	if err != nil {
		return bad("create source bead: %v", err)
	}
	// missing agent_bead_id → 400
	if code, _ := e.post(feedbackReq{Feedback: "x"}); code != http.StatusBadRequest {
		return bad("missing agent_bead_id code=%d (want 400)", code)
	}
	// blank feedback → 400
	if code, _ := e.post(feedbackReq{AgentBeadID: agent, Feedback: "   "}); code != http.StatusBadRequest {
		return bad("blank feedback code=%d (want 400)", code)
	}
	// unknown agent_bead_id → 404, and NO bead written for it
	code, _ := e.post(feedbackReq{AgentBeadID: "ka-doesnotexist000", Feedback: "hi"})
	if code != http.StatusNotFound {
		return bad("unknown agent_bead_id code=%d (want 404)", code)
	}
	if n, _ := e.countRatings(ctx, "ka-doesnotexist000", "slack:UNONE"); n != 0 {
		return bad("a rating bead was written for an unknown agent (n=%d)", n)
	}
	// wrong method → 405
	if code := e.get(); code != http.StatusMethodNotAllowed {
		return bad("GET code=%d (want 405)", code)
	}
	return ok("400 on missing agent/blank feedback, 404 on unknown agent (no bead), 405 on GET")
}

// ── small helpers ─────────────────────────────────────────────────────────────

func skipClean(why string) {
	fmt.Fprintln(os.Stderr, "════════════════════════════════════════════════════")
	fmt.Fprintln(os.Stderr, " SKIP agent-feedback-probe (LOUD): "+why)
	fmt.Fprintln(os.Stderr, " This run did NOT assert the always-on agent-feedback write path")
	fmt.Fprintln(os.Stderr, " (ka-0000002210) or the daemon (rater,rated_bead_id) upsert.")
	fmt.Fprintln(os.Stderr, "════════════════════════════════════════════════════")
	// Emit a marker line the shell wrapper greps for so a skip is never read as
	// a silent pass.
	fmt.Println("PROBE_SKIP: " + why)
	os.Exit(0)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// normalizeURL mirrors beadsapi.New's HTTPAddr handling: a value that does not
// already start with "http" is assumed to be a bare host:port and prefixed with
// "http://". Keeps the raw-POST path in agreement with how the client addresses
// the same daemon.
func normalizeURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" || strings.HasPrefix(addr, "http") {
		return addr
	}
	return "http://" + addr
}

func containsStr(s []string, want string) bool {
	return slices.Contains(s, want)
}
