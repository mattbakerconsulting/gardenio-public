package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// seedLandProject seeds claimlayer's project bead with land_frozen set to
// frozen (raw JSON: true, "true" or false).
func (f *scopedFixture) seedLandProject(t *testing.T, frozen string) {
	t.Helper()
	f.seedTyped("proj-claimlayer", "project", "")
	f.ms.beads["proj-claimlayer"].Title = "claimlayer"
	f.ms.beads["proj-claimlayer"].Kind = "config"
	f.ms.beads["proj-claimlayer"].Fields = []byte(`{"land_kind":"tenant","land_frozen":` + frozen + `}`)
}

// The Lands kill switch at the daemon (step 10.5): a frozen land takes no
// create from its members or its land principal, while the operator still
// writes; a thawed land takes them again.
func TestFrozenLandRefusesScopedCreates(t *testing.T) {
	f := newScopedFixture(t)
	f.seedChannel(t, "ch-general", "claimlayer", map[string]any{"name": "general", "channel_kind": "general"})
	f.seedLandProject(t, "true")

	for _, c := range []struct {
		name, token string
	}{
		{"member", tokScopedWriter},
		{"owner", tokScopedOwner},
		{"guest", tokScopedGuest},
		{"land principal", tokLandClaimlayer},
	} {
		if code, body := f.say(t, c.token, "ch-general"); code != http.StatusForbidden || !strings.Contains(body, "land claimlayer is frozen") {
			t.Errorf("%s says in a frozen land: %d %s, want 403 naming the freeze", c.name, code, body)
		}
	}
	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads", map[string]any{
		"title": "a wish", "type": "task", "labels": []string{"project:claimlayer"},
	}); code != http.StatusForbidden || !strings.Contains(body, "frozen") {
		t.Errorf("member files a task in a frozen land: %d %s, want 403", code, body)
	}
	// The operator is never scoped: they can still say why.
	if code, body := f.say(t, tokOwnerAdmin, "ch-general"); code != http.StatusCreated {
		t.Errorf("operator says in a frozen land: %d %s, want 201", code, body)
	}

	// A string "true" freezes too; false thaws.
	f.seedLandProject(t, `"true"`)
	if code, _ := f.say(t, tokScopedWriter, "ch-general"); code != http.StatusForbidden {
		t.Errorf("land_frozen=\"true\": %d, want 403", code)
	}
	f.seedLandProject(t, "false")
	if code, body := f.say(t, tokScopedWriter, "ch-general"); code != http.StatusCreated {
		t.Errorf("thawed land: %d %s, want 201", code, body)
	}
}

// A frozen land's people can still talk to us in its #support (Terms §9.1,
// gd-mJw9pxK2wu): a member's or the owner's post there is taken, joined to the
// channel or not, since nobody can join during a freeze. Nothing else is: not
// another channel, not another land's #support, not a card, not a task, not an
// archived #support, and not the land principal's post. A thawed land's
// #support is a user channel like any other.
func TestFrozenLandTakesSupportPosts(t *testing.T) {
	f := newScopedFixture(t)
	f.seedChannel(t, "ch-general", "claimlayer", map[string]any{"name": "general", "channel_kind": "general"})
	f.seedChannel(t, "ch-support", "claimlayer", map[string]any{"name": "support", "channel_kind": "user"})
	f.seedChannel(t, "ch-old-support", "claimlayer", map[string]any{"name": "support", "channel_kind": "user"})
	f.ms.beads["ch-old-support"].Status = model.StatusClosed
	f.seedChannel(t, "ch-other-support", "other", map[string]any{"name": "support", "channel_kind": "user"})
	f.seedLandProject(t, "true")

	for _, tok := range []string{tokScopedWriter, tokScopedOwner} {
		if code, body := f.say(t, tok, "ch-support"); code != http.StatusCreated {
			t.Errorf("%s posts in the frozen land's #support: %d %s, want 201", tok, code, body)
		}
	}
	if code, body := f.say(t, tokLandClaimlayer, "ch-support"); code != http.StatusForbidden || !strings.Contains(body, "land claimlayer is frozen") {
		t.Errorf("land principal posts in the frozen land's #support: %d %s, want 403 naming the freeze", code, body)
	}
	if code, body := f.say(t, tokScopedWriter, "ch-general"); code != http.StatusForbidden || !strings.Contains(body, "land claimlayer is frozen") {
		t.Errorf("member says in #general of the frozen land: %d %s, want 403 naming the freeze", code, body)
	}
	if code, body := f.say(t, tokScopedWriter, "ch-old-support"); code != http.StatusForbidden || !strings.Contains(body, "land claimlayer is frozen") {
		t.Errorf("member posts in an archived #support: %d %s, want 403 naming the freeze", code, body)
	}
	if code, body := f.say(t, tokScopedWriter, "ch-other-support"); code == http.StatusCreated {
		t.Errorf("member posts in another land's #support: %d %s, want it refused", code, body)
	}
	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads", map[string]any{
		"title": "a card", "type": "message", "status": "closed", "labels": []string{"project:claimlayer"},
		"fields": map[string]any{"kind": "card", "subject_id": beadClaimlayer, "channel_id": "ch-support"},
	}); code != http.StatusForbidden || !strings.Contains(body, "land claimlayer is frozen") {
		t.Errorf("member posts a card in the frozen land's #support: %d %s, want 403 naming the freeze", code, body)
	}
	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads", map[string]any{
		"title": "a wish", "type": "task", "labels": []string{"project:claimlayer"},
		"fields": map[string]any{"channel_id": "ch-support"},
	}); code != http.StatusForbidden || !strings.Contains(body, "frozen") {
		t.Errorf("member files a task naming #support in a frozen land: %d %s, want 403", code, body)
	}
	if code, body := f.setMembers(t, tokScopedWriter, "ch-support", []string{principalWriter}); code != http.StatusForbidden || !strings.Contains(body, "frozen") {
		t.Errorf("member joins #support in a frozen land: %d %s, want 403", code, body)
	}
	if code, body := f.say(t, tokOwnerAdmin, "ch-support"); code != http.StatusCreated {
		t.Errorf("operator posts in the frozen land's #support: %d %s, want 201", code, body)
	}

	// Thawed, #support is a user channel again: members post once joined.
	f.seedLandProject(t, "false")
	if code, body := f.say(t, tokScopedWriter, "ch-support"); code != http.StatusForbidden || !strings.Contains(body, "not a member of #support") {
		t.Errorf("non-member posts in a thawed land's #support: %d %s, want 403 naming membership", code, body)
	}
	if code, body := f.setMembers(t, tokScopedWriter, "ch-support", []string{principalWriter}); code != http.StatusOK {
		t.Fatalf("member joins #support in a thawed land: %d %s", code, body)
	}
	if code, body := f.say(t, tokScopedWriter, "ch-support"); code != http.StatusCreated {
		t.Errorf("member posts in a thawed land's #support: %d %s, want 201", code, body)
	}
}

// frozenWriteRoute is one write route a scoped principal can reach in its
// land, other than create. callers names the tokens whose tier may take it
// (actionTable), so the thawed run shows the freeze is what refuses it.
type frozenWriteRoute struct {
	name, method, path string
	body               any
	callers            []string
}

func frozenWriteRoutes() []frozenWriteRoute {
	both := []string{tokScopedWriter, tokLandClaimlayer}
	member := []string{tokScopedWriter}
	routes := []frozenWriteRoute{
		{"comment", http.MethodPost, "/v1/beads/fz-task/comments", map[string]any{"text": "hi"}, both},
		{"update", http.MethodPatch, "/v1/beads/fz-task", map[string]any{"description": "d"}, both},
		{"close", http.MethodPost, "/v1/beads/fz-task/close", map[string]any{}, both},
		{"add label", http.MethodPost, "/v1/beads/fz-task/labels", map[string]any{"label": "wish"}, both},
		{"add dependency", http.MethodPost, "/v1/beads/fz-task/dependencies", map[string]any{"depends_on_id": "fz-task2", "type": "related"}, both},
		{"react", http.MethodPatch, "/v1/beads/fz-msg", map[string]any{"fields": map[string]any{"reactions": map[string][]string{"+1": {principalWriter}}}}, member},
		{"join", http.MethodPatch, "/v1/beads/ch-user", map[string]any{"fields": map[string]any{"members": []string{principalWriter}}}, member},
		{"resolve decision", http.MethodPost, "/v1/decisions/fz-dec/resolve", map[string]any{"selected_option": "a"}, member},
		{"approve escalation", http.MethodPost, "/v1/escalations/fz-esc/approve", map[string]any{}, member},
	}
	for _, c := range fieldEndpointCalls("fz-task") {
		routes = append(routes, frozenWriteRoute{"field " + c.name, http.MethodPost, c.path, c.body, both})
	}
	return routes
}

// seedFrozenWriteTargets seeds one claimlayer bead for each frozenWriteRoute.
func (f *scopedFixture) seedFrozenWriteTargets(t *testing.T) {
	t.Helper()
	f.seedTyped("fz-task", model.TypeTask, "project:claimlayer")
	f.seedTyped("fz-task2", model.TypeTask, "project:claimlayer")
	f.seedTyped("fz-msg", typeMessage, "project:claimlayer")
	f.ms.beads["fz-msg"].Kind = "data"
	f.ms.beads["fz-msg"].Status = model.StatusClosed
	f.ms.beads["fz-msg"].Fields = []byte(`{"text":"hi","channel_id":"ch-general"}`)
	f.seedChannel(t, "ch-general", "claimlayer", map[string]any{"name": "general", "channel_kind": "general"})
	f.seedChannel(t, "ch-user", "claimlayer", map[string]any{"name": "design", "channel_kind": "user"})
	f.seedTyped("fz-dec", model.TypeDecision, "project:claimlayer")
	f.seedEscalation("fz-esc", "project:claimlayer", "create", taskSpec("claimlayer"), nil)
}

// Every other write route takes nothing from a scoped principal in a frozen
// land either (gd-vV9NSBoND2): before, only create checked the freeze, so a
// frozen land still took comments, reactions, joins, field writes, closes,
// labels, dependencies, answers and approvals. The same request in the thawed
// land is not refused, and the operator still writes in the frozen one.
func TestFrozenLandRefusesScopedWrites(t *testing.T) {
	for _, r := range frozenWriteRoutes() {
		for _, tok := range r.callers {
			t.Run(r.name+"/"+tok, func(t *testing.T) {
				f := newScopedFixture(t)
				f.seedFrozenWriteTargets(t)
				f.seedLandProject(t, "true")
				code, body := f.do(t, tok, r.method, r.path, r.body)
				if code != http.StatusForbidden || !strings.Contains(body, "land claimlayer is frozen") {
					t.Fatalf("frozen land: %d %s, want 403 naming the freeze", code, body)
				}
				if r.name == "approve escalation" && f.hasBeadTitled("replayed") {
					t.Error("a refused approval still replayed its write")
				}

				f = newScopedFixture(t)
				f.seedFrozenWriteTargets(t)
				f.seedLandProject(t, "false")
				if code, body := f.do(t, tok, r.method, r.path, r.body); code == http.StatusForbidden {
					t.Errorf("thawed land: %d %s, want it not refused", code, body)
				}
			})
		}
	}

	f := newScopedFixture(t)
	f.seedFrozenWriteTargets(t)
	f.seedLandProject(t, "true")
	for _, r := range frozenWriteRoutes() {
		if r.name == "react" || r.name == "join" {
			continue // the patches name the member's principal, not the operator's
		}
		if code, body := f.do(t, tokOwnerAdmin, r.method, r.path, r.body); code == http.StatusForbidden {
			t.Errorf("operator %s in a frozen land: %d %s, want it not refused", r.name, code, body)
		}
	}
}

// The gRPC twins of the routes above refuse a land member in a frozen land
// too. Land principals never reach them (TestLandPrincipal_GRPCRefused).
func TestFrozenLandRefusesScopedGRPCWrites(t *testing.T) {
	calls := []struct {
		name string
		call func(srv *BeadsServer, ctx context.Context) error
	}{
		{"AddComment", func(srv *BeadsServer, ctx context.Context) error {
			_, err := srv.AddComment(ctx, &beadsv1.AddCommentRequest{BeadId: "fz-task", Text: "hi"})
			return err
		}},
		{"UpdateBead", func(srv *BeadsServer, ctx context.Context) error {
			d := "d"
			_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "fz-task", Description: &d})
			return err
		}},
		{"CloseBead", func(srv *BeadsServer, ctx context.Context) error {
			_, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "fz-task"})
			return err
		}},
		{"AddLabel", func(srv *BeadsServer, ctx context.Context) error {
			_, err := srv.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: "fz-task", Label: "wish"})
			return err
		}},
		{"AddDependency", func(srv *BeadsServer, ctx context.Context) error {
			_, err := srv.AddDependency(ctx, &beadsv1.AddDependencyRequest{BeadId: "fz-task", DependsOnId: "fz-task2", Type: "related"})
			return err
		}},
		{"RemoveLabel", func(srv *BeadsServer, ctx context.Context) error {
			_, err := srv.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: "fz-task", Label: "wish"})
			return err
		}},
		{"RemoveDependency", func(srv *BeadsServer, ctx context.Context) error {
			_, err := srv.RemoveDependency(ctx, &beadsv1.RemoveDependencyRequest{BeadId: "fz-task", DependsOnId: "fz-task2", Type: "related"})
			return err
		}},
	}
	seed := func(ms *mockStore, frozen string) {
		seedBead(ms, "fz-task", model.TypeTask, model.StatusOpen, []string{"project:claimlayer"}, nil)
		seedBead(ms, "fz-task2", model.TypeTask, model.StatusOpen, []string{"project:claimlayer"}, nil)
		seedBead(ms, "proj-claimlayer", model.TypeProject, model.StatusOpen, nil, nil)
		ms.beads["proj-claimlayer"].Title = "claimlayer"
		ms.beads["proj-claimlayer"].Kind = "config"
		ms.beads["proj-claimlayer"].Fields = []byte(`{"land_frozen":` + frozen + `}`)
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			srv, ms, _ := testCtx(t)
			seed(ms, "true")
			err := c.call(srv, scopedCtx(t, []string{"/project-claimlayer"}))
			if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "land claimlayer is frozen") {
				t.Fatalf("frozen land: %v, want PermissionDenied naming the freeze", err)
			}

			srv, ms, _ = testCtx(t)
			seed(ms, "false")
			if err := c.call(srv, scopedCtx(t, []string{"/project-claimlayer"})); status.Code(err) == codes.PermissionDenied {
				t.Errorf("thawed land: %v, want it not refused", err)
			}
		})
	}

	// DeleteBead: no tier may delete (actionTable), so the tier gate refuses
	// it ahead of the freeze, frozen or thawed, and the bead stays.
	for _, frozen := range []string{"true", "false"} {
		srv, ms, _ := testCtx(t)
		seed(ms, frozen)
		_, err := srv.DeleteBead(scopedCtx(t, []string{"/project-claimlayer"}), &beadsv1.DeleteBeadRequest{Id: "fz-task"})
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("DeleteBead, land_frozen=%s: %v, want PermissionDenied", frozen, err)
		}
		if ms.beads["fz-task"] == nil {
			t.Errorf("DeleteBead, land_frozen=%s: a refused delete removed the bead", frozen)
		}
	}
}

// The writes whose target is not the bead written: a relabel or a link into
// a frozen land is refused while the bead itself sits in a thawed one.
func TestFrozenLandRefusesWritesInto(t *testing.T) {
	ms := newMockStore()
	seedBead(ms, "proj-claimlayer", model.TypeProject, model.StatusOpen, nil, nil)
	ms.beads["proj-claimlayer"].Title = "claimlayer"
	ms.beads["proj-claimlayer"].Fields = []byte(`{"land_frozen":true}`)
	seedBead(ms, "cl-target", model.TypeTask, model.StatusOpen, []string{"project:claimlayer"}, nil)
	srv := &BeadsServer{store: ms}
	ctx := scopedCtx(t, []string{"/project-claimlayer", "/project-other"})

	if d := scopedLabelChangeDenial(ctx, ms, model.TypeTask, "project:claimlayer", true); d == nil || !strings.Contains(d.Message, "frozen") {
		t.Errorf("label a bead into the frozen land: %+v, want the freeze", d)
	}
	if d := scopedLabelChangeDenial(ctx, ms, model.TypeTask, "project:other", true); d != nil {
		t.Errorf("label a bead into a thawed land: %+v", d)
	}
	if _, d := srv.scopedDependencyTargetDenial(ctx, "cl-target"); d == nil || !strings.Contains(d.Message, "frozen") {
		t.Errorf("link to a bead in the frozen land: %+v, want the freeze", d)
	}
}

// failingProjectStore fails the project lookup.
type failingProjectStore struct{ *mockStore }

func (failingProjectStore) FindProjectByName(context.Context, string) (*model.Bead, error) {
	return nil, errors.New("connection reset")
}

func TestFrozenLandDenial_FailsClosedAndIgnoresUnscoped(t *testing.T) {
	st := failingProjectStore{newMockStore()}
	if d := frozenLandDenial(context.Background(), st, "create", typeMessage, []string{"project:claimlayer"}); d != nil {
		t.Errorf("an unscoped caller is never checked: %+v", d)
	}
	if fr, err := landFrozen(context.Background(), newMockStore(), "nowhere"); err != nil || fr {
		t.Errorf("no such project: frozen %v err %v", fr, err)
	}
	if _, err := landFrozen(context.Background(), st, "claimlayer"); err == nil {
		t.Error("a failed lookup is an error, which frozenLandDenial refuses on")
	}
}

// landFrozen fails closed (gd-dMof1PW1oJ): project fields that do not parse,
// or a land_frozen that is neither a boolean nor "true"/"false", is an error,
// which frozenLandDenial refuses on. Absent and null read as not frozen.
func TestLandFrozen_FailsClosedOnBadValue(t *testing.T) {
	ctx := scopedCtx(t, []string{"/project-claimlayer"})
	for _, c := range []struct {
		fields       string
		frozen, fail bool
	}{
		{`{}`, false, false},
		{`{"land_frozen":null}`, false, false},
		{`{"land_frozen":false}`, false, false},
		{`{"land_frozen":"false"}`, false, false},
		{`{"land_frozen":true}`, true, false},
		{`{"land_frozen":" true "}`, true, false},
		{`{"land_frozen":1}`, false, true},
		{`{"land_frozen":"yes"}`, false, true},
		{`{"land_frozen":{"on":true}}`, false, true},
		{`{"land_frozen":[true]}`, false, true},
		{`{"land_frozen":`, false, true},
		{`["land_frozen"]`, false, true},
	} {
		ms := newMockStore()
		seedBead(ms, "proj-claimlayer", model.TypeProject, model.StatusOpen, nil, nil)
		ms.beads["proj-claimlayer"].Title = "claimlayer"
		ms.beads["proj-claimlayer"].Fields = []byte(c.fields)

		frozen, err := landFrozen(context.Background(), ms, "claimlayer")
		if frozen != c.frozen || (err != nil) != c.fail {
			t.Errorf("%s: frozen %v err %v, want frozen %v fail %v", c.fields, frozen, err, c.frozen, c.fail)
		}
		d := frozenLandDenial(ctx, ms, "create", model.TypeTask, []string{"project:claimlayer"})
		if refused := d != nil; refused != (c.frozen || c.fail) {
			t.Errorf("%s: frozenLandDenial = %+v, want refused %v", c.fields, d, c.frozen || c.fail)
		}
	}
}

// A scoped approver's replay is checked against the freeze of the land it
// writes into, not only the escalation's own land (gd-dMof1PW1oJ): before,
// the approval was recorded and the replay then refused, leaving the
// escalation approved but stuck.
func TestScopedReplayDenial_RefusesFrozenTargetLand(t *testing.T) {
	ms := newMockStore()
	seedBead(ms, "proj-claimlayer", model.TypeProject, model.StatusOpen, nil, nil)
	ms.beads["proj-claimlayer"].Title = "claimlayer"
	ms.beads["proj-claimlayer"].Fields = []byte(`{"land_frozen":false}`)
	seedBead(ms, "proj-other", model.TypeProject, model.StatusOpen, nil, nil)
	ms.beads["proj-other"].Title = "other"
	ms.beads["proj-other"].Fields = []byte(`{"land_frozen":true}`)
	seedBead(ms, "ot-task", model.TypeTask, model.StatusOpen, []string{"project:other"}, nil)
	seedBead(ms, "cl-task", model.TypeTask, model.StatusOpen, []string{"project:claimlayer"}, nil)
	srv := &BeadsServer{store: ms}
	ctx := scopedCtx(t, []string{"/project-claimlayer", "/project-other"})

	esc := func(opType string, spec map[string]any, target string) *model.Bead {
		fields := map[string]any{"operation_type": opType, "operation_spec": spec}
		if target != "" {
			fields["target_bead_id"] = target
		}
		raw, _ := json.Marshal(fields)
		return &model.Bead{ID: "esc", Type: model.TypeEscalation, Labels: []string{"project:claimlayer"}, Fields: raw}
	}
	for _, c := range []struct {
		name   string
		esc    *model.Bead
		frozen bool
	}{
		{"create into the frozen land", esc("create", taskSpec("other"), ""), true},
		{"update a bead in the frozen land", esc("update", map[string]any{"description": "d"}, "ot-task"), true},
		{"close a bead in the frozen land", esc("close", map[string]any{}, "ot-task"), true},
		{"relabel a bead into the frozen land", esc("update", map[string]any{"labels": []string{"project:other"}}, "cl-task"), true},
		{"create into the thawed land", esc("create", taskSpec("claimlayer"), ""), false},
		{"update a bead in the thawed land", esc("update", map[string]any{"description": "d"}, "cl-task"), false},
	} {
		msg, err := srv.scopedReplayDenial(ctx, c.esc)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := strings.Contains(msg, "land other is frozen"); got != c.frozen || (!c.frozen && msg != "") {
			t.Errorf("%s: denial %q, want frozen=%v", c.name, msg, c.frozen)
		}
	}
	// The operator is never scoped.
	if msg, _ := srv.scopedReplayDenial(context.Background(), esc("create", taskSpec("other"), "")); msg != "" {
		t.Errorf("unscoped caller: denial %q, want none", msg)
	}
}
