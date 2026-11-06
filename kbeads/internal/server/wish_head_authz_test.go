package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Wish-head marks (wish_head_authz.go, gd-3HAfTiByJK): a tenant may not write
// the bare thread label or fields.promoted_from / promoted_to; the bridge's
// fleet token and an admin may.

func TestWishHead_CreateRefusesTenantMarks(t *testing.T) {
	f := newScopedFixture(t)
	task := func(labels []string, fields map[string]any) map[string]any {
		return map[string]any{"title": "x", "type": "task", "labels": append([]string{"project:claimlayer"}, labels...), "fields": fields}
	}
	keyed := func(body map[string]any, key string) map[string]any {
		body["idempotency_key"] = key
		return body
	}
	for _, c := range []struct {
		name, token string
		body        map[string]any
		want        int
	}{
		{"member plain task", tokScopedWriter, task(nil, nil), http.StatusCreated},
		{"member prefixed thread label", tokScopedWriter, task([]string{"thread:gd-x", "thread-say"}, nil), http.StatusCreated},
		{"member thread label", tokScopedWriter, task([]string{"thread"}, nil), http.StatusForbidden},
		{"member promoted_from", tokScopedWriter, task(nil, map[string]any{"promoted_from": "msg-1"}), http.StatusForbidden},
		{"member forged head", tokScopedWriter, task([]string{"thread"}, map[string]any{"promoted_from": "msg-1"}), http.StatusForbidden},
		{"owner thread label", tokScopedOwner, task([]string{"thread"}, nil), http.StatusForbidden},
		{"land principal message promoted_to", tokLandClaimlayer, map[string]any{
			"title": "hi", "type": "message", "status": "closed",
			"labels": []string{"project:claimlayer"},
			"fields": map[string]any{"text": "hi", "promoted_to": "gd-x"},
		}, http.StatusForbidden},
		{"guest message labelled thread", tokScopedGuest, map[string]any{
			"title": "hi", "type": "message", "status": "closed",
			"labels": []string{"project:claimlayer", "thread"},
			"fields": map[string]any{"text": "hi"},
		}, http.StatusForbidden},
		{"member slack-thread label", tokScopedWriter, task([]string{"slack-thread:C0A:1700000000.000100"}, nil), http.StatusForbidden},
		{"member own idempotency key", tokScopedWriter, keyed(task(nil, nil), "mine-1"), http.StatusCreated},
		{"member commons-promote key", tokScopedWriter, keyed(task(nil, nil), "commons-promote:msg-1"), http.StatusForbidden},
		{"member slack-thread key", tokScopedWriter, keyed(task(nil, nil), "slack-thread:C0A:1700000000.000100"), http.StatusForbidden},
		{"member commons-promote key in fields", tokScopedWriter, task(nil, map[string]any{"idempotency_key": "commons-promote:msg-2"}), http.StatusForbidden},
		{"bridge head", tokFleetBridge, task([]string{"thread"}, map[string]any{"promoted_from": "msg-1"}), http.StatusCreated},
		{"bridge commons-promote head", tokFleetBridge, keyed(task([]string{"thread"}, map[string]any{"promoted_from": "msg-3"}), "commons-promote:msg-3"), http.StatusCreated},
		{"admin head", tokOwnerAdmin, task([]string{"thread"}, map[string]any{"promoted_from": "msg-1"}), http.StatusCreated},
	} {
		code, body := f.do(t, c.token, http.MethodPost, "/v1/beads", c.body)
		if code != c.want {
			t.Errorf("%s: got %d, want %d: %s", c.name, code, c.want, body)
		}
		if c.want == http.StatusForbidden && !strings.Contains(body, "bridge") {
			t.Errorf("%s: denial does not say who writes wish heads: %s", c.name, body)
		}
	}
}

func TestWishHead_UpdateRefusesTenantMarks(t *testing.T) {
	f := newScopedFixture(t)
	seed := func(id string, labels []string, fields map[string]any) {
		f.seedTyped(id, "task", "project:claimlayer")
		f.ms.labels[id] = append(f.ms.labels[id], labels...)
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		f.ms.beads[id].Fields = raw
	}
	for _, c := range []struct {
		name, token string
		head        bool
		patch       map[string]any
		want        int
	}{
		{"member edits a task", tokScopedWriter, false, map[string]any{"title": "y"}, http.StatusOK},
		{"member labels a task thread", tokScopedWriter, false, map[string]any{"labels": []string{"project:claimlayer", "thread"}}, http.StatusForbidden},
		{"member sets promoted_from", tokScopedWriter, false, map[string]any{"fields": map[string]any{"promoted_from": "msg-1"}}, http.StatusForbidden},
		{"member strips a head's label", tokScopedWriter, true, map[string]any{"labels": []string{"project:claimlayer"}}, http.StatusForbidden},
		{"member repoints a head", tokScopedWriter, true, map[string]any{"fields": map[string]any{"promoted_from": "msg-2"}}, http.StatusForbidden},
		{"member clears a head's link", tokScopedWriter, true, map[string]any{"fields": map[string]any{"promoted_from": ""}}, http.StatusForbidden},
		{"member echoes a head's link", tokScopedWriter, true, map[string]any{"fields": map[string]any{"promoted_from": "msg-1", "slack_root_text": "n"}}, http.StatusOK},
		{"member keeps a head's label", tokScopedWriter, true, map[string]any{"labels": []string{"project:claimlayer", "thread", "extra"}}, http.StatusOK},
		{"member labels a task slack-thread", tokScopedWriter, false, map[string]any{"labels": []string{"project:claimlayer", "slack-thread:C0A:1700000000.000100"}}, http.StatusForbidden},
		{"member keys a task as a head", tokScopedWriter, false, map[string]any{"fields": map[string]any{"idempotency_key": "commons-promote:msg-1"}}, http.StatusForbidden},
		{"member rekeys a head", tokScopedWriter, true, map[string]any{"fields": map[string]any{"idempotency_key": "mine"}}, http.StatusForbidden},
		{"member echoes a head's key", tokScopedWriter, true, map[string]any{"fields": map[string]any{"idempotency_key": "commons-promote:msg-1"}}, http.StatusOK},
		{"member keys a task its own way", tokScopedWriter, false, map[string]any{"fields": map[string]any{"idempotency_key": "mine"}}, http.StatusOK},
		{"bridge stamps promoted_from", tokFleetBridge, false, map[string]any{"fields": map[string]any{"promoted_from": "msg-1"}}, http.StatusOK},
	} {
		id := "cl-wish-" + strings.ReplaceAll(c.name, " ", "-")
		if c.head {
			seed(id, []string{"thread"}, map[string]any{"promoted_from": "msg-1", "idempotency_key": "commons-promote:msg-1"})
		} else {
			seed(id, nil, map[string]any{})
		}
		code, body := f.do(t, c.token, http.MethodPatch, "/v1/beads/"+id, c.patch)
		if code != c.want {
			t.Errorf("%s: got %d, want %d: %s", c.name, code, c.want, body)
		}
	}
}

func TestWishHead_LabelRoutesRefuseTenant(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("cl-add", "task", "project:claimlayer")
	f.seedTyped("cl-head", "task", "project:claimlayer")
	f.ms.labels["cl-head"] = append(f.ms.labels["cl-head"], "thread")

	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads/cl-add/labels", map[string]any{"label": "thread"}); code != http.StatusForbidden {
		t.Errorf("member adds thread: got %d, want 403: %s", code, body)
	}
	if code, body := f.do(t, tokScopedWriter, http.MethodDelete, "/v1/beads/cl-head/labels/thread", nil); code != http.StatusForbidden {
		t.Errorf("member removes thread: got %d, want 403: %s", code, body)
	}
	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads/cl-add/labels", map[string]any{"label": "slack-thread:C0A:1700000000.000100"}); code != http.StatusForbidden {
		t.Errorf("member adds slack-thread: got %d, want 403: %s", code, body)
	}
	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads/cl-add/labels", map[string]any{"label": "thread:gd-x"}); code >= 300 {
		t.Errorf("member adds thread:<id>: got %d, want 2xx: %s", code, body)
	}
	if code, body := f.do(t, tokFleetBridge, http.MethodPost, "/v1/beads/cl-add/labels", map[string]any{"label": "thread"}); code >= 300 {
		t.Errorf("bridge adds thread: got %d, want 2xx: %s", code, body)
	}
}

func TestWishHead_FieldEndpointRefusesTenant(t *testing.T) {
	f := newScopedFixture(t)
	f.seedTyped("cl-head", "task", "project:claimlayer")
	f.ms.beads["cl-head"].Fields = json.RawMessage(`{"promoted_from":"msg-1"}`)
	code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads/cl-head/fields/promoted_from/edit", map[string]any{"path": "x", "value": "y"})
	if code != http.StatusForbidden || !strings.Contains(body, "promoted_from") {
		t.Errorf("member edits promoted_from: got %d, want 403 naming the field: %s", code, body)
	}
	code, body = f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads/cl-head/fields/idempotency_key/edit", map[string]any{"path": "x", "value": "y"})
	if code != http.StatusForbidden || !strings.Contains(body, "idempotency_key") {
		t.Errorf("member edits idempotency_key: got %d, want 403 naming the field: %s", code, body)
	}
}

// An agent of a tenant land is a tenant too (callerIsTenant): its per-agent
// token is refused a head's marks and identity; an agent of an operator
// project is not.
func TestWishHead_TenantLandAgentRefused(t *testing.T) {
	f := seedLandAccounts(t)
	task := func(project string, labels []string, fields map[string]any) map[string]any {
		return map[string]any{"title": "x", "type": "task", "labels": append([]string{"project:" + project}, labels...), "fields": fields}
	}
	for _, c := range []struct {
		name, token string
		body        map[string]any
		want        int
	}{
		{"tenant agent plain task", tokClAgent, task("claimlayer", nil, nil), http.StatusCreated},
		{"tenant agent thread label", tokClAgent, task("claimlayer", []string{"thread"}, nil), http.StatusForbidden},
		{"tenant agent promoted_from", tokClAgent, task("claimlayer", nil, map[string]any{"promoted_from": "msg-1"}), http.StatusForbidden},
		{"tenant agent head key", tokClAgent, task("claimlayer", nil, map[string]any{"idempotency_key": "commons-promote:msg-1"}), http.StatusForbidden},
		{"operator agent head", tokGardenioAgent, task("gardenio", []string{"thread"}, map[string]any{"promoted_from": "msg-1"}), http.StatusCreated},
	} {
		code, body := f.do(t, c.token, http.MethodPost, "/v1/beads", c.body)
		if code != c.want {
			t.Errorf("%s: got %d, want %d: %s", c.name, code, c.want, body)
		}
		if c.want == http.StatusForbidden && !strings.Contains(body, "reserved for the bridge") {
			t.Errorf("%s: refused, but not by the wish-head gate: %s", c.name, body)
		}
	}
}

// A tenant-land agent whose land lookup fails is taken for a tenant: the gate
// fails closed.
func TestWishHead_TenantAgentLookupFailsClosed(t *testing.T) {
	ctx := WithTokenProject(WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "steward"}), "agent-cl-agent-1"), "claimlayer")
	if d := (&BeadsServer{store: newMockStore()}).wishHeadLabelChangeDenial(ctx, "update", &model.Bead{ID: "cl-x", Type: "task"}, "thread"); d != nil {
		t.Errorf("control: an agent of no tenant land was refused: %+v", d)
	}
	srv := &BeadsServer{store: failingProjectStore{newMockStore()}}
	if d := srv.wishHeadLabelChangeDenial(ctx, "update", &model.Bead{ID: "cl-x", Type: "task"}, "thread"); d == nil {
		t.Error("a tenant agent whose land lookup failed was let write the thread label")
	}
	if d := srv.wishHeadKeyDenial(ctx, "update", &model.Bead{ID: "cl-x", Type: "task"}, "promoted_from"); d == nil {
		t.Error("a tenant agent whose land lookup failed was let write promoted_from")
	}
}

func TestChangedWishHeadLabels(t *testing.T) {
	got := changedWishHeadLabels([]string{"thread", "a"}, []string{"a", "slack-thread:C:1", "thread:gd-x"})
	if want := "label slack-thread:C:1,label thread"; strings.Join(got, ",") != want {
		t.Errorf("got %v, want %s", got, want)
	}
	if got := changedWishHeadLabels([]string{"thread"}, []string{"thread", "b"}); got != nil {
		t.Errorf("unchanged: got %v", got)
	}
}

func TestChangedWishHeadFields(t *testing.T) {
	for _, c := range []struct {
		name            string
		patch, existing string
		want            []string
	}{
		{"none", `{"a":"b"}`, ``, nil},
		{"set on create", `{"promoted_to":"x","promoted_from":"y"}`, ``, []string{"promoted_from", "promoted_to"}},
		{"echo", `{"promoted_from":"y"}`, `{"promoted_from":"y"}`, nil},
		{"null against unset", `{"promoted_from":null}`, `{}`, nil},
		{"clear", `{"promoted_from":null}`, `{"promoted_from":"y"}`, []string{"promoted_from"}},
		{"bad json", `{`, ``, nil},
		{"own key", `{"idempotency_key":"mine"}`, ``, nil},
		{"head key on create", `{"idempotency_key":"commons-promote:m"}`, ``, []string{"idempotency_key"}},
		{"head key changed", `{"idempotency_key":"mine"}`, `{"idempotency_key":"slack-thread:C:1"}`, []string{"idempotency_key"}},
		{"head key echoed", `{"idempotency_key":"slack-thread:C:1"}`, `{"idempotency_key":"slack-thread:C:1"}`, nil},
	} {
		got := changedWishHeadFields(json.RawMessage(c.patch), json.RawMessage(c.existing))
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
