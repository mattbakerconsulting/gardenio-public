package server

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func seedRunnerAndAgent(ms *mockStore, runnerID, agentName string) {
	seedRunnerFor(ms, runnerID, "oidc:a@example.com", agentName)
}

// seedRunnerFor stores an online runner bead owned by owner and, when
// agentName is set, the agent joined on it (fields.runtime_id).
func seedRunnerFor(ms *mockStore, runnerID, owner, agentName string) {
	ms.beadsMu.Lock()
	defer ms.beadsMu.Unlock()
	seen := time.Now().UTC().Format(time.RFC3339)
	ms.beads[runnerID] = &model.Bead{ID: runnerID, Type: "runner", Kind: "config", Title: "laptop", Status: "open", Fields: []byte(`{"owner":"` + owner + `","hostname":"laptop","state":"online","last_seen_at":"` + seen + `","projects":["pet"]}`)}
	if agentName != "" {
		ms.beads["gd-"+agentName] = &model.Bead{ID: "gd-" + agentName, Type: "agent", Title: agentName, Assignee: agentName, Status: "open", Fields: []byte(`{"agent":"` + agentName + `","runtime_id":"` + runnerID + `","execution_target":"local"}`)}
	}
}

func getMockBead(t *testing.T, ms *mockStore, id string) *model.Bead {
	t.Helper()
	b, err := ms.GetBead(context.Background(), id)
	if err != nil || b == nil {
		t.Fatalf("bead %s: %v %v", id, b, err)
	}
	return b
}

func TestEscalationCreate_LocalWithoutTargetRejected_WithTargetAccepted(t *testing.T) {
	s, ms, _ := newTestServer()
	seedRunnerAndAgent(ms, "gd-r1", "")
	bad := &model.Bead{ID: "gd-e1", Type: "escalation", Fields: []byte(`{"operation_type":"shell","operation_spec":{},"execution_target":"local"}`)}
	if err := s.checkEscalationTargets(context.Background(), bad); err == nil {
		t.Fatal("execution_target=local without target_runner/target_owner must be rejected")
	}
	good := &model.Bead{ID: "gd-e2", Type: "escalation", Fields: []byte(`{"operation_type":"shell","operation_spec":{},"execution_target":"local","target_runner":"gd-r1"}`)}
	if err := s.checkEscalationTargets(context.Background(), good); err != nil {
		t.Fatalf("positive control failed: %v", err)
	}
	byOwner := &model.Bead{ID: "gd-e5", Type: "escalation", Fields: []byte(`{"operation_type":"shell","operation_spec":{},"execution_target":"local","target_owner":"oidc:a@example.com"}`)}
	if err := s.checkEscalationTargets(context.Background(), byOwner); err != nil {
		t.Fatalf("target_owner alone names the runner: %v", err)
	}
	unknown := &model.Bead{ID: "gd-e3", Type: "escalation", Fields: []byte(`{"operation_type":"shell","operation_spec":{},"execution_target":"local","target_runner":"gd-nope"}`)}
	if err := s.checkEscalationTargets(context.Background(), unknown); err == nil {
		t.Fatal("an unknown target_runner must be rejected")
	}
	srv := &model.Bead{ID: "gd-e4", Type: "escalation", Fields: []byte(`{"operation_type":"shell","operation_spec":{},"execution_target":"server"}`)}
	if err := s.checkEscalationTargets(context.Background(), srv); err != nil {
		t.Fatalf("a server target needs no runner: %v", err)
	}
}

// The guard is wired into create. By default POST /v1/beads refuses a local
// escalation with no target (400); under BEADS_ESCALATION_LOCAL_TARGET=warn it
// is allowed with an X-Beads-Warning. A target_owner alone names the runner in
// both modes, and an unregistered target_runner is refused in both.
func TestEscalationCreate_LocalTarget_HTTP(t *testing.T) {
	body := func(extra map[string]any) map[string]any {
		f := map[string]any{"operation_type": "shell", "operation_spec": map[string]any{"cmd": "ls"}, "execution_target": "local"}
		for k, v := range extra {
			f[k] = v
		}
		return map[string]any{"title": "run on my laptop", "type": "escalation", "kind": "data", "fields": f}
	}
	_, _, h := newTestServer()

	for _, mode := range []string{"", "warn"} {
		t.Setenv("BEADS_ESCALATION_LOCAL_TARGET", mode)
		if rec := doJSON(t, h, http.MethodPost, "/v1/beads", body(map[string]any{"target_owner": "oidc:a@example.com"})); rec.Code != http.StatusCreated {
			t.Fatalf("mode %q: a target_owner-only filing must be accepted, got %d %s", mode, rec.Code, rec.Body.String())
		}
		if rec := doJSON(t, h, http.MethodPost, "/v1/beads", body(map[string]any{"target_runner": "gd-nope"})); rec.Code != http.StatusBadRequest {
			t.Fatalf("mode %q: an unregistered target_runner must 400, got %d", mode, rec.Code)
		}
	}

	t.Setenv("BEADS_ESCALATION_LOCAL_TARGET", "")
	if rec := doJSON(t, h, http.MethodPost, "/v1/beads", body(nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("default: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	t.Setenv("BEADS_ESCALATION_LOCAL_TARGET", "warn")
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", body(nil))
	if rec.Code != http.StatusCreated || !strings.Contains(strings.Join(rec.Header().Values("X-Beads-Warning"), "\n"), "target_runner") {
		t.Fatalf("warn: want 201 with a warning, got %d %v %s", rec.Code, rec.Header().Values("X-Beads-Warning"), rec.Body.String())
	}
}

func TestEmitEscalationApprovedMail_ToJoinedAgent_OrParkedForRunner(t *testing.T) {
	s, ms, _ := newTestServer()
	seedRunnerAndAgent(ms, "gd-r1", "local-laptop")
	ms.beadsMu.Lock()
	ms.beads["gd-e1"] = &model.Bead{ID: "gd-e1", Type: "escalation", Status: "in_progress", Fields: []byte(`{"operation_type":"shell","operation_spec":{"cmd":"ls"},"execution_target":"local","execution_dispatch":"local","target_runner":"gd-r1","executor":"local-laptop"}`)}
	esc := ms.beads["gd-e1"]
	ms.beadsMu.Unlock()
	mailID, delivered, err := s.emitEscalationApprovedMail(ctxAsTestSubject(), esc)
	if err != nil || !delivered || mailID == "" {
		t.Fatalf("expected mail to the joined agent: id=%q delivered=%v err=%v", mailID, delivered, err)
	}
	m := getMockBead(t, ms, mailID)
	if m.Type != "mail" || m.Assignee != "local-laptop" {
		t.Fatalf("mail not addressed to the runner's agent: %+v", m)
	}
	f := beadFieldMap(m.Fields)
	if f["escalation_id"] != "gd-e1" {
		t.Fatalf("mail must name the escalation: %v", f)
	}
	if !slices.Contains(m.Labels, "control:escalation-approved") || !slices.Contains(m.Labels, "from:kbeads") {
		t.Fatalf("mail must carry control:escalation-approved and from:kbeads: %v", m.Labels)
	}
	escF := beadFieldMap(getMockBead(t, ms, "gd-e1").Fields)
	if got := fieldStr(escF, "executor_mail_id"); got != mailID {
		t.Fatalf("executor_mail_id = %q, want %q", got, mailID)
	}
	if !strings.Contains(m.Description, "gb escalation close gd-e1") {
		t.Fatalf("mail must point at gb escalation close (POST /result): %q", m.Description)
	}

	// Offline runner: parked mail, not delivered, approval still fine (spec §10).
	s2, ms2, _ := newTestServer()
	seedRunnerAndAgent(ms2, "gd-r2", "")
	esc2 := &model.Bead{ID: "gd-e2", Type: "escalation", Status: "in_progress", Fields: []byte(`{"operation_type":"shell","operation_spec":{},"execution_target":"local","execution_dispatch":"local","target_runner":"gd-r2"}`)}
	id2, delivered2, err := s2.emitEscalationApprovedMail(ctxAsTestSubject(), esc2)
	if err != nil || delivered2 || id2 == "" {
		t.Fatalf("offline runner must park the mail: id=%q delivered=%v err=%v", id2, delivered2, err)
	}
	if parked := getMockBead(t, ms2, id2); parked.Assignee != "runner:gd-r2" {
		t.Fatalf("parked mail must be addressed to runner:<id>, got %q", parked.Assignee)
	}

	// Non-local targets, and a local one naming no runner (the bridge's legacy
	// local path), keep their own paths: no mail.
	// A local target the daemon did not dispatch locally (a server replay, a
	// verdict) is never mailed either.
	for _, fields := range []string{
		`{"operation_type":"shell","operation_spec":{},"execution_target":"service_account","execution_dispatch":"service_account"}`,
		`{"operation_type":"shell","operation_spec":{},"execution_target":"local","execution_dispatch":"local"}`,
		`{"operation_type":"update","operation_spec":{"title":"x"},"execution_target":"local","execution_dispatch":"server","target_runner":"gd-r1"}`,
		`{"operation_type":"github_pr_merge","execution_target":"local","execution_dispatch":"none","target_runner":"gd-r1"}`,
	} {
		other := &model.Bead{ID: "gd-e3", Type: "escalation", Fields: []byte(fields)}
		if id, d, err := s.emitEscalationApprovedMail(ctxAsTestSubject(), other); id != "" || d || err != nil {
			t.Fatalf("%s must send nothing: id=%q delivered=%v err=%v", fields, id, d, err)
		}
	}
}

func TestEmitEscalationApprovedMail_TargetOwnerResolvesTheOnlineRunner(t *testing.T) {
	s, ms, _ := newTestServer()
	seedRunnerAndAgent(ms, "gd-r1", "local-laptop")
	ms.beadsMu.Lock()
	ms.beads["gd-e1"] = &model.Bead{ID: "gd-e1", Type: "escalation", Status: "in_progress", Fields: []byte(`{"operation_type":"shell","operation_spec":{},"execution_target":"local","execution_dispatch":"local","target_owner":"oidc:a@example.com"}`)}
	esc := ms.beads["gd-e1"]
	ms.beadsMu.Unlock()
	// startEscalationExecutor is the one executor writer; the mail follows it.
	if err := s.startEscalationExecutor(ctxAsTestSubject(), esc, dispatchLocal, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	esc = getMockBead(t, ms, "gd-e1")
	mailID, delivered, err := s.emitEscalationApprovedMail(ctxAsTestSubject(), esc)
	if err != nil || !delivered {
		t.Fatalf("target_owner's one online runner must be resolved: id=%q delivered=%v err=%v", mailID, delivered, err)
	}
	if m := getMockBead(t, ms, mailID); m.Assignee != "local-laptop" {
		t.Fatalf("mail addressed to %q", m.Assignee)
	}

	// A stale heartbeat is not online: nothing to resolve, an error, no mail.
	ms.beadsMu.Lock()
	ms.beads["gd-r1"].Fields = []byte(`{"owner":"oidc:a@example.com","state":"online","last_seen_at":"2020-01-01T00:00:00Z"}`)
	ms.beadsMu.Unlock()
	if id, _, err := s.emitEscalationApprovedMail(ctxAsTestSubject(), esc); err == nil || id != "" {
		t.Fatalf("a stale runner must not be resolved: id=%q err=%v", id, err)
	}
}

// seedLocalEscalation files an open escalation through the mock, as the
// classed server's approve tests expect.
func seedLocalEscalation(t *testing.T, ms *mockStore, id string, fields map[string]any) {
	t.Helper()
	f := map[string]any{"operation_type": "shell", "operation_spec": map[string]any{"cmd": "ls"}, "execution_target": "local", "requesting_agent": "crew-x"}
	for k, v := range fields {
		f[k] = v
	}
	seedEscalation(t, ms, id, "", f)
}

func approveLocalAs(t *testing.T, h http.Handler, id, subject string, body map[string]any) map[string]any {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	rec := doBearerJSON(t, h, http.MethodPost, "/v1/escalations/"+id+"/approve", subject, body, nil)
	requireStatus(t, rec, http.StatusOK)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp
}

// The runner's owner approves: the claim is local, the joined agent is the
// recorded executor, it gets one mail, and the response says so.
func TestApproveEscalation_OwnerApprovalMailsAndRecordsExecutor(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedRunnerAndAgent(ms, "gd-r1", "local-laptop")
	seedLocalEscalation(t, ms, "gd-e1", map[string]any{"target_runner": "gd-r1"})

	resp := approveLocalAs(t, h, "gd-e1", "a@example.com", nil)
	if resp["executor_notified"] != true {
		t.Fatalf("approve response must report executor_notified=true, got %v", resp)
	}
	f := escFields(t, ms, "gd-e1")
	if f["execution_dispatch"] != dispatchLocal || f["executor"] != "local-laptop" || f["executor_mail_id"] == nil {
		t.Fatalf("want local dispatch, executor and mail recorded: %v", f)
	}
	if getMockBead(t, ms, "gd-e1").Status != model.StatusInProgress {
		t.Fatal("approval must move the escalation to in_progress")
	}
}

// Spec §8.2 step 2: only the runner's owner approves work on that machine. A
// different operator, or a target_owner naming someone else, is refused with
// nothing written; the owner (the positive control) succeeds.
func TestApproveEscalation_NonOwnerRefused(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedRunnerAndAgent(ms, "gd-r1", "local-laptop")
	seedLocalEscalation(t, ms, "gd-e1", map[string]any{"target_runner": "gd-r1"})
	seedLocalEscalation(t, ms, "gd-e2", map[string]any{"target_owner": "oidc:a@example.com"})

	for _, id := range []string{"gd-e1", "gd-e2"} {
		rec := doBearerJSON(t, h, http.MethodPost, "/v1/escalations/"+id+"/approve", "b@example.com", map[string]any{}, nil)
		requireStatus(t, rec, http.StatusForbidden)
		if f := escFields(t, ms, id); f["approved_by"] != nil || f["execution_dispatch"] != nil || f["executor_mail_id"] != nil {
			t.Fatalf("%s: a refused approval must write nothing: %v", id, f)
		}
	}
	approveLocalAs(t, h, "gd-e1", "a@example.com", nil)
	if resp := approveLocalAs(t, h, "gd-e2", "a@example.com", nil); resp["executor_notified"] != true {
		t.Fatalf("target_owner's own approval must reach the agent on their online runner: %v", resp)
	}
}

// The runner check runs again at approve: a runner revoked after filing, or a
// target_runner on a service_account escalation switched to local at approve,
// is refused.
func TestApproveEscalation_RunnerRecheckedAtApprove(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedRunnerAndAgent(ms, "gd-r1", "local-laptop")
	seedLocalEscalation(t, ms, "gd-e1", map[string]any{"target_runner": "gd-r1"})
	seedLocalEscalation(t, ms, "gd-e2", map[string]any{"execution_target": "service_account", "target_runner": "gd-bogus"})

	ms.beadsMu.Lock()
	ms.beads["gd-r1"].Fields = []byte(`{"owner":"oidc:a@example.com","state":"revoked"}`)
	ms.beadsMu.Unlock()
	requireStatus(t, doBearerJSON(t, h, http.MethodPost, "/v1/escalations/gd-e1/approve", "a@example.com", map[string]any{}, nil), http.StatusBadRequest)
	requireStatus(t, doBearerJSON(t, h, http.MethodPost, "/v1/escalations/gd-e2/approve", "a@example.com", map[string]any{"execution_target": "local"}, nil), http.StatusBadRequest)
	for _, id := range []string{"gd-e1", "gd-e2"} {
		if f := escFields(t, ms, id); f["approved_by"] != nil || f["execution_dispatch"] != nil {
			t.Fatalf("%s: a refused approval must write nothing: %v", id, f)
		}
	}
	// Positive control: as filed (service_account), gd-e2 is no runner's work.
	approveLocalAs(t, h, "gd-e2", "a@example.com", nil)
}

// Main's single-executor dispatch decides: an operation the daemon replays
// (dispatch=server) or a verdict (none) is never also mailed to a runner.
func TestApproveEscalation_NonLocalDispatchSendsNoMail(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedRunnerAndAgent(ms, "gd-r1", "local-laptop")
	seedLocalEscalation(t, ms, "gd-e1", map[string]any{"operation_type": "update", "operation_spec": map[string]any{"title": "x"}, "target_bead_id": "gd-missing", "target_runner": "gd-r1"})
	seedLocalEscalation(t, ms, "gd-e2", map[string]any{"operation_type": "github_pr_merge", "operation_spec": map[string]any{}, "target_runner": "gd-r1"})

	for id, want := range map[string]string{"gd-e1": dispatchServer, "gd-e2": dispatchNone} {
		resp := approveLocalAs(t, h, id, "owner@example.com", nil)
		if _, ok := resp["executor_notified"]; ok {
			t.Fatalf("%s: no executor mail for dispatch %s: %v", id, want, resp)
		}
		if f := escFields(t, ms, id); f["execution_dispatch"] != want || f["executor_mail_id"] != nil {
			t.Fatalf("%s: want dispatch %s and no mail: %v", id, want, f)
		}
	}
}

// A mail that cannot be sent never fails the approval (spec §10): the owner
// has no online runner to resolve.
func TestApproveEscalation_MailFailureDoesNotFailApproval(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedLocalEscalation(t, ms, "gd-e1", map[string]any{"target_owner": "oidc:a@example.com"})
	resp := approveLocalAs(t, h, "gd-e1", "a@example.com", nil)
	if resp["executor_notified"] != false {
		t.Fatalf("executor_notified must be false when no runner resolved, got %v", resp["executor_notified"])
	}
}

// The control: sender-guard bypass (withEscalationApprovedMail) is reachable
// only from emitEscalationApprovedMail, and read only by the mail control
// guard: any other caller could mint control:escalation-approved mail as the
// daemon.
func TestEscalationApprovedMailBypass_OnlyFromEmit(t *testing.T) {
	allowed := map[string]string{
		"withEscalationApprovedMail": "emitEscalationApprovedMail",
		"isEscalationApprovedMail":   "applyMailControlGuard",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]int{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if want, gated := allowed[id.Name]; gated {
					seen[id.Name]++
					if fn.Name.Name != want {
						t.Errorf("%s: %s used in %s; only %s may", fset.Position(id.Pos()), id.Name, fn.Name.Name, want)
					}
				}
				if id.Name == "escalationApprovedMailKey" && fn.Name.Name != "withEscalationApprovedMail" && fn.Name.Name != "isEscalationApprovedMail" {
					t.Errorf("%s: escalationApprovedMailKey used in %s", fset.Position(id.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
	for name := range allowed {
		if seen[name] == 0 {
			t.Errorf("%s is never used: the guard above checks nothing", name)
		}
	}
}

// checkLocalApproval narrows who may approve a local escalation that names a
// runner: only the runner's owner (or target_owner). A coordinator lane that
// checkMayApprove admits (the merge desk here) is refused with a 403 and
// nothing written. Positive control: the same coordinator approves the same
// escalation with no local runner, so the runner check alone refuses it.
func TestApproveEscalation_CoordinatorLaneRefusedOnLocalRunner(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRunnerAndAgent(ms, "gd-r1", "local-laptop")
	local := mergeDeskEscalation("gd-crew1")
	local["operation_type"], local["operation_spec"] = "shell", map[string]any{"cmd": "ls"}
	local["execution_target"], local["target_runner"] = "local", "gd-r1"
	seedRoutedEscalation(ms, "gd-co-local", "gardenio", local)
	plain := mergeDeskEscalation("gd-crew1")
	plain["operation_type"], plain["operation_spec"] = "shell", map[string]any{"cmd": "ls"}
	seedRoutedEscalation(ms, "gd-co-plain", "gardenio", plain)

	if err := s.checkLocalApproval(context.Background(), ms.beads["gd-co-local"], "reviewer-a"); err == nil {
		t.Fatal("checkLocalApproval must refuse a coordinator-lane approver")
	}
	if err := s.checkLocalApproval(context.Background(), ms.beads["gd-co-local"], "a@example.com"); err != nil {
		t.Fatalf("positive control: the runner's owner: %v", err)
	}

	rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleApproveEscalation, "gd-co-local", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "only its owner may approve") {
		t.Fatalf("merge desk approving a runner's escalation: got %d %s, want the runner-owner 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-co-local")
	if f := escFields(t, ms, "gd-co-local"); f["execution_dispatch"] != nil || f["executor"] != nil || f["executor_mail_id"] != nil {
		t.Fatalf("a refused approval must write nothing: %v", f)
	}

	rec = callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleApproveEscalation, "gd-co-plain", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("positive control: merge desk approve with no runner: got %d %s", rec.Code, rec.Body.String())
	}
}
