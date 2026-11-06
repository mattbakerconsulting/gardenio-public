package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestCreateLocksReleaseInReverseOrder(t *testing.T) {
	var got []string
	var locks createLocks
	locks.hold(func() { got = append(got, "release") })
	locks.hold(func() { got = append(got, "channel") })
	locks.hold(func() { got = append(got, "card") })
	locks.release()
	if want := "card,channel,release"; strings.Join(got, ",") != want {
		t.Fatalf("release order = %v, want %s", got, want)
	}

	var empty createLocks
	empty.release() // no locks taken: a no-op
}

func TestScrubCreateInput(t *testing.T) {
	const key = "sk-ant-oat01-AbCdEf012345_gh-ijkl"
	in := createBeadInput{
		Title:       "title " + key,
		ShortTitle:  "short " + key,
		Description: "desc " + key,
		Notes:       "notes " + key,
		Fields:      json.RawMessage(`{"litellm_key":"` + key + `"}`),
	}
	if err := scrubCreateInput(&in); err != nil {
		t.Fatalf("scrubCreateInput: %v", err)
	}
	for name, v := range map[string]string{"title": in.Title, "short_title": in.ShortTitle, "description": in.Description, "notes": in.Notes} {
		if strings.Contains(v, key) {
			t.Errorf("%s still carries the key: %q", name, v)
		}
	}
	if !strings.Contains(string(in.Fields), key) {
		t.Errorf("fields were redacted, want them untouched: %s", in.Fields)
	}

	bad := createBeadInput{Title: "x", Heraldry: "not arms at all"}
	err := scrubCreateInput(&bad)
	var ie inputError
	if !errors.As(err, &ie) {
		t.Fatalf("invalid arms: err = %v, want an inputError", err)
	}
}

func TestCanonicalCreateType(t *testing.T) {
	for in, want := range map[string]model.BeadType{
		"template": model.TypeFormula,
		"task":     model.TypeTask,
		"bundle":   model.TypeBundle,
		"":         "",
	} {
		if got := canonicalCreateType(in); got != want {
			t.Errorf("canonicalCreateType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyCreateIdempotencyKey(t *testing.T) {
	releaseFields := json.RawMessage(`{"project":"gardenio","tag":"2026.11.01"}`)
	tests := []struct {
		name     string
		in       createBeadInput
		beadType model.BeadType
		wantKey  string
	}{
		{"no key", createBeadInput{}, model.TypeTask, ""},
		{"caller key", createBeadInput{IdempotencyKey: "k1"}, model.TypeTask, "k1"},
		{"release key wins", createBeadInput{IdempotencyKey: "k1", Fields: releaseFields}, model.TypeRelease, "release:gardenio:2026.11.01"},
		{"release without tag takes the open release key", createBeadInput{IdempotencyKey: "k1", Fields: json.RawMessage(`{"project":"gardenio"}`)}, model.TypeRelease, "release:gardenio/next"},
		{"release without project keeps caller key", createBeadInput{IdempotencyKey: "k1", Fields: json.RawMessage(`{"tag":"2026.11.01"}`)}, model.TypeRelease, "k1"},
		{"release fields on another type", createBeadInput{Fields: releaseFields}, model.TypeTask, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			applyCreateIdempotencyKey(&in, tt.beadType)
			if in.IdempotencyKey != tt.wantKey {
				t.Fatalf("IdempotencyKey = %q, want %q", in.IdempotencyKey, tt.wantKey)
			}
			stored := parseFieldsToStringMap(in.Fields)[idempotencyKeyField]
			if stored != tt.wantKey {
				t.Fatalf("fields.%s = %q, want %q", idempotencyKeyField, stored, tt.wantKey)
			}
		})
	}
}

func TestNewBeadForCreate(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	opID := int64(7)
	plan := &createPlan{now: now, operatedBy: "oidc:a@b.c", operatedByID: &opID}
	tc := &model.TypeConfig{Kind: model.KindIssue}
	ctx := context.Background()

	t.Run("defaults to open with the plan's stamp", func(t *testing.T) {
		bead, err := newBeadForCreate(ctx, createBeadInput{Title: "t", Priority: 2}, "gd-1", model.TypeTask, tc, plan)
		if err != nil {
			t.Fatal(err)
		}
		if bead.Status != model.StatusOpen || bead.ClosedAt != nil {
			t.Fatalf("status = %q closed_at = %v, want open and unset", bead.Status, bead.ClosedAt)
		}
		if bead.ID != "gd-1" || bead.Kind != model.KindIssue || bead.Priority != 2 {
			t.Fatalf("bead = %+v", bead)
		}
		if !bead.CreatedAt.Equal(now) || !bead.UpdatedAt.Equal(now) || bead.OperatedBy != "oidc:a@b.c" || bead.OperatedByID != &opID {
			t.Fatalf("stamp = %v %v %q %v", bead.CreatedAt, bead.UpdatedAt, bead.OperatedBy, bead.OperatedByID)
		}
		if bead.Fields != nil {
			t.Fatalf("fields = %s, want nil when none given", bead.Fields)
		}
	})

	t.Run("closed stamps closed_at", func(t *testing.T) {
		bead, err := newBeadForCreate(ctx, createBeadInput{Title: "t", Status: "closed"}, "gd-1", model.BeadType("event"), tc, plan)
		if err != nil {
			t.Fatal(err)
		}
		if bead.ClosedAt == nil || !bead.ClosedAt.Equal(now) {
			t.Fatalf("closed_at = %v, want %v", bead.ClosedAt, now)
		}
	})

	t.Run("invalid status", func(t *testing.T) {
		_, err := newBeadForCreate(ctx, createBeadInput{Title: "t", Status: "nope"}, "gd-1", model.TypeTask, tc, plan)
		if err == nil || err.Error() != "invalid status: nope" {
			t.Fatalf("err = %v, want invalid status", err)
		}
	})

	t.Run("doc defaults format to md", func(t *testing.T) {
		bead, err := newBeadForCreate(ctx, createBeadInput{Title: "t"}, "gd-1", "doc", tc, plan)
		if err != nil {
			t.Fatal(err)
		}
		if got := parseFieldsToStringMap(bead.Fields)["format"]; got != "md" {
			t.Fatalf("format = %q, want md", got)
		}
		bead, err = newBeadForCreate(ctx, createBeadInput{Title: "t", Fields: json.RawMessage(`{"format":"html"}`)}, "gd-1", "doc", tc, plan)
		if err != nil {
			t.Fatal(err)
		}
		if got := parseFieldsToStringMap(bead.Fields)["format"]; got != "html" {
			t.Fatalf("format = %q, want the caller's html", got)
		}
	})
}

func TestCreatePlanDep(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	opID := int64(3)
	plan := &createPlan{now: now, operatedByID: &opID}
	bead := &model.Bead{ID: "gd-child", CreatedBy: "alice", OperatedBy: "oidc:alice@x.y"}
	dep := plan.dep(bead, "gd-parent", model.DepParentChild)
	want := model.Dependency{BeadID: "gd-child", DependsOnID: "gd-parent", Type: model.DepParentChild, CreatedAt: now, CreatedBy: "alice", OperatedBy: "oidc:alice@x.y", OperatedByID: &opID}
	if *dep != want {
		t.Fatalf("dep = %+v, want %+v", *dep, want)
	}
}

func TestCheckAuthoringHumanGate(t *testing.T) {
	tests := []struct {
		mode, operator string
		beadType       model.BeadType
		wantErr        bool
	}{
		{"", "system:machine", "trigger", false},
		{"warn", "system:machine", "trigger", false},
		{"enforce", "system:machine", "trigger", true},
		{"enforce", "system:machine", model.TypeTask, false},
		{"enforce", "oidc:alice@example.com", "schedule", false},
	}
	for _, tt := range tests {
		t.Run(tt.mode+"/"+string(tt.beadType)+"/"+tt.operator, func(t *testing.T) {
			t.Setenv("BEADS_AUTHORING_HUMAN_GATE", tt.mode)
			err := checkAuthoringHumanGate(tt.beadType, tt.operator)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckShortTitleGate(t *testing.T) {
	t.Setenv("BEADS_SHORT_TITLE_GATE", "enforce")
	ctx := context.Background()
	if err := checkShortTitleGate(ctx, model.TypeTask, "fix the thing"); err != nil {
		t.Fatalf("supplied short_title: %v", err)
	}
	if err := checkShortTitleGate(ctx, model.TypeTask, ""); err != nil {
		t.Fatalf("human caller without short_title is only warned: %v", err)
	}
	if err := checkShortTitleGate(ctx, model.BeadType("event"), ""); err != nil {
		t.Fatalf("ungated type: %v", err)
	}

	agent := WithAuthIdentity(ctx, AuthIdentity{Type: "static"})
	if err := checkShortTitleGate(agent, model.TypeTask, ""); err == nil {
		t.Fatal("agent caller without short_title under enforce: want a refusal")
	}
	t.Setenv("BEADS_SHORT_TITLE_GATE", "off")
	if err := checkShortTitleGate(agent, model.TypeTask, ""); err != nil {
		t.Fatalf("gate off: %v", err)
	}
}

func TestExplicitParentDepWithoutStore(t *testing.T) {
	s := &BeadsServer{}
	plan := &createPlan{}
	bead := &model.Bead{ID: "gd-1"}
	dep, err := s.explicitParentDep(context.Background(), bead, "", plan)
	if dep != nil || err != nil {
		t.Fatalf("no parent: dep = %v err = %v", dep, err)
	}
	_, err = s.explicitParentDep(context.Background(), bead, "gd-1", plan)
	if err == nil || err.Error() != "parent_bead_id cannot equal the new bead's id" {
		t.Fatalf("self parent: err = %v", err)
	}
}

func TestValidateLandsCreateIgnoresOtherTypes(t *testing.T) {
	s := &BeadsServer{}
	if err := s.validateLandsCreate(context.Background(), &model.Bead{Type: model.TypeTask}); err != nil {
		t.Fatalf("task: %v", err)
	}
}

func TestValidateCreateTypeFieldsWrapsAsInputError(t *testing.T) {
	s := &BeadsServer{}
	ctx := context.Background()
	if err := s.validateCreateTypeFields(ctx, &model.Bead{Type: model.BeadType("event")}); err != nil {
		t.Fatalf("type without rules: %v", err)
	}
	err := s.validateCreateTypeFields(ctx, &model.Bead{Type: "schedule", Fields: json.RawMessage(`{}`)})
	var ie inputError
	if err == nil || !errors.As(err, &ie) {
		t.Fatalf("bad schedule: err = %v, want an inputError", err)
	}
}
