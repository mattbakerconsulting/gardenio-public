package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// Store methods SetTaint/GetTaint/ListTaints against the taints table
// (docs/adr/taints.md §3/§6.2/§10.1), gd-u0EiDErErR.

func TestQuerySetTaint_CreatesWithValidPolicy(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT type FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("team-acme").
		WillReturnRows(sqlmock.NewRows([]string{"type"}).AddRow("team"))
	policy := []byte(`{"default":"human-four-eyes","rules":[{"when":{"kind":"purge"},"mode":"human-four-eyes","approvers":["team:acme-prod-owners"]}]}`)
	mock.ExpectQuery(`INSERT INTO taints`).
		WithArgs("acme-production", "prod data", "team-acme", policy, pq.Array([]string{"gate"})).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow(int64(1), now, now))

	taint := &model.Taint{
		Name:        "acme-production",
		Description: "prod data",
		OwnerTeam:   "team-acme",
		ScrubPolicy: policy,
	}
	if err := querySetTaint(context.Background(), db, taint); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if taint.ID != 1 {
		t.Fatalf("expected id=1, got %d", taint.ID)
	}
	if string(taint.ScrubPolicy) != string(policy) {
		t.Fatalf("scrub_policy not round-tripped unchanged: got %s", taint.ScrubPolicy)
	}
}

func TestQuerySetTaint_NoScrubPolicyStoresHumanFourEyesDefault(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT type FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("team-acme").
		WillReturnRows(sqlmock.NewRows([]string{"type"}).AddRow("team"))
	mock.ExpectQuery(`INSERT INTO taints`).
		WithArgs("acme-production", "", "team-acme", sqlmock.AnyArg(), pq.Array([]string{"gate"})).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow(int64(1), now, now))

	taint := &model.Taint{Name: "acme-production", OwnerTeam: "team-acme"}
	if err := querySetTaint(context.Background(), db, taint); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got model.ScrubPolicy
	if err := json.Unmarshal(taint.ScrubPolicy, &got); err != nil {
		t.Fatalf("unmarshal stored scrub_policy: %v", err)
	}
	if got.Default != model.ScrubModeHumanFourEyes {
		t.Fatalf("expected default human-four-eyes, got %q", got.Default)
	}
	if len(got.Approvers) != 1 || got.Approvers[0] != "team:team-acme" {
		t.Fatalf("expected owner team as approver, got %v", got.Approvers)
	}
}

func TestQuerySetTaint_RejectsDefaultAuto(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT type FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("team-acme").
		WillReturnRows(sqlmock.NewRows([]string{"type"}).AddRow("team"))

	taint := &model.Taint{
		Name:        "acme-production",
		OwnerTeam:   "team-acme",
		ScrubPolicy: []byte(`{"default":"auto"}`),
	}
	err := querySetTaint(context.Background(), db, taint)
	assertValidationErrorOnField(t, err, "scrub_policy.default")
}

func TestQuerySetTaint_RejectsAutoRuleMissingMaxBeads(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT type FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("team-acme").
		WillReturnRows(sqlmock.NewRows([]string{"type"}).AddRow("team"))

	taint := &model.Taint{
		Name:      "acme-production",
		OwnerTeam: "team-acme",
		ScrubPolicy: []byte(`{
			"default": "human-four-eyes",
			"rules": [{"when": {"older_than_days": 30}, "mode": "auto"}]
		}`),
	}
	err := querySetTaint(context.Background(), db, taint)
	assertValidationErrorOnField(t, err, "scrub_policy.rules[0].max_beads")
}

func TestQuerySetTaint_RejectsUnknownMode(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT type FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("team-acme").
		WillReturnRows(sqlmock.NewRows([]string{"type"}).AddRow("team"))

	taint := &model.Taint{
		Name:      "acme-production",
		OwnerTeam: "team-acme",
		ScrubPolicy: []byte(`{
			"default": "human-four-eyes",
			"rules": [{"when": {"kind": "purge"}, "mode": "yolo"}]
		}`),
	}
	err := querySetTaint(context.Background(), db, taint)
	assertValidationErrorOnField(t, err, "scrub_policy.rules[0].mode")
}

func TestQuerySetTaint_RejectsOwnerTeamWrongType(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT type FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("gd-not-a-team").
		WillReturnRows(sqlmock.NewRows([]string{"type"}).AddRow("task"))

	taint := &model.Taint{
		Name:        "acme-production",
		OwnerTeam:   "gd-not-a-team",
		ScrubPolicy: []byte(`{"default":"human"}`),
	}
	err := querySetTaint(context.Background(), db, taint)
	assertValidationErrorOnField(t, err, "owner_team")
}

func TestQuerySetTaint_RejectsOwnerTeamNotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT type FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("gd-ghost").
		WillReturnError(sql.ErrNoRows)

	taint := &model.Taint{
		Name:        "acme-production",
		OwnerTeam:   "gd-ghost",
		ScrubPolicy: []byte(`{"default":"human"}`),
	}
	err := querySetTaint(context.Background(), db, taint)
	assertValidationErrorOnField(t, err, "owner_team")
}

func TestQueryGetTaint(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	policy := []byte(`{"default":"human-four-eyes"}`)
	mock.ExpectQuery(`SELECT .+ FROM taints WHERE name = \$1`).
		WithArgs("acme-production").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "owner_team", "scrub_policy", "effects", "created_at", "updated_at", "retired_at"}).
			AddRow(int64(1), "acme-production", "prod", "team-acme", policy, "{gate}", now, now, nil))

	got, err := queryGetTaint(context.Background(), db, "acme-production")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "acme-production" || string(got.ScrubPolicy) != string(policy) {
		t.Fatalf("unexpected taint: %+v", got)
	}
	if got.RetiredAt != nil {
		t.Fatalf("expected nil retired_at, got %v", got.RetiredAt)
	}
	if len(got.Effects) != 1 || got.Effects[0] != "gate" {
		t.Fatalf("expected effects [gate], got %v", got.Effects)
	}
}

func TestQueryGetTaint_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT .+ FROM taints WHERE name = \$1`).
		WithArgs("nonexistent").
		WillReturnError(sql.ErrNoRows)

	if _, err := queryGetTaint(context.Background(), db, "nonexistent"); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestQueryListTaints(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .+ FROM taints ORDER BY name`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "owner_team", "scrub_policy", "effects", "created_at", "updated_at", "retired_at"}).
			AddRow(int64(1), "acme-production", "", "team-acme", []byte(`{"default":"human"}`), "{gate}", now, now, nil).
			AddRow(int64(2), "jira-prod", "", "team-acme", []byte(`{"default":"human"}`), "{}", now, now, nil))

	taints, err := queryListTaints(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(taints) != 2 {
		t.Fatalf("expected 2 taints, got %d", len(taints))
	}
	if taints[0].Name != "acme-production" || taints[1].Name != "jira-prod" {
		t.Fatalf("unexpected order: %q, %q", taints[0].Name, taints[1].Name)
	}
	if len(taints[0].Effects) != 1 || taints[0].Effects[0] != "gate" {
		t.Fatalf("expected effects [gate], got %v", taints[0].Effects)
	}
	if len(taints[1].Effects) != 0 {
		t.Fatalf("expected empty effects, got %v", taints[1].Effects)
	}
}

func assertValidationErrorOnField(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	ve, ok := err.(*model.ValidationError)
	if !ok {
		t.Fatalf("expected *model.ValidationError, got %T: %v", err, err)
	}
	for _, fe := range ve.Errors {
		if fe.Field == field {
			return
		}
	}
	t.Fatalf("expected a validation error on field %q, got %+v", field, ve.Errors)
}
