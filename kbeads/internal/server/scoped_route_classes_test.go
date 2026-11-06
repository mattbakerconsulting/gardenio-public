package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// routeRecorder is a routeRegistrar that records patterns instead of serving.
type routeRecorder struct{ patterns []string }

func (r *routeRecorder) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, pattern)
}

// registeredRoutePatterns returns every pattern registerRoutes registers — the
// same helper NewHTTPHandler calls, so the list cannot drift from the server.
func registeredRoutePatterns() []string {
	rec := &routeRecorder{}
	(&BeadsServer{}).registerRoutes(rec)
	return rec.patterns
}

// unclassifiedRoutes returns the patterns missing from table.
func unclassifiedRoutes(patterns []string, table map[string]routeScopeClass) []string {
	var missing []string
	for _, p := range patterns {
		if _, ok := table[p]; !ok {
			missing = append(missing, p)
		}
	}
	return missing
}

func TestEveryRouteHasScopedClass(t *testing.T) {
	patterns := registeredRoutePatterns()
	if len(patterns) < 100 {
		t.Fatalf("only %d routes recorded; the registration walk is broken", len(patterns))
	}

	if missing := unclassifiedRoutes(patterns, scopedRouteClasses); len(missing) > 0 {
		t.Errorf("routes with no entry in scopedRouteClasses (classify them in scoped_route_classes.go):\n  %s",
			strings.Join(missing, "\n  "))
	}

	// Positive control: dropping one entry from a copy of the table must be
	// reported by the same walk.
	victim := patterns[0]
	trimmed := map[string]routeScopeClass{}
	for p, c := range scopedRouteClasses {
		if p != victim {
			trimmed[p] = c
		}
	}
	if got := unclassifiedRoutes(patterns, trimmed); !slices.Equal(got, []string{victim}) {
		t.Errorf("positive control: want [%s] reported missing, got %v", victim, got)
	}

	// Stale entries and unknown class values.
	for p, c := range scopedRouteClasses {
		if !slices.Contains(patterns, p) {
			t.Errorf("scopedRouteClasses has %q but no such route is registered", p)
		}
		if c != classFiltered && c != classAdminOnly && c != classScopedSafe {
			t.Errorf("%q has invalid class %d", p, c)
		}
	}
	seen := map[string]bool{}
	for _, p := range patterns {
		if seen[p] {
			t.Errorf("pattern %q registered twice", p)
		}
		seen[p] = true
	}
}

// TestScopedRouteGate_UnclassifiedFailsClosed: a route that slips past the table
// is refused for a scoped principal and untouched for everyone else.
func TestScopedRouteGate_UnclassifiedFailsClosed(t *testing.T) {
	mux := http.NewServeMux()
	scopeGatedMux{mux: mux}.HandleFunc("GET /v1/not-in-the-table", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	// Without an authenticated principal the gate must be a pass-through.
	resp, err := http.Get(ts.URL + "/v1/not-in-the-table")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous caller: status %d, want 200", resp.StatusCode)
	}
}

// A scoped principal on a route missing from the table is refused by
// scopedRouteGate, while an admin reaches the handler.
func TestScopedRouteGate_UnclassifiedRefusesScopedCaller(t *testing.T) {
	handler := scopedRouteGate(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, tc := range []struct {
		name  string
		roles []string
		want  int
	}{
		{"scoped", []string{RoleViewer, RoleOperator, RoleProjectMember}, http.StatusForbidden},
		{"admin", []string{RoleViewer, RoleOperator, RoleAdmin}, http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/not-in-the-table", nil)
		req.Pattern = "GET /v1/not-in-the-table"
		ctx := WithRoles(withAuthMethod(req.Context(), authMethodOIDC), tc.roles)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req.WithContext(ctx))
		if rec.Code != tc.want {
			t.Errorf("%s caller: status %d, want %d", tc.name, rec.Code, tc.want)
		}
		if tc.want == http.StatusForbidden && !strings.Contains(rec.Body.String(), gateRefusal) {
			t.Errorf("%s caller: body %q lacks %q", tc.name, rec.Body.String(), gateRefusal)
		}
	}
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// routeRequest turns a mux pattern into a method and a concrete path.
func routeRequest(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	return method, pathParam.ReplaceAllString(path, "x")
}

const gateRefusal = "not available to project-scoped principals"

func doRoute(t *testing.T, f *scopedFixture, token, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s as %s: %v", method, path, token, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

// isStreamRoute: SSE routes never end, so the admin positive control skips them.
func isStreamRoute(pattern string) bool {
	return strings.HasSuffix(pattern, "/stream") || strings.HasSuffix(pattern, "/bus/events")
}

// newUnlimitedScopedFixture is newScopedFixture with the rate limiter off: the
// matrix fires one request per route per principal, which would trip it.
func newUnlimitedScopedFixture(t *testing.T) *scopedFixture {
	t.Helper()
	member := []string{RoleViewer, RoleProjectMember}
	keycloak := &oidcIssuerMock{tokens: map[string]tokenInfo{
		tokScopedClaimlayer: {email: "cl@example.com", roles: member, groups: []string{"/project-claimlayer"}},
		tokScopedNoGroup:    {email: "nogroup@example.com", roles: member},
		tokOwnerAdmin:       {email: "owner@example.com", roles: []string{RoleViewer, RoleOperator, RoleAdmin}},
	}}
	fc := NewFleetConfig("gardenio", nil)
	registerLandPrincipal(fc, tokLandClaimlayer, "claimlayer")
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetFleetAuth(fc)
	srv.SetRateLimit(RateLimitConfig{})
	ts := httptest.NewServer(srv.NewHTTPHandler(&multiIssuerMock{issuers: []*oidcIssuerMock{keycloak}}, withTestToken(fc, scopedAdminToken)))
	t.Cleanup(ts.Close)

	for id, project := range map[string]string{beadGardenio: "gardenio", beadClaimlayer: "claimlayer", beadNoLabel: "", beadOther: "other"} {
		// The title is the leak marker: a batch read echoes requested ids, a title
		// only appears when the bead itself was served.
		title := "visible-" + id
		if id == beadGardenio || id == beadNoLabel {
			title = "leak-" + id
		}
		ms.beads[id] = &model.Bead{ID: id, Title: title, Type: "task", Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
		if project != "" {
			ms.labels[id] = []string{"project:" + project}
		}
	}
	return &scopedFixture{ts: ts, ms: ms}
}

func TestScopedRoutes_Matrix(t *testing.T) {
	f := newUnlimitedScopedFixture(t)
	// The same matrix for a scoped human and for a land principal
	// (land_principal.go), each bound to claimlayer: a land-A principal gets
	// nothing of land B through any route class.
	for name, scoped := range map[string]string{"human": tokScopedClaimlayer, "land principal": tokLandClaimlayer} {
		t.Run(name, func(t *testing.T) {

			t.Run("admin-only routes refuse the scoped token", func(t *testing.T) {
				n := 0
				for _, pattern := range registeredRoutePatterns() {
					if scopedRouteClasses[pattern] != classAdminOnly {
						continue
					}
					n++
					method, path := routeRequest(pattern)
					code, body := doRoute(t, f, scoped, method, path)
					if code != http.StatusForbidden || !strings.Contains(body, gateRefusal) {
						t.Errorf("%s as scoped: got %d %q, want the scoped-route 403", pattern, code, body)
					}
					// A scoped token with no groups is just as refused.
					if code, _ := doRoute(t, f, tokScopedNoGroup, method, path); code != http.StatusForbidden {
						t.Errorf("%s as no-group scoped: got %d, want 403", pattern, code)
					}
					if isStreamRoute(pattern) {
						continue
					}
					// Positive control: the gate does not fire for an admin. The mock
					// store may still answer 4xx/5xx for a bodyless request, so only the
					// gate's own refusal is excluded here.
					code, body = doRoute(t, f, tokOwnerAdmin, method, path)
					if strings.Contains(body, gateRefusal) {
						t.Errorf("%s as admin: refused by the scoped-route gate (%d)", pattern, code)
					}
				}
				if n < 50 {
					t.Fatalf("only %d admin-only routes exercised", n)
				}
			})

			t.Run("admin-only reads return 2xx for the admin token", func(t *testing.T) {
				for _, path := range []string{"/v1/stats", "/v1/configs?namespace=type", "/v1/operators", "/v1/taints"} {
					if code, body := doRoute(t, f, tokOwnerAdmin, http.MethodGet, path); code/100 != 2 {
						t.Errorf("GET %s as admin: %d %q, want 2xx", path, code, body)
					}
				}
			})

			t.Run("scoped-safe routes pass for the scoped token", func(t *testing.T) {
				for _, path := range []string{"/v1/whoami", "/v1/health", "/v1/health/live"} {
					if code, body := doRoute(t, f, scoped, http.MethodGet, path); code/100 != 2 {
						t.Errorf("GET %s as scoped: %d %q, want 2xx", path, code, body)
					}
				}
			})

			// Filtered list routes: the scoped token must see none of the out-of-scope
			// content (the leak- beads), the admin must see it (so the check is not vacuous).
			seed := func(id string, typ model.BeadType, project string) {
				title := "visible-" + id
				if project != "claimlayer" {
					title = "leak-" + id
				}
				f.ms.beads[id] = &model.Bead{ID: id, Title: title, Type: typ, Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
				f.ms.labels[id] = []string{"project:" + project}
			}
			seed("gd-1-decision", model.TypeDecision, "gardenio")
			seed("gd-1-escalation", model.BeadType("escalation"), "gardenio")
			seed("cl-1-decision", model.TypeDecision, "claimlayer")
			seed("cl-1-escalation", model.BeadType("escalation"), "claimlayer")
			seed("gd-1-release", model.TypeRelease, "gardenio")
			seed("cl-1-release", model.TypeRelease, "claimlayer")

			lists := []string{
				"/v1/beads?limit=100",
				"/v1/beads?limit=100&search=gd-1",
				"/v1/ready",
				"/v1/decisions",
				"/v1/escalations",
				"/v1/releases",
				"/v1/agents/roster",
				"/v1/events?limit=100",
				"/v1/graph?limit=100",
				"/v1/batch/beads?ids=gd-1,nl-1,gd-1-decision",
			}
			for _, path := range lists {
				t.Run("filtered "+path, func(t *testing.T) {
					code, body := doRoute(t, f, scoped, http.MethodGet, path)
					if code/100 != 2 {
						t.Fatalf("as scoped: %d %q, want 2xx", code, body)
					}
					if strings.Contains(body, "leak-") {
						t.Errorf("scoped response leaks out-of-scope content: %s", body)
					}
					if strings.Contains(path, "/v1/graph") && strings.Contains(body, `"total_open"`) {
						t.Errorf("scoped graph response carries store-wide stats: %s", body)
					}
					if code, body := doRoute(t, f, tokScopedNoGroup, http.MethodGet, path); code/100 != 2 || strings.Contains(body, "leak-") || strings.Contains(body, "visible-") {
						t.Errorf("no-group scoped: %d, want an empty 2xx, got %s", code, body)
					}
				})
			}

			// An out-of-scope bead answers as a missing one (gd-7APTAIhdC5).
			t.Run("per-bead reads refuse an out-of-scope bead", func(t *testing.T) {
				n := 0
				for _, pattern := range registeredRoutePatterns() {
					if scopedRouteClasses[pattern] != classFiltered || !strings.HasPrefix(pattern, "GET /v1/beads/{id}") {
						continue
					}
					n++
					method, path := routeRequest(strings.Replace(pattern, "{id}", beadGardenio, 1))
					for _, tok := range []string{scoped, tokScopedNoGroup} {
						if code, body := doRoute(t, f, tok, method, path); code != http.StatusNotFound || strings.Contains(body, "leak-") {
							t.Errorf("%s as %s on %s: got %d %q, want 404", pattern, tok, beadGardenio, code, body)
						}
					}
					// The unlabelled bead belongs to no project: also refused.
					_, path = routeRequest(strings.Replace(pattern, "{id}", beadNoLabel, 1))
					if code, _ := doRoute(t, f, scoped, method, path); code != http.StatusNotFound {
						t.Errorf("%s as scoped on unlabelled bead: got %d, want 404", pattern, code)
					}
					// Positive control: the in-scope bead is served.
					_, path = routeRequest(strings.Replace(pattern, "{id}", beadClaimlayer, 1))
					if code, body := doRoute(t, f, scoped, method, path); code == http.StatusForbidden || code == http.StatusNotFound {
						t.Errorf("%s as scoped on in-scope bead: got %d %q", pattern, code, body)
					}
				}
				if n < 10 {
					t.Fatalf("only %d per-bead read routes exercised", n)
				}
			})

			t.Run("admin sees the content the scoped token is denied", func(t *testing.T) {
				for _, path := range []string{"/v1/beads?limit=100", "/v1/decisions", "/v1/escalations", "/v1/graph?limit=100", "/v1/batch/beads?ids=gd-1,nl-1,gd-1-decision"} {
					code, body := doRoute(t, f, tokOwnerAdmin, http.MethodGet, path)
					if code/100 != 2 || !strings.Contains(body, "leak-") {
						t.Errorf("GET %s as admin: %d, want 2xx containing leak-; got %s", path, code, body)
					}
				}
			})
		})
	}
}
