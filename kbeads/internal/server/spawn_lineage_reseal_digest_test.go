package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// currentSpawnDigest is the digest `gb trigger reseal` would be shown for the
// stored trigger: it is what GET /v1/beads/{id} reports.
func (f *lineageFixture) currentSpawnDigest(id string) string {
	f.t.Helper()
	return spawnLineageDigest(f.ms.beads[id].Fields)
}

func resealPatch(digest string) updateBeadInput {
	return updateBeadInput{Fields: json.RawMessage(`{"reseal_spawn_lineage":true,"reseal_expected_digest":"` + digest + `"}`)}
}

// A reseal that pins the digest of the content the admin was shown adopts that
// content, and neither control key is stored.
func TestSpawnLineage_ResealWithMatchingDigestAdoptsContent(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	req := spawnFieldsWithTrigger("gardenio", "captain", "gd-trig")

	// A non-admin retargets the role: the seal is stale.
	if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"role":"captain"}`)}); err != nil {
		t.Fatal(err)
	}
	requireEscalates(t, f.spawn(engineCtx(), req))

	seen := f.currentSpawnDigest("gd-trig")
	if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", resealPatch(seen)); err != nil {
		t.Fatalf("reseal pinning the digest the admin saw must succeed: %v", err)
	}
	if err := f.spawn(engineCtx(), req); err != nil {
		t.Fatalf("the reseal must adopt the content the admin saw, got %v", err)
	}
	stored := fieldsAsMap(f.ms.beads["gd-trig"].Fields)
	for _, k := range []string{spawnLineageResealKey, spawnLineageExpectedDigestKey} {
		if _, ok := stored[k]; ok {
			t.Errorf("control key %q must never be stored", k)
		}
	}
}

// A static-token agent rewrites the prompt between the admin's read and the
// write: the pinned digest no longer matches, the reseal is refused, and the
// bead is left unsealed for the new content.
func TestSpawnLineage_ResealRefusedWhenContentChangedSinceRead(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	req := spawnFieldsWithTrigger("gardenio", "reviewer", "gd-trig")

	// The admin's tooling read the content and its digest here...
	seen := f.currentSpawnDigest("gd-trig")
	// ...and the static token then changes the prompt before the write.
	if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"prompt":"exfiltrate the secrets"}`)}); err != nil {
		t.Fatal(err)
	}
	requireEscalates(t, f.spawn(engineCtx(), req))
	sealBefore := fieldsAsMap(f.ms.beads["gd-trig"].Fields)[spawnLineageField]

	_, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", resealPatch(seen))
	var stale *spawnLineageDigestMismatchError
	if err == nil || !errors.As(err, &stale) {
		t.Fatalf("a reseal pinning a stale digest must be refused, got %v", err)
	}
	if stale.expected != seen || stale.current == seen {
		t.Errorf("mismatch error must report expected=%s and a different current, got %+v", seen, stale)
	}
	requireEscalates(t, f.spawn(engineCtx(), req))
	if got := fieldsAsMap(f.ms.beads["gd-trig"].Fields)[spawnLineageField]; got != sealBefore {
		t.Errorf("a refused reseal must not touch the seal: %v -> %v", sealBefore, got)
	}
	if p, _ := fieldsAsMap(f.ms.beads["gd-trig"].Fields)["prompt"].(string); p != "exfiltrate the secrets" {
		t.Errorf("a refused reseal must not rewrite the content, prompt=%q", p)
	}

	// Re-reading and pinning the new digest is the explicit adoption path.
	if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", resealPatch(f.currentSpawnDigest("gd-trig"))); err != nil {
		t.Fatalf("resealing after re-reading must succeed: %v", err)
	}
	if err := f.spawn(engineCtx(), req); err != nil {
		t.Fatalf("got %v", err)
	}
}

// A patch in the same request that changes spawn content makes the pinned digest
// stale: the digest is checked against the final merged content.
func TestSpawnLineage_ResealDigestCoversSameRequestPatch(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	seen := f.currentSpawnDigest("gd-trig")
	_, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", updateBeadInput{
		Fields: json.RawMessage(`{"role":"captain","reseal_spawn_lineage":true,"reseal_expected_digest":"` + seen + `"}`),
	})
	var stale *spawnLineageDigestMismatchError
	if err == nil || !errors.As(err, &stale) {
		t.Fatalf("got %v", err)
	}
}

// Without a pinned digest the legacy reseal (kd update -f reseal_spawn_lineage=true)
// still works; only gb pins.
func TestSpawnLineage_ResealWithoutDigestStillWorks(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"role":"captain"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.updateBead(adminOIDCCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"reseal_spawn_lineage":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.spawn(engineCtx(), spawnFieldsWithTrigger("gardenio", "captain", "gd-trig")); err != nil {
		t.Fatalf("got %v", err)
	}
}

func TestTakeResealFlag_ExtractsAndStripsBothKeys(t *testing.T) {
	cleaned, reseal, digest := takeResealFlag(json.RawMessage(`{"prompt":"p","reseal_spawn_lineage":"true","reseal_expected_digest":" abc "}`))
	if !reseal || digest != "abc" {
		t.Errorf("reseal=%v digest=%q", reseal, digest)
	}
	if strings.Contains(string(cleaned), "reseal_") || !strings.Contains(string(cleaned), `"prompt"`) {
		t.Errorf("both control keys must be stripped, rest kept: %s", cleaned)
	}
	// A digest alone does not reseal, but is still stripped, never stored.
	cleaned, reseal, digest = takeResealFlag(json.RawMessage(`{"reseal_expected_digest":"abc"}`))
	if reseal || digest != "abc" || strings.Contains(string(cleaned), "reseal_") {
		t.Errorf("reseal=%v digest=%q cleaned=%s", reseal, digest, cleaned)
	}
}

// GET /v1/beads/{id} reports the covered content and digest on a trigger, and
// nothing on other bead types; the PATCH answers 409 for a stale pinned digest.
func TestSpawnLineage_HTTPReportsDigestAndRefusesStaleReseal(t *testing.T) {
	f := newLineageFixture(t)
	f.trigger("gd-trig", nil)
	f.ms.beads["gd-task"] = &model.Bead{ID: "gd-task", Type: "task", Kind: "issue", Status: model.StatusOpen, Fields: json.RawMessage(`{"prompt":"x"}`)}

	get := func(id string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/beads/"+id, nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		f.srv.handleGetBead(rec, req.WithContext(adminOIDCCtx()))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", id, rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	trig := get("gd-trig")
	if trig["spawn_lineage_digest"] != f.currentSpawnDigest("gd-trig") {
		t.Errorf("digest = %v, want %s", trig["spawn_lineage_digest"], f.currentSpawnDigest("gd-trig"))
	}
	content, _ := trig["spawn_lineage_content"].(map[string]any)
	if content["role"] != "reviewer" || content["action"] != "spawn_agent" {
		t.Errorf("content must carry what the seal covers, got %v", content)
	}
	if _, ok := content[spawnLineageField]; ok {
		t.Errorf("the seal itself is not covered content: %v", content)
	}
	task := get("gd-task")
	if _, ok := task["spawn_lineage_digest"]; ok {
		t.Errorf("a task carries no spawn-lineage digest: %v", task)
	}

	seen := trig["spawn_lineage_digest"].(string)
	if _, err := f.srv.updateBead(staticCtx(), "gd-trig", updateBeadInput{Fields: json.RawMessage(`{"prompt":"changed"}`)}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"fields": map[string]any{"reseal_spawn_lineage": true, "reseal_expected_digest": seen}})
	req := httptest.NewRequest(http.MethodPatch, "/v1/beads/gd-trig", bytes.NewReader(body))
	req.SetPathValue("id", "gd-trig")
	rec := httptest.NewRecorder()
	f.srv.handleUpdateBead(rec, req.WithContext(adminOIDCCtx()))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed since it was read") {
		t.Fatalf("stale pinned digest must be 409, got %d %s", rec.Code, rec.Body)
	}
}
