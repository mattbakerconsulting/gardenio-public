package main

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/provision"
)

// fakeProvisionDaemon holds plot applications for gb lands provision / close.
type fakeProvisionDaemon struct {
	beads   map[string]*beadsapi.BeadDetail
	updates []map[string]any
}

func (f *fakeProvisionDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		ok := slices.Contains(q.Types, b.Type)
		for k, v := range q.Fields {
			ok = ok && b.Fields[k] == v
		}
		if ok && q.Offset == 0 {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeProvisionDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := f.beads[id]; ok {
		return b, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeProvisionDaemon) CreateBeadDetail(context.Context, beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	return nil, errors.New("gb never creates here")
}

func (f *fakeProvisionDaemon) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	f.updates = append(f.updates, fields)
	for k, v := range fields {
		switch x := v.(type) {
		case string:
			f.beads[id].Fields[k] = x
		case int:
			f.beads[id].Fields[k] = strconv.Itoa(x)
		}
	}
	return nil
}

func (f *fakeProvisionDaemon) CloseBead(context.Context, string, map[string]string) error {
	return errors.New("gb never closes here")
}

func (f *fakeProvisionDaemon) AddComment(context.Context, string, string, string) error { return nil }

func newFakeProvisionDaemon(state string) *fakeProvisionDaemon {
	return &fakeProvisionDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-app1": {ID: "gd-app1", Type: provision.ApplicationType, Labels: []string{provision.IntakeLabel},
			Fields: map[string]string{"state": state, "land_name": "acme", "provision_step": "purse", "provision_attempt": "1", "provision_error": "keycloak: boom"}},
	}}
}

func TestRetryProvision_FailedGoesBackToProvisioning(t *testing.T) {
	d := newFakeProvisionDaemon(provision.StateFailed)
	app, err := retryProvision(context.Background(), d, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if app.State != provision.StateProvisioning || app.Attempt != 2 || app.Error != "" || app.Step != "purse" {
		t.Fatalf("app = %+v", app)
	}
	if f := d.beads["gd-app1"].Fields; f["state"] != provision.StateProvisioning || f["provision_error"] != "" {
		t.Fatalf("fields = %v", f)
	}
	if _, err := retryProvision(context.Background(), d, "gd-app1"); err == nil {
		t.Fatal("a provisioning application must not be retried")
	}
}

func TestRequestLandClose_RecordsTheOperatorAndReason(t *testing.T) {
	d := newFakeProvisionDaemon(provision.StateProvisioned)
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	app, err := requestLandClose(context.Background(), d, "acme", "op@example.com", "abuse", now)
	if err != nil {
		t.Fatal(err)
	}
	f := d.beads["gd-app1"].Fields
	if f["close_requested_by"] != "op@example.com" || f["close_reason"] != "abuse" ||
		f["close_requested_at"] != now.Format(time.RFC3339) || f["provision_error"] != "" || app.CloseRequestedAt == "" {
		t.Fatalf("fields = %v", f)
	}
	if _, err := requestLandClose(context.Background(), d, "acme", "", "", now); err == nil {
		t.Fatal("a close with no operator must be refused")
	}
	if _, err := requestLandClose(context.Background(), d, "nope", "op@example.com", "", now); !errors.Is(err, provision.ErrNoApplication) {
		t.Fatalf("unknown land: %v", err)
	}
}
