package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestListReleases_QueryAndDecode(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"releases":[{"id":"gd-r1","project":"gardenio","tag":"2026.300.1","status":"live",`+
			`"deployments":[{"target":"prod","kind":"k3s","since":"2026-10-27T05:06:00Z","until":""},`+
			`{"target":"box","since":"2026-10-27T05:00:00Z","until":"2026-10-27T06:00:00Z"}]}]}`)
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "tok")

	rels, err := c.ListReleases(context.Background(), "gardenio", "live", 5)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/releases" || gotQuery != "limit=5&project=gardenio&status=live" {
		t.Errorf("request = %s?%s", gotPath, gotQuery)
	}
	if len(rels) != 1 || rels[0].Tag != "2026.300.1" || rels[0].Status != ReleaseStatusLive {
		t.Fatalf("releases = %+v", rels)
	}
	if got := rels[0].RunsOn(); !slices.Equal(got, []string{"prod"}) {
		t.Errorf("RunsOn = %v, want [prod]", got)
	}
}

func TestDeployRelease_ByTag(t *testing.T) {
	var gotMethod, gotURI string
	var gotBody DeployReleaseRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURI = r.Method, r.URL.RequestURI()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"id":"gd-r1","tag":"2026.300.1","status":"live","deployments":[]}`)
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "tok")

	r, err := c.DeployRelease(context.Background(), "2026.300.1", "gardenio", DeployReleaseRequest{Target: "prod", Revision: "412"})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotURI != "/v1/releases/2026.300.1/deployments?project=gardenio" {
		t.Errorf("request = %s %s", gotMethod, gotURI)
	}
	if gotBody.Target != "prod" || gotBody.Revision != "412" || r.ID != "gd-r1" {
		t.Errorf("body %+v, release %+v", gotBody, r)
	}
}

func TestParseReleaseSource(t *testing.T) {
	if src := parseReleaseSource("quiet", map[string]string{"default_branch": "main"}); src != nil {
		t.Errorf("no release_tag_pattern: got %+v, want nil", src)
	}
	src := parseReleaseSource("gardenio", map[string]string{
		"release_tag_pattern": `^20\d{2}\.\d{1,3}\.\d+$`,
		"release_targets":     `[{"name":"prod","kind":"k3s"}]`,
		"default_branch":      "main",
	})
	if src == nil || src.Branch != "main" || len(src.Targets) != 1 || src.Targets[0] != (ReleaseTarget{Name: "prod", Kind: "k3s"}) {
		t.Fatalf("release source = %+v", src)
	}
	src = parseReleaseSource("gardenio", map[string]string{"release_tag_pattern": "^v", "release_branch": "release", "default_branch": "main"})
	if src.Branch != "release" {
		t.Errorf("release_branch must win over default_branch: %+v", src)
	}
}
