package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// allWritableTypeCandidates is every bead type the server knows: the daemon's
// built-in type registry plus the model.BeadType constants (some, such as
// epic and chore, are not registry entries). TestActionTable_AllTypesAllTiers
// checks every one of them against the reviewed decisions for every tier, so
// a new type is denied to every tier until someone decides otherwise.
func allWritableTypeCandidates() []model.BeadType {
	seen := map[model.BeadType]bool{}
	var out []model.BeadType
	add := func(t model.BeadType) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, n := range BuiltinBeadTypeNames() {
		add(model.BeadType(n))
	}
	for _, t := range []model.BeadType{
		model.TypeEpic, model.TypeTask, model.TypeFeature, model.TypeChore, model.TypeBug,
		model.TypeAdvice, model.TypeJack, model.TypeDecision, model.TypeEscalation,
		model.TypeReport, model.TypeFormula, model.TypeInvestigationRequest,
		model.TypeScrubRequest, model.TypeFleet, model.TypeProject, model.TypeAccount,
		model.TypeTeam, model.TypeMolecule, model.TypeRichReport, model.TypeBundle,
	} {
		add(t)
	}
	// Types the task names explicitly; they are registry entries today, listed
	// so a rename of one cannot silently drop it from the table.
	for _, n := range []string{"config", "role", "spawn_policy", "trigger", "user", "operator", "taint", "agent"} {
		add(model.BeadType(n))
	}
	return out
}

type scopedWriteCell struct{ name, label string }

var scopedWriteCells = []scopedWriteCell{
	{"own", "project:claimlayer"},
	{"other", "project:gardenio"},
	{"nolabel", ""},
}

func (f *scopedFixture) seedTyped(id string, typ model.BeadType, label string) {
	f.ms.beads[id] = &model.Bead{ID: id, Title: id, Type: typ, Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
	if label != "" {
		f.ms.labels[id] = []string{label}
	}
}

func (f *scopedFixture) do(t *testing.T, token, method, path string, body any) (int, string) {
	t.Helper()
	resp := oidcE2ERequest(t, f.ts, method, path, token, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The admin positive control: the scoped gate never refuses an admin, and an
// operator (not a scoped principal) is unchanged. Re-seeded per cell because
// the close/delete above consumed the scoped principal's beads.
func TestScopedWrites_AdminAndOperatorUnchanged(t *testing.T) {
	f := newScopedFixture(t)
	for _, tok := range []string{tokOwnerAdmin, tokOperator} {
		for _, cell := range scopedWriteCells {
			id := "ctl-" + tok + "-" + cell.name
			f.seedTyped(id, model.TypeTask, cell.label)
			var labels []string
			if cell.label != "" {
				labels = []string{cell.label}
			}
			if code, body := f.do(t, tok, http.MethodPost, "/v1/beads", map[string]any{"title": id, "type": "task", "labels": labels}); code != http.StatusCreated {
				t.Errorf("%s create in %s: %d %s", tok, cell.name, code, body)
			}
			if code, body := f.do(t, tok, http.MethodPatch, "/v1/beads/"+id, map[string]any{"description": "d"}); code != http.StatusOK {
				t.Errorf("%s update in %s: %d %s", tok, cell.name, code, body)
			}
			if code, body := f.do(t, tok, http.MethodPost, "/v1/beads/"+id+"/close", map[string]any{}); code >= 300 {
				t.Errorf("%s close in %s: %d %s", tok, cell.name, code, body)
			}
		}
	}
	// A non-allowlisted type is still writable by an admin in any project.
	f.seedTyped("ctl-adv", model.TypeAdvice, "project:gardenio")
	if code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/ctl-adv", map[string]any{"description": "d"}); code != http.StatusOK {
		t.Errorf("admin update advice: %d %s", code, body)
	}
}

func TestScopedWrites_CreateWithoutProjectLabelDenied(t *testing.T) {
	f := newScopedFixture(t)
	code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads", map[string]any{"title": "x", "type": "task"})
	if code != http.StatusForbidden {
		t.Fatalf("create with no project label: %d %s, want 403", code, body)
	}
	if !strings.Contains(body, "no project label") {
		t.Errorf("denial should say why: %s", body)
	}
	// A label in scope plus one out of scope is still a deny.
	code, _ = f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads", map[string]any{
		"title": "x", "type": "task", "labels": []string{"project:claimlayer", "project:gardenio"},
	})
	if code != http.StatusForbidden {
		t.Errorf("create with a mixed project label set: %d, want 403", code)
	}
	// fields.project cannot launder a bead out of the principal's projects. The
	// task schema rejects the key with a 400 over HTTP, so pin the check itself.
	ctx := scopedCtx(t, []string{"/project-claimlayer"})
	if d := scopedFieldsProjectDenial(ctx, "create", model.TypeTask, []byte(`{"project":"gardenio"}`)); d == nil {
		t.Error("fields.project=gardenio not denied")
	}
	if d := scopedFieldsProjectDenial(ctx, "create", model.TypeTask, []byte(`{"project":"claimlayer"}`)); d != nil {
		t.Errorf("fields.project=claimlayer denied: %v", d)
	}
	if d := scopedFieldsProjectDenial(ctx, "create", model.TypeTask, nil); d != nil {
		t.Errorf("no fields denied: %v", d)
	}
}

func TestScopedWrites_RelabelOutOfProjectDenied(t *testing.T) {
	f := newScopedFixture(t)
	// PATCH labels from claimlayer to gardenio.
	code, body := f.do(t, tokScopedWriter, http.MethodPatch, "/v1/beads/"+beadClaimlayer, map[string]any{
		"labels": []string{"project:gardenio"},
	})
	if code != http.StatusForbidden {
		t.Errorf("relabel via PATCH: %d %s, want 403", code, body)
	}
	// PATCH that drops the project label.
	code, _ = f.do(t, tokScopedWriter, http.MethodPatch, "/v1/beads/"+beadClaimlayer, map[string]any{"labels": []string{"x"}})
	if code != http.StatusForbidden {
		t.Errorf("dropping the project label via PATCH: %d, want 403", code)
	}
	// Label endpoints: add an out-of-scope project label, remove the own one.
	code, _ = f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads/"+beadClaimlayer+"/labels", map[string]any{"label": "project:gardenio"})
	if code != http.StatusForbidden {
		t.Errorf("add out-of-scope project label: %d, want 403", code)
	}
	code, _ = f.do(t, tokScopedWriter, http.MethodDelete, "/v1/beads/"+beadClaimlayer+"/labels/project:claimlayer", nil)
	if code != http.StatusForbidden {
		t.Errorf("remove own project label: %d, want 403", code)
	}
	// fields.project moved out of scope.
	code, _ = f.do(t, tokScopedWriter, http.MethodPatch, "/v1/beads/"+beadClaimlayer, map[string]any{
		"fields": map[string]any{"project": "gardenio"},
	})
	if code != http.StatusForbidden {
		t.Errorf("fields.project out of scope via PATCH: %d, want 403", code)
	}
	if got := f.ms.labels[beadClaimlayer]; !slices.Equal(got, []string{"project:claimlayer"}) {
		t.Errorf("labels changed despite denials: %v", got)
	}
	// Controls: a non-project label and an in-scope relabel still work.
	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads/"+beadClaimlayer+"/labels", map[string]any{"label": "area:x"}); code >= 300 {
		t.Errorf("add plain label: %d %s", code, body)
	}
	if code, body := f.do(t, tokOwnerAdmin, http.MethodPost, "/v1/beads/"+beadClaimlayer+"/labels", map[string]any{"label": "project:gardenio"}); code >= 300 {
		t.Errorf("admin add project label: %d %s", code, body)
	}
}

func TestScopedWrites_LinkAcrossProjectsDenied(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("cl-2", model.TypeTask, "project:claimlayer")
	dep := func(token, from, to string) int {
		code, _ := f.do(t, token, http.MethodPost, "/v1/beads/"+from+"/dependencies", map[string]any{"depends_on_id": to, "type": "blocks"})
		return code
	}
	if c := dep(tokScopedWriter, beadClaimlayer, beadGardenio); c != http.StatusNotFound {
		t.Errorf("cl-1 -> gd-1: %d, want 404 (target out of project: as missing)", c)
	}
	if c := dep(tokScopedWriter, beadGardenio, beadClaimlayer); c != http.StatusNotFound {
		t.Errorf("gd-1 -> cl-1: %d, want 404 (source out of project: as missing)", c)
	}
	if c := dep(tokScopedWriter, beadClaimlayer, beadNoLabel); c != http.StatusNotFound {
		t.Errorf("cl-1 -> nl-1: %d, want 404 (target unlabeled: as missing)", c)
	}
	if c := dep(tokScopedWriter, beadClaimlayer, "cl-2"); c == http.StatusForbidden {
		t.Errorf("cl-1 -> cl-2 (same project) denied, want allowed")
	}
	if c := dep(tokOwnerAdmin, beadClaimlayer, beadGardenio); c == http.StatusForbidden {
		t.Errorf("admin cl-1 -> gd-1 denied, want allowed")
	}
}

func TestScopedWrites_Comments(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("cl-esc", model.TypeEscalation, "project:claimlayer")
	f.seedTyped("cl-cfg", "config", "project:claimlayer")
	comment := func(id string) int {
		code, _ := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads/"+id+"/comments", map[string]any{"text": "hi"})
		return code
	}
	if c := comment(beadClaimlayer); c == http.StatusForbidden {
		t.Errorf("comment on own task denied")
	}
	// Out of its projects a bead answers as missing (gd-7APTAIhdC5); in them,
	// a type it may not comment on is refused.
	for id, want := range map[string]int{
		beadGardenio: http.StatusNotFound, beadNoLabel: http.StatusNotFound, beadOther: http.StatusNotFound,
		"cl-esc": http.StatusForbidden, "cl-cfg": http.StatusForbidden,
	} {
		if c := comment(id); c != want {
			t.Errorf("comment on %s: %d, want %d", id, c, want)
		}
	}
}

// Decisions are respond-only: resolve/cancel in the project work, an
// escalation verb or a decision in another project does not.
func TestScopedWrites_ResolutionVerbs(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("cl-dec", model.TypeDecision, "project:claimlayer")
	f.seedTyped("gd-dec", model.TypeDecision, "project:gardenio")
	f.seedTyped("cl-esc", model.TypeEscalation, "project:claimlayer")

	if c, b := f.do(t, tokScopedWriter, http.MethodPost, "/v1/decisions/gd-dec/resolve", map[string]any{"selected_option": "a"}); c != http.StatusNotFound {
		t.Errorf("resolve other-project decision: %d %s, want 404", c, b)
	}
	if c, b := f.do(t, tokScopedWriter, http.MethodPost, "/v1/escalations/cl-esc/reject", map[string]any{}); c != http.StatusForbidden {
		t.Errorf("reject own-project escalation: %d %s, want 403", c, b)
	}
	if c, b := f.do(t, tokScopedWriter, http.MethodPost, "/v1/decisions/cl-dec/resolve", map[string]any{"selected_option": "a"}); c != http.StatusOK {
		t.Errorf("resolve own-project decision: %d %s, want 200", c, b)
	}
}

// A scoped principal with the operator role may spawn only into its projects,
// whatever the user bead's allowed_projects says.
func TestScopedWrites_SpawnProjectInTokenProjects(t *testing.T) {
	f := newScopedFixture(t)
	makeUserBead(f.ms, "gd-user-clw", "clw@example.com", "*", "")
	ctx := scopedCtx(t, []string{"/project-claimlayer"})
	for project, wantErr := range map[string]bool{"claimlayer": false, "gardenio": true, "": true} {
		err := authorizeAgentSpawn(ctx, f.ms, []byte(`{"project":"`+project+`","role":"crew"}`))
		if (err != nil) != wantErr {
			t.Errorf("spawn project %q: err=%v, wantErr=%v", project, err, wantErr)
		}
	}
}

func scopedCtx(t *testing.T, groups []string) context.Context {
	t.Helper()
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleViewer, RoleOperator, RoleProjectMember})
	ctx = WithGroups(ctx, groups)
	return WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: "clw@example.com"})
}

// fieldEndpointCalls are the four field-mutation routes. They share
// tokenAuthorizedForBead, which must apply the same scoped type/project gate as
// PATCH (gd-HLmbBqDKRv).
func fieldEndpointCalls(id string) []struct {
	name, path string
	body       any
} {
	base := "/v1/beads/" + id
	return []struct {
		name, path string
		body       any
	}{
		{"increment", base + "/increment", map[string]float64{"n": 1}},
		{"append", base + "/fields/steps/append", map[string]any{"element": "a"}},
		{"reorder", base + "/fields/steps/reorder", map[string]any{"expected": []string{}, "next": []string{}}},
		{"edit", base + "/fields/prompt/edit", map[string]any{"op": "set", "key": "x", "value": "y"}},
	}
}

func TestScopedWrites_FieldEndpoints(t *testing.T) {
	f := newScopedFixture(t)
	// Non-allowlisted types in the caller's own project: the type gate must
	// deny and name the type, exactly as PATCH does.
	for _, typ := range []model.BeadType{"trigger", "spawn_policy", "config", model.TypeAdvice, "role"} {
		id := "fe-deny-" + string(typ)
		f.seedTyped(id, typ, "project:claimlayer")
		for _, c := range fieldEndpointCalls(id) {
			code, body := f.do(t, tokScopedWriter, http.MethodPost, c.path, c.body)
			if code != http.StatusForbidden || !strings.Contains(body, "scoped principal may not update beads of type "+string(typ)) {
				t.Errorf("%s on %s: status %d %s, want 403 naming the type", c.name, typ, code, body)
			}
		}
		if _, ok := f.ms.beads[id]; ok && len(f.ms.beads[id].Fields) != 0 {
			t.Errorf("%s: denied field writes still changed fields: %s", typ, f.ms.beads[id].Fields)
		}
	}
	// Allowlisted type in the caller's own project: not refused by the scoped gate.
	for _, typ := range []model.BeadType{model.TypeTask, model.TypeBug, model.TypeFeature} {
		id := "fe-ok-" + string(typ)
		f.seedTyped(id, typ, "project:claimlayer")
		for _, c := range fieldEndpointCalls(id) {
			code, body := f.do(t, tokScopedWriter, http.MethodPost, c.path, c.body)
			if code == http.StatusForbidden || strings.Contains(body, "scoped principal") {
				t.Errorf("%s on in-project %s: status %d %s, want not the scoped 403", c.name, typ, code, body)
			}
		}
	}
	// Allowlisted type in another project, and with no project label: answered
	// as a missing bead (gd-7APTAIhdC5).
	for _, cell := range []scopedWriteCell{{"other", "project:gardenio"}, {"nolabel", ""}} {
		id := "fe-" + cell.name
		f.seedTyped(id, model.TypeTask, cell.label)
		for _, c := range fieldEndpointCalls(id) {
			if code, body := f.do(t, tokScopedWriter, http.MethodPost, c.path, c.body); code != http.StatusNotFound {
				t.Errorf("%s on %s task: status %d %s, want 404", c.name, cell.name, code, body)
			}
		}
	}
	// Positive control: an admin is never refused by the scoped gate.
	f.seedTyped("fe-admin", "trigger", "project:claimlayer")
	for _, c := range fieldEndpointCalls("fe-admin") {
		code, body := f.do(t, tokOwnerAdmin, http.MethodPost, c.path, c.body)
		if code == http.StatusForbidden || strings.Contains(body, "scoped principal") {
			t.Errorf("admin %s on trigger: status %d %s, want not the scoped 403", c.name, code, body)
		}
	}
}

// A message is create-only for a scoped principal, plus a reactions-only
// update that touches nothing but its own entries (Lands spec §7.3 step 4.1).
func TestScopedWrites_MessageCreateOnlyPlusOwnReactions(t *testing.T) {
	f := newScopedFixture(t)
	const self = "oidc:clw@example.com"
	seed := func(id, label, fields string) {
		f.seedTyped(id, "message", label)
		closedAt := time.Now().UTC()
		f.ms.beads[id].Status, f.ms.beads[id].ClosedAt = model.StatusClosed, &closedAt
		if fields != "" {
			f.ms.beads[id].Fields = []byte(fields)
		}
	}
	react := func(token, id string, reactions any) (int, string) {
		return f.do(t, token, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"reactions": reactions}})
	}

	// Create, already closed, as the Commons writes a human's message.
	code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads", map[string]any{
		"title": "hi", "type": "message", "status": "closed",
		"labels": []string{"project:claimlayer"}, "fields": map[string]any{"text": "hi"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create closed message in own project: %d %s, want 201", code, body)
	}
	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads", map[string]any{
		"title": "hi", "type": "message", "labels": []string{"project:gardenio"},
	}); code != http.StatusForbidden {
		t.Errorf("create message in other project: %d %s, want 403", code, body)
	}

	// Someone else's message in the own project: adding and removing its own
	// reaction is allowed, whoever wrote the message.
	seed("m-own", "project:claimlayer", `{"text":"x","reactions":{"eyes":["oidc:op@example.com"]}}`)
	f.ms.beads["m-own"].CreatedBy = "oidc:op@example.com"
	f.ms.beads["m-own"].OperatedBy = "oidc:op@example.com"
	if code, body := react(tokScopedWriter, "m-own", map[string][]string{"eyes": {"oidc:op@example.com", self}, "+1": {self}}); code != http.StatusOK {
		t.Fatalf("add own reactions: %d %s, want 200", code, body)
	}
	if got := string(f.ms.beads["m-own"].Fields); !strings.Contains(got, self) || !strings.Contains(got, `"text":"x"`) {
		t.Errorf("reactions not merged into fields: %s", got)
	}
	if got := f.ms.beads["m-own"].OperatedBy; got != "oidc:op@example.com" {
		t.Errorf("reacting re-attributed the message to %q, want oidc:op@example.com", got)
	}
	if code, body := react(tokScopedWriter, "m-own", map[string][]string{"eyes": {"oidc:op@example.com"}}); code != http.StatusOK {
		t.Errorf("remove own reactions: %d %s, want 200", code, body)
	}

	// Anyone else's entry is off limits: forging, erasing, or a stale map.
	for name, reactions := range map[string]map[string][]string{
		"forge another principal": {"eyes": {"oidc:op@example.com", "oidc:someone@example.com"}},
		"erase another principal": {"eyes": {}},
		"drop another's reaction": {"+1": {self}},
	} {
		if code, body := react(tokScopedWriter, "m-own", reactions); code != http.StatusForbidden || !strings.Contains(body, "only its own reactions") {
			t.Errorf("%s: %d %s, want 403 naming the rule", name, code, body)
		}
	}
	if code, body := react(tokScopedWriter, "m-own", "not-a-map"); code != http.StatusForbidden {
		t.Errorf("malformed reactions: %d %s, want 403", code, body)
	}

	// Anything beyond reactions is an update, which a message does not allow:
	// text, reactions plus text, labels, description, close, delete, comment,
	// and the field endpoints.
	denied := []struct {
		name, method, path string
		body               any
	}{
		{"edit text", http.MethodPatch, "/v1/beads/m-own", map[string]any{"fields": map[string]any{"text": "edited"}}},
		{"reactions plus text", http.MethodPatch, "/v1/beads/m-own", map[string]any{"fields": map[string]any{"text": "edited", "reactions": map[string][]string{"eyes": {"oidc:op@example.com", self}}}}},
		{"reactions plus description", http.MethodPatch, "/v1/beads/m-own", map[string]any{"description": "d", "fields": map[string]any{"reactions": map[string][]string{"eyes": {"oidc:op@example.com", self}}}}},
		{"no-op reactions", http.MethodPatch, "/v1/beads/m-own", map[string]any{"fields": map[string]any{"reactions": map[string][]string{"eyes": {"oidc:op@example.com"}}}}},
		{"reopen", http.MethodPatch, "/v1/beads/m-own", map[string]any{"status": "open"}},
		{"comment", http.MethodPost, "/v1/beads/m-own/comments", map[string]any{"text": "hi"}},
		{"close", http.MethodPost, "/v1/beads/m-own/close", map[string]any{}},
		{"delete", http.MethodDelete, "/v1/beads/m-own", nil},
		{"field edit endpoint", http.MethodPost, "/v1/beads/m-own/fields/reactions/edit", map[string]any{"op": "set", "key": "eyes", "value": self}},
	}
	for _, d := range denied {
		if code, body := f.do(t, tokScopedWriter, d.method, d.path, d.body); code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", d.name, code, body)
		}
	}
	if got := string(f.ms.beads["m-own"].Fields); strings.Contains(got, "edited") || strings.Contains(got, self) {
		t.Errorf("denied writes changed the message: %s", got)
	}

	// A message in another project or with no project label: no reactions, and
	// it answers as a missing one (gd-7APTAIhdC5).
	seed("m-other", "project:gardenio", "")
	seed("m-nolabel", "", "")
	for _, id := range []string{"m-other", "m-nolabel"} {
		if code, body := react(tokScopedWriter, id, map[string][]string{"eyes": {self}}); code != http.StatusNotFound {
			t.Errorf("react on %s: %d %s, want 404", id, code, body)
		}
	}

	// Positive control: an admin still edits any message field.
	if code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/m-other", map[string]any{"fields": map[string]any{"text": "admin", "reactions": map[string][]string{"eyes": {"oidc:x@example.com"}}}}); code != http.StatusOK {
		t.Errorf("admin edit message: %d %s, want 200", code, body)
	}
}

// scopedUpdateAction maps only a scoped principal's reactions-only message
// patch to the react action.
func TestScopedUpdateAction(t *testing.T) {
	scoped := scopedCtx(t, []string{"/project-claimlayer"})
	reactions := json.RawMessage(`{"reactions":{"eyes":["oidc:clw@example.com"]}}`)
	msg := &model.Bead{Type: "message"}
	task := &model.Bead{Type: model.TypeTask}
	desc := "d"
	for _, c := range []struct {
		name string
		ctx  context.Context
		bead *model.Bead
		in   updateBeadInput
		want string
	}{
		{"scoped reactions on message", scoped, msg, updateBeadInput{Fields: reactions}, scopedActionReact},
		{"scoped reactions on task", scoped, task, updateBeadInput{Fields: reactions}, "update"},
		{"scoped reactions plus description", scoped, msg, updateBeadInput{Fields: reactions, Description: &desc}, "update"},
		{"scoped other field", scoped, msg, updateBeadInput{Fields: json.RawMessage(`{"text":"x"}`)}, "update"},
		{"not scoped", context.Background(), msg, updateBeadInput{Fields: reactions}, "update"},
	} {
		if got := scopedUpdateAction(c.ctx, c.bead, &c.in); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
