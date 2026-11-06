package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"

	"google.golang.org/grpc/codes"
)

// Case-variant field keys are refused on every write path (gd-tDMK1PuKzT).
// encoding/json decodes "Author" into a struct field tagged "author", so a
// guard reading fields["author"] and a reader decoding a struct must never see
// two different keys.

type fieldKeyCaseCase struct {
	name     string
	beadType string
	kind     model.Kind
	clean    map[string]any // stored before an update
	forged   map[string]any // fields (create) or patch (update) to refuse
}

func fieldKeyCaseCases() []fieldKeyCaseCase {
	msg := map[string]any{"text": "hi", "kind": "text", "author": "oidc:test@example.com", "origin": "cli"}
	return []fieldKeyCaseCase{
		{"author/Author", "message", model.KindData, msg,
			map[string]any{"text": "hi", "kind": "text", "author": "oidc:test@example.com", "Author": "oidc:ada@example.com"}},
		{"Author alone", "message", model.KindData, msg,
			map[string]any{"text": "hi", "kind": "text", "Author": "oidc:ada@example.com"}},
		{"origin/Origin", "message", model.KindData, msg,
			map[string]any{"text": "hi", "kind": "text", "origin": "cli", "Origin": "commons"}},
		{"land_kind/Land_kind", "project", model.KindConfig, map[string]any{"land_kind": "tenant"},
			map[string]any{"land_kind": "tenant", "Land_kind": "operator"}},
		{"Land_kind alone", "project", model.KindConfig, map[string]any{"land_kind": "tenant"},
			map[string]any{"Land_kind": "operator"}},
		// type:agent is open-schema, so before the check the plain unknown-key
		// rule did not catch these.
		{"agent role/Role", "agent", model.KindConfig, map[string]any{"agent": "a1", "role": "crew", "project": "gardenio"},
			map[string]any{"agent": "a1", "role": "crew", "project": "gardenio", "Role": "infra"}},
		{"agent Project alone", "agent", model.KindConfig, map[string]any{"agent": "a1", "role": "crew", "project": "gardenio"},
			map[string]any{"agent": "a1", "role": "crew", "project": "gardenio", "Project": "other"}},
		{"agent undeclared pair", "agent", model.KindConfig, map[string]any{"agent": "a1", "role": "crew", "project": "gardenio"},
			map[string]any{"agent": "a1", "role": "crew", "project": "gardenio", "x_note": "a", "X_Note": "b"}},
		{"undeclared pair", "task", model.KindIssue, map[string]any{"note": "a"},
			map[string]any{"note": "a", "NOTE": "b"}},
	}
}

func isFieldKeyCaseRefusal(msg string) bool {
	return strings.Contains(msg, "differs only in letter case") || strings.Contains(msg, "case-folds to the declared field")
}

func seedFieldKeyCaseBead(t *testing.T, ms *mockStore, id string, c fieldKeyCaseCase) {
	t.Helper()
	raw, err := json.Marshal(c.clean)
	if err != nil {
		t.Fatal(err)
	}
	ms.beads[id] = &model.Bead{ID: id, Title: "t-" + id, Kind: c.kind, Type: model.BeadType(c.beadType),
		Status: model.StatusOpen, CreatedBy: defaultTestSubject, Fields: raw}
	ms.labels[id] = []string{"project:gardenio"}
}

func TestFieldKeyCase_CreateHTTP(t *testing.T) {
	for _, c := range fieldKeyCaseCases() {
		t.Run(c.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			before := len(ms.beads)
			rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
				"title": "case " + c.name, "type": c.beadType, "labels": []string{"project:gardenio"}, "fields": c.forged,
			})
			if rec.Code != http.StatusBadRequest || !isFieldKeyCaseRefusal(rec.Body.String()) {
				t.Fatalf("create = %d %s, want 400 case-variant refusal", rec.Code, rec.Body.String())
			}
			if len(ms.beads) != before {
				t.Errorf("a bead was stored")
			}
		})
	}
}

func TestFieldKeyCase_CreateGRPC(t *testing.T) {
	for _, c := range fieldKeyCaseCases() {
		t.Run(c.name, func(t *testing.T) {
			srv, ms, ctx := testCtx(t)
			raw, _ := json.Marshal(c.forged)
			before := len(ms.beads)
			_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
				Title: "case " + c.name, Type: c.beadType, Labels: []string{"project:gardenio"}, Fields: raw,
			})
			requireCode(t, err, codes.InvalidArgument)
			if !isFieldKeyCaseRefusal(err.Error()) {
				t.Errorf("err = %v, want a case-variant refusal", err)
			}
			if len(ms.beads) != before {
				t.Errorf("a bead was stored")
			}
		})
	}
}

func TestFieldKeyCase_UpdateHTTP(t *testing.T) {
	for _, c := range fieldKeyCaseCases() {
		t.Run(c.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			const id = "gd-fkc-http"
			seedFieldKeyCaseBead(t, ms, id, c)
			stored := string(ms.beads[id].Fields)
			rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"fields": c.forged})
			if rec.Code != http.StatusBadRequest || !isFieldKeyCaseRefusal(rec.Body.String()) {
				t.Fatalf("PATCH = %d %s, want 400 case-variant refusal", rec.Code, rec.Body.String())
			}
			if got := string(ms.beads[id].Fields); got != stored {
				t.Errorf("fields = %s, want unchanged %s", got, stored)
			}
		})
	}
}

func TestFieldKeyCase_UpdateGRPC(t *testing.T) {
	for _, c := range fieldKeyCaseCases() {
		t.Run(c.name, func(t *testing.T) {
			srv, ms, ctx := testCtx(t)
			const id = "gd-fkc-grpc"
			seedFieldKeyCaseBead(t, ms, id, c)
			stored := string(ms.beads[id].Fields)
			raw, _ := json.Marshal(c.forged)
			_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: raw})
			requireCode(t, err, codes.InvalidArgument)
			if !isFieldKeyCaseRefusal(err.Error()) {
				t.Errorf("err = %v, want a case-variant refusal", err)
			}
			if got := string(ms.beads[id].Fields); got != stored {
				t.Errorf("fields = %s, want unchanged %s", got, stored)
			}
		})
	}
}

// The per-field endpoints write one key at a time, so the collision is with a
// key already stored on the bead.
func TestFieldKeyCase_FieldEndpoints(t *testing.T) {
	c := fieldKeyCaseCase{name: "endpoints", beadType: "task", kind: model.KindIssue,
		clean: map[string]any{"count": 1.0, "items": []any{"a"}, "meta": map[string]any{"k": "v"}}}
	for _, ep := range []struct {
		name, method, path string
		body               any
	}{
		{"increment", "POST", "/v1/beads/gd-fkc-ep/increment", map[string]any{"Count": 1}},
		{"increment pair in one write", "POST", "/v1/beads/gd-fkc-ep/increment", map[string]any{"total": 1, "Total": 1}},
		{"append", "POST", "/v1/beads/gd-fkc-ep/fields/Items/append", map[string]any{"element": "b"}},
		{"reorder", "POST", "/v1/beads/gd-fkc-ep/fields/ITEMS/reorder", map[string]any{"expected": []string{"a"}, "next": []string{"a"}}},
		{"edit", "POST", "/v1/beads/gd-fkc-ep/fields/Meta/edit", map[string]any{"op": "set", "key": "k", "value": "w"}},
	} {
		t.Run(ep.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			seedFieldKeyCaseBead(t, ms, "gd-fkc-ep", c)
			stored := string(ms.beads["gd-fkc-ep"].Fields)
			rec := doJSON(t, h, ep.method, ep.path, ep.body)
			if rec.Code != http.StatusBadRequest || !isFieldKeyCaseRefusal(rec.Body.String()) {
				t.Fatalf("%s = %d %s, want 400 case-variant refusal", ep.path, rec.Code, rec.Body.String())
			}
			if got := string(ms.beads["gd-fkc-ep"].Fields); got != stored {
				t.Errorf("fields = %s, want unchanged %s", got, stored)
			}
		})
	}
}

// A bead that already holds a variant (written before the check) is not
// wedged: a PATCH of an unrelated field, or one that removes the variant,
// goes through. Rewriting the variant does not.
func TestFieldKeyCase_GrandfathersStoredVariant(t *testing.T) {
	_, ms, h := newTestServer()
	const id = "gd-fkc-legacy"
	ms.beads[id] = &model.Bead{ID: id, Title: "legacy", Kind: model.KindIssue, Type: model.TypeTask,
		Status: model.StatusOpen, CreatedBy: defaultTestSubject,
		Fields: json.RawMessage(`{"note":"a","Note":"b","other":"w"}`)}
	ms.labels[id] = []string{"project:gardenio"}

	requireStatus(t, doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"fields": map[string]any{"other": "x"}}), http.StatusOK)
	// A read-modify-write client echoing the stored pair unchanged is fine too.
	requireStatus(t, doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"fields": map[string]any{"note": "a", "Note": "b", "other": "y"}}), http.StatusOK)

	rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"fields": map[string]any{"Note": "forged"}})
	if rec.Code != http.StatusBadRequest || !isFieldKeyCaseRefusal(rec.Body.String()) {
		t.Fatalf("rewriting the variant = %d %s, want 400", rec.Code, rec.Body.String())
	}

	requireStatus(t, doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"fields": map[string]any{"Note": nil}}), http.StatusOK)
	if fields := fieldsMapOf(t, ms.beads[id].Fields); fields["Note"] != nil || fields["note"] != "a" {
		t.Errorf("fields = %v, want Note removed and note kept", fields)
	}
}

// Exact keys keep working.
func TestFieldKeyCase_ExactKeysAccepted(t *testing.T) {
	_, ms, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "exact", "type": "message", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"text": "hi", "kind": "text", "origin": "cli"},
	})
	requireStatus(t, rec, http.StatusCreated)
	c := fieldKeyCaseCase{beadType: "task", kind: model.KindIssue, clean: map[string]any{"count": 1.0}}
	seedFieldKeyCaseBead(t, ms, "gd-fkc-ok", c)
	requireStatus(t, doJSON(t, h, "POST", "/v1/beads/gd-fkc-ok/increment", map[string]any{"count": 1}), http.StatusOK)
	requireStatus(t, doJSON(t, h, "PATCH", "/v1/beads/gd-fkc-ok", map[string]any{"fields": map[string]any{"count": 5}}), http.StatusOK)
}

// Before the check, a field endpoint could store a variant key in any JSON
// type (the store primitives skip schema validation), and PATCH then
// grandfathered it as an already-stored unknown key, so its value could be
// rewritten freely: an edit of fields/Land_kind followed by a PATCH of
// {"Land_kind":"operator"} passed every exact-key guard.
func TestFieldKeyCase_StoredVariantCannotBeRewritten(t *testing.T) {
	for _, c := range []fieldKeyCaseCase{
		{"project Land_kind", "project", model.KindConfig,
			map[string]any{"Land_kind": map[string]any{"k": "v"}}, map[string]any{"Land_kind": "operator"}},
		{"message Author", "message", model.KindData,
			map[string]any{"text": "hi", "kind": "text", "Author": map[string]any{"k": "v"}}, map[string]any{"Author": "oidc:ada@example.com"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			const id = "gd-fkc-chain"
			seedFieldKeyCaseBead(t, ms, id, c)
			rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"fields": c.forged})
			if rec.Code != http.StatusBadRequest || !isFieldKeyCaseRefusal(rec.Body.String()) {
				t.Fatalf("PATCH = %d %s, want 400 case-variant refusal", rec.Code, rec.Body.String())
			}
		})
	}
}
