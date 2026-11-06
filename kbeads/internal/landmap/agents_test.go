package landmap

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"gardenio/lands/contract/landsv1"
)

var acmeAgents = []AgentRecord{
	{ID: "ac-a2", Name: "studio-mm4k", StartedBy: "oidc:sam@example.com", StartedAt: t0},
	{ID: "ac-a1", Name: "laptop-x7rq", StartedBy: "oidc:robin@example.com", StartedAt: t0, Stopped: true},
}

func TestAgents_ListAndGet(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{agents: acmeAgents})
	page := decodeAs[landsv1.AgentPage](t, do(t, h, "GET", "/acme/agents?limit=1", ""), http.StatusOK)
	if len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("page = %+v", page)
	}
	if a := page.Items[0]; a.Id != "ac-a2" || a.Name != "studio-mm4k" || a.StartedBy != "sam" || a.Status != landsv1.AgentStatusActive || a.StartedAt.Nanosecond() != 0 {
		t.Errorf("first agent = %+v", a)
	}
	next := decodeAs[landsv1.AgentPage](t, do(t, h, "GET", "/acme/agents?cursor="+*page.NextCursor, ""), http.StatusOK)
	if len(next.Items) != 1 || next.Items[0].Status != landsv1.AgentStatusStopped || next.NextCursor != nil {
		t.Errorf("second page = %+v", next)
	}

	if a := decodeAs[landsv1.Agent](t, do(t, h, "GET", "/acme/agents/ac-a1", ""), http.StatusOK); a.Name != "laptop-x7rq" {
		t.Errorf("get = %+v", a)
	}
	problemOf(t, do(t, h, "GET", "/acme/agents/ac-zz", ""), landsv1.NotFound)
	// Another land reads as absent, before Lands is asked.
	problemOf(t, do(t, h, "GET", "/zeta/agents", ""), landsv1.NotFound)
}

func TestAgents_Create(t *testing.T) {
	var got AgentMintRequest
	_, h := newMapFixture(t, fakeLands{minted: &got})
	r := do(t, h, "POST", "/acme/agents", `{"name":"studio"}`, "Idempotency-Key", "k1")
	g := decodeAs[landsv1.AgentGrant](t, r, http.StatusCreated)
	if g.AgentId != fakeGrant.AgentID || g.Bearer != fakeGrant.Bearer || g.Refresh != fakeGrant.Refresh || !g.ExpiresAt.Equal(fakeGrant.ExpiresAt.Truncate(time.Second)) {
		t.Errorf("grant = %+v", g)
	}
	if g.DoorUrl != "https://example.com/land/acme" {
		t.Errorf("door_url = %q", g.DoorUrl)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("a credential must not be cached: %v", r.header)
	}
	if got != (AgentMintRequest{Name: "studio", IdempotencyKey: "k1"}) {
		t.Errorf("mint request = %+v", got)
	}

	decodeAs[landsv1.AgentGrant](t, do(t, h, "POST", "/acme/agents", `{}`), http.StatusCreated)
	if got != (AgentMintRequest{Name: agentNameDefault}) {
		t.Errorf("unnamed mint request = %+v", got)
	}

	for _, body := range []string{`{"name":"Studio"}`, `{"name":"9lives"}`, `{"name":""}`, `{"role":"steward"}`, `{"name":"a-very-long-name-that-goes-past-32"}`} {
		problemOf(t, do(t, h, "POST", "/acme/agents", body), landsv1.InvalidRequest)
	}
	problemOf(t, do(t, h, "POST", "/zeta/agents", `{}`), landsv1.NotFound)

	_, paused := newMapFixture(t, fakeLands{paused: true})
	problemOf(t, do(t, paused, "POST", "/acme/agents", `{}`), landsv1.LandPaused)
}

// What the server decides reaches the tenant as the contract's word for it,
// and nothing else of the error.
func TestAgents_Refusals(t *testing.T) {
	for err, code := range map[error]landsv1.ProblemCode{
		ErrAgentForbidden:                         landsv1.Forbidden,
		ErrAgentNotFound:                          landsv1.NotFound,
		ErrAgentConflict:                          landsv1.Conflict,
		errors.New("kbeads: store down"):          landsv1.UpstreamUnavailable,
		errors.Join(ErrAgentConflict, errTestCap): landsv1.Conflict,
	} {
		_, h := newMapFixture(t, fakeLands{agents: acmeAgents, agentErr: err})
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/acme/agents", ""},
			{"GET", "/acme/agents/ac-a1", ""},
			{"POST", "/acme/agents", `{}`},
			{"POST", "/acme/agents/ac-a1/refresh", `{"refresh":"x"}`},
			{"POST", "/acme/agents/ac-a1/revoke", ""},
		} {
			problemOf(t, do(t, h, c.method, c.path, c.body), code)
		}
	}
}

var errTestCap = errors.New("land acme runs 3 agents")

func TestAgents_RefreshAndRevoke(t *testing.T) {
	_, h := newMapFixture(t, fakeLands{agents: acmeAgents})
	g := decodeAs[landsv1.AgentGrant](t, do(t, h, "POST", "/acme/agents/ac-a1/refresh", `{"refresh":"`+fakeGrant.Refresh+`"}`), http.StatusOK)
	if g.Bearer != fakeGrant.Bearer || g.DoorUrl != "https://example.com/land/acme" {
		t.Errorf("refreshed grant = %+v", g)
	}
	problemOf(t, do(t, h, "POST", "/acme/agents/ac-a1/refresh", `{"refresh":"other"}`), landsv1.NotFound)
	problemOf(t, do(t, h, "POST", "/acme/agents/ac-a1/refresh", `{}`), landsv1.InvalidRequest)
	problemOf(t, do(t, h, "POST", "/acme/agents/ac-a1/refresh", `{"refresh":"x","agent":"ac-a2"}`), landsv1.InvalidRequest)

	if a := decodeAs[landsv1.Agent](t, do(t, h, "POST", "/acme/agents/ac-a2/revoke", ""), http.StatusOK); a.Status != landsv1.AgentStatusStopped {
		t.Errorf("revoked = %+v", a)
	}
	problemOf(t, do(t, h, "POST", "/acme/agents/ac-zz/revoke", ""), landsv1.NotFound)

	// A paused land still lets its members stop an agent, and refreshes none.
	_, paused := newMapFixture(t, fakeLands{paused: true, agents: acmeAgents})
	decodeAs[landsv1.Agent](t, do(t, paused, "POST", "/acme/agents/ac-a2/revoke", ""), http.StatusOK)
	problemOf(t, do(t, paused, "POST", "/acme/agents/ac-a1/refresh", `{"refresh":"`+fakeGrant.Refresh+`"}`), landsv1.LandPaused)
}
