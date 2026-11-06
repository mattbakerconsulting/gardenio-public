package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

type fakeInviteStore struct {
	created []beadsapi.CreateBeadRequest
	beads   map[string]*beadsapi.BeadDetail
	updates map[string]map[string]any
	query   beadsapi.ListBeadsQuery
}

func newFakeInviteStore() *fakeInviteStore {
	return &fakeInviteStore{beads: map[string]*beadsapi.BeadDetail{}, updates: map[string]map[string]any{}}
}

func (f *fakeInviteStore) CreateBeadDetail(_ context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	f.created = append(f.created, req)
	fields := map[string]string{}
	var raw map[string]any
	_ = json.Unmarshal(req.Fields, &raw)
	for k, v := range raw {
		fields[k] = fmt.Sprint(v)
	}
	b := &beadsapi.BeadDetail{ID: fmt.Sprintf("gd-inv%d", len(f.created)), Type: req.Type, Title: req.Title, Status: "open", Fields: fields}
	f.beads[b.ID] = b
	return b, nil
}

func (f *fakeInviteStore) ListAllBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.query = q
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeInviteStore) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := f.beads[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("%s not found", id)
}

func (f *fakeInviteStore) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	f.updates[id] = fields
	return nil
}

var inviteNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func TestFileInvite(t *testing.T) {
	st := newFakeInviteStore()
	b, err := fileInvite(context.Background(), st, inviteRequest{
		Email: "  Someone@Example.COM ", InvitedBy: "oidc:operator@example.com", TTL: 14 * 24 * time.Hour,
	}, inviteNow)
	if err != nil {
		t.Fatal(err)
	}
	req := st.created[0]
	if req.Type != "invite" || req.Kind != "data" || len(req.Labels) != 0 || strings.Contains(req.ShortTitle, "@") {
		t.Fatalf("request = %+v", req)
	}
	want := map[string]string{
		"email": "someone@example.com", "invited_by": "oidc:operator@example.com", "state": "pending",
		"expires_at": "2026-11-11T12:00:00Z",
	}
	for k, v := range want {
		if b.Fields[k] != v {
			t.Errorf("%s = %q, want %q", k, b.Fields[k], v)
		}
	}
	if _, ok := b.Fields["land"]; ok {
		t.Errorf("a land-less invite carries land: %v", b.Fields)
	}

	b, err = fileInvite(context.Background(), st, inviteRequest{
		Email: "a@example.com", Land: "tomato-patch", InvitedBy: "oidc:operator@example.com", TTL: time.Hour,
	}, inviteNow)
	if err != nil || b.Fields["land"] != "tomato-patch" || b.Fields["role"] != "member" {
		t.Fatalf("land invite = %v, %v", b, err)
	}
}

func TestFileInvite_Refused(t *testing.T) {
	for name, in := range map[string]inviteRequest{
		"named address": {Email: "Some One <a@example.com>"},
		"two addresses": {Email: "a@example.com, b@example.com"},
		"no at":         {Email: "someone"},
		"role, no land": {Email: "a@example.com", Role: "guest"},
		"owner":         {Email: "a@example.com", Land: "x", Role: "owner"},
		"short ttl":     {Email: "a@example.com", TTL: time.Minute},
		"no operator":   {Email: "a@example.com", InvitedBy: " "},
	} {
		if in.InvitedBy == "" {
			in.InvitedBy = "oidc:operator@example.com"
		}
		if in.TTL == 0 {
			in.TTL = time.Hour
		}
		st := newFakeInviteStore()
		if _, err := fileInvite(context.Background(), st, in, inviteNow); err == nil || len(st.created) != 0 {
			t.Errorf("%s: err %v, created %d", name, err, len(st.created))
		}
	}
}

func TestListAndUninvite(t *testing.T) {
	st := newFakeInviteStore()
	b, err := fileInvite(context.Background(), st, inviteRequest{Email: "a@example.com", InvitedBy: "oidc:op@example.com", TTL: time.Hour}, inviteNow)
	if err != nil {
		t.Fatal(err)
	}
	got, err := listInvites(context.Background(), st, false)
	if err != nil || len(got) != 1 || strings.Contains(strings.Join(st.query.Statuses, ","), "closed") {
		t.Fatalf("list = %v %v query %+v", got, err, st.query)
	}
	if _, _ = listInvites(context.Background(), st, true); !strings.Contains(strings.Join(st.query.Statuses, ","), "closed") {
		t.Errorf("--all query %+v leaves out closed", st.query)
	}
	var out bytes.Buffer
	printInvites(&out, got)
	if !strings.Contains(out.String(), "a@example.com") || !strings.Contains(out.String(), "pending") {
		t.Errorf("printInvites:\n%s", out.String())
	}

	if err := requestUninvite(context.Background(), st, b.ID, "oidc:op@example.com", inviteNow); err != nil {
		t.Fatal(err)
	}
	if u := st.updates[b.ID]; u["revoke_requested_at"] != "2026-10-28T12:00:00Z" || u["revoke_requested_by"] != "oidc:op@example.com" {
		t.Fatalf("revoke = %v", u)
	}
	b.Fields["state"] = "claimed"
	if err := requestUninvite(context.Background(), st, b.ID, "oidc:op@example.com", inviteNow); err == nil {
		t.Error("uninvited a claimed invite")
	}
	st.beads["gd-task"] = &beadsapi.BeadDetail{ID: "gd-task", Type: "task", Fields: map[string]string{}}
	if err := requestUninvite(context.Background(), st, "gd-task", "oidc:op@example.com", inviteNow); err == nil {
		t.Error("uninvited a task")
	}
}
