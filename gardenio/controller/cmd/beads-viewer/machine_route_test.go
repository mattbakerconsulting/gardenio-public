package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const machineFilePath = "/internal/commons/f/gardenio/ch-1/0123456789abcdef0123456789abcdef/a.png"

// machineChain is the viewer's chain from machine to the mux, over the full
// route table, with who standing in for the daemon's /v1/me.
func machineChain(t *testing.T, who machineWho) http.Handler {
	t.Helper()
	m := registeredViewerRoutes(t)
	patternOf := func(r *http.Request) string { _, p := m.mux.Handler(r); return p }
	return machineMiddleware(scopeMiddleware(m.mux, patternOf), patternOf, who, testLogger())
}

// fakeMe answers /v1/me from a bearer -> answer table, and counts the asks.
type fakeMe struct {
	answers map[string]*beadsapi.MeResponse
	err     error
	asked   int
}

func (f *fakeMe) who(_ context.Context, bearer string) (*beadsapi.MeResponse, error) {
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	if me, ok := f.answers[bearer]; ok {
		return me, nil
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusUnauthorized, Message: "invalid token"}
}

// bearerRequest is a direct in-cluster request: a bearer and no edge identity.
func bearerRequest(path, bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	return r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer))
}

func serveReq(h http.Handler, r *http.Request) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

// TestMachineRoute_OnlyAMachinePrincipal: the Commons machine file route is
// served to a machine principal and refused to everything else (gd-qnF6mJZtkQ).
func TestMachineRoute_OnlyAMachinePrincipal(t *testing.T) {
	me := &fakeMe{answers: map[string]*beadsapi.MeResponse{
		"bridge-sa":    {OperatorID: "machine:gardenio-slack-bridge", Source: "token"},
		"human-oidc":   {OperatorID: "matt@example.com", Source: "auth"},
		"tenant-human": {OperatorID: "cl@claimlayer.example", Source: "auth"},
		"land-token":   {OperatorID: "land:claimlayer", Source: "token", TokenProject: "claimlayer"},
		"fleet-token":  {OperatorID: "gardenio", Source: "token"},
		"agent-token":  {OperatorID: "machine:gardenio-crew", Source: "token", Agent: "crew-1", AgentBead: "gd-a1"},
		"agent-own":    {OperatorID: "crew-2", Source: "token", TokenProject: "gardenio", Agent: "crew-2", AgentBead: "gd-a2"},
	}}
	h := machineChain(t, me.who)

	if code := serveReq(h, bearerRequest(machineFilePath, "bridge-sa")); code != http.StatusOK {
		t.Errorf("machine principal = %d, want 200", code)
	}
	// An agent's own token, bound to its land, is admitted (gd-WXY7Jm5ugY);
	// the Commons handler then holds it to that land (commonsMachine).
	if code := serveReq(h, bearerRequest(machineFilePath, "agent-own")); code != http.StatusOK {
		t.Errorf("agent token bound to a land = %d, want 200", code)
	}
	for _, bearer := range []string{"human-oidc", "tenant-human", "land-token", "fleet-token", "agent-token", "forged"} {
		if code := serveReq(h, bearerRequest(machineFilePath, bearer)); code != http.StatusForbidden {
			t.Errorf("bearer %s = %d, want 403", bearer, code)
		}
	}
	if code := serveReq(h, httptest.NewRequest(http.MethodGet, machineFilePath, nil)); code != http.StatusForbidden {
		t.Errorf("no bearer = %d, want 403", code)
	}

	// A human the edge signed in, admin or scoped, is refused without asking
	// the daemon, even when the bearer they carry is a machine's.
	asked := me.asked
	for name, p := range map[string]principal{"admin": adminPrincipal(), "scoped": scopedPrincipal()} {
		r := requestAs(http.MethodGet, machineFilePath, p)
		r = r.WithContext(beadsapi.WithUserBearer(r.Context(), "bridge-sa"))
		if code := serveReq(h, r); code != http.StatusForbidden {
			t.Errorf("%s human = %d, want 403", name, code)
		}
	}
	if me.asked != asked {
		t.Errorf("the daemon was asked about a signed-in human %d times, want 0", me.asked-asked)
	}

	// Any other route never asks the daemon about the caller.
	asked = me.asked
	serveReq(h, bearerRequest("/active", "bridge-sa"))
	if me.asked != asked {
		t.Error("machineMiddleware asked the daemon on a route that is not routeMachine")
	}
}

// A daemon that cannot say who the caller is fails the request (502), and
// never admits it.
func TestMachineRoute_DaemonDownIs502(t *testing.T) {
	h := machineChain(t, (&fakeMe{err: errors.New("dial tcp: connection refused")}).who)
	if code := serveReq(h, bearerRequest(machineFilePath, "bridge-sa")); code != http.StatusBadGateway {
		t.Errorf("daemon down = %d, want 502", code)
	}
}

// Without machineMiddleware in the chain the route is refused to everyone.
func TestMachineRoute_FailsClosedWithoutTheMiddleware(t *testing.T) {
	h := scopeChain(registeredViewerRoutes(t))
	if code := serveReq(h, bearerRequest(machineFilePath, "bridge-sa")); code != http.StatusForbidden {
		t.Errorf("no machine middleware = %d, want 403", code)
	}
}

func TestMachineFromMe(t *testing.T) {
	for _, tc := range []struct {
		me   *beadsapi.MeResponse
		want bool
	}{
		{&beadsapi.MeResponse{OperatorID: "machine:gardenio-slack-bridge"}, true},
		{&beadsapi.MeResponse{OperatorID: "machine:"}, false},
		{&beadsapi.MeResponse{OperatorID: "oidc:machine:x@example.com"}, false},
		{&beadsapi.MeResponse{OperatorID: "machine:gardenio-crew", Agent: "crew-1"}, false},
		{&beadsapi.MeResponse{OperatorID: "crew-1", Agent: "crew-1", AgentBead: "gd-a1"}, false},
		{&beadsapi.MeResponse{OperatorID: "crew-1", AgentBead: "gd-a1", TokenProject: "gardenio"}, false},
		{&beadsapi.MeResponse{OperatorID: "crew-1", Agent: "crew-1", AgentBead: "gd-a1", TokenProject: "gardenio"}, true},
		{nil, false},
	} {
		if _, got := machineFromMe(tc.me); got != tc.want {
			t.Errorf("machineFromMe(%+v) = %v, want %v", tc.me, got, tc.want)
		}
	}
}

// An agent reads the files of its own land only; a service account reads any
// land's; a request machineMiddleware did not admit reads none (gd-WXY7Jm5ugY).
func TestCommonsMachine_AgentReadsOnlyItsLand(t *testing.T) {
	as := func(c *machineCaller) *http.Request {
		r := httptest.NewRequest(http.MethodGet, machineFilePath, nil)
		if c != nil {
			r = r.WithContext(withMachineCaller(r.Context(), *c))
		}
		return r
	}
	agent := &machineCaller{Subject: "crew-2", Agent: "crew-2", Land: "gardenio"}
	bridge := &machineCaller{Subject: "machine:gardenio-slack-bridge"}
	for _, tc := range []struct {
		name string
		c    *machineCaller
		land string
		want bool
	}{
		{"agent, own land", agent, "gardenio", true},
		{"agent, another land", agent, "claimlayer", false},
		{"service account", bridge, "claimlayer", true},
		{"not admitted", nil, "gardenio", false},
	} {
		if got := commonsMachine(as(tc.c), tc.land); got != tc.want {
			t.Errorf("%s: commonsMachine = %v, want %v", tc.name, got, tc.want)
		}
	}
}
