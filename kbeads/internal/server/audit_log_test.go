package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/kbeads/internal/model"
)

func TestRecordAuditEntry_CreatesAuditBead(t *testing.T) {
	srv, ms, _ := newTestServer()

	// The audit bead's CreatedBy is the entry's actor, which every caller
	// derives from the ctx identity (AUDIT §C3).
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	srv.recordAuditEntry(ctx, auditEntry{
		Actor:        "alice@example.com",
		Action:       "create",
		Resource:     "gd-test-123",
		ResourceType: "agent",
		Outcome:      "deny",
		Reason:       "insufficient permissions",
		AuthMethod:   "oidc",
		SourceIP:     "10.0.0.1",
	})

	// Verify an audit bead was created.
	var found bool
	for _, b := range ms.beads {
		if b.Type == "audit" {
			found = true
			var fields map[string]string
			if err := json.Unmarshal(b.Fields, &fields); err != nil {
				t.Fatalf("failed to unmarshal audit fields: %v", err)
			}
			if fields["actor"] != "alice@example.com" {
				t.Errorf("actor=%q, want alice@example.com", fields["actor"])
			}
			if fields["action"] != "create" {
				t.Errorf("action=%q, want create", fields["action"])
			}
			if fields["resource"] != "gd-test-123" {
				t.Errorf("resource=%q, want gd-test-123", fields["resource"])
			}
			if fields["resource_type"] != "agent" {
				t.Errorf("resource_type=%q, want agent", fields["resource_type"])
			}
			if fields["outcome"] != "deny" {
				t.Errorf("outcome=%q, want deny", fields["outcome"])
			}
			if fields["reason"] != "insufficient permissions" {
				t.Errorf("reason=%q, want 'insufficient permissions'", fields["reason"])
			}
			if fields["auth_method"] != "oidc" {
				t.Errorf("auth_method=%q, want oidc", fields["auth_method"])
			}
			if fields["source_ip"] != "10.0.0.1" {
				t.Errorf("source_ip=%q, want 10.0.0.1", fields["source_ip"])
			}
			if b.CreatedBy != "alice@example.com" {
				t.Errorf("created_by=%q, want alice@example.com", b.CreatedBy)
			}
			break
		}
	}
	if !found {
		t.Error("expected an audit bead to be created")
	}
}

func TestRecordAuditEntry_TitleFormat(t *testing.T) {
	srv, ms, _ := newTestServer()

	srv.recordAuditEntry(context.Background(), auditEntry{
		Actor:        "bob@example.com",
		Action:       "spawn",
		ResourceType: "agent",
		Outcome:      "deny",
	})

	for _, b := range ms.beads {
		if b.Type == "audit" {
			if b.Title != "authz:deny:spawn:agent" {
				t.Errorf("title=%q, want authz:deny:spawn:agent", b.Title)
			}
			return
		}
	}
	t.Error("expected an audit bead")
}

func TestRecordAuditEntry_OmitsEmptyFields(t *testing.T) {
	srv, ms, _ := newTestServer()

	srv.recordAuditEntry(context.Background(), auditEntry{
		Actor:   "static",
		Action:  "delete",
		Outcome: "allow",
	})

	for _, b := range ms.beads {
		if b.Type == "audit" {
			var fields map[string]any
			if err := json.Unmarshal(b.Fields, &fields); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}
			if _, ok := fields["source_ip"]; ok {
				t.Error("source_ip should be omitted when empty")
			}
			if _, ok := fields["escalation_id"]; ok {
				t.Error("escalation_id should be omitted when empty")
			}
			return
		}
	}
	t.Error("expected an audit bead")
}

func TestAuditEscalationApproval_CreatesBead(t *testing.T) {
	_, ms, h := newTestServer()

	// Create an escalation bead directly.
	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Escalation: test",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type":   "create",
			"operation_spec":   map[string]any{"title": "Test", "type": "task"},
			"requesting_agent": "alice@example.com",
			"context":          "denied",
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	escID := created["id"].(string)

	// Approve the escalation. approved_by in the body is a spoof attempt
	// (gd-731gBeGgl7): the audit entry must record the server-derived OIDC
	// identity (defaultTestSubject) instead, never the body value.
	rec := doJSON(t, h, "POST", "/v1/escalations/"+escID+"/approve", map[string]any{
		"approved_by": "operator@example.com",
	})
	requireStatus(t, rec, http.StatusOK)

	// Verify an audit bead exists with action=approve.
	var found bool
	for _, b := range ms.beads {
		if b.Type == "audit" {
			var fields map[string]string
			if err := json.Unmarshal(b.Fields, &fields); err != nil {
				continue
			}
			if fields["action"] == "approve" {
				found = true
				if fields["outcome"] != "allow" {
					t.Errorf("outcome=%q, want allow", fields["outcome"])
				}
				if fields["resource"] != escID {
					t.Errorf("resource=%q, want %s", fields["resource"], escID)
				}
				if fields["actor"] != defaultTestSubject {
					t.Errorf("actor=%q, want %s", fields["actor"], defaultTestSubject)
				}
				break
			}
		}
	}
	if !found {
		t.Error("expected an audit bead with action=approve")
	}
}

func TestAuditEscalationRejection_CreatesBead(t *testing.T) {
	_, ms, h := newTestServer()

	// Create an escalation bead.
	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Escalation: test reject",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type":   "create",
			"operation_spec":   map[string]any{"title": "Test", "type": "task"},
			"requesting_agent": "bob@example.com",
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	escID := created["id"].(string)

	// Reject the escalation.
	rec := doJSON(t, h, "POST", "/v1/escalations/"+escID+"/reject", map[string]any{
		"rejected_by": "operator@example.com",
		"reason":      "not justified",
	})
	requireStatus(t, rec, http.StatusOK)

	// Verify an audit bead exists with action=reject.
	var found bool
	for _, b := range ms.beads {
		if b.Type == "audit" {
			var fields map[string]string
			if err := json.Unmarshal(b.Fields, &fields); err != nil {
				continue
			}
			if fields["action"] == "reject" {
				found = true
				if fields["outcome"] != "deny" {
					t.Errorf("outcome=%q, want deny", fields["outcome"])
				}
				if fields["reason"] != "not justified" {
					t.Errorf("reason=%q, want 'not justified'", fields["reason"])
				}
				// The body's rejected_by is not trusted: the actor is the caller.
				if fields["actor"] != "test@example.com" {
					t.Errorf("actor=%q, want the authenticated caller test@example.com", fields["actor"])
				}
				break
			}
		}
	}
	if !found {
		t.Error("expected an audit bead with action=reject")
	}
}

// A denied write is recorded as an audit bead whatever the caller's token is
// bound to (gd-mRYje6R9TQ). The audit bead is the server's record: it carries
// the caller as actor and CreatedBy, but no project label, operator or scope
// taken from the caller's token. Before the fix a project-bound fleet token's
// denial was never recorded (its project binding refused the label-less audit
// bead), and a per-agent token's audit bead was stamped with the agent's
// project, filing it into the tenant project that had just been denied.
func TestAuditDenial_ScopedTokensRecordCleanAuditBead(t *testing.T) {
	const (
		projectFleetToken = "project-fleet-tok"
		typeScopedToken   = "type-scoped-tok"
		opScopedToken     = "op-scoped-tok"
	)
	for _, tc := range []struct {
		name      string
		bearer    string
		body      map[string]any
		wantActor string
	}{
		{
			// Denied by createBead's token project check.
			name:      "project-bound fleet token",
			bearer:    projectFleetToken,
			body:      map[string]any{"title": "t", "type": "task", "labels": []string{"project:other"}},
			wantActor: "proj-fleet",
		},
		{
			// Denied by the same check; this token's identity also made
			// createBead stamp project:gardenio on the audit bead.
			name:      "per-agent token",
			bearer:    testAgentToken,
			body:      map[string]any{"title": "t", "type": "task", "labels": []string{"project:other"}},
			wantActor: "crew-a",
		},
		{
			// Denied by checkTokenScopes; its type scope must not refuse
			// type=audit either.
			name:      "type-scoped token",
			bearer:    typeScopedToken,
			body:      map[string]any{"title": "t", "type": "bug", "labels": []string{"project:gardenio"}},
			wantActor: "type-fleet",
		},
		{
			name:      "op-scoped token",
			bearer:    opScopedToken,
			body:      map[string]any{"title": "t", "type": "task", "labels": []string{"project:gardenio"}},
			wantActor: "op-fleet",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, h := newClassedTestServer(t)
			s.fleetCfg.RegisterTokenFull(projectFleetToken, "proj-fleet", "gardenio", nil, nil, time.Time{}, "", false)
			s.fleetCfg.RegisterTokenFull(typeScopedToken, "type-fleet", "", []string{"task"}, nil, time.Time{}, "", false)
			s.fleetCfg.RegisterTokenFull(opScopedToken, "op-fleet", "", nil, []string{"update"}, time.Time{}, "", false)

			rec := doBearerJSON(t, h, "POST", "/v1/beads", tc.bearer, tc.body, map[string]string{"X-Operated-By": "oidc:alice@example.com"})
			requireStatus(t, rec, http.StatusForbidden)

			var audits []*model.Bead
			for _, b := range ms.beads {
				if b.Type == "audit" {
					audits = append(audits, b)
				}
			}
			if len(audits) != 1 {
				t.Fatalf("got %d audit beads, want 1 (denial: %s)", len(audits), rec.Body.String())
			}
			a := audits[0]
			var fields map[string]string
			if err := json.Unmarshal(a.Fields, &fields); err != nil {
				t.Fatalf("unmarshal audit fields: %v", err)
			}
			if fields["actor"] != tc.wantActor || fields["outcome"] != "deny" || fields["action"] != "create" {
				t.Errorf("audit fields = %v, want actor %s, deny create", fields, tc.wantActor)
			}
			if a.CreatedBy != tc.wantActor {
				t.Errorf("created_by = %q, want %q", a.CreatedBy, tc.wantActor)
			}
			if !slices.Equal(a.Labels, []string{"auto:authz-audit"}) {
				t.Errorf("labels = %v, want only auto:authz-audit", a.Labels)
			}
			// The server, not the caller's X-Operated-By or agent chain, wrote it.
			if a.OperatedBy != operatorMachine {
				t.Errorf("operated_by = %q, want %q", a.OperatedBy, operatorMachine)
			}
			if strings.HasPrefix(a.Fleet, agentFleetPrefix) {
				t.Errorf("fleet = %q, want no agent fleet on the server's record", a.Fleet)
			}
		})
	}
}

// recordAuditEntry ignores every credential value on the caller's ctx: token
// project, type and op scopes, agent identity and header-derived values.
func TestRecordAuditEntry_IgnoresCallerScopes(t *testing.T) {
	srv, ms, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"})
	ctx = WithFleet(ctx, "agent-gd-a1")
	ctx = withAuthMethod(ctx, authMethodStatic)
	ctx = WithTokenProject(ctx, "gardenio")
	ctx = WithTokenTypeScope(ctx, []string{"task"})
	ctx = WithTokenOpScope(ctx, []string{"update"})
	ctx = WithOwnerOverride(ctx, "oidc:mallory@example.com")
	srv.recordAuditEntry(ctx, auditEntry{Actor: "crew-a", Action: "create", Outcome: "deny"})

	for _, b := range ms.beads {
		if b.Type != "audit" {
			continue
		}
		if b.CreatedBy != "crew-a" {
			t.Errorf("created_by = %q, want crew-a", b.CreatedBy)
		}
		if !slices.Equal(b.Labels, []string{"auto:authz-audit"}) {
			t.Errorf("labels = %v, want only auto:authz-audit", b.Labels)
		}
		if b.Owner != "" {
			t.Errorf("owner = %q, want none from the caller's owner override", b.Owner)
		}
		return
	}
	t.Fatal("expected an audit bead")
}

// prodStoredAuditTypeConfig is the type:audit config row the prod store holds
// (gd-PUOImlkbuS, written 2026-09-22). It shadows the builtin; only the enum
// union in resolveTypeConfig carries newer builtin values to it.
const prodStoredAuditTypeConfig = `{"kind":"data","fields":[{"name":"actor","type":"string","required":true},{"name":"action","type":"enum","required":true,"values":["create","update","close","delete","spawn","approve","reject"]},{"name":"resource","type":"string"},{"name":"resource_type","type":"string"},{"name":"outcome","type":"enum","required":true,"values":["allow","deny"]},{"name":"reason","type":"string"},{"name":"auth_method","type":"enum","values":["oidc","static","fleet","anonymous"]},{"name":"source_ip","type":"string"},{"name":"escalation_id","type":"string"}]}`

// Every action and identity type that reaches auditDenial persists an audit
// bead through the real createBead path, under the prod store's stale
// type:audit row (gd-jZuL63txCs). An action or auth_method outside the
// type:audit enum used to fail the write as invalid fields, logged only as a
// slog.Warn.
func TestAuditDenial_PersistsUnderProdStoredConfig(t *testing.T) {
	actions := []string{
		"create", "update", "close", "delete", "spawn", "approve", "reject",
		"hook-execute", "revoke_token", "undo", "react", "join", "comment",
		"hook-emit", "edit_holds", "steward_delegate", "window_manage", "result", "deploy",
	}
	identityTypes := []string{"oidc", "static", "fleet", "machine", "land", runnerIdentityType, "edge", "window", "anonymous"}

	for _, idType := range identityTypes {
		for _, action := range actions {
			t.Run(idType+"/"+action, func(t *testing.T) {
				srv, ms, _ := newTestServer()
				ms.configs["type:audit"] = &model.Config{Key: "type:audit", Value: json.RawMessage(prodStoredAuditTypeConfig)}
				written := testutil.ToFloat64(auditWritesTotal)

				r := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
				r = r.WithContext(WithAuthIdentity(r.Context(), AuthIdentity{Type: idType, Subject: "crew-a"}))
				srv.auditDenial(r, &AuthzDenial{Action: action, BeadType: "task", Message: "denied"}, "gd-x")

				var audits []*model.Bead
				for _, b := range ms.beads {
					if b.Type == "audit" {
						audits = append(audits, b)
					}
				}
				if len(audits) != 1 {
					t.Fatalf("got %d audit beads, want 1", len(audits))
				}
				var fields map[string]string
				if err := json.Unmarshal(audits[0].Fields, &fields); err != nil {
					t.Fatalf("unmarshal audit fields: %v", err)
				}
				if fields["action"] != action || fields["auth_method"] != idType || fields["actor"] != "crew-a" {
					t.Errorf("audit fields = %v, want action %s, auth_method %s, actor crew-a", fields, action, idType)
				}
				if got := testutil.ToFloat64(auditWritesTotal) - written; got != 1 {
					t.Errorf("gardenio_audit_writes_total grew by %v, want 1", got)
				}
			})
		}
	}
}

// A refused audit write is counted by reason, not only logged
// (gd-jZuL63txCs), and is not counted as written.
func TestRecordAuditEntry_CountsFailedWrite(t *testing.T) {
	srv, ms, _ := newTestServer()
	written := testutil.ToFloat64(auditWritesTotal)
	invalid := testutil.ToFloat64(auditWriteFailuresTotal.WithLabelValues(auditWriteFailInvalid))

	srv.recordAuditEntry(context.Background(), auditEntry{Actor: "crew-a", Action: "not-an-action", Outcome: "deny"})

	for _, b := range ms.beads {
		if b.Type == "audit" {
			t.Fatalf("audit bead %s written for an action outside the enum", b.ID)
		}
	}
	if got := testutil.ToFloat64(auditWriteFailuresTotal.WithLabelValues(auditWriteFailInvalid)) - invalid; got != 1 {
		t.Errorf("gardenio_audit_write_failures_total{reason=invalid} grew by %v, want 1", got)
	}
	if got := testutil.ToFloat64(auditWritesTotal) - written; got != 0 {
		t.Errorf("gardenio_audit_writes_total grew by %v, want 0", got)
	}
}

func TestAuditWriteFailureReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{inputError("invalid fields: action: bad"), auditWriteFailInvalid},
		{&AuthzDenial{Action: "create", BeadType: "audit"}, auditWriteFailDenied},
		{&operatorRefusalError{Source: "header_missing"}, auditWriteFailOperator},
		{fmt.Errorf("failed to create bead: %w", errors.New("connection reset")), auditWriteFailStore},
	} {
		if got := auditWriteFailureReason(tc.err); got != tc.want {
			t.Errorf("auditWriteFailureReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
