package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// createChannel POSTs a type=channel bead and returns the recorder.
func createChannel(t *testing.T, h http.Handler, labels []string, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":  "#" + fieldString(fields, "name"),
		"type":   "channel",
		"labels": labels,
		"fields": fields,
	})
}

func fieldString(fields map[string]any, key string) string {
	s, _ := fields[key].(string)
	return s
}

func createdID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	requireStatus(t, rec, http.StatusCreated)
	var b model.Bead
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode created bead: %v", err)
	}
	return b.ID
}

func requireRejected(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 containing %q; body: %s", rec.Code, want, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if !strings.Contains(body.Error, want) {
		t.Fatalf("error %q does not contain %q", body.Error, want)
	}
}

func TestChannelCreate_Valid(t *testing.T) {
	_, ms, h := newTestServer()
	id := createdID(t, createChannel(t, h, []string{"project:gardenio"}, map[string]any{
		"name": "general", "channel_kind": "general", "mirror": "both",
		"slack_channel_id": "C0GENERAL", "channel_roles": []string{"crew", "infra"},
	}))
	if got := ms.beads[id].Kind; got != model.KindData {
		t.Errorf("kind = %q, want data", got)
	}
}

// TestChannelCreate_ProjectLabelRule pins §6.2 rule 1: a channel with no
// project label would be visible in every land, and one with two in both.
func TestChannelCreate_ProjectLabelRule(t *testing.T) {
	_, _, h := newTestServer()
	fields := map[string]any{"name": "general", "channel_kind": "general"}
	requireRejected(t, createChannel(t, h, nil, fields), "exactly one project:<land> label, got none")
	requireRejected(t, createChannel(t, h, []string{"project:a", "project:b"}, fields), "got 2 (a, b)")
}

func TestChannelCreate_FieldRules(t *testing.T) {
	_, _, h := newTestServer()
	land := []string{"project:gardenio"}
	cases := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"missing name", map[string]any{"channel_kind": "crew"}, "name"},
		{"missing kind", map[string]any{"name": "crew"}, "channel_kind"},
		{"bad kind", map[string]any{"name": "crew", "channel_kind": "lobby"}, "channel_kind"},
		{"bad mirror", map[string]any{"name": "crew", "channel_kind": "crew", "mirror": "in"}, "mirror"},
		{"name not a slug", map[string]any{"name": "Crew Room", "channel_kind": "crew"}, "must be a slug"},
		{"dm without members", map[string]any{"name": "dm-a-b", "channel_kind": "dm"}, "at least two principals"},
		{"dm one member", map[string]any{"name": "dm-a-b", "channel_kind": "dm", "members": []string{"oidc:a@x.io"}}, "at least two principals"},
		{"dm bad member", map[string]any{"name": "dm-a-b", "channel_kind": "dm", "members": []string{"oidc:a@x.io", "bob"}}, `"bob" must be oidc:<email> or agent:<name>`},
		{"members on non-dm", map[string]any{"name": "crew", "channel_kind": "crew", "members": []string{"oidc:a@x.io", "agent:b"}}, "only for channel_kind=user or dm"},
		{"machines bound to Slack", map[string]any{"name": "machines", "channel_kind": "machines", "slack_channel_id": "C0MACHINES"}, "never mirrored"},
		{"machines mirrored", map[string]any{"name": "machines", "channel_kind": "machines", "mirror": "out"}, "never mirrored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireRejected(t, createChannel(t, h, land, tc.fields), tc.want)
		})
	}
	createdID(t, createChannel(t, h, land, map[string]any{
		"name": "dm-a-b", "channel_kind": "dm", "members": []string{"oidc:a@x.io", "agent:gardenio-crew-1"},
	}))
	createdID(t, createChannel(t, h, land, map[string]any{"name": "machines", "channel_kind": "machines", "mirror": "off"}))
}

func TestChannelCreate_NameUniquePerLand(t *testing.T) {
	_, _, h := newTestServer()
	general := map[string]any{"name": "general", "channel_kind": "general"}
	first := createdID(t, createChannel(t, h, []string{"project:a"}, general))
	requireRejected(t, createChannel(t, h, []string{"project:a"}, general), "already has an open channel named \"general\" ("+first+")")
	// Another land may use the same name.
	createdID(t, createChannel(t, h, []string{"project:b"}, general))

	// Archiving frees the name; re-opening the archived one then clashes.
	requireStatus(t, doJSON(t, h, "POST", "/v1/beads/"+first+"/close", map[string]any{}), http.StatusOK)
	createdID(t, createChannel(t, h, []string{"project:a"}, general))
	requireRejected(t, doJSON(t, h, "PATCH", "/v1/beads/"+first, map[string]any{"status": "open"}), "already has an open channel")
}

// TestChannelCreate_SlackChannelUniqueAcrossLands pins §6.2 rule 2: one Slack
// channel never mirrors into two lands, archived or not.
func TestChannelCreate_SlackChannelUniqueAcrossLands(t *testing.T) {
	_, _, h := newTestServer()
	first := createdID(t, createChannel(t, h, []string{"project:a"}, map[string]any{
		"name": "crew", "channel_kind": "crew", "slack_channel_id": "C0CREW",
	}))
	requireRejected(t, createChannel(t, h, []string{"project:b"}, map[string]any{
		"name": "crew", "channel_kind": "crew", "slack_channel_id": "C0CREW",
	}), "already bound to channel "+first)
	requireStatus(t, doJSON(t, h, "POST", "/v1/beads/"+first+"/close", map[string]any{}), http.StatusOK)
	requireRejected(t, createChannel(t, h, []string{"project:b"}, map[string]any{
		"name": "crew", "channel_kind": "crew", "slack_channel_id": "C0CREW",
	}), "already bound")

	// Updating the bound channel itself is not a clash with itself.
	requireStatus(t, doJSON(t, h, "PATCH", "/v1/beads/"+first, map[string]any{
		"fields": map[string]any{"mirror": "out"},
	}), http.StatusOK)
}

func TestChannelUpdate_LandIsFixed(t *testing.T) {
	_, _, h := newTestServer()
	id := createdID(t, createChannel(t, h, []string{"project:a"}, map[string]any{"name": "crew", "channel_kind": "crew"}))
	requireRejected(t, doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"labels": []string{"project:b"}}), "cannot move between lands")
	requireRejected(t, doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"labels": []string{"project:a", "project:b"}}), "got 2")
	requireRejected(t, doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"labels": []string{"topic"}}), "got none")
	requireStatus(t, doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"labels": []string{"project:a", "topic"}}), http.StatusOK)
}

func TestChannelLabelRoutes_ProjectLabelRule(t *testing.T) {
	_, _, h := newTestServer()
	id := createdID(t, createChannel(t, h, []string{"project:a"}, map[string]any{"name": "crew", "channel_kind": "crew"}))
	requireRejected(t, doJSON(t, h, "POST", "/v1/beads/"+id+"/labels", map[string]any{"label": "project:b"}), "got 2")
	requireRejected(t, doJSON(t, h, "DELETE", "/v1/beads/"+id+"/labels/project:a", nil), "got none")
	// Non-project labels are untouched by the rule.
	requireStatus(t, doJSON(t, h, "POST", "/v1/beads/"+id+"/labels", map[string]any{"label": "pinned"}), http.StatusCreated)
}

func createMessage(t *testing.T, h http.Handler, labels []string, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":  "hello",
		"type":   "message",
		"labels": labels,
		"fields": fields,
	})
}

// TestMessageWithChannel_LandRule pins §6.2 rule 1 for messages: one project
// label, equal to the channel's. A message without channel_id is untouched,
// so every existing producer keeps working.
func TestMessageWithChannel_LandRule(t *testing.T) {
	_, _, h := newTestServer()
	ch := createdID(t, createChannel(t, h, []string{"project:a"}, map[string]any{"name": "general", "channel_kind": "general"}))
	other := createdID(t, createChannel(t, h, []string{"project:b"}, map[string]any{"name": "general", "channel_kind": "general"}))

	createdID(t, createMessage(t, h, []string{"project:a", "say"}, map[string]any{"text": "hi", "channel_id": ch}))
	requireRejected(t, createMessage(t, h, []string{"say"}, map[string]any{"text": "hi", "channel_id": ch}), "got none")
	requireRejected(t, createMessage(t, h, []string{"project:a", "project:b"}, map[string]any{"text": "hi", "channel_id": ch}), "got 2")
	requireRejected(t, createMessage(t, h, []string{"project:a"}, map[string]any{"text": "hi", "channel_id": other}), `message is in land "a" but channel `+other+` is in land "b"`)
	requireRejected(t, createMessage(t, h, []string{"project:a"}, map[string]any{"text": "hi", "channel_id": "gd-nope"}), "channel_id is not a channel in this land")

	// No channel_id: the legacy shape, no project label needed.
	createdID(t, createMessage(t, h, nil, map[string]any{"text": "squawk"}))
}

func TestMessageWithChannel_UpdateAndLabelRoutes(t *testing.T) {
	_, _, h := newTestServer()
	ch := createdID(t, createChannel(t, h, []string{"project:a"}, map[string]any{"name": "general", "channel_kind": "general"}))
	legacy := createdID(t, createMessage(t, h, nil, map[string]any{"text": "old"}))
	// Stamping channel_id on a label-less message is refused until it has the land.
	requireRejected(t, doJSON(t, h, "PATCH", "/v1/beads/"+legacy, map[string]any{"fields": map[string]any{"channel_id": ch}}), "got none")
	requireStatus(t, doJSON(t, h, "PATCH", "/v1/beads/"+legacy, map[string]any{
		"labels": []string{"project:a"}, "fields": map[string]any{"channel_id": ch},
	}), http.StatusOK)
	requireRejected(t, doJSON(t, h, "POST", "/v1/beads/"+legacy+"/labels", map[string]any{"label": "project:b"}), "got 2")
	requireRejected(t, doJSON(t, h, "DELETE", "/v1/beads/"+legacy+"/labels/project:a", nil), "got none")
}

func TestBuiltinChannelTypeDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:channel"]
	if !ok {
		t.Fatal(`builtinConfigs missing "type:channel"`)
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:channel: %v", err)
	}
	have := map[string]model.FieldDef{}
	for _, f := range tc.Fields {
		have[f.Name] = f
	}
	for _, want := range []string{"name", "channel_kind", "channel_roles", "default_role", "runtime_class", "card_theme", "slack_channel_id", "mirror", "members"} {
		if _, ok := have[want]; !ok {
			t.Errorf("type:channel missing field %q (spec §2.2)", want)
		}
	}
	if !have["name"].Required || !have["channel_kind"].Required {
		t.Error("name and channel_kind must be required")
	}
	if tc.WritePolicy == nil || !tc.WritePolicy.ProjectScoped {
		t.Error("type:channel must be project_scoped")
	}
}

// TestChannelLabelRoutes_GRPC covers the gRPC twins of the label routes.
func TestChannelLabelRoutes_GRPC(t *testing.T) {
	s, _, h := newTestServer()
	id := createdID(t, createChannel(t, h, []string{"project:a"}, map[string]any{"name": "crew", "channel_kind": "crew"}))
	ctx := context.Background()
	if _, err := s.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: id, Label: "project:b"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("AddLabel project:b err = %v, want InvalidArgument", err)
	}
	if _, err := s.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: id, Label: "project:a"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("RemoveLabel project:a err = %v, want InvalidArgument", err)
	}
	if _, err := s.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: id, Label: "pinned"}); err != nil {
		t.Errorf("AddLabel pinned: %v", err)
	}
}

// TestChannelErrors_CrossLandNameNothing pins gd-1ZsyX7gdyv: a writer in one
// land learns neither the id nor the land of another land's bead from the land
// rules' errors, and an out-of-land channel_id fails exactly like a missing one.
// Admins and same-land callers keep the detailed text. Only an OIDC admin binds
// a channel to Slack (gd-ecFKkjtdvS), so a non-admin's attempt is refused before
// the slack_channel_id uniqueness check and learns nothing from it either.
func TestChannelErrors_CrossLandNameNothing(t *testing.T) {
	const admin = "channel-admin-tok"
	const oidcAdmin = "channel-oidc-admin-tok"
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	fc := NewFleetConfig("default", nil)
	fc.RegisterTokenWithProject("tok-north", "default", "north")
	fc.RegisterTokenWithProject("tok-south", "default", "south")
	srv.SetFleetAuth(fc)
	srv.SetRateLimit(RateLimitConfig{})
	verifier := &multiIssuerMock{issuers: []*oidcIssuerMock{{tokens: map[string]tokenInfo{
		oidcAdmin: {email: "owner@example.com", roles: []string{RoleViewer, RoleOperator, RoleAdmin}, azp: testBrowserClient},
	}}}}
	h := srv.NewHTTPHandler(verifier, withTestToken(fc, admin))
	as := func(token, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if token == admin {
			req.Header.Set("X-Operated-By", "oidc:owner@example.com")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	channels := 0
	channel := func(land, slackID string) map[string]any {
		// A fresh name each time, so only the slack_channel_id rule can clash.
		channels++
		name := fmt.Sprintf("crew-%d", channels)
		return map[string]any{
			"title": "#" + name, "type": "channel", "labels": []string{"project:" + land},
			"fields": map[string]any{"name": name, "channel_kind": "crew", "slack_channel_id": slackID},
		}
	}
	message := func(land, channelID string) map[string]any {
		return map[string]any{
			"title": "hello", "type": "message", "labels": []string{"project:" + land},
			"fields": map[string]any{"text": "hi", "channel_id": channelID},
		}
	}
	// leaksNothing rejects the call and checks the whole body, not just the
	// error field, for south's id and land name.
	leaksNothing := func(t *testing.T, rec *httptest.ResponseRecorder, want, southID string) {
		t.Helper()
		requireRejected(t, rec, want)
		for _, secret := range []string{southID, "south"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("cross-land error body %s names %q", rec.Body.String(), secret)
			}
		}
	}

	south := createdID(t, as(oidcAdmin, "POST", "/v1/beads", channel("south", "C0SHARED")))
	north := createdID(t, as(oidcAdmin, "POST", "/v1/beads", channel("north", "C0NORTH")))

	t.Run("non-admin binding to a channel bound in another land", func(t *testing.T) {
		// The binding gate refuses before the uniqueness check, so the body
		// names neither the other channel, its land, nor the Slack channel.
		for _, rec := range []*httptest.ResponseRecorder{
			as("tok-north", "POST", "/v1/beads", channel("north", "C0SHARED")),
			as("tok-north", "PATCH", "/v1/beads/"+north, map[string]any{
				"fields": map[string]any{"slack_channel_id": "C0SHARED"},
			}),
		} {
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
			}
			for _, secret := range []string{south, "south", "C0SHARED", "already bound"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("binding refusal %s names %q", rec.Body.String(), secret)
				}
			}
		}
	})

	t.Run("channel_id in another land matches a missing one", func(t *testing.T) {
		cross := as("tok-north", "POST", "/v1/beads", message("north", south))
		missing := as("tok-north", "POST", "/v1/beads", message("north", "gd-nope"))
		leaksNothing(t, cross, "channel_id is not a channel in this land", south)
		if cross.Body.String() != missing.Body.String() {
			t.Errorf("out-of-land body %s differs from missing-id body %s", cross.Body.String(), missing.Body.String())
		}
	})

	t.Run("same land and admin keep the detail", func(t *testing.T) {
		requireRejected(t, as(oidcAdmin, "POST", "/v1/beads", channel("north", "C0SHARED")),
			"already bound to channel "+south)
		requireRejected(t, as(admin, "POST", "/v1/beads", message("north", south)),
			`message is in land "north" but channel `+south+` is in land "south"`)
		createdID(t, as("tok-north", "POST", "/v1/beads", message("north", north)))
	})
}
