package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// bridgeCtx is the slack-bridge's fleet token carrying an operator header, as
// the bridge's daemon client sends it.
func bridgeCtx() context.Context {
	ctx := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"}), "slack-bridge")
	return WithOperatedByHeader(ctx, "oidc:"+defaultTestSubject)
}

// TestApplyProjectLabelInvariant guards gd-YdsEpnN3Qo: a project-bearing bead
// gets its project:<fields.project> label at create, and one whose project
// labels all name another project is refused.
func TestApplyProjectLabelInvariant(t *testing.T) {
	projectTC := &model.TypeConfig{Fields: []model.FieldDef{{Name: "project", Type: model.FieldTypeString}}}
	plainTC := &model.TypeConfig{Fields: []model.FieldDef{{Name: "text", Type: model.FieldTypeString}}}
	cases := []struct {
		name     string
		beadType model.BeadType
		tc       *model.TypeConfig
		labels   []string
		fields   string
		want     []string
		refused  bool
	}{
		{"derived when absent", "session", projectTC, []string{"x"}, `{"project":"gardenio"}`, []string{"x", "project:gardenio"}, false},
		{"trimmed", "session", projectTC, nil, `{"project":" gardenio "}`, []string{"project:gardenio"}, false},
		{"matching label kept", "trigger", projectTC, []string{"project:gardenio"}, `{"project":"gardenio"}`, []string{"project:gardenio"}, false},
		{"one of several matches", "agent", projectTC, []string{"project:a", "project:gardenio"}, `{"project":"gardenio"}`, []string{"project:a", "project:gardenio"}, false},
		{"contradicting label refused", "agent", projectTC, []string{"project:other"}, `{"project":"gardenio"}`, []string{"project:other"}, true},
		{"unknown placeholder left alone", "event", projectTC, []string{"project:unknown"}, `{"project":"gardenio"}`, []string{"project:unknown"}, false},
		{"no fields.project", "session", projectTC, []string{"x"}, `{}`, []string{"x"}, false},
		{"type without a project field", "message", plainTC, nil, `{"project":"gardenio"}`, nil, false},
		{"plot_application exempt", typePlotApplication, projectTC, []string{"project:lands-intake"}, `{"project":"newland"}`, []string{"project:lands-intake"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyProjectLabelInvariant(context.Background(), tc.beadType, tc.tc, tc.labels, json.RawMessage(tc.fields))
			if tc.refused {
				var ie inputError
				if err == nil || !errors.As(err, &ie) || !strings.Contains(err.Error(), "does not match fields.project") {
					t.Fatalf("err = %v, want an input error naming the mismatch", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestApplyProjectLabelInvariant_MismatchWarnOnly: BEADS_PROJECT_LABEL_MISMATCH=warn
// logs a contradicting label instead of refusing it (gd-YdsEpnN3Qo).
func TestApplyProjectLabelInvariant_MismatchWarnOnly(t *testing.T) {
	t.Setenv("BEADS_PROJECT_LABEL_MISMATCH", "warn")
	tc := &model.TypeConfig{Fields: []model.FieldDef{{Name: "project", Type: model.FieldTypeString}}}
	got, err := applyProjectLabelInvariant(context.Background(), "agent", tc, []string{"project:other"}, json.RawMessage(`{"project":"gardenio"}`))
	if err != nil || !reflect.DeepEqual(got, []string{"project:other"}) {
		t.Fatalf("warn mode: (%v, %v), want the labels unchanged and no error", got, err)
	}
}

// The builtin project-bearing set is the one migration-free backfill and the
// lint both use; pin it so a new type with a `project` field is a deliberate
// addition (does its fields.project name the bead's own project?).
func TestProjectBearingBuiltinTypes(t *testing.T) {
	want := []string{"agent", "event", "memory", "reaction-rule", "release", "schedule", "script", "session", "trigger"}
	if got := projectBearingBuiltinTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("project-bearing builtin types = %v, want %v; if a new type declares `project`, check it names the bead's own project or add it to projectFieldNotHomeTypes", got, want)
	}
}

// A session registered with no labels (beadsapi RegisterSession when the
// project is only in fields) is born with project:<fields.project>.
func TestCreateBead_SessionDerivesProjectLabel(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "session:crew-a:now",
		Type:   "session",
		Kind:   "config",
		Fields: mustJSON(map[string]any{"agent": "crew-a", "project": "gardenio"}),
	})
	if err != nil {
		t.Fatalf("createBead: %v", err)
	}
	if !slices.Contains(ms.labels[bead.ID], "project:gardenio") {
		t.Fatalf("session labels = %v, want project:gardenio derived from fields.project", ms.labels[bead.ID])
	}
}

// The trigger engine's audit event (bridge/trigger_permissions.go) carries
// fields.project and no project label.
func TestCreateBead_TriggerAuditEventDerivesProjectLabel(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "trigger:slack fired x",
		Type:   "event",
		Status: "closed",
		Labels: []string{"auto:trigger-audit", "source:slack", "trigger:gd-t1"},
		Fields: mustJSON(map[string]any{"trigger_id": "gd-t1", "project": "gardenio", "outcome": "fired"}),
	})
	if err != nil {
		t.Fatalf("createBead: %v", err)
	}
	if !slices.Contains(ms.labels[bead.ID], "project:gardenio") {
		t.Fatalf("event labels = %v, want project:gardenio", ms.labels[bead.ID])
	}
}

func TestCreateBead_ContradictingProjectLabelRefused(t *testing.T) {
	s, _, _ := newTestServer()
	_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "session:crew-a:now",
		Type:   "session",
		Kind:   "config",
		Labels: []string{"project:other"},
		Fields: mustJSON(map[string]any{"agent": "crew-a", "project": "gardenio"}),
	})
	if err == nil || !strings.Contains(err.Error(), "does not match fields.project") {
		t.Fatalf("err = %v, want a refusal naming the mismatch", err)
	}
}

// The slack-bridge's two writes for a thread, in the shapes it sends them
// (bridge/thread_bead.go getOrCreateThreadBead, bot_thread_forward.go
// writeThreadConversationBead): the head is a task carrying project:<p>, and
// the reply is a message carrying thread_id=<head> and no project label. The
// reply must come out labelled with the head's project, or a thread agent's
// project-scoped token 403s on it.
func TestCreateBead_BridgeThreadHeadAndReplyCarryProjectLabel(t *testing.T) {
	s, ms, _ := newTestServer()
	ctx := bridgeCtx()

	head, err := s.createBead(ctx, createBeadInput{
		Title:          "Slack thread: help",
		Type:           "task",
		Kind:           "issue",
		Labels:         []string{"source:slack", "slack-thread:C1:1.2", "thread", "project:gardenio"},
		Fields:         mustJSON(map[string]string{"slack_thread_channel": "C1", "slack_thread_ts": "1.2", "spawn_source": "slack-thread"}),
		IdempotencyKey: "slack-thread:C1:1.2",
	})
	if err != nil {
		t.Fatalf("thread head: %v", err)
	}
	if !slices.Contains(ms.labels[head.ID], "project:gardenio") {
		t.Fatalf("head labels = %v, want project:gardenio", ms.labels[head.ID])
	}

	reply, err := s.createBead(ctx, createBeadInput{
		Title:  "can you look?",
		Type:   "message",
		Kind:   "data",
		Labels: []string{"slack-thread-message", "thread:" + head.ID},
		Fields: mustJSON(map[string]string{
			"text": "can you look?", "slack_thread_channel": "C1", "slack_thread_ts": "1.2",
			"thread_id": head.ID, "slack_user_id": "U1", "origin": "slack",
		}),
		IdempotencyKey: "slack-reply:C1:1.3",
	})
	if err != nil {
		t.Fatalf("thread reply: %v", err)
	}
	if got := ms.labels[reply.ID]; !slices.Contains(got, "project:gardenio") {
		t.Fatalf("reply labels = %v, want project:gardenio inherited from the thread head", got)
	}

	// The thread agent's project-scoped token can read the reply.
	agent := agentCtx("gd-agent1", "thread-a", "gardenio")
	if !beadMatchesTokenProject(agent, ms.labels[reply.ID]) {
		t.Fatalf("a gardenio-scoped token cannot see the reply (labels %v)", ms.labels[reply.ID])
	}
}

// A message whose head has no single project label, or that names no head,
// stays as it was written.
func TestCreateBead_MessageWithoutProjectHeadUnlabelled(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.beads["gd-head0"] = &model.Bead{ID: "gd-head0", Type: "task", Kind: model.KindIssue, Title: "h", Status: model.StatusOpen}
	ms.labels["gd-head0"] = []string{"thread"}
	ms.beads["gd-head2"] = &model.Bead{ID: "gd-head2", Type: "task", Kind: model.KindIssue, Title: "h", Status: model.StatusOpen}
	ms.labels["gd-head2"] = []string{"project:a", "project:b"}

	for _, head := range []string{"gd-head0", "gd-head2", "gd-missing", ""} {
		bead, err := s.createBead(bridgeCtx(), createBeadInput{
			Title:  "hi",
			Type:   "message",
			Kind:   "data",
			Labels: []string{"slack-thread-message"},
			Fields: mustJSON(map[string]string{"text": "hi", "thread_id": head}),
		})
		if err != nil {
			t.Fatalf("head %q: %v", head, err)
		}
		if hasProjectLabel(ms.labels[bead.ID]) {
			t.Fatalf("head %q: labels = %v, want no project label", head, ms.labels[bead.ID])
		}
	}
}

// An agent's own message (gb squawk) keeps its token's project, whatever
// thread it names.
func TestCreateBead_AgentMessageKeepsTokenProject(t *testing.T) {
	s, ms, _ := newTestServer()
	seedAgent(ms, "gd-agent1", "crew", "gardenio")
	ms.beads["gd-headx"] = &model.Bead{ID: "gd-headx", Type: "task", Kind: model.KindIssue, Title: "h", Status: model.StatusOpen}
	ms.labels["gd-headx"] = []string{"project:other"}

	bead, err := s.createBead(agentCtx("gd-agent1", "crew-a", "gardenio"), createBeadInput{
		Title:  "squawk",
		Type:   "message",
		Kind:   "data",
		Fields: mustJSON(map[string]string{"text": "squawk", "thread_id": "gd-headx"}),
	})
	if err != nil {
		t.Fatalf("createBead: %v", err)
	}
	if got := ms.labels[bead.ID]; !slices.Contains(got, "project:gardenio") || slices.Contains(got, "project:other") {
		t.Fatalf("labels = %v, want the token's project:gardenio only", got)
	}
}

// A scoped principal's message never inherits from a head it cannot read:
// the denial would name the head's project, so any thread_id would probe a
// bead's existence and project. An out-of-scope head and a missing head must
// get the same refusal, and neither may name the head's project. A head in
// the caller's own scope still lends its label.
func TestCreateBead_ScopedMessageDoesNotInheritUnreadableHead(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.beads["gd-secret"] = &model.Bead{ID: "gd-secret", Type: "task", Kind: model.KindIssue, Title: "h", Status: model.StatusOpen}
	ms.labels["gd-secret"] = []string{"project:secretland", "thread"}
	ms.beads["gd-own"] = &model.Bead{ID: "gd-own", Type: "task", Kind: model.KindIssue, Title: "h", Status: model.StatusOpen}
	ms.labels["gd-own"] = []string{"project:claimlayer", "thread"}
	ctx := scopedCtx(t, []string{"/project-claimlayer"})

	create := func(head string) (*model.Bead, error) {
		return s.createBead(ctx, createBeadInput{
			Title:  "hi",
			Type:   "message",
			Kind:   "data",
			Fields: mustJSON(map[string]string{"text": "hi", "thread_id": head}),
		})
	}

	_, errSecret := create("gd-secret")
	_, errMissing := create("gd-missing")
	if errSecret == nil || errMissing == nil {
		t.Fatalf("out-of-scope head err = %v, missing head err = %v; want both refused", errSecret, errMissing)
	}
	if strings.Contains(errSecret.Error(), "secretland") {
		t.Fatalf("out-of-scope head denial names its project: %v", errSecret)
	}
	if errSecret.Error() != errMissing.Error() {
		t.Fatalf("out-of-scope head denial %q differs from missing head denial %q", errSecret, errMissing)
	}

	bead, err := create("gd-own")
	if err != nil {
		t.Fatalf("in-scope head: %v", err)
	}
	if got := ms.labels[bead.ID]; !slices.Contains(got, "project:claimlayer") {
		t.Fatalf("in-scope head: labels = %v, want project:claimlayer inherited", got)
	}
}

// The lint counts what the invariant would fix, the backfill fixes it, and a
// re-run finds nothing (gd-YdsEpnN3Qo).
func TestProjectLabelLintAndBackfill(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-ag1"] = &model.Bead{ID: "gd-ag1", Type: "agent", Fields: json.RawMessage(`{"project":"gardenio","role":"infra"}`)}
	ms.beads["gd-ag2"] = &model.Bead{ID: "gd-ag2", Type: "agent", Fields: json.RawMessage(`{"project":"gardenio"}`)}
	ms.labels["gd-ag2"] = []string{"project:gardenio"}
	ms.beads["gd-ses"] = &model.Bead{ID: "gd-ses", Type: "session", Fields: json.RawMessage(`{"project":"gardenio"}`)}
	ms.beads["gd-hd"] = &model.Bead{ID: "gd-hd", Type: "task"}
	ms.labels["gd-hd"] = []string{"project:gardenio", "thread"}
	ms.beads["gd-msg"] = &model.Bead{ID: "gd-msg", Type: "message", Fields: json.RawMessage(`{"thread_id":"gd-hd"}`)}
	ms.beads["gd-plot"] = &model.Bead{ID: "gd-plot", Type: typePlotApplication, Fields: json.RawMessage(`{"project":"newland"}`)}

	c := newProjectLabelLintCollector(ms, 0)
	c.refresh(context.Background())
	if want := map[string]int{"agent": 1, "session": 1, "message": 1}; !reflect.DeepEqual(c.counts, want) {
		t.Fatalf("lint counts = %v, want %v", c.counts, want)
	}

	var dry BackfillProjectLabelsResponse
	rec := doJSON(t, h, http.MethodPost, "/v1/admin/backfill-project-labels", map[string]any{"dry_run": true})
	requireStatus(t, rec, http.StatusOK)
	_ = json.Unmarshal(rec.Body.Bytes(), &dry)
	if dry.Missing["agent"] != 1 || len(ms.labels["gd-ag1"]) != 0 {
		t.Fatalf("dry run = %+v (agent labels %v), want a count and no write", dry, ms.labels["gd-ag1"])
	}

	rec = doJSON(t, h, http.MethodPost, "/v1/admin/backfill-project-labels", map[string]any{"batch_size": 2})
	requireStatus(t, rec, http.StatusOK)
	var page BackfillProjectLabelsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Done {
		t.Fatalf("first page of 2 with 3 candidates reported done: %+v", page)
	}
	rec = doJSON(t, h, http.MethodPost, "/v1/admin/backfill-project-labels", map[string]any{"batch_size": 2})
	requireStatus(t, rec, http.StatusOK)
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if !page.Done {
		t.Fatalf("second page = %+v, want done", page)
	}
	for _, id := range []string{"gd-ag1", "gd-ses", "gd-msg"} {
		if !slices.Contains(ms.labels[id], "project:gardenio") {
			t.Errorf("%s labels = %v, want project:gardenio", id, ms.labels[id])
		}
	}
	if hasProjectLabel(ms.labels["gd-plot"]) {
		t.Errorf("plot_application got a project label: %v", ms.labels["gd-plot"])
	}

	c.refresh(context.Background())
	if len(c.counts) != 0 {
		t.Fatalf("lint after backfill = %v, want empty", c.counts)
	}
}

// An agent token cannot run the backfill.
func TestBackfillProjectLabels_AdminOnly(t *testing.T) {
	_, _, h := newClassedTestServer(t)
	rec := doBearerJSON(t, h, http.MethodPost, "/v1/admin/backfill-project-labels", testAgentToken, map[string]any{"dry_run": true}, nil)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("agent token: status %d, want 401/403", rec.Code)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestProjectLabelUpdateError(t *testing.T) {
	projectTC := &model.TypeConfig{Fields: []model.FieldDef{{Name: "project", Type: model.FieldTypeString}}}
	plainTC := &model.TypeConfig{Fields: []model.FieldDef{{Name: "text", Type: model.FieldTypeString}}}
	cases := []struct {
		name          string
		tc            *model.TypeConfig
		prior, labels []string
		fields        string
		refused       bool
	}{
		{"label matches", projectTC, []string{"project:a"}, []string{"project:a"}, `{"project":"a"}`, false},
		{"fields.project repointed", projectTC, []string{"project:a"}, []string{"project:a"}, `{"project":"gardenio"}`, true},
		{"label and fields moved together", projectTC, []string{"project:a"}, []string{"project:b"}, `{"project":"b"}`, false},
		{"label moved, fields not", projectTC, []string{"project:a"}, []string{"project:b"}, `{"project":"a"}`, true},
		{"label removed", projectTC, []string{"project:a"}, nil, `{"project":"a"}`, true},
		{"legacy label-less stays", projectTC, nil, []string{"x"}, `{"project":"a"}`, false},
		{"unknown placeholder stays", projectTC, []string{"project:unknown"}, []string{"project:unknown"}, `{"project":"a"}`, false},
		{"fields.project cleared", projectTC, []string{"project:a"}, []string{"project:a"}, `{}`, false},
		{"type without a project field", plainTC, []string{"project:a"}, []string{"project:a"}, `{"project":"b"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := projectLabelUpdateError(context.Background(), "reaction-rule", tc.tc, tc.prior, tc.labels, json.RawMessage(tc.fields))
			var ie inputError
			if tc.refused != (err != nil) || (err != nil && !errors.As(err, &ie)) {
				t.Fatalf("err = %v, refused want %v", err, tc.refused)
			}
		})
	}
	t.Setenv("BEADS_PROJECT_LABEL_MISMATCH", "warn")
	if err := projectLabelUpdateError(context.Background(), "reaction-rule", projectTC, []string{"project:a"}, []string{"project:a"}, json.RawMessage(`{"project":"b"}`)); err != nil {
		t.Fatalf("warn mode: %v, want no error", err)
	}
}

// A reaction-rule written in project a cannot be repointed at gardenio by
// fields.project, nor lose its label, by PATCH or by the label routes; the
// slack-bridge spawns in fields.project (gd-bRLiEzvMsy). The check holds for
// every caller, an operator included.
func TestUpdateBead_ProjectLabelInvariant(t *testing.T) {
	s, _, _ := newTestServer()
	ctx := ctxAsTestSubject()
	rule, err := s.createBead(ctx, createBeadInput{
		Title:  "eyes rule",
		Type:   "reaction-rule",
		Kind:   "config",
		Labels: []string{"project:a"},
		Fields: mustJSON(map[string]any{"emoji": "eyes", "prompt": "p", "project": "a"}),
	})
	if err != nil {
		t.Fatalf("createBead: %v", err)
	}
	if _, err := s.updateBead(ctx, rule.ID, updateBeadInput{Fields: mustJSON(map[string]any{"project": "gardenio", "role": "crew"})}); err == nil || !strings.Contains(err.Error(), "does not match fields.project") {
		t.Fatalf("repoint fields.project: err = %v, want a refusal", err)
	}
	if _, err := s.updateBead(ctx, rule.ID, updateBeadInput{Labels: []string{"other"}, labelsSet: true}); err == nil || !strings.Contains(err.Error(), "needs its label") {
		t.Fatalf("drop the project label: err = %v, want a refusal", err)
	}
	if _, err := s.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: rule.ID, Label: "project:a"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("RemoveLabel project:a: err = %v, want InvalidArgument", err)
	}
	if _, err := s.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: rule.ID, Label: "project:gardenio"}); err != nil {
		t.Fatalf("AddLabel project:gardenio beside the matching label: %v", err)
	}
	if _, err := s.updateBead(ctx, rule.ID, updateBeadInput{Fields: mustJSON(map[string]any{"prompt": "q"})}); err != nil {
		t.Fatalf("unrelated patch: %v", err)
	}
	if _, err := s.updateBead(ctx, rule.ID, updateBeadInput{Labels: []string{"project:b"}, labelsSet: true, Fields: mustJSON(map[string]any{"project": "b"})}); err != nil {
		t.Fatalf("move label and fields.project together: %v", err)
	}
}

// The single-field routes write the key straight to the store, so they may
// not set fields.project on a project-bearing bead at all, for any caller.
func TestProjectFieldEndpoints_Refused(t *testing.T) {
	f := newScopedFixture(t)
	id := "gd-pl-agent"
	f.seedTyped(id, "agent", "project:claimlayer")
	f.ms.beads[id].Fields = json.RawMessage(`{"project":"claimlayer","role":"crew"}`)
	for _, c := range []struct {
		name, path string
		body       any
	}{
		{"csv edit", "/v1/beads/" + id + "/fields/project/edit", map[string]any{"op": "remove", "element": "claimlayer"}},
		{"close body", "/v1/beads/" + id + "/close", map[string]any{"project": "gardenio"}},
	} {
		code, body := f.do(t, tokOwnerAdmin, http.MethodPost, c.path, c.body)
		if code != http.StatusBadRequest || !strings.Contains(body, "written with a fields update") {
			t.Errorf("%s: %d %.200s, want 400 pointing at a fields update", c.name, code, body)
		}
	}
	if got := projectFromFields(f.ms.beads[id].Fields); got != "claimlayer" {
		t.Fatalf("fields.project = %q after refusals, want claimlayer", got)
	}
}
