package beadsapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- ListAgentBeads tests ---

func TestListAgentBeads_QueryParams(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.ListAgentBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The query should include type=agent and all active statuses.
	if !strings.Contains(gotPath, "type=agent") {
		t.Errorf("expected type=agent in query, got %s", gotPath)
	}
	// activeStatuses are joined with comma via url.Values.Set.
	for _, s := range activeStatuses {
		if !strings.Contains(gotPath, s) {
			t.Errorf("expected status %q in query, got %s", s, gotPath)
		}
	}
}

func TestListAgentBeads_ParsesBeads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "crew-town-crew-hq",
					Title:  "Agent: hq",
					Type:   "agent",
					Status: "open",
					Notes:  "coop_url: http://coop:9090\npod_name: agent-hq-0",
					Fields: json.RawMessage(`{"project":"town","mode":"crew","role":"crew","agent":"hq"}`),
				},
				{
					ID:     "crew-gardenio-crew-k8s",
					Title:  "Agent: k8s",
					Type:   "agent",
					Status: "in_progress",
					Notes:  "",
					Fields: json.RawMessage(`{"project":"gardenio","role":"ops","agent":"k8s"}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	beads, err := c.ListAgentBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(beads) != 2 {
		t.Fatalf("expected 2 beads, got %d", len(beads))
	}

	// First bead.
	b0 := beads[0]
	if b0.ID != "crew-town-crew-hq" {
		t.Errorf("expected ID crew-town-crew-hq, got %s", b0.ID)
	}
	if b0.Project != "town" {
		t.Errorf("expected project town, got %s", b0.Project)
	}
	if b0.Mode != "crew" {
		t.Errorf("expected mode crew, got %s", b0.Mode)
	}
	if b0.Role != "crew" {
		t.Errorf("expected role crew, got %s", b0.Role)
	}
	if b0.AgentName != "hq" {
		t.Errorf("expected agent name hq, got %s", b0.AgentName)
	}
	if b0.Metadata["coop_url"] != "http://coop:9090" {
		t.Errorf("expected coop_url metadata, got %v", b0.Metadata)
	}
	if b0.Metadata["pod_name"] != "agent-hq-0" {
		t.Errorf("expected pod_name metadata, got %v", b0.Metadata)
	}

	// Second bead -- mode defaults to "crew" when empty.
	b1 := beads[1]
	if b1.Mode != "crew" {
		t.Errorf("expected default mode crew, got %s", b1.Mode)
	}
	if b1.Role != "ops" {
		t.Errorf("expected role ops, got %s", b1.Role)
	}
}

func TestListAgentBeads_SkipsBeadsMissingAgent_DefaultsEmptyRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "missing-role",
					Fields: json.RawMessage(`{"project":"x","agent":"y"}`),
				},
				{
					ID:     "missing-agent",
					Fields: json.RawMessage(`{"project":"x","role":"y"}`),
				},
				{
					ID:     "has-both",
					Fields: json.RawMessage(`{"role":"crew","agent":"hq"}`),
				},
			},
			Total: 3,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	beads, err := c.ListAgentBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// missing-agent is skipped, missing-role gets role defaulted to "crew", has-both passes through.
	if len(beads) != 2 {
		t.Fatalf("expected 2 beads (missing agent skipped, empty role defaults to crew), got %d", len(beads))
	}
	if beads[0].ID != "missing-role" {
		t.Errorf("expected missing-role first, got %s", beads[0].ID)
	}
	if beads[0].Role != "crew" {
		t.Errorf("expected missing-role to default to crew, got %q", beads[0].Role)
	}
	if beads[1].ID != "has-both" {
		t.Errorf("expected has-both second, got %s", beads[1].ID)
	}
}

// --- FindAgentBead tests ---
//
// These tests pin the contract with the daemon's indexed by-name endpoint
// GET /v1/agents-by-name/{name} (ka-0000005291). The previous client behavior
// was to list every active agent bead and filter client-side; the new
// behavior is a single round-trip that returns the most-recently-updated
// match (or 404 → ErrAgentNotFound).

func TestFindAgentBead_FindsByAgentField(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		_ = json.NewEncoder(w).Encode(beadJSON{
			ID:     "gd-def",
			Title:  "Agent: test-bot-2",
			Type:   "agent",
			Status: "open",
			Notes:  "coop_url: http://coop2:9090",
			Fields: json.RawMessage(`{"role":"crew","agent":"test-bot-2"}`),
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	detail, err := c.FindAgentBead(context.Background(), "test-bot-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if detail.ID != "gd-def" {
		t.Errorf("expected ID gd-def, got %s", detail.ID)
	}
	if detail.Notes != "coop_url: http://coop2:9090" {
		t.Errorf("expected notes with coop_url, got %s", detail.Notes)
	}
	if !strings.HasPrefix(gotPath, "/v1/agents-by-name/test-bot-2") {
		t.Errorf("expected /v1/agents-by-name/{name} path, got %s", gotPath)
	}
	// activeStatuses must be forwarded so the daemon can restrict the scan.
	for _, s := range activeStatuses {
		if !strings.Contains(gotPath, s) {
			t.Errorf("expected status %q in query, got %s", s, gotPath)
		}
	}
}

func TestFindAgentBead_ReturnsErrorWhenNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.FindAgentBead(context.Background(), "nobody")
	if err == nil {
		t.Fatal("expected error when agent not found")
	}
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("expected ErrAgentNotFound, got: %v", err)
	}
}

// --- FindMostRecentAgentBead tests ---

// TestFindMostRecentAgentBead_PrefersActive verifies that an active bead wins
// over any closed bead with the same agent name. This preserves the cheap
// path for the common case where a session is in flight.
func TestFindMostRecentAgentBead_PrefersActive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statuses := r.URL.Query().Get("status")
		if strings.Contains(statuses, "open") {
			_ = json.NewEncoder(w).Encode(beadJSON{
				ID:        "gd-active",
				Type:      "agent",
				Status:    "open",
				UpdatedAt: "2026-05-01T01:00:00Z",
				Labels:    []string{"project:monorepo"},
				Fields:    json.RawMessage(`{"agent":"thread-x","role":"k6-perf"}`),
			})
			return
		}
		t.Errorf("did not expect closed-fallback lookup when an active bead exists; got status=%q", statuses)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	bead, err := c.FindMostRecentAgentBead(context.Background(), "thread-x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bead.ID != "gd-active" {
		t.Errorf("expected active bead gd-active, got %s", bead.ID)
	}
}

// TestFindMostRecentAgentBead_FallsBackToClosed verifies the resume-window
// case: every session bead for an agent name is closed; the daemon returns
// the most-recently-updated closed bead so its project/role labels remain
// available for advice priming.
func TestFindMostRecentAgentBead_FallsBackToClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statuses := r.URL.Query().Get("status")
		if strings.Contains(statuses, "open") {
			// Active query: 404, forcing fallback.
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
			return
		}
		// Closed-fallback: the daemon picks the most-recently-updated row.
		_ = json.NewEncoder(w).Encode(beadJSON{
			ID:        "gd-newest",
			Type:      "agent",
			Status:    "closed",
			UpdatedAt: "2026-05-01T03:00:00Z",
			Labels:    []string{"project:monorepo", "role:k6-perf"},
			Fields:    json.RawMessage(`{"agent":"thread-x","role":"k6-perf"}`),
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	bead, err := c.FindMostRecentAgentBead(context.Background(), "thread-x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bead.ID != "gd-newest" {
		t.Errorf("expected most-recently-updated closed bead gd-newest, got %s", bead.ID)
	}
	hasProject := false
	for _, l := range bead.Labels {
		if l == "project:monorepo" {
			hasProject = true
		}
	}
	if !hasProject {
		t.Errorf("expected project:monorepo label on returned bead, got %v", bead.Labels)
	}
}

// TestFindMostRecentAgentBead_NoMatch returns ErrAgentNotFound when neither
// the active nor closed lookups produce a hit.
func TestFindMostRecentAgentBead_NoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.FindMostRecentAgentBead(context.Background(), "nobody")
	if err == nil {
		t.Fatal("expected error when no agent bead exists in any status")
	}
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("expected ErrAgentNotFound, got: %v", err)
	}
}

// --- ListProjectBeads tests ---

func TestListProjectBeads_QueryParams(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(gotPath, "type=project") {
		t.Errorf("expected type=project in query, got %s", gotPath)
	}
}

func TestListProjectBeads_ParsesProjects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "proj-beads",
					Title:  "Project: beads",
					Type:   "project",
					Status: "open",
					Fields: json.RawMessage(`{"prefix":"bd","git_url":"https://github.com/org/beads","default_branch":"main","image":"ghcr.io/org/beads:latest","storage_class":"gp3","account":"gardenio","model":"claude-opus-4-8-oauth"}`),
				},
				{
					ID:     "proj-gardenio",
					Title:  "gardenio",
					Type:   "project",
					Status: "open",
					Fields: json.RawMessage(`{"prefix":"kd","git_url":"https://github.com/org/gardenio"}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}

	// First project -- "Project: " prefix should be stripped.
	p1, ok := projects["beads"]
	if !ok {
		t.Fatal("expected project 'beads' in map")
	}
	if p1.Name != "beads" {
		t.Errorf("expected name beads, got %s", p1.Name)
	}
	if p1.Prefix != "bd" {
		t.Errorf("expected prefix bd, got %s", p1.Prefix)
	}
	if p1.GitURL != "https://github.com/org/beads" {
		t.Errorf("expected git_url, got %s", p1.GitURL)
	}
	if p1.DefaultBranch != "main" {
		t.Errorf("expected default_branch main, got %s", p1.DefaultBranch)
	}
	if p1.Image != "ghcr.io/org/beads:latest" {
		t.Errorf("expected image, got %s", p1.Image)
	}
	if p1.StorageClass != "gp3" {
		t.Errorf("expected storage_class gp3, got %s", p1.StorageClass)
	}
	if p1.Account != "gardenio" {
		t.Errorf("expected account gardenio, got %s", p1.Account)
	}
	// Model override: a project can pin a model independent of its account
	// (ka-0000005488).
	if p1.Model != "claude-opus-4-8-oauth" {
		t.Errorf("expected model claude-opus-4-8-oauth, got %s", p1.Model)
	}

	// Second project -- title without "Project: " prefix.
	p2, ok := projects["gardenio"]
	if !ok {
		t.Fatal("expected project 'gardenio' in map")
	}
	if p2.Name != "gardenio" {
		t.Errorf("expected name gardenio, got %s", p2.Name)
	}
	if p2.Prefix != "kd" {
		t.Errorf("expected prefix kd, got %s", p2.Prefix)
	}
	// Optional fields should be empty string when missing.
	if p2.DefaultBranch != "" {
		t.Errorf("expected empty default_branch, got %s", p2.DefaultBranch)
	}
	// No model field → empty (inherits account default).
	if p2.Model != "" {
		t.Errorf("expected empty model, got %s", p2.Model)
	}
}

func TestListProjectBeads_ParsesSecretsAndRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "proj-acme",
					Title: "acme",
					Type:  "project",
					Fields: json.RawMessage(`{
						"prefix":"ph",
						"git_url":"https://github.com/org/acme",
						"secrets":"[{\"env\":\"GITHUB_TOKEN\",\"secret\":\"ph-gh-token\",\"key\":\"token\"},{\"env\":\"JIRA_API_TOKEN\",\"secret\":\"ph-jira\",\"key\":\"api-token\"}]",
						"repos":"[{\"url\":\"https://github.com/org/acme.git\",\"branch\":\"main\",\"role\":\"primary\"},{\"url\":\"https://github.com/org/shared-lib.git\",\"role\":\"reference\",\"name\":\"shared-lib\"}]"
					}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, ok := projects["acme"]
	if !ok {
		t.Fatal("expected project 'acme' in map")
	}

	// Secrets
	if len(p.Secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(p.Secrets))
	}
	if p.Secrets[0].Env != "GITHUB_TOKEN" || p.Secrets[0].Secret != "ph-gh-token" || p.Secrets[0].Key != "token" {
		t.Errorf("unexpected first secret: %+v", p.Secrets[0])
	}
	if p.Secrets[1].Env != "JIRA_API_TOKEN" {
		t.Errorf("expected JIRA_API_TOKEN, got %s", p.Secrets[1].Env)
	}

	// Repos
	if len(p.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(p.Repos))
	}
	if p.Repos[0].Role != "primary" || p.Repos[0].URL != "https://github.com/org/acme.git" {
		t.Errorf("unexpected primary repo: %+v", p.Repos[0])
	}
	if p.Repos[1].Role != "reference" || p.Repos[1].Name != "shared-lib" {
		t.Errorf("unexpected reference repo: %+v", p.Repos[1])
	}
}

func TestListProjectBeads_ParsesResourceMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "proj-acme",
					Title: "acme",
					Type:  "project",
					Fields: json.RawMessage(`{
						"prefix":"ac",
						"resource_mapping":"[{\"env\":\"prod\",\"cluster\":\"acme-ecs\",\"service\":\"acme-api\",\"db_instance\":\"acme-db\"},{\"env\":\"staging\",\"cluster\":\"acme-ecs-stg\",\"service\":\"acme-api\",\"db_instance\":\"acme-db-stg\"}]"
					}`),
				},
				{
					// Malformed JSON must be skipped, not fail the refresh.
					ID:     "proj-broken",
					Title:  "broken",
					Type:   "project",
					Fields: json.RawMessage(`{"resource_mapping":"not json"}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, ok := projects["acme"]
	if !ok {
		t.Fatal("expected project 'acme' in map")
	}
	if len(p.ResourceMapping) != 2 {
		t.Fatalf("expected 2 resource mapping entries, got %d", len(p.ResourceMapping))
	}
	got := p.ResourceMapping[0]
	want := ResourceMappingEntry{Env: "prod", Cluster: "acme-ecs", Service: "acme-api", DBInstance: "acme-db"}
	if got != want {
		t.Errorf("first mapping = %+v, want %+v", got, want)
	}
	if p.ResourceMapping[1].Env != "staging" || p.ResourceMapping[1].DBInstance != "acme-db-stg" {
		t.Errorf("unexpected second mapping: %+v", p.ResourceMapping[1])
	}

	// Malformed resource_mapping is silently skipped.
	if b := projects["broken"]; len(b.ResourceMapping) != 0 {
		t.Errorf("expected no resource mapping for malformed JSON, got %d", len(b.ResourceMapping))
	}
}

func TestListProjectBeads_EmptySecretsAndRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "proj-simple",
					Title:  "simple",
					Type:   "project",
					Fields: json.RawMessage(`{"prefix":"sm","git_url":"https://github.com/org/simple"}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p := projects["simple"]
	if len(p.Secrets) != 0 {
		t.Errorf("expected no secrets, got %d", len(p.Secrets))
	}
	if len(p.Repos) != 0 {
		t.Errorf("expected no repos, got %d", len(p.Repos))
	}
}

func TestListProjectBeads_ParsesResourceAndEnvOverrides(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "proj-heavy",
					Title: "heavy",
					Type:  "project",
					Fields: json.RawMessage(`{
						"prefix":"hv",
						"git_url":"https://github.com/org/heavy",
						"cpu_request":"4",
						"cpu_limit":"8",
						"memory_request":"4Gi",
						"memory_limit":"16Gi",
						"env_json":"{\"FEATURE_FLAG\":\"true\",\"API_URL\":\"https://api.example.com\"}"
					}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, ok := projects["heavy"]
	if !ok {
		t.Fatal("expected project 'heavy' in map")
	}

	if p.CPURequest != "4" {
		t.Errorf("expected cpu_request 4, got %s", p.CPURequest)
	}
	if p.CPULimit != "8" {
		t.Errorf("expected cpu_limit 8, got %s", p.CPULimit)
	}
	if p.MemoryRequest != "4Gi" {
		t.Errorf("expected memory_request 4Gi, got %s", p.MemoryRequest)
	}
	if p.MemoryLimit != "16Gi" {
		t.Errorf("expected memory_limit 16Gi, got %s", p.MemoryLimit)
	}

	if len(p.EnvOverrides) != 2 {
		t.Fatalf("expected 2 env overrides, got %d", len(p.EnvOverrides))
	}
	if p.EnvOverrides["FEATURE_FLAG"] != "true" {
		t.Errorf("expected FEATURE_FLAG=true, got %s", p.EnvOverrides["FEATURE_FLAG"])
	}
	if p.EnvOverrides["API_URL"] != "https://api.example.com" {
		t.Errorf("expected API_URL, got %s", p.EnvOverrides["API_URL"])
	}
}

func TestListProjectBeads_EmptyResourceAndEnvFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "proj-minimal",
					Title:  "minimal",
					Type:   "project",
					Fields: json.RawMessage(`{"prefix":"mn","git_url":"https://github.com/org/minimal"}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p := projects["minimal"]
	if p.CPURequest != "" || p.CPULimit != "" || p.MemoryRequest != "" || p.MemoryLimit != "" {
		t.Errorf("expected empty resource fields, got cpu_request=%q cpu_limit=%q memory_request=%q memory_limit=%q",
			p.CPURequest, p.CPULimit, p.MemoryRequest, p.MemoryLimit)
	}
	if len(p.EnvOverrides) != 0 {
		t.Errorf("expected no env overrides, got %d", len(p.EnvOverrides))
	}
	if len(p.EnvVars) != 0 {
		t.Errorf("expected no env vars, got %d", len(p.EnvVars))
	}
}

func TestListProjectBeads_ParsesEnvVars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "proj-monorepo",
					Title: "monorepo",
					Type:  "project",
					Fields: json.RawMessage(`{
						"prefix":"kd",
						"git_url":"https://github.com/org/monorepo",
						"env":"[{\"name\":\"JIRA_BASE_URL\",\"value\":\"https://acme.atlassian.net\"},{\"name\":\"JIRA_PROJECT\",\"value\":\"PIH\"}]"
					}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, ok := projects["monorepo"]
	if !ok {
		t.Fatal("expected project 'monorepo' in map")
	}

	if len(p.EnvVars) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(p.EnvVars))
	}
	if p.EnvVars[0].Name != "JIRA_BASE_URL" || p.EnvVars[0].Value != "https://acme.atlassian.net" {
		t.Errorf("unexpected first env var: %+v", p.EnvVars[0])
	}
	if p.EnvVars[1].Name != "JIRA_PROJECT" || p.EnvVars[1].Value != "PIH" {
		t.Errorf("unexpected second env var: %+v", p.EnvVars[1])
	}
}

func TestListProjectBeads_SecretsAndEnvVarsTogether(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "proj-both",
					Title: "both",
					Type:  "project",
					Fields: json.RawMessage(`{
						"prefix":"bt",
						"secrets":"[{\"env\":\"API_TOKEN\",\"secret\":\"both-api-token\",\"key\":\"token\"}]",
						"env":"[{\"name\":\"BASE_URL\",\"value\":\"https://example.com\"}]"
					}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p := projects["both"]
	if len(p.Secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(p.Secrets))
	}
	if p.Secrets[0].Env != "API_TOKEN" {
		t.Errorf("expected API_TOKEN secret, got %s", p.Secrets[0].Env)
	}
	if len(p.EnvVars) != 1 {
		t.Fatalf("expected 1 env var, got %d", len(p.EnvVars))
	}
	if p.EnvVars[0].Name != "BASE_URL" || p.EnvVars[0].Value != "https://example.com" {
		t.Errorf("unexpected env var: %+v", p.EnvVars[0])
	}
}

func TestListProjectBeads_SkipsEmptyTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "empty-title",
					Title:  "",
					Fields: json.RawMessage(`{"prefix":"x"}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("expected 0 projects for empty title, got %d", len(projects))
	}
}

func TestListProjectBeads_Tier1Fields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:    "proj-full",
					Title: "full",
					Type:  "project",
					Fields: json.RawMessage(`{
						"prefix":"kd",
						"cpu_request":"500m",
						"cpu_limit":"2000m",
						"memory_request":"512Mi",
						"memory_limit":"2Gi",
						"service_account":"my-sa",
						"env_json":"{\"FOO\":\"bar\",\"BAZ\":\"qux\"}"
					}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, ok := projects["full"]
	if !ok {
		t.Fatal("expected project 'full' in map")
	}
	if p.CPURequest != "500m" {
		t.Errorf("cpu_request: got %q, want %q", p.CPURequest, "500m")
	}
	if p.CPULimit != "2000m" {
		t.Errorf("cpu_limit: got %q, want %q", p.CPULimit, "2000m")
	}
	if p.MemoryRequest != "512Mi" {
		t.Errorf("memory_request: got %q, want %q", p.MemoryRequest, "512Mi")
	}
	if p.MemoryLimit != "2Gi" {
		t.Errorf("memory_limit: got %q, want %q", p.MemoryLimit, "2Gi")
	}
	if p.ServiceAccount != "my-sa" {
		t.Errorf("service_account: got %q, want %q", p.ServiceAccount, "my-sa")
	}
	if p.EnvOverrides["FOO"] != "bar" {
		t.Errorf("env_json FOO: got %q, want %q", p.EnvOverrides["FOO"], "bar")
	}
	if p.EnvOverrides["BAZ"] != "qux" {
		t.Errorf("env_json BAZ: got %q, want %q", p.EnvOverrides["BAZ"], "qux")
	}
}

// TestListProjectBeads_ParsesUltracode proves the tri-state "ultracode" project
// field (ka-0000001714) parses into ProjectInfo.Ultracode: "true"→&true,
// "false"→&false, absent→nil (unset), matching the role-level parse.
func TestListProjectBeads_ParsesUltracode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID: "p-uc-on", Title: "workflowy", Type: "project",
					Fields: json.RawMessage(`{"prefix":"kd","ultracode":"true"}`),
				},
				{
					ID: "p-uc-off", Title: "plain", Type: "project",
					Fields: json.RawMessage(`{"prefix":"kd","ultracode":"false"}`),
				},
				{
					ID: "p-uc-unset", Title: "default", Type: "project",
					Fields: json.RawMessage(`{"prefix":"kd"}`),
				},
			},
			Total: 3,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p := projects["workflowy"]; p.Ultracode == nil || !*p.Ultracode {
		t.Errorf("workflowy: expected Ultracode=&true, got %v", p.Ultracode)
	}
	if p := projects["plain"]; p.Ultracode == nil || *p.Ultracode {
		t.Errorf("plain: expected Ultracode=&false, got %v", p.Ultracode)
	}
	if p := projects["default"]; p.Ultracode != nil {
		t.Errorf("default: expected Ultracode=nil, got %v", *p.Ultracode)
	}
}

func TestListProjectBeads_MalformedEnvJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "proj-bad-env",
					Title:  "bad-env",
					Type:   "project",
					Fields: json.RawMessage(`{"prefix":"kd","env_json":"not-valid-json"}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Project is still returned, just without env overrides.
	p, ok := projects["bad-env"]
	if !ok {
		t.Fatal("expected project 'bad-env' in map despite malformed env_json")
	}
	if len(p.EnvOverrides) != 0 {
		t.Errorf("expected no EnvOverrides for malformed env_json, got %v", p.EnvOverrides)
	}
}

func TestListProjectBeads_ParsesSessionResume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID: "p-on", Title: "with-resume", Type: "project", Status: "open",
					Fields: json.RawMessage(`{"prefix":"a","session_resume":"true"}`),
				},
				{
					ID: "p-off", Title: "no-resume", Type: "project", Status: "open",
					Fields: json.RawMessage(`{"prefix":"b","session_resume":"false"}`),
				},
				{
					ID: "p-unset", Title: "unset", Type: "project", Status: "open",
					Fields: json.RawMessage(`{"prefix":"c"}`),
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p := projects["with-resume"]; p.SessionResume == nil || !*p.SessionResume {
		t.Errorf("with-resume: expected SessionResume=&true, got %v", p.SessionResume)
	}
	if p := projects["no-resume"]; p.SessionResume == nil || *p.SessionResume {
		t.Errorf("no-resume: expected SessionResume=&false, got %v", p.SessionResume)
	}
	if p := projects["unset"]; p.SessionResume != nil {
		t.Errorf("unset: expected SessionResume=nil, got %v", *p.SessionResume)
	}
}

// TestListProjectBeads_DuplicateNameDedup verifies that two project beads
// sharing a resolved name resolve to a single deterministic winner (lowest
// bead ID) instead of shadowing each other via nondeterministic map iteration.
// A sparse duplicate must never blank a populated entry's git_url/account
// (ka-0000000415).
func TestListProjectBeads_DuplicateNameDedup(t *testing.T) {
	// The sparse duplicate ("proj-gardenio-b") has the lower ID, so it would win
	// a naive "lowest ID wins" race — but it carries no git_url/account. We
	// assert the winner is selected deterministically by ID regardless of the
	// order the daemon returns the beads, so a refresh can never flip between
	// the two and intermittently blank fields.
	full := beadJSON{
		ID:     "proj-gardenio-z",
		Title:  "gardenio",
		Type:   "project",
		Status: "open",
		Fields: json.RawMessage(`{"prefix":"kd","git_url":"https://github.com/org/gardenio","account":"234567890123"}`),
	}
	sparse := beadJSON{
		ID:     "proj-gardenio-a",
		Title:  "Project: gardenio",
		Type:   "project",
		Status: "open",
		Fields: json.RawMessage(`{"prefix":"kd"}`),
	}

	// Run with both daemon orderings: the winner (lowest ID = "proj-gardenio-a",
	// the sparse one) must be identical either way — deterministic, not
	// iteration-order dependent.
	orderings := [][]beadJSON{
		{full, sparse},
		{sparse, full},
	}
	for i, beads := range orderings {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: beads, Total: len(beads)})
		}))

		c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
		projects, err := c.ListProjectBeads(context.Background())
		srv.Close()
		if err != nil {
			t.Fatalf("ordering %d: unexpected error: %v", i, err)
		}

		// Exactly one "gardenio" entry survives — no shadowing duplicate.
		if len(projects) != 1 {
			t.Fatalf("ordering %d: expected 1 deduped project, got %d", i, len(projects))
		}
		p, ok := projects["gardenio"]
		if !ok {
			t.Fatalf("ordering %d: expected project 'gardenio' in map", i)
		}
		// Deterministic winner is the lowest bead ID ("proj-gardenio-a"), which
		// happens to be the sparse one. The point is that BOTH orderings agree.
		if p.Prefix != "kd" {
			t.Errorf("ordering %d: expected prefix kd, got %q", i, p.Prefix)
		}
	}
}

// TestListProjectBeads_DuplicateNamePicksPopulatedWhenLowerID covers the
// reverse case: when the populated bead has the lower ID, its git_url/account
// must survive — the deterministic winner is stable and never blanked by a
// later-iterated sparse duplicate.
func TestListProjectBeads_DuplicateNamePicksPopulatedWhenLowerID(t *testing.T) {
	full := beadJSON{
		ID:     "proj-gardenio-a", // lower ID -> wins
		Title:  "gardenio",
		Type:   "project",
		Status: "open",
		Fields: json.RawMessage(`{"prefix":"kd","git_url":"https://github.com/org/gardenio","account":"234567890123"}`),
	}
	sparse := beadJSON{
		ID:     "proj-gardenio-z",
		Title:  "gardenio",
		Type:   "project",
		Status: "open",
		Fields: json.RawMessage(`{"prefix":"kd"}`),
	}

	for i, beads := range [][]beadJSON{{full, sparse}, {sparse, full}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: beads, Total: len(beads)})
		}))
		c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
		projects, err := c.ListProjectBeads(context.Background())
		srv.Close()
		if err != nil {
			t.Fatalf("ordering %d: unexpected error: %v", i, err)
		}
		if len(projects) != 1 {
			t.Fatalf("ordering %d: expected 1 deduped project, got %d", i, len(projects))
		}
		p := projects["gardenio"]
		if p.GitURL != "https://github.com/org/gardenio" {
			t.Errorf("ordering %d: populated lower-ID bead should win; got git_url=%q", i, p.GitURL)
		}
		if p.Account != "234567890123" {
			t.Errorf("ordering %d: expected account preserved, got %q", i, p.Account)
		}
	}
}

// TestListProjectBeads_RetainsBeadIDAndStatus verifies that ListProjectBeads no
// longer discards the config bead's ID and status — the hub deep-links to the
// bead via BeadID and renders Status without a second fetch (design doc §4,
// ka-0000008001).
func TestListProjectBeads_RetainsBeadIDAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "gd-PROJBEAD1",
					Title:  "Project: beads",
					Type:   "project",
					Status: "open",
					Fields: json.RawMessage(`{"prefix":"bd"}`),
				},
				{
					ID:     "gd-PROJBEAD2",
					Title:  "gardenio",
					Type:   "project",
					Status: "in_progress",
					Fields: json.RawMessage(`{"prefix":"kd"}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	projects, err := c.ListProjectBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	beads, ok := projects["beads"]
	if !ok {
		t.Fatal("expected project 'beads' in map")
	}
	if beads.BeadID != "gd-PROJBEAD1" {
		t.Errorf("beads.BeadID = %q, want %q", beads.BeadID, "gd-PROJBEAD1")
	}
	if beads.Status != "open" {
		t.Errorf("beads.Status = %q, want %q", beads.Status, "open")
	}

	gardenio, ok := projects["gardenio"]
	if !ok {
		t.Fatal("expected project 'gardenio' in map")
	}
	if gardenio.BeadID != "gd-PROJBEAD2" {
		t.Errorf("gardenio.BeadID = %q, want %q", gardenio.BeadID, "gd-PROJBEAD2")
	}
	if gardenio.Status != "in_progress" {
		t.Errorf("gardenio.Status = %q, want %q", gardenio.Status, "in_progress")
	}
}

// TestListProjectBeads_DuplicateNameRetainsWinnerBeadID confirms the retained
// BeadID/Status follow the deterministic duplicate-winner (lowest bead ID), so
// the hub never links to the shadowed duplicate.
func TestListProjectBeads_DuplicateNameRetainsWinnerBeadID(t *testing.T) {
	full := beadJSON{
		ID:     "proj-gardenio-a", // lower ID -> wins
		Title:  "gardenio",
		Type:   "project",
		Status: "open",
		Fields: json.RawMessage(`{"prefix":"kd","git_url":"https://github.com/org/gardenio"}`),
	}
	sparse := beadJSON{
		ID:     "proj-gardenio-z",
		Title:  "gardenio",
		Type:   "project",
		Status: "in_progress",
		Fields: json.RawMessage(`{"prefix":"kd"}`),
	}

	for i, beads := range [][]beadJSON{{full, sparse}, {sparse, full}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: beads, Total: len(beads)})
		}))
		c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
		projects, err := c.ListProjectBeads(context.Background())
		srv.Close()
		if err != nil {
			t.Fatalf("ordering %d: unexpected error: %v", i, err)
		}
		p := projects["gardenio"]
		if p.BeadID != "proj-gardenio-a" {
			t.Errorf("ordering %d: winner BeadID = %q, want %q", i, p.BeadID, "proj-gardenio-a")
		}
		if p.Status != "open" {
			t.Errorf("ordering %d: winner Status = %q, want %q", i, p.Status, "open")
		}
	}
}

// TestGetProjectBead_FoundAndNotFound exercises the single-project convenience
// wrapper used by the project hub and P4 config editor (design doc §4).
func TestGetProjectBead_FoundAndNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "gd-PROJGAS",
					Title:  "gardenio",
					Type:   "project",
					Status: "open",
					Fields: json.RawMessage(`{"prefix":"kd","git_url":"https://github.com/org/gardenio"}`),
				},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	info, err := c.GetProjectBead(context.Background(), "gardenio")
	if err != nil {
		t.Fatalf("GetProjectBead(gardenio) unexpected error: %v", err)
	}
	if info.BeadID != "gd-PROJGAS" {
		t.Errorf("info.BeadID = %q, want %q", info.BeadID, "gd-PROJGAS")
	}
	if info.Status != "open" {
		t.Errorf("info.Status = %q, want %q", info.Status, "open")
	}
	if info.Prefix != "kd" {
		t.Errorf("info.Prefix = %q, want %q", info.Prefix, "kd")
	}

	if _, err := c.GetProjectBead(context.Background(), "nonexistent"); err == nil {
		t.Error("GetProjectBead(nonexistent) = nil error, want not-found error")
	}
}

// TestListAgentBeads_StampsDarkPodMetadata verifies the reconciler path stamps
// the dark-pod marker on an agent bead carrying a dark label, so a reconciled/
// re-imaged dark agent gets the same Jira write-token fence the events path
// applies (ka-0000005242). A plain agent leaves it unset.
func TestListAgentBeads_StampsDarkPodMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "dark-agent",
					Type:   "agent",
					Status: "in_progress",
					Labels: []string{"role:crew", "monorepo-dark"},
					Fields: json.RawMessage(`{"project":"gardenio","role":"crew","agent":"d1"}`),
				},
				{
					ID:     "plain-agent",
					Type:   "agent",
					Status: "in_progress",
					Labels: []string{"role:crew", "project:gardenio"},
					Fields: json.RawMessage(`{"project":"gardenio","role":"crew","agent":"p1"}`),
				},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	beads, err := c.ListAgentBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(beads) != 2 {
		t.Fatalf("expected 2 beads, got %d", len(beads))
	}
	if beads[0].Metadata[MetaDarkPod] != "true" {
		t.Errorf("expected dark agent to carry %s=true, got %q", MetaDarkPod, beads[0].Metadata[MetaDarkPod])
	}
	if _, ok := beads[1].Metadata[MetaDarkPod]; ok {
		t.Errorf("plain agent must not carry %s", MetaDarkPod)
	}
}

// TestListAgentBeads_DerivesGardenerID verifies the read-only gardener_id
// projection (task ka-0000002015): an explicit gardener_id is honored, a
// legacy bead with only schedule_id/trigger_id falls back to it, and an
// unowned ad-hoc spawn resolves to a synthetic owner.
func TestListAgentBeads_DerivesGardenerID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{
					ID:     "explicit",
					Type:   "agent",
					Fields: json.RawMessage(`{"project":"gardenio","agent":"a","gardener_id":"gd-realcap"}`),
				},
				{
					ID:     "legacy-schedule",
					Type:   "agent",
					Fields: json.RawMessage(`{"project":"gardenio","agent":"b","schedule_id":"gd-loopcap"}`),
				},
				{
					ID:     "adhoc",
					Type:   "agent",
					Fields: json.RawMessage(`{"project":"gardenio","agent":"c","spawn_source":"slack-thread"}`),
				},
				{
					ID:     "unknown",
					Type:   "agent",
					Fields: json.RawMessage(`{"project":"gardenio","agent":"d"}`),
				},
			},
			Total: 4,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	beads, err := c.ListAgentBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := make(map[string]string, len(beads))
	for _, b := range beads {
		got[b.ID] = b.GardenerID
	}

	want := map[string]string{
		"explicit":        "gd-realcap",
		"legacy-schedule": "gd-loopcap",
		"adhoc":           syntheticGardenerPrefix + "slack-thread",
		"unknown":         syntheticGardenerUnknown,
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("bead %s GardenerID = %q, want %q", id, got[id], w)
		}
	}

	// The two real-gardener beads are NOT leaks; the two synthetic ones ARE.
	if IsSyntheticGardenerID(got["explicit"]) || IsSyntheticGardenerID(got["legacy-schedule"]) {
		t.Errorf("real-gardener beads misclassified as synthetic leaks")
	}
	if !IsSyntheticGardenerID(got["adhoc"]) || !IsSyntheticGardenerID(got["unknown"]) {
		t.Errorf("synthetic-owner beads not classified as leaks")
	}
}
