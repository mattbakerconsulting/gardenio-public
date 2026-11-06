package freeze

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

type fakeDaemon struct {
	beads    []*beadsapi.BeadDetail
	writes   []map[string]any
	comments []string
	// failWrite fails the n-th field write (1-based); 0 never.
	failWrite int
	listErr   error
}

func (f *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if slices.Contains(q.Types, b.Type) && slices.Contains(q.Statuses, b.Status) && q.Offset == 0 {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeDaemon) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	f.writes = append(f.writes, fields)
	if len(f.writes) == f.failWrite {
		return errors.New("unknown field")
	}
	for _, b := range f.beads {
		if b.ID == id {
			for k, v := range fields {
				switch v := v.(type) {
				case bool:
					b.Fields[k] = map[bool]string{true: "true", false: "false"}[v]
				case string:
					b.Fields[k] = v
				}
			}
		}
	}
	return nil
}

func (f *fakeDaemon) AddComment(_ context.Context, _, _, text string) error {
	f.comments = append(f.comments, text)
	return nil
}

func farm() *fakeDaemon {
	return &fakeDaemon{beads: []*beadsapi.BeadDetail{
		{ID: "kd-1", Type: "project", Title: "tomato-patch", Status: "open", Fields: map[string]string{"land_kind": "tenant"}},
		{ID: "kd-2", Type: "project", Title: "gardenio", Status: "open", Fields: map[string]string{}},
	}}
}

func TestFreezeThenThaw(t *testing.T) {
	d := farm()
	s, err := Freeze(context.Background(), d, "tomato-patch", "spam in #general", "oidc:op@example.com", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Frozen || !s.Changed || s.Warning != "" || s.ProjectID != "kd-1" {
		t.Fatalf("freeze = %+v", s)
	}
	// The switch is written first and alone, as a JSON boolean.
	if v, ok := d.writes[0][FieldFrozen].(bool); !ok || !v || len(d.writes[0]) != 1 {
		t.Fatalf("first write = %v, want land_frozen=true alone", d.writes[0])
	}
	f := d.beads[0].Fields
	if f[FieldBy] != "oidc:op@example.com" || f[FieldAt] != "2026-10-20T09:00:00Z" || f[FieldReason] != "spam in #general" {
		t.Fatalf("who/when/why = %v", f)
	}
	if len(d.comments) != 1 || !strings.Contains(d.comments[0], "spam in #general") {
		t.Fatalf("comments = %q", d.comments)
	}
	got, err := Read(context.Background(), d, "tomato-patch")
	if err != nil || !got.Frozen || got.Reason != "spam in #general" {
		t.Fatalf("Read = %+v %v", got, err)
	}

	// Freezing again changes nothing: the first reason stands.
	again, err := Freeze(context.Background(), d, "tomato-patch", "second thoughts", "oidc:op@example.com", t0.Add(time.Hour))
	if err != nil || again.Changed || again.Reason != "spam in #general" || len(d.writes) != 2 {
		t.Fatalf("second freeze = %+v %v (writes %d)", again, err, len(d.writes))
	}

	th, err := Thaw(context.Background(), d, "tomato-patch", "oidc:op@example.com")
	if err != nil || th.Frozen || !th.Changed {
		t.Fatalf("thaw = %+v %v", th, err)
	}
	if f[FieldFrozen] != "false" || f[FieldBy] != "" || f[FieldAt] != "" || f[FieldReason] != "" {
		t.Fatalf("after thaw = %v", f)
	}
	if len(d.comments) != 2 || !strings.Contains(d.comments[1], "thawed") || !strings.Contains(d.comments[1], "spam in #general") {
		t.Fatalf("thaw comment = %q", d.comments)
	}
	if th2, err := Thaw(context.Background(), d, "tomato-patch", ""); err != nil || th2.Changed {
		t.Fatalf("thawing a thawed land = %+v %v", th2, err)
	}
}

func TestFreezeNeedsAReason(t *testing.T) {
	d := farm()
	if _, err := Freeze(context.Background(), d, "tomato-patch", "  ", "op", t0); err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("err = %v", err)
	}
	if len(d.writes) != 0 {
		t.Fatal("no reason, no write")
	}
}

func TestFreezeRefusesOperatorLandsAndStrangers(t *testing.T) {
	d := farm()
	if _, err := Freeze(context.Background(), d, "gardenio", "x", "op", t0); !errors.Is(err, ErrOperatorLand) {
		t.Errorf("operator land: err = %v", err)
	}
	if _, err := Freeze(context.Background(), d, "nowhere", "x", "op", t0); !errors.Is(err, purse.ErrNotLand) {
		t.Errorf("no such land: err = %v", err)
	}
	d.listErr = errors.New("daemon down")
	if _, err := Freeze(context.Background(), d, "tomato-patch", "x", "op", t0); err == nil {
		t.Error("an unreadable land is an error, not a silent no-op")
	}
	if len(d.writes) != 0 {
		t.Fatal("nothing is written on a refusal")
	}
}

// A daemon that does not know the who/when/why fields still takes the
// switch: the freeze holds, with a warning.
func TestFreezeHoldsWhenTheRecordWriteFails(t *testing.T) {
	d := farm()
	d.failWrite = 2
	s, err := Freeze(context.Background(), d, "tomato-patch", "abuse report", "op", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Frozen || d.beads[0].Fields[FieldFrozen] != "true" || !strings.Contains(s.Warning, "unknown field") {
		t.Fatalf("freeze = %+v, fields %v", s, d.beads[0].Fields)
	}
	if len(d.comments) != 1 || !strings.Contains(d.comments[0], "abuse report") {
		t.Fatal("the comment still records the reason")
	}
}

func TestFreezeFailsWhenTheSwitchWriteFails(t *testing.T) {
	d := farm()
	d.failWrite = 1
	if _, err := Freeze(context.Background(), d, "tomato-patch", "x", "op", t0); err == nil {
		t.Fatal("a failed switch write is an error")
	}
	if len(d.comments) != 0 {
		t.Fatal("no comment claims a freeze that did not happen")
	}
}
