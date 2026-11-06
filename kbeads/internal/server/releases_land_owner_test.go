package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// A land's owner reports where its own releases run (the Lands contract's
// reportDeployment, releases plan gd-UZKZ9H25UD step 6): through the deploy
// route, on its scoped bearer, as via=contract. A member of the land, an owner
// of another land and a release of another land are refused, and a refusal
// changes nothing.
func TestRelease_LandOwnerReportsDeployment(t *testing.T) {
	f := newScopedFixture(t)
	land := `{"release_tag_pattern":"^v\\d+\\.\\d+\\.\\d+$","release_targets":[{"name":"prod","kind":"land"}]}`
	seedReleaseProject(f.ms, "proj-claimlayer", "claimlayer", land)
	seedReleaseProject(f.ms, "proj-other", "other", land)

	create := func(project, tag string) releaseView {
		t.Helper()
		code, body := f.do(t, tokOwnerAdmin, http.MethodPost, "/v1/releases", map[string]any{"project": project, "tag": tag})
		if code != http.StatusCreated {
			t.Fatalf("create %s %s as admin: %d %s", project, tag, code, body)
		}
		return decodeRelease(t, []byte(body))
	}
	own := create("claimlayer", "v1.0.0")
	theirs := create("other", "v1.0.0")

	deploy := func(token, ref string, body map[string]any) (int, string) {
		t.Helper()
		path, query, _ := strings.Cut(ref, "?")
		path = "/v1/releases/" + path + "/deployments"
		if query != "" {
			path += "?" + query
		}
		return f.do(t, token, http.MethodPost, path, body)
	}
	unchanged := func(what, id string) {
		t.Helper()
		if v := releaseViewOf(f.ms.beads[id]); v.Status != releaseStatusCut || len(v.Deployments) != 0 {
			t.Errorf("%s changed the release: %s %+v", what, v.Status, v.Deployments)
		}
	}

	if code, body := deploy(tokScopedWriter, own.ID, map[string]any{"target": "prod"}); code != http.StatusForbidden {
		t.Errorf("deploy as a member of the land: %d %s, want 403", code, body)
	}
	unchanged("a member's deploy", own.ID)
	// Another land's release answers as the read gate answers it.
	if code, body := deploy(tokScopedOwner, theirs.ID, map[string]any{"target": "prod"}); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Errorf("deploy another land's release: %d %s, want 403 or 404", code, body)
	}
	unchanged("a deploy of another land's release", theirs.ID)

	code, body := deploy(tokScopedOwner, "v1.0.0?project=claimlayer", map[string]any{"target": "prod", "revision": "8", "via": "script"})
	if code != http.StatusOK {
		t.Fatalf("deploy as the land's owner: %d %s", code, body)
	}
	got := decodeRelease(t, []byte(body))
	if got.ID != own.ID || got.Status != releaseStatusLive {
		t.Fatalf("owner deploy: %+v", got)
	}
	if d := got.Deployments; len(d) != 1 || d[0].Via != "contract" || d[0].Revision != "8" || d[0].Kind != "land" {
		t.Errorf("owner's row = %+v, want via=contract (whatever the body says), revision 8, kind land", d)
	}
	if code, _ := deploy(tokScopedOwner, own.ID, map[string]any{"target": "staging"}); code != http.StatusBadRequest {
		t.Errorf("owner deploy to an undeclared target: %d, want 400", code)
	}
}

// A release with no single land is no owner's: releaseDeployDenial refuses it
// before the tier lookup, which would otherwise pass a release with no
// project label at all.
func TestReleaseDeployDenial_NeedsOneLand(t *testing.T) {
	for _, labels := range [][]string{nil, {"project:claimlayer", "project:other"}} {
		rel := &model.Bead{ID: "r", Type: model.TypeRelease, Labels: labels, Fields: json.RawMessage(`{}`)}
		if releaseDeployDenial(t.Context(), rel) == nil {
			t.Errorf("labels %v: admitted, want a denial", labels)
		}
	}
}
