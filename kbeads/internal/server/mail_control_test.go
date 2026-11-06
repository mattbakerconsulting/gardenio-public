package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

func controlMailBead() *model.Bead {
	return &model.Bead{ID: "gd-m1", Type: "mail", Title: "Stop", Assignee: "crew-victim", Labels: []string{"from:crew-attacker", "control:kill"}}
}

func ctxWithIdentity(id AuthIdentity, fleet string) context.Context {
	ctx := WithAuthIdentity(context.Background(), id)
	if fleet != "" {
		ctx = WithFleet(ctx, fleet)
	}
	return ctx
}

// Review Focus 2: a crew with its own agent token cannot kill another crew;
// the bridge can (positive control).
func TestCreateMail_ControlLabel_PerAgentTokenRefused_BridgeAllowed(t *testing.T) {
	s, _, _ := newTestServer()
	s.controlMailFleets = map[string]bool{"slack-bridge": true}

	err := s.applyMailControlGuard(ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "crew-attacker"}, "agents"), controlMailBead())
	var denial *AuthzDenial
	if !errors.As(err, &denial) {
		t.Fatalf("per-agent fleet token must be refused with AuthzDenial, got %v", err)
	}
	if denial.Escalatable {
		t.Fatal("control-label denial must not be escalatable")
	}

	if err := s.applyMailControlGuard(ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "slack-bridge"}, "slack-bridge"), controlMailBead()); err != nil {
		t.Fatalf("bridge fleet must be allowed: %v", err)
	}
	if err := s.applyMailControlGuard(ctxWithIdentity(AuthIdentity{Type: "oidc", Subject: "owner@example.com"}, ""), controlMailBead()); err != nil {
		t.Fatalf("operator must be allowed: %v", err)
	}
	// The retired shared static token (gd-ke6PC7bRto) is no longer one of
	// the allowed callers: no request carries this identity any more.
	if err := s.applyMailControlGuard(ctxWithIdentity(AuthIdentity{Type: "static", Subject: "static"}, ""), controlMailBead()); err == nil {
		t.Fatal("the retired static identity must be refused")
	}
}

func TestCreateMail_ControlLabel_UnlistedIdentitiesRefused(t *testing.T) {
	s, _, _ := newTestServer()
	for _, id := range []AuthIdentity{
		{Type: "anonymous"},
		{Type: "none"},
		{Type: "machine", Subject: "svc-client"},
		{Type: "land", Subject: "land:demo"},
		{Type: "fleet", Subject: "agents", Privileged: true},
	} {
		if err := s.applyMailControlGuard(ctxWithIdentity(id, "agents"), controlMailBead()); err == nil {
			t.Errorf("identity %+v must be refused a control: label", id)
		}
	}
}

func TestCreateMail_NoControlLabel_AnyoneMaySend(t *testing.T) {
	s, _, _ := newTestServer()
	plain := &model.Bead{ID: "gd-m2", Type: "mail", Title: "hi", Assignee: "crew-b", Labels: []string{"from:crew-a", "delivery:interrupt"}}
	if err := s.applyMailControlGuard(ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "crew-a"}, "agents"), plain); err != nil {
		t.Fatalf("plain mail must pass: %v", err)
	}
}

func TestCreateMail_ControlLabel_RunnerOnlyForOwnAgent(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.beadsMu.Lock()
	ms.beads["gd-agent1"] = &model.Bead{ID: "gd-agent1", Type: "agent", Title: "local-laptop", Assignee: "local-laptop", Status: "open", Fields: []byte(`{"agent":"local-laptop","runtime_id":"gd-runner1"}`)}
	ms.beads["gd-agent2"] = &model.Bead{ID: "gd-agent2", Type: "agent", Title: "crew-cloud", Assignee: "crew-cloud", Status: "open", Fields: []byte(`{"agent":"crew-cloud"}`)}
	ms.beadsMu.Unlock()

	own := &model.Bead{ID: "gd-m3", Type: "mail", Assignee: "local-laptop", Labels: []string{"control:kill"}}
	other := &model.Bead{ID: "gd-m4", Type: "mail", Assignee: "crew-cloud", Labels: []string{"control:kill"}}
	unknown := &model.Bead{ID: "gd-m5", Type: "mail", Assignee: "nobody", Labels: []string{"control:kill"}}
	ctx := WithRunnerID(ctxWithIdentity(AuthIdentity{Type: "runner", Subject: "owner@example.com"}, ""), "gd-runner1")
	if err := s.applyMailControlGuard(ctx, own); err != nil {
		t.Fatalf("runner must control its own agent: %v", err)
	}
	if err := s.applyMailControlGuard(ctx, other); err == nil {
		t.Fatal("runner must not control another runtime's agent")
	}
	if err := s.applyMailControlGuard(ctx, unknown); err == nil {
		t.Fatal("runner must not control an agent it cannot resolve")
	}
}

// control:permission answers a Claude Code tool-approval prompt, so only
// bridges, the controller and operators may send it: a runner is refused even
// for its own agent (it shares that agent's machine), and so is a per-agent
// token, which would let a crew approve its own prompts.
func TestCreateMail_ControlPermission_BridgesOnly(t *testing.T) {
	s, ms, _ := newTestServer()
	s.controlMailFleets = map[string]bool{"slack-bridge": true}
	ms.beadsMu.Lock()
	ms.beads["gd-agent1"] = &model.Bead{ID: "gd-agent1", Type: "agent", Title: "local-laptop", Assignee: "local-laptop", Status: "open", Fields: []byte(`{"agent":"local-laptop","runtime_id":"gd-runner1"}`)}
	ms.beadsMu.Unlock()
	verdict := &model.Bead{ID: "gd-m6", Type: "mail", Assignee: "local-laptop", Labels: []string{"from:slack-bridge", "control:permission"}}

	runner := WithRunnerID(ctxWithIdentity(AuthIdentity{Type: "runner", Subject: "owner@example.com"}, ""), "gd-runner1")
	if err := s.applyMailControlGuard(runner, verdict); err == nil {
		t.Fatal("runner must not send control:permission, even to its own agent")
	}
	if err := s.applyMailControlGuard(ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "local-laptop"}, "agents"), verdict); err == nil {
		t.Fatal("per-agent token must not send control:permission")
	}
	// The retired shared static token (gd-ke6PC7bRto) is refused; the Slack
	// bridge sends verdicts on its own bridge fleet token.
	if err := s.applyMailControlGuard(ctxWithIdentity(AuthIdentity{Type: "static", Subject: "static"}, ""), verdict); err == nil {
		t.Fatal("the retired static identity must not send control:permission")
	}
	for _, ctx := range []context.Context{
		ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "slack-bridge"}, "slack-bridge"),
		ctxWithIdentity(AuthIdentity{Type: "oidc", Subject: "owner@example.com"}, ""),
	} {
		if err := s.applyMailControlGuard(ctx, verdict); err != nil {
			t.Fatalf("%v must be allowed control:permission: %v", AuthIdentityFromContext(ctx), err)
		}
	}
}

func TestParseControlMailFleets(t *testing.T) {
	def := parseControlMailFleets("")
	for _, f := range []string{"slack-bridge", "gitlab-bridge", "jira-bridge", "github-bridge"} {
		if !def[f] {
			t.Errorf("default set missing %q", f)
		}
	}
	got := parseControlMailFleets(" slack-bridge , ,ops ")
	if len(got) != 2 || !got["slack-bridge"] || !got["ops"] {
		t.Errorf("parseControlMailFleets = %v, want {slack-bridge, ops}", got)
	}
}

// The guard runs at the createBead choke point: a per-agent fleet token's
// control:kill mail is refused with an AuthzDenial before anything is stored.
func TestCreateBead_MailControlLabel_RefusedAtChokePoint(t *testing.T) {
	s, ms, _ := newTestServer()
	s.controlMailFleets = map[string]bool{"slack-bridge": true}
	ctx := ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "crew-attacker"}, "agents")
	_, err := s.createBead(ctx, createBeadInput{Title: "Stop", Kind: string(model.KindData), Type: "mail", Assignee: "crew-victim", Labels: []string{"from:crew-attacker", "control:kill"}})
	var denial *AuthzDenial
	if !errors.As(err, &denial) {
		t.Fatalf("createBead must refuse with AuthzDenial, got %v", err)
	}
	ms.beadsMu.Lock()
	defer ms.beadsMu.Unlock()
	for _, b := range ms.beads {
		if b.Type == "mail" {
			t.Fatalf("refused mail was stored: %+v", b)
		}
	}
}

// controlLabelFixture stores plain mail from crew-attacker to crew-victim, the
// first step of the attack the label-add and update guards close.
func controlLabelFixture(t *testing.T) (*BeadsServer, *mockStore, context.Context) {
	t.Helper()
	s, ms, _ := newTestServer()
	s.controlMailFleets = map[string]bool{"slack-bridge": true}
	ms.beadsMu.Lock()
	ms.beads["gd-mail1"] = &model.Bead{ID: "gd-mail1", Type: "mail", Kind: model.KindData, Title: "hi", CreatedBy: "crew-attacker", Assignee: "crew-victim", Status: model.StatusOpen}
	ms.labels["gd-mail1"] = []string{"from:crew-attacker"}
	ms.beadsMu.Unlock()
	return s, ms, ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "crew-attacker"}, "agents")
}

func mailLabels(ms *mockStore, id string) []string {
	ms.beadsMu.Lock()
	defer ms.beadsMu.Unlock()
	return append([]string(nil), ms.labels[id]...)
}

func assertNoControlLabel(t *testing.T, ms *mockStore, id string) {
	t.Helper()
	for _, l := range mailLabels(ms, id) {
		if strings.HasPrefix(l, mailControlPrefix) {
			t.Fatalf("refused control: label was stored on %s: %v", id, mailLabels(ms, id))
		}
	}
}

func TestAddLabel_MailControlLabel_PerAgentRefused(t *testing.T) {
	s, ms, ctx := controlLabelFixture(t)
	_, err := s.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: "gd-mail1", Label: "control:kill"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("gRPC AddLabel control:kill = %v, want PermissionDenied", err)
	}
	assertNoControlLabel(t, ms, "gd-mail1")

	// A plain label still lands, and an operator may add control:kill.
	if _, err := s.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: "gd-mail1", Label: "delivery:interrupt"}); err != nil {
		t.Fatalf("plain label must pass: %v", err)
	}
	operator := controlMailAllowedCallers()["operator"]
	if _, err := s.AddLabel(operator, &beadsv1.AddLabelRequest{BeadId: "gd-mail1", Label: "control:kill"}); err != nil {
		t.Fatalf("operator must be allowed control:kill: %v", err)
	}
}

func TestAddLabel_ControlLabelOnNonMail_Unaffected(t *testing.T) {
	s, _, ctx := controlLabelFixture(t)
	task := &model.Bead{ID: "gd-task1", Type: model.TypeTask, Kind: model.KindIssue, Title: "t", Status: model.StatusOpen}
	if d := s.mailControlLabelChangeDenial(ctx, task, "control:kill"); d != nil {
		t.Fatalf("control: on a non-mail bead is not reserved: %v", d)
	}
}

func TestHandleAddLabel_MailControlLabel_PerAgentRefused(t *testing.T) {
	s, ms, ctx := controlLabelFixture(t)
	body, _ := json.Marshal(map[string]string{"label": "control:kill"})
	req := httptest.NewRequest("POST", "/v1/beads/gd-mail1/labels", bytes.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	bareMux(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("HTTP add control:kill = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	var denial AuthzDenial
	if err := json.Unmarshal(rec.Body.Bytes(), &denial); err != nil || denial.Escalatable || denial.BeadType != "mail" {
		t.Fatalf("want a non-escalatable mail AuthzDenial, got %s (%v)", rec.Body.String(), err)
	}
	assertNoControlLabel(t, ms, "gd-mail1")
}

func TestUpdateBead_MailControlLabel_PerAgentRefused(t *testing.T) {
	s, ms, ctx := controlLabelFixture(t)
	_, err := s.updateBead(ctx, "gd-mail1", updateBeadInput{Labels: []string{"from:crew-attacker", "control:kill"}, labelsSet: true})
	var denial *AuthzDenial
	if !errors.As(err, &denial) || denial.Escalatable {
		t.Fatalf("update adding control:kill must be refused with a non-escalatable AuthzDenial, got %v", err)
	}
	assertNoControlLabel(t, ms, "gd-mail1")

	operator := controlMailAllowedCallers()["operator"]
	if _, err := s.updateBead(operator, "gd-mail1", updateBeadInput{Labels: []string{"from:crew-attacker", "control:kill"}, labelsSet: true}); err != nil {
		t.Fatalf("operator must be allowed control:kill on update: %v", err)
	}
	// The recipient may still edit labels on its control mail when it keeps
	// the existing control: label and the recipient.
	if _, err := s.updateBead(ctx, "gd-mail1", updateBeadInput{Labels: []string{"from:crew-attacker", "control:kill", "seen"}, labelsSet: true}); err != nil {
		t.Fatalf("keeping an existing control: label must pass: %v", err)
	}
}

// Reverse order: control mail an operator sent to one agent cannot be
// retargeted at another by a per-agent token.
func TestUpdateBead_MailControlLabel_RetargetRefused(t *testing.T) {
	s, ms, ctx := controlLabelFixture(t)
	ms.beadsMu.Lock()
	ms.beads["gd-mail1"].Assignee = "crew-attacker"
	ms.labels["gd-mail1"] = []string{"control:kill"}
	ms.beadsMu.Unlock()
	victim := "crew-victim"
	_, err := s.updateBead(ctx, "gd-mail1", updateBeadInput{Assignee: &victim})
	var denial *AuthzDenial
	if !errors.As(err, &denial) {
		t.Fatalf("retargeting control mail must be refused with AuthzDenial, got %v", err)
	}
	ms.beadsMu.Lock()
	got := ms.beads["gd-mail1"].Assignee
	ms.beadsMu.Unlock()
	if got != "crew-attacker" {
		t.Fatalf("refused retarget was stored: assignee %q", got)
	}
}

// controlMailFixture stores control:kill mail the slack bridge sent to
// crew-victim, and returns crew-victim's own per-agent token. The ownership
// gate already refuses other crews (neither creator nor assignee), so the
// token that can reach these paths is the recipient's: it must not cancel its
// own kill by stripping the label before mailsub's Drain honours it.
func controlMailFixture(t *testing.T) (*BeadsServer, *mockStore, context.Context) {
	t.Helper()
	s, ms, _ := newTestServer()
	s.controlMailFleets = map[string]bool{"slack-bridge": true}
	ms.beadsMu.Lock()
	ms.beads["gd-mail1"] = &model.Bead{ID: "gd-mail1", Type: "mail", Kind: model.KindData, Title: "Stop", CreatedBy: "slack-bridge", Assignee: "crew-victim", Status: model.StatusOpen}
	ms.labels["gd-mail1"] = []string{"from:slack-bridge", "control:kill"}
	ms.beadsMu.Unlock()
	return s, ms, ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "crew-victim"}, "agents")
}

func assertHasControlKill(t *testing.T, ms *mockStore, id string) {
	t.Helper()
	for _, l := range mailLabels(ms, id) {
		if l == "control:kill" {
			return
		}
	}
	t.Fatalf("refused removal stripped control:kill from %s: %v", id, mailLabels(ms, id))
}

// The identities the create guard allows may remove a control: label. The
// operator is an admin so it passes the ownership gate on mail it did not
// send; the bridge passes it as the sender.
func controlMailAllowedCallers() map[string]context.Context {
	return map[string]context.Context{
		"operator": WithRoles(withAuthMethod(ctxWithIdentity(AuthIdentity{Type: "oidc", Subject: "admin@example.com"}, ""), authMethodOIDC), []string{RoleAdmin}),
		"bridge":   ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "slack-bridge"}, "slack-bridge"),
	}
}

func TestRemoveLabel_MailControlLabel_PerAgentRefused(t *testing.T) {
	s, ms, ctx := controlMailFixture(t)
	_, err := s.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: "gd-mail1", Label: "control:kill"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("gRPC RemoveLabel control:kill = %v, want PermissionDenied", err)
	}
	assertHasControlKill(t, ms, "gd-mail1")

	// A plain label can still be removed by the same token.
	if _, err := s.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: "gd-mail1", Label: "from:slack-bridge"}); err != nil {
		t.Fatalf("plain label removal must pass: %v", err)
	}
}

func TestRemoveLabel_MailControlLabel_AllowedCallers(t *testing.T) {
	for name, caller := range controlMailAllowedCallers() {
		t.Run(name, func(t *testing.T) {
			s, ms, _ := controlMailFixture(t)
			if _, err := s.RemoveLabel(caller, &beadsv1.RemoveLabelRequest{BeadId: "gd-mail1", Label: "control:kill"}); err != nil {
				t.Fatalf("%s must be allowed to remove control:kill: %v", name, err)
			}
			for _, l := range mailLabels(ms, "gd-mail1") {
				if l == "control:kill" {
					t.Fatalf("control:kill still on gd-mail1 after %s removed it", name)
				}
			}
		})
	}
}

func TestRemoveLabel_ControlLabelOnNonMail_Unaffected(t *testing.T) {
	s, ms, ctx := controlMailFixture(t)
	ms.beadsMu.Lock()
	ms.beads["gd-task1"] = &model.Bead{ID: "gd-task1", Type: model.TypeTask, Kind: model.KindIssue, Title: "t", Status: model.StatusOpen}
	ms.labels["gd-task1"] = []string{"control:kill"}
	ms.beadsMu.Unlock()
	if _, err := s.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: "gd-task1", Label: "control:kill"}); err != nil {
		t.Fatalf("control: on a non-mail bead is not reserved: %v", err)
	}
}

func TestHandleRemoveLabel_MailControlLabel_PerAgentRefused(t *testing.T) {
	s, ms, ctx := controlMailFixture(t)
	req := httptest.NewRequest("DELETE", "/v1/beads/gd-mail1/labels/control:kill", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	bareMux(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("HTTP remove control:kill = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	var denial AuthzDenial
	if err := json.Unmarshal(rec.Body.Bytes(), &denial); err != nil || denial.Escalatable || denial.BeadType != "mail" {
		t.Fatalf("want a non-escalatable mail AuthzDenial, got %s (%v)", rec.Body.String(), err)
	}
	assertHasControlKill(t, ms, "gd-mail1")
}

func TestHandleRemoveLabel_MailControlLabel_AllowedCallers(t *testing.T) {
	for name, caller := range controlMailAllowedCallers() {
		t.Run(name, func(t *testing.T) {
			s, _, _ := controlMailFixture(t)
			req := httptest.NewRequest("DELETE", "/v1/beads/gd-mail1/labels/control:kill", nil).WithContext(caller)
			rec := httptest.NewRecorder()
			bareMux(s).ServeHTTP(rec, req)
			if rec.Code/100 != 2 {
				t.Fatalf("%s HTTP remove control:kill = %d, want 2xx; body %s", name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestUpdateBead_MailControlLabelRemoval_PerAgentRefused(t *testing.T) {
	s, ms, ctx := controlMailFixture(t)
	_, err := s.updateBead(ctx, "gd-mail1", updateBeadInput{Labels: []string{"from:slack-bridge"}, labelsSet: true})
	var denial *AuthzDenial
	if !errors.As(err, &denial) || denial.Escalatable {
		t.Fatalf("update dropping control:kill must be refused with a non-escalatable AuthzDenial, got %v", err)
	}
	assertHasControlKill(t, ms, "gd-mail1")

	// Keeping control:kill while editing other labels still passes.
	if _, err := s.updateBead(ctx, "gd-mail1", updateBeadInput{Labels: []string{"control:kill", "seen"}, labelsSet: true}); err != nil {
		t.Fatalf("keeping an existing control: label must pass: %v", err)
	}
}

// Dropping control:kill while retargeting is checked against the recipient
// it was aimed at: a runner may strip it from its own agent's mail, but the
// new recipient's runner cannot strip it from mail aimed at crew-victim. The
// guard is called directly: the ownership gate refuses this runner on
// updateBead before the guard runs.
func TestMailControlUpdateDenial_RemovalCheckedAgainstPriorRecipient(t *testing.T) {
	s, ms, _ := controlMailFixture(t)
	ms.beadsMu.Lock()
	ms.beads["gd-agent1"] = &model.Bead{ID: "gd-agent1", Type: "agent", Title: "local-laptop", Assignee: "local-laptop", Status: "open", Fields: []byte(`{"agent":"local-laptop","runtime_id":"gd-runner1"}`)}
	ms.beadsMu.Unlock()
	runner := WithRunnerID(ctxWithIdentity(AuthIdentity{Type: "runner", Subject: "owner@example.com"}, ""), "gd-runner1")
	after := &model.Bead{Type: "mail", Assignee: "local-laptop", Labels: []string{"from:slack-bridge"}}
	old := []string{"from:slack-bridge", "control:kill"}
	if d := s.mailControlUpdateDenial(runner, after, old, "crew-victim"); d == nil {
		t.Fatal("runner of the new recipient must not strip control:kill aimed at crew-victim")
	}
	if d := s.mailControlUpdateDenial(runner, after, old, "local-laptop"); d != nil {
		t.Fatalf("runner may strip control:kill from its own agent's mail: %v", d)
	}
}

func TestUpdateBead_MailControlLabelRemoval_AllowedCallers(t *testing.T) {
	for name, caller := range controlMailAllowedCallers() {
		t.Run(name, func(t *testing.T) {
			s, ms, _ := controlMailFixture(t)
			if _, err := s.updateBead(caller, "gd-mail1", updateBeadInput{Labels: []string{"from:slack-bridge"}, labelsSet: true}); err != nil {
				t.Fatalf("%s must be allowed to drop control:kill on update: %v", name, err)
			}
			for _, l := range mailLabels(ms, "gd-mail1") {
				if l == "control:kill" {
					t.Fatalf("control:kill still on gd-mail1 after %s's update", name)
				}
			}
		})
	}
}

// controlEndMux registers the routes that end a bead without AuthMiddleware,
// as bareMux does for the label routes.
func controlEndMux(s *BeadsServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/beads/{id}/close", s.handleCloseBead)
	mux.HandleFunc("DELETE /v1/beads/{id}", s.handleDeleteBead)
	mux.HandleFunc("PATCH /v1/beads/{id}", s.handleUpdateBead)
	return mux
}

func serveControlEnd(s *BeadsServer, ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	controlEndMux(s).ServeHTTP(rec, req)
	return rec
}

func assertControlMailLive(t *testing.T, ms *mockStore, id string) {
	t.Helper()
	ms.beadsMu.Lock()
	defer ms.beadsMu.Unlock()
	b := ms.beads[id]
	if b.Status != model.StatusOpen || b.DeletedAt != nil {
		t.Fatalf("refused call ended %s: status %s, deleted_at %v", id, b.Status, b.DeletedAt)
	}
}

func assertNonEscalatableMailDenial(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	var denial AuthzDenial
	if err := json.Unmarshal(rec.Body.Bytes(), &denial); err != nil || denial.Escalatable || denial.BeadType != "mail" {
		t.Fatalf("want a non-escalatable mail AuthzDenial, got %s (%v)", rec.Body.String(), err)
	}
}

// The recipient's own token cannot cancel a kill by closing or deleting the
// mail before mailsub's Drain (which reads open mail) honours it.
func TestCloseBead_ControlMail_RecipientRefused(t *testing.T) {
	s, ms, ctx := controlMailFixture(t)
	if _, err := s.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-mail1"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("gRPC CloseBead = %v, want PermissionDenied", err)
	}
	assertControlMailLive(t, ms, "gd-mail1")

	for _, body := range []string{``, `{}`, `{"reason":"done"}`, `{"control_acted":""}`, `{"control_acted":"  "}`, `{"control_acted":true}`} {
		assertNonEscalatableMailDenial(t, serveControlEnd(s, ctx, "POST", "/v1/beads/gd-mail1/close", body))
		assertControlMailLive(t, ms, "gd-mail1")
	}
}

func TestDeleteBead_ControlMail_RecipientRefused(t *testing.T) {
	s, ms, ctx := controlMailFixture(t)
	if _, err := s.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{Id: "gd-mail1"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("gRPC DeleteBead = %v, want PermissionDenied", err)
	}
	assertControlMailLive(t, ms, "gd-mail1")
	assertNonEscalatableMailDenial(t, serveControlEnd(s, ctx, "DELETE", "/v1/beads/gd-mail1", ""))
	assertControlMailLive(t, ms, "gd-mail1")
}

// Drain lists status=open, so any status change hides the kill, not only
// closed. A PATCH cannot carry the control_acted exception.
func TestUpdateBead_ControlMailStatus_RecipientRefused(t *testing.T) {
	s, ms, ctx := controlMailFixture(t)
	for _, st := range []string{"closed", "in_progress", "deferred"} {
		_, err := s.updateBead(ctx, "gd-mail1", updateBeadInput{Status: &st, Fields: json.RawMessage(`{"control_acted":"2026-10-28T23:00:00Z"}`)})
		var denial *AuthzDenial
		if !errors.As(err, &denial) || denial.Escalatable {
			t.Fatalf("status %s: want a non-escalatable AuthzDenial, got %v", st, err)
		}
		assertControlMailLive(t, ms, "gd-mail1")
	}
	// Other edits to its own control mail still pass.
	title := "Stop (seen)"
	if _, err := s.updateBead(ctx, "gd-mail1", updateBeadInput{Title: &title}); err != nil {
		t.Fatalf("a non-status edit must pass: %v", err)
	}
}

// mailsub's close path: the recipient token closes kill mail once it has
// handed the kill to coop, stamping control_acted.
func TestHandleCloseBead_ControlMail_RecipientWithActedStampAllowed(t *testing.T) {
	s, ms, ctx := controlMailFixture(t)
	rec := serveControlEnd(s, ctx, "POST", "/v1/beads/gd-mail1/close", `{"control_acted":"2026-10-28T23:00:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("close with control_acted = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	ms.beadsMu.Lock()
	defer ms.beadsMu.Unlock()
	b := ms.beads["gd-mail1"]
	if b.Status != model.StatusClosed {
		t.Fatalf("status = %s, want closed", b.Status)
	}
	if !strings.Contains(string(b.Fields), `"control_acted":"2026-10-28T23:00:00Z"`) {
		t.Fatalf("control_acted not recorded on the mail: %s", b.Fields)
	}
}

func TestControlMailEnd_AllowedCallers(t *testing.T) {
	for name, caller := range controlMailAllowedCallers() {
		t.Run(name+"/grpc-close", func(t *testing.T) {
			s, _, _ := controlMailFixture(t)
			if _, err := s.CloseBead(caller, &beadsv1.CloseBeadRequest{Id: "gd-mail1"}); err != nil {
				t.Fatalf("%s must be allowed to close control mail: %v", name, err)
			}
		})
		t.Run(name+"/http-close", func(t *testing.T) {
			s, _, _ := controlMailFixture(t)
			if rec := serveControlEnd(s, caller, "POST", "/v1/beads/gd-mail1/close", ``); rec.Code != http.StatusOK {
				t.Fatalf("%s HTTP close = %d; body %s", name, rec.Code, rec.Body.String())
			}
		})
		t.Run(name+"/update-status", func(t *testing.T) {
			s, _, _ := controlMailFixture(t)
			st := "closed"
			if _, err := s.updateBead(caller, "gd-mail1", updateBeadInput{Status: &st}); err != nil {
				t.Fatalf("%s must be allowed to close control mail by update: %v", name, err)
			}
		})
		t.Run(name+"/grpc-delete", func(t *testing.T) {
			s, _, _ := controlMailFixture(t)
			if _, err := s.DeleteBead(caller, &beadsv1.DeleteBeadRequest{Id: "gd-mail1"}); err != nil {
				t.Fatalf("%s must be allowed to delete control mail: %v", name, err)
			}
		})
	}
}

// Plain mail and control: labels on other bead types are not reserved.
func TestControlMailEnd_PlainMailAndNonMailUnaffected(t *testing.T) {
	s, ms, ctx := controlMailFixture(t)
	ms.beadsMu.Lock()
	ms.beads["gd-mail2"] = &model.Bead{ID: "gd-mail2", Type: "mail", Kind: model.KindData, Title: "hi", CreatedBy: "slack-bridge", Assignee: "crew-victim", Status: model.StatusOpen}
	ms.labels["gd-mail2"] = []string{"from:slack-bridge"}
	ms.beads["gd-task1"] = &model.Bead{ID: "gd-task1", Type: model.TypeTask, Kind: model.KindIssue, Title: "t", CreatedBy: "crew-victim", Assignee: "crew-victim", Status: model.StatusOpen}
	ms.labels["gd-task1"] = []string{"control:kill"}
	ms.beadsMu.Unlock()
	if _, err := s.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-mail2"}); err != nil {
		t.Fatalf("recipient must close its plain mail: %v", err)
	}
	if _, err := s.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-task1"}); err != nil {
		t.Fatalf("control: on a non-mail bead is not reserved: %v", err)
	}
}

// The revoke marker exempts only the ctx handleRevokeRunner builds; the same
// per-agent caller without it is still refused (gd-qoiurW3S7m).
func TestMailControlGuard_RunnerRevokeKillMarker(t *testing.T) {
	s, _, _ := newTestServer()
	s.controlMailFleets = map[string]bool{"slack-bridge": true}
	ctx := ctxWithIdentity(AuthIdentity{Type: "fleet", Subject: "crew-attacker"}, "agents")
	if err := s.applyMailControlGuard(ctx, controlMailBead()); err == nil {
		t.Fatal("per-agent control: mail must be refused without the marker")
	}
	if err := s.applyMailControlGuard(withRunnerRevokeKill(ctx), controlMailBead()); err != nil {
		t.Fatalf("revoke kill mail must pass the guard: %v", err)
	}
}
