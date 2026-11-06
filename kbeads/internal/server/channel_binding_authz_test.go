package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gd-ecFKkjtdvS: only an admin binds a channel to Slack. slack_channel_id and
// mirror decide which Slack channel the bridge copies into a land and posts the
// land's messages to, so a non-admin who could set them could claim an
// operator's private Slack channel. Every denial sits next to a positive control
// (an admin, or the same caller without the binding) so it cannot pass vacuously.

func TestChangedChannelBindingFields(t *testing.T) {
	bound := json.RawMessage(`{"name":"ops","slack_channel_id":"C0OPS","mirror":"both"}`)
	for _, tc := range []struct {
		name            string
		patch, existing string
		want            []string
	}{
		{"create sets id", `{"name":"x","slack_channel_id":"C1"}`, ``, []string{"slack_channel_id"}},
		{"create sets mirror", `{"name":"x","mirror":"off"}`, ``, []string{"mirror"}},
		{"create sets both", `{"slack_channel_id":"C1","mirror":"both"}`, ``, []string{"mirror", "slack_channel_id"}},
		{"create without binding", `{"name":"x","channel_kind":"crew"}`, ``, nil},
		{"create with empty id", `{"slack_channel_id":""}`, ``, nil},
		{"change id", `{"slack_channel_id":"C0OTHER"}`, string(bound), []string{"slack_channel_id"}},
		{"clear id", `{"slack_channel_id":""}`, string(bound), []string{"slack_channel_id"}},
		{"null id", `{"slack_channel_id":null}`, string(bound), []string{"slack_channel_id"}},
		{"change mirror", `{"mirror":"off"}`, string(bound), []string{"mirror"}},
		{"echo unchanged", `{"name":"ops-2","slack_channel_id":"C0OPS","mirror":"both"}`, string(bound), nil},
		{"unrelated field", `{"name":"ops-2","members":["oidc:a@example.com"]}`, string(bound), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := changedChannelBindingFields(json.RawMessage(tc.patch), json.RawMessage(tc.existing))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("changed = %v, want %v", got, tc.want)
			}
		})
	}
}

// The decision point itself, for every caller class, independent of the route
// gates and the scoped action table that also stand in front of it.
func TestDenyChannelBindingFields_Callers(t *testing.T) {
	s, _, _ := newTestServer()
	scopedMember := WithGroups(WithRoles(oidcCtx("clw@example.com", nil), []string{RoleViewer, RoleOperator, RoleProjectMember}), []string{"/project-claimlayer"})
	denied := map[string]context.Context{
		"land principal":     landPrincipalContext(context.Background(), nil, "claimlayer"),
		"land member":        scopedMember,
		"non-admin operator": WithRoles(oidcCtx("op@example.com", nil), []string{RoleViewer, RoleOperator}),
		"static token":       machineUserCtx(),
		"fleet token":        fleetUserCtx(),
		"machine client":     withAuthMethod(context.Background(), authMethodMachine),
		"v1 edge":            withAuthMethod(context.Background(), authMethodEdge),
	}
	for name, ctx := range denied {
		if d := s.denyChannelBindingFields(ctx, "update", []string{"slack_channel_id"}); d == nil {
			t.Errorf("%s: binding allowed, want denied", name)
		} else if d.RequiredRole != RoleAdmin {
			t.Errorf("%s: RequiredRole = %q, want %q", name, d.RequiredRole, RoleAdmin)
		}
		if d := s.denyChannelBindingFields(ctx, "update", nil); d != nil {
			t.Errorf("%s: a write that changes no binding was denied: %v", name, d)
		}
	}
	allowed := map[string]context.Context{
		"oidc admin":        adminOIDCCtx(),
		"escalation replay": withMachineWrite(machineUserCtx()),
		"auth disabled":     withAuthMethod(context.Background(), authMethodNone),
	}
	for name, ctx := range allowed {
		if d := s.denyChannelBindingFields(ctx, "update", []string{"mirror", "slack_channel_id"}); d != nil {
			t.Errorf("%s: binding denied: %v", name, d)
		}
	}
}

// HTTP: the full stack (auth middleware, route gates, scoped action table,
// field gate) with real OIDC and land-principal tokens.
func TestChannelBinding_HTTP(t *testing.T) {
	f := newScopedFixture(t)
	boundFields := map[string]any{
		"name": "ops", "channel_kind": "user", "slack_channel_id": "C0OPS", "mirror": "both",
		"members": []string{principalOwner},
	}
	f.seedChannel(t, "ch-bound", "claimlayer", boundFields)
	f.seedChannel(t, "ch-plain", "claimlayer", map[string]any{"name": "crew", "channel_kind": "crew"})
	// tokOperator created both, so the ownership gate lets the operator edit
	// them and only the binding gate stands in the way. Land members and land
	// principals are also held off channel edits by the scoped action table
	// (channels are join-only); the binding gate is the second wall there.
	f.ms.beads["ch-bound"].CreatedBy = "op@example.com"
	f.ms.beads["ch-plain"].CreatedBy = "op@example.com"
	snapshot := func() map[string]string {
		return map[string]string{
			"ch-bound": string(f.ms.beads["ch-bound"].Fields),
			"ch-plain": string(f.ms.beads["ch-plain"].Fields),
		}
	}
	create := func(fields map[string]any) map[string]any {
		return map[string]any{
			"title": "#" + fieldString(fields, "name"), "type": "channel",
			"labels": []string{"project:claimlayer"}, "fields": fields,
		}
	}
	patch := func(fields map[string]any) map[string]any { return map[string]any{"fields": fields} }

	attempts := []struct {
		name         string
		method, path string
		body         any
	}{
		{"create with slack_channel_id", http.MethodPost, "/v1/beads", create(map[string]any{"name": "ops-new", "channel_kind": "crew", "slack_channel_id": "C0PRIVATE"})},
		{"create with mirror", http.MethodPost, "/v1/beads", create(map[string]any{"name": "ops-mirror", "channel_kind": "crew", "mirror": "both"})},
		{"bind an unbound channel", http.MethodPatch, "/v1/beads/ch-plain", patch(map[string]any{"slack_channel_id": "C0PRIVATE"})},
		{"re-bind a bound channel", http.MethodPatch, "/v1/beads/ch-bound", patch(map[string]any{"slack_channel_id": "C0PRIVATE"})},
		{"clear a binding", http.MethodPatch, "/v1/beads/ch-bound", patch(map[string]any{"slack_channel_id": ""})},
		{"change mirror", http.MethodPatch, "/v1/beads/ch-bound", patch(map[string]any{"mirror": "off"})},
		{"join while re-binding", http.MethodPatch, "/v1/beads/ch-bound", patch(map[string]any{"members": []string{principalOwner, principalWriter}, "slack_channel_id": "C0PRIVATE"})},
		{"field endpoint", http.MethodPost, "/v1/beads/ch-plain/fields/slack_channel_id/append", appendArrayFieldRequest{Element: "C0PRIVATE"}},
	}
	for who, tok := range map[string]string{
		"land principal":     tokLandClaimlayer,
		"land member":        tokScopedWriter,
		"land owner":         tokScopedOwner,
		"non-admin operator": tokOperator,
	} {
		for _, a := range attempts {
			t.Run(who+"/"+a.name, func(t *testing.T) {
				before := snapshot()
				code, body := f.do(t, tok, a.method, a.path, a.body)
				if code != http.StatusForbidden {
					t.Fatalf("%s %s = %d %s, want 403", a.method, a.path, code, body)
				}
				if strings.Contains(body, "C0OPS") {
					t.Errorf("refusal names the bound Slack channel: %s", body)
				}
				if after := snapshot(); !reflect.DeepEqual(before, after) {
					t.Errorf("channel fields changed despite 403: %v -> %v", before, after)
				}
			})
		}
	}

	t.Run("non-admin controls", func(t *testing.T) {
		// The operator's 403s above are the binding gate: without a binding
		// the same caller creates and renames channels.
		if code, body := f.do(t, tokOperator, http.MethodPost, "/v1/beads", create(map[string]any{"name": "builds", "channel_kind": "crew"})); code != http.StatusCreated {
			t.Fatalf("operator create without binding = %d %s, want 201", code, body)
		}
		// A read-modify-write rename echoes the unchanged binding back.
		if code, body := f.do(t, tokOperator, http.MethodPatch, "/v1/beads/ch-bound", map[string]any{
			"title": "#ops-renamed", "fields": boundFields,
		}); code != http.StatusOK {
			t.Fatalf("operator rename echoing the binding = %d %s, want 200", code, body)
		}
		// A member still joins a bound user channel.
		if code, body := f.setMembers(t, tokScopedWriter, "ch-bound", []string{principalOwner, principalWriter}); code != http.StatusOK {
			t.Fatalf("member join of a bound channel = %d %s, want 200", code, body)
		}
		if got := readChannelFields(f.ms.beads["ch-bound"].Fields); got.SlackChannelID != "C0OPS" {
			t.Errorf("slack_channel_id = %q after rename and join, want C0OPS", got.SlackChannelID)
		}
	})

	t.Run("admin binds, re-binds and clears", func(t *testing.T) {
		if code, body := f.do(t, tokOwnerAdmin, http.MethodPost, "/v1/beads", create(map[string]any{"name": "alerts", "channel_kind": "crew", "slack_channel_id": "C0ALERTS", "mirror": "out"})); code != http.StatusCreated {
			t.Fatalf("admin create with binding = %d %s, want 201", code, body)
		}
		for _, fields := range []map[string]any{{"slack_channel_id": "C0CREW", "mirror": "both"}, {"slack_channel_id": ""}} {
			if code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/ch-plain", patch(fields)); code != http.StatusOK {
				t.Fatalf("admin patch %v = %d %s, want 200", fields, code, body)
			}
		}
	})
}

// gRPC: the CreateBead and UpdateBead handlers map the denial to
// PermissionDenied. Land principals never reach a gRPC handler (the
// interceptor refuses them, TestLandPrincipal_GRPCRefused); the handler
// refuses one anyway.
func TestChannelBinding_GRPC(t *testing.T) {
	s, ms, _ := newTestServer()
	seed := func(id, fields string) {
		ms.beads[id] = &model.Bead{ID: id, Title: "#" + id, Type: typeChannel, Kind: model.KindData, Status: model.StatusOpen, Fields: json.RawMessage(fields)}
		ms.labels[id] = []string{"project:gardenio"}
	}
	const boundJSON = `{"name":"ops","channel_kind":"crew","slack_channel_id":"C0OPS","mirror":"both"}`
	seed("gd-chbound", boundJSON)
	seed("gd-chplain", `{"name":"crew","channel_kind":"crew"}`)
	// The operator may write in gardenio (checkProjectScope), so only the
	// binding gate refuses its binding writes.
	seedUserBead(t, ms, "gd-userop", map[string]any{"email": "op@example.com", "allowed_projects": []string{"gardenio"}})
	ms.beads["gd-userop"].Kind = model.KindConfig
	channel := func(name, extra string) *beadsv1.CreateBeadRequest {
		return &beadsv1.CreateBeadRequest{
			Title: "#" + name, Type: "channel", Labels: []string{"project:gardenio"},
			Fields: []byte(`{"name":"` + name + `","channel_kind":"crew"` + extra + `}`),
		}
	}
	update := func(id, fields string) *beadsv1.UpdateBeadRequest {
		return &beadsv1.UpdateBeadRequest{Id: id, Fields: []byte(fields)}
	}

	callers := map[string]context.Context{
		"land principal":     landPrincipalContext(context.Background(), nil, "gardenio"),
		"non-admin operator": WithRoles(oidcCtx("op@example.com", nil), []string{RoleViewer, RoleOperator}),
		"static token":       machineUserCtx(),
		"fleet token":        fleetUserCtx(),
	}
	for who, ctx := range callers {
		t.Run(who, func(t *testing.T) {
			if _, err := s.CreateBead(ctx, channel("ops-new", `,"slack_channel_id":"C0PRIVATE"`)); status.Code(err) != codes.PermissionDenied {
				t.Errorf("CreateBead with slack_channel_id err = %v, want PermissionDenied", err)
			}
			if _, err := s.CreateBead(ctx, channel("ops-mirror", `,"mirror":"both"`)); status.Code(err) != codes.PermissionDenied {
				t.Errorf("CreateBead with mirror err = %v, want PermissionDenied", err)
			}
			for _, u := range []*beadsv1.UpdateBeadRequest{
				update("gd-chplain", `{"slack_channel_id":"C0PRIVATE"}`),
				update("gd-chbound", `{"slack_channel_id":"C0PRIVATE"}`),
				update("gd-chbound", `{"slack_channel_id":""}`),
				update("gd-chbound", `{"mirror":"off"}`),
			} {
				if _, err := s.UpdateBead(ctx, u); status.Code(err) != codes.PermissionDenied {
					t.Errorf("UpdateBead %s %s err = %v, want PermissionDenied", u.Id, u.Fields, err)
				}
			}
			if got := string(ms.beads["gd-chbound"].Fields); got != boundJSON {
				t.Errorf("bound channel fields changed despite denial: %s", got)
			}
		})
	}

	t.Run("controls", func(t *testing.T) {
		// A service token and a non-admin operator still create unbound
		// channels and rename bound ones with the binding echoed back.
		for who, ctx := range map[string]context.Context{"fleet token": callers["fleet token"], "non-admin operator": callers["non-admin operator"]} {
			if _, err := s.CreateBead(ctx, channel("plain-"+strings.ReplaceAll(who, " ", "-"), "")); err != nil {
				t.Errorf("%s: CreateBead without binding: %v", who, err)
			}
			title := "#ops " + who
			if _, err := s.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-chbound", Title: &title, Fields: []byte(boundJSON)}); err != nil {
				t.Errorf("%s: rename echoing the binding: %v", who, err)
			}
		}
		admin := adminOIDCCtx()
		if _, err := s.CreateBead(admin, channel("alerts", `,"slack_channel_id":"C0ALERTS","mirror":"out"`)); err != nil {
			t.Errorf("admin CreateBead with binding: %v", err)
		}
		if _, err := s.UpdateBead(admin, update("gd-chplain", `{"slack_channel_id":"C0CREW","mirror":"both"}`)); err != nil {
			t.Errorf("admin UpdateBead binding: %v", err)
		}
		if _, err := s.UpdateBead(admin, update("gd-chplain", `{"slack_channel_id":""}`)); err != nil {
			t.Errorf("admin UpdateBead clearing: %v", err)
		}
	})
}
