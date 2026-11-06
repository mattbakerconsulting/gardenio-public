package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/lands/purse"
)

// glassPoolFake is coopmux's credential pool (http_cred.rs credentials_pool) behind the Glass.
type glassPoolFake struct {
	pool  *coopmuxapi.CredentialPool
	err   error
	asked int
}

func (f *glassPoolFake) CredentialPool(context.Context) (*coopmuxapi.CredentialPool, error) {
	f.asked++
	return f.pool, f.err
}

func fixturePool() *coopmuxapi.CredentialPool {
	next := uint64(1)
	return &coopmuxapi.CredentialPool{
		Accounts: []coopmuxapi.PoolAccount{
			{Name: "max-a", Provider: "claude", Status: "healthy", SessionCount: 3, CumulativeAssignments: 41, HasAPIKey: true,
				ThrottleStatus: "allowed", Utilization5h: "0.4200", Utilization7d: "0.1000", RepresentativeClaim: "five_hour"},
			{Name: "max-b", Provider: "claude", Status: "Throttled", SessionCount: 1, CumulativeAssignments: 7, HasAPIKey: true,
				Utilization5h: "1.0000", Utilization7d: "not-a-number", ThrottleResetAt: "2026-10-26T05:00:00Z", ExpiresAt: 1792980000},
			{Name: "spare", Provider: "openai", Status: "missing"},
		},
		TotalSessions: 4, HealthyAccounts: 1, TotalAccounts: 3, Strategy: "round_robin", RotatorNextIndex: &next,
	}
}

func getPool(t *testing.T, h *Handler, path, roles string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Test-Email", "op@example.com")
	req.Header.Set("X-Test-Roles", roles)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	if !current(rec.Header()) {
		t.Errorf("%s: Cache-Control %q, want no-store or revalidated", path, rec.Header().Get("Cache-Control"))
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rec.Code, doc
}

// GET /garden/glass/credentials (GL-15) is coopmux's pool for an admin, read only: each account's health in lower case,
// its sessions now and in all, its probe's 5 h and 7 d windows as fractions (none for one not probed or not a number),
// and the broker's strategy and next account. An operator gets 403 before coopmux is asked; no pool is a 503, coopmux
// not deployed a 503, coopmux down a 502.
func TestGlassPoolForAnAdmin(t *testing.T) {
	pool := &glassPoolFake{pool: fixturePool()}
	h := &Handler{BasePath: "/v", Glass: &Glass{Who: glassWho, Pool: pool}}

	if code, _ := getPool(t, h, "/garden/glass/credentials", "gardenio-operator"); code != http.StatusForbidden || pool.asked != 0 {
		t.Fatalf("operator: %d after %d asks, want 403 before coopmux is asked", code, pool.asked)
	}
	code, doc := getPool(t, h, "/garden/glass/credentials", "gardenio-admin")
	if code != http.StatusOK {
		t.Fatalf("admin: %d", code)
	}
	if doc["sessions"] != 4.0 || doc["healthy"] != 1.0 || doc["total"] != 3.0 || doc["strategy"] != "round_robin" || doc["next"] != 1.0 || doc["uncounted"] != nil {
		t.Errorf("totals = %v", doc)
	}
	if v, _ := doc["read_at"].(float64); v <= 0 {
		t.Errorf("read_at = %v", doc["read_at"])
	}
	accts, _ := doc["accounts"].([]any)
	if len(accts) != 3 {
		t.Fatalf("accounts = %v", doc["accounts"])
	}
	a, b, c := accts[0].(map[string]any), accts[1].(map[string]any), accts[2].(map[string]any)
	if a["name"] != "max-a" || a["status"] != "healthy" || a["sessions"] != 3.0 || a["assigned"] != 41.0 || a["has_key"] != true || a["util_5h"] != 0.42 || a["util_7d"] != 0.1 || a["claim"] != "five_hour" {
		t.Errorf("max-a = %v", a)
	}
	if b["status"] != "throttled" || b["util_5h"] != 1.0 || b["util_7d"] != nil || b["reset_at"] != "2026-10-26T05:00:00Z" || b["expires_at"] != 1792980000.0 {
		t.Errorf("max-b = %v", b)
	}
	if c["status"] != "missing" || c["has_key"] != false || c["util_5h"] != nil || c["sessions"] != 0.0 {
		t.Errorf("spare = %v", c)
	}

	// auto-assign off: the broker counts nothing, so no count is sent as if it were one
	off := fixturePool()
	off.AutoAssignDisabled = true
	h.Glass.Pool = &glassPoolFake{pool: off}
	_, doc = getPool(t, h, "/garden/glass/credentials", "gardenio-admin")
	if doc["uncounted"] != true || doc["sessions"] != 0.0 || doc["accounts"].([]any)[0].(map[string]any)["sessions"] != 0.0 {
		t.Errorf("uncounted = %v", doc)
	}

	for _, c := range []struct {
		name string
		pool GlassPool
		want int
	}{
		{"no pool", nil, http.StatusServiceUnavailable},
		{"coopmux not deployed", &glassPoolFake{err: coopmuxapi.ErrDisabled}, http.StatusServiceUnavailable},
		{"coopmux down", &glassPoolFake{err: errors.New("dial tcp: refused")}, http.StatusBadGateway},
	} {
		h.Glass.Pool = c.pool
		if code, _ := getPool(t, h, "/garden/glass/credentials", "gardenio-admin"); code != c.want {
			t.Errorf("%s: %d, want %d", c.name, code, c.want)
		}
	}
	if code, _ := getPool(t, &Handler{}, "/garden/glass/credentials", "gardenio-admin"); code != http.StatusServiceUnavailable {
		t.Errorf("no glass: %d, want 503", code)
	}
}

// The project panel's header offers the pool (credential_pool) on an operator land to a viewer who may read it, and
// never on a tenant's land (its purse says it is one), to an operator, or where the pool is not wired.
func TestProjectHeaderOffersThePoolOnAnOperatorLand(t *testing.T) {
	f := &fakeProjects{docs: map[string]*ProjectDoc{
		"gardenio": {Name: "gardenio"},
		"acme":     {Name: "acme", Purse: &purse.Purse{}},
	}}
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Projects: f, Glass: &Glass{Who: glassWho, Pool: &glassPoolFake{pool: fixturePool()}}}
	for _, c := range []struct {
		project, roles string
		want           bool
	}{
		{"gardenio", "gardenio-admin", true},
		{"gardenio", "gardenio-operator", false},
		{"acme", "gardenio-admin", false},
	} {
		code, doc := getPool(t, h, "/garden/project/"+c.project, c.roles)
		if code != http.StatusOK {
			t.Fatalf("%s as %s: %d", c.project, c.roles, code)
		}
		if got := doc["credential_pool"] == true; got != c.want {
			t.Errorf("%s as %s: credential_pool = %v, want %v", c.project, c.roles, got, c.want)
		}
	}
	h.Glass.Pool = nil
	if _, doc := getPool(t, h, "/garden/project/gardenio", "gardenio-admin"); doc["credential_pool"] != nil {
		t.Errorf("no pool wired: credential_pool = %v", doc["credential_pool"])
	}
}
