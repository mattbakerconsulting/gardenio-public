package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// roleSetWireBead is the minimal bead shape the /v1/beads fake needs to serve
// role/trigger/project beads for the liveRoleSet tests.
type roleSetWireBead struct {
	ID     string            `json:"id"`
	Title  string            `json:"title"`
	Type   string            `json:"type"`
	Status string            `json:"status"`
	Fields map[string]string `json:"fields,omitempty"`
}

// roleSetFake describes the bead catalog the /v1/beads fake serves, per type,
// plus which of those pages should report a Total larger than the rows it
// returns (limit-truncated) to exercise a fail-open guard. Named fields rather
// than positional args because liveRoleSet now fans out over three bead types.
type roleSetFake struct {
	roles, trigs, projects                      []roleSetWireBead
	truncateRoles, truncateTrigs, truncateProjs bool
}

// setupLiveRoleSetDaemon points the global daemon at a fake /v1/beads server
// serving f's catalog per ?type. It returns the query captured on the LAST
// role-typed fetch so a caller can assert the fetch shape (status/limit);
// callers otherwise read liveRoleSet's own output.
func setupLiveRoleSetDaemon(t *testing.T, f roleSetFake) *url.Values {
	t.Helper()

	roleQuery := &url.Values{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		typ := r.URL.Query().Get("type")
		var (
			beads    []roleSetWireBead
			truncate bool
		)
		switch typ {
		case "role":
			*roleQuery = r.URL.Query()
			beads, truncate = f.roles, f.truncateRoles
		case "trigger":
			beads, truncate = f.trigs, f.truncateTrigs
		case "project":
			beads, truncate = f.projects, f.truncateProjs
		default:
			beads = nil
		}

		// A truncated response reports a Total larger than the rows it returns,
		// exactly what a >page-cap catalog produces on the daemon. Otherwise
		// Total == len(beads) and the guard passes.
		total := len(beads)
		if truncate {
			total = len(beads) + 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": total})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
	return roleQuery
}

// TestLiveRoleSet_UnionOfBuiltinsRolesAndTriggers checks the happy path: the
// returned set is the union of builtin roles, type:role bead titles, the role
// field of open triggers, and every project's channel_roles wiring.
func TestLiveRoleSet_UnionOfBuiltinsRolesAndTriggers(t *testing.T) {
	roleQuery := setupLiveRoleSetDaemon(t, roleSetFake{
		roles: []roleSetWireBead{
			{ID: "gd-role1", Title: "hema-analyst", Type: "role", Status: "open"},
			{ID: "gd-role2", Title: "dataplane-fixer", Type: "role", Status: "open"},
			{ID: "gd-role3", Title: "", Type: "role", Status: "open"}, // empty title skipped
		},
		trigs: []roleSetWireBead{
			{ID: "gd-trig1", Title: "gardener trigger", Type: "trigger", Status: "open", Fields: map[string]string{"role": "plan-gardener-live"}},
			{ID: "gd-trig2", Title: "no-role trigger", Type: "trigger", Status: "open"}, // no role field
		},
		projects: []roleSetWireBead{
			{ID: "gd-proj1", Title: "monorepo", Type: "project", Status: "open", Fields: map[string]string{
				"channel_roles": `{"C1":"mbaker-assistant","C2":"crew-lite,pt-assistant","C3":""}`,
			}},
		},
	})

	live := liveRoleSet(context.Background())
	if live == nil {
		t.Fatal("liveRoleSet returned nil on a clean fetch, want a populated set")
	}
	for _, want := range []string{"crew", "gardener", "job", "hema-analyst", "dataplane-fixer", "plan-gardener-live"} {
		if _, ok := live[want]; !ok {
			t.Errorf("liveRoleSet missing expected role %q (set=%v)", want, live)
		}
	}
	if _, ok := live[""]; ok {
		t.Error("liveRoleSet included an empty-title role")
	}
	// channel_roles is the fourth live spawn path: a mention in a wired channel
	// spawns that role with no type:role bead required. Comma-separated values
	// contribute every role in the list.
	for _, want := range []string{"mbaker-assistant", "crew-lite", "pt-assistant"} {
		if _, ok := live[want]; !ok {
			t.Errorf("liveRoleSet missing channel_roles-wired role %q (set=%v)", want, live)
		}
	}

	// The role fetch must carry the active-status set (mirroring ListRoleBeads'
	// activeStatuses) and a limit — otherwise it would count closed role beads
	// or silently truncate below the guard. Lock the query shape so a future
	// refactor can't drift it back to a status-less or unbounded fetch.
	if got := roleQuery.Get("status"); got != "open,in_progress,blocked,deferred" {
		t.Errorf("role fetch status = %q, want the active-status set", got)
	}
	if got := roleQuery.Get("limit"); got != "1000" {
		t.Errorf("role fetch limit = %q, want 1000 (daemon default page cap, so Total>len can detect truncation)", got)
	}
}

// TestLiveRoleSet_FailOpenOnRoleFetchError covers the fail-open-to-nil path when
// the role-bead fetch itself errors (network/5xx) — the branch this refactor
// introduced alongside the truncation guard. A non-2xx role fetch must yield a
// nil (unknown-registry) set, never a builtins-only partial that would flag
// every non-builtin role: advice as dead.
func TestLiveRoleSet_FailOpenOnRoleFetchError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	if live := liveRoleSet(context.Background()); live != nil {
		t.Errorf("liveRoleSet returned a non-nil set on a role-fetch error, want nil (fail-open); set=%v", live)
	}
}

// TestLiveRoleSet_FailOpenOnTruncatedRolePage is the core regression guard for
// gd-LhS8M8Dlno fix #2: a role-bead page whose Total exceeds the returned rows
// (a >cap catalog) must fail open to nil, NOT return a partial set that would
// manufacture spurious dead-role warnings for beyond-cap roles.
func TestLiveRoleSet_FailOpenOnTruncatedRolePage(t *testing.T) {
	setupLiveRoleSetDaemon(t, roleSetFake{
		roles:         []roleSetWireBead{{ID: "gd-role1", Title: "hema-analyst", Type: "role", Status: "open"}},
		truncateRoles: true, // Total > len(beads); triggers fine and must never be reached
	})

	if live := liveRoleSet(context.Background()); live != nil {
		t.Errorf("liveRoleSet returned a non-nil set on a truncated role page, want nil (fail-open); set=%v", live)
	}
}

// TestLiveRoleSet_FailOpenOnTruncatedTriggerPage keeps the pre-existing trigger
// guard covered alongside the new role guard.
func TestLiveRoleSet_FailOpenOnTruncatedTriggerPage(t *testing.T) {
	setupLiveRoleSetDaemon(t, roleSetFake{
		roles:         []roleSetWireBead{{ID: "gd-role1", Title: "hema-analyst", Type: "role", Status: "open"}},
		trigs:         []roleSetWireBead{{ID: "gd-trig1", Title: "gardener trigger", Type: "trigger", Status: "open", Fields: map[string]string{"role": "plan-gardener-live"}}},
		truncateTrigs: true,
	})

	if live := liveRoleSet(context.Background()); live != nil {
		t.Errorf("liveRoleSet returned a non-nil set on a truncated trigger page, want nil (fail-open); set=%v", live)
	}
}

// TestLiveRoleSet_FailOpenOnTruncatedProjectPage extends the same guard to the
// project fetch: a partial project page would drop channel_roles wiring and
// manufacture exactly the dead-role warning this path exists to prevent.
func TestLiveRoleSet_FailOpenOnTruncatedProjectPage(t *testing.T) {
	setupLiveRoleSetDaemon(t, roleSetFake{
		roles:         []roleSetWireBead{{ID: "gd-role1", Title: "hema-analyst", Type: "role", Status: "open"}},
		projects:      []roleSetWireBead{{ID: "gd-proj1", Title: "monorepo", Type: "project", Status: "open", Fields: map[string]string{"channel_roles": `{"C1":"mbaker-assistant"}`}}},
		truncateProjs: true,
	})

	if live := liveRoleSet(context.Background()); live != nil {
		t.Errorf("liveRoleSet returned a non-nil set on a truncated project page, want nil (fail-open); set=%v", live)
	}
}

// TestLiveRoleSet_ChannelRolesWithoutRoleBead is the regression guard for
// ka-0000002346. A role wired ONLY into channel_roles — no type:role bead, no
// trigger, not a builtin — still spawns an agent on every Slack mention in that
// channel, so it MUST count as live. Before this fix all 11 personal assistants
// looked retired to lint, and the hygiene sweep closed their personality advice
// while the channels kept working: a silent 3-week loss of cross-session memory.
func TestLiveRoleSet_ChannelRolesWithoutRoleBead(t *testing.T) {
	setupLiveRoleSetDaemon(t, roleSetFake{
		roles: []roleSetWireBead{{ID: "gd-role1", Title: "hema-analyst", Type: "role", Status: "open"}},
		projects: []roleSetWireBead{
			{ID: "gd-proj1", Title: "monorepo", Type: "project", Status: "open", Fields: map[string]string{
				"channel_roles": `{"C0BE33KJKUY":"mbaker-assistant","C0BHLE0QL1F":"pt-assistant"}`,
			}},
			// A type-flipped / malformed blob on ONE project must not fail the
			// whole check open — the other project's wiring still counts
			// (ka-0000000372 flips this field from object to string).
			{ID: "gd-proj2", Title: "gardenio", Type: "project", Status: "open", Fields: map[string]string{
				"channel_roles": `not-json`,
			}},
		},
	})

	live := liveRoleSet(context.Background())
	if live == nil {
		t.Fatal("liveRoleSet returned nil, want a populated set")
	}
	for _, want := range []string{"mbaker-assistant", "pt-assistant"} {
		if _, ok := live[want]; !ok {
			t.Errorf("role %q is wired in channel_roles and spawns on mention, but liveRoleSet omitted it — lint would flag its advice dead-role-target (ka-0000002346); set=%v", want, live)
		}
	}
	if _, ok := live["hema-analyst"]; !ok {
		t.Error("liveRoleSet dropped a type:role bead role while adding channel_roles")
	}
}

// configWireBead is the minimal bead shape the /v1/beads fake needs to serve
// config beads (prerun payload in Description) for the prerunReplacedAdvice test.
type configWireBead struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
}

// setupConfigDaemon points the global daemon at a fake /v1/beads server that
// returns the given config beads for ?type=config (and nothing otherwise).
func setupConfigDaemon(t *testing.T, cfgBeads []configWireBead) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var beads []configWireBead
		if r.URL.Query().Get("type") == "config" {
			beads = cfgBeads
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

// TestPrerunReplacedAdvice_CollectsReplacesAdvice verifies the exemption set is
// the union of every prerun config bead's replaces_advice — this is what keeps
// the undeliverable-capability check from flagging a bead the capability-probe
// prerun already delivers live (ka-0000006777). Non-prerun config beads and
// entries without replaces_advice contribute nothing.
func TestPrerunReplacedAdvice_CollectsReplacesAdvice(t *testing.T) {
	setupConfigDaemon(t, []configWireBead{
		{
			ID: "gd-global-prerun", Title: "prerun", Type: "config", Status: "open",
			Description: `{"prerun":[{"id":"capability-probe","cmd":"echo hi","replaces_advice":["ka-0000006777","ka-0000005080"]}]}`,
		},
		{
			// A prerun bead whose entry replaces nothing contributes nothing.
			ID: "gd-role-prerun", Title: "prerun", Type: "config", Status: "open",
			Description: `{"prerun":[{"id":"assigned-bead","cmd":"kd show x"}]}`,
		},
		{
			// A non-prerun config bead is ignored even if its JSON mentions ids.
			ID: "gd-other", Title: "wrapup", Type: "config", Status: "open",
			Description: `{"wrapup":{"replaces_advice":["gd-should-not-appear"]}}`,
		},
	})

	got := prerunReplacedAdvice(context.Background())
	want := map[string]struct{}{"ka-0000006777": {}, "ka-0000005080": {}}
	if len(got) != len(want) {
		t.Fatalf("prerunReplacedAdvice = %v, want %v", got, want)
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("prerunReplacedAdvice missing %q (got %v)", id, got)
		}
	}
	if _, ok := got["gd-should-not-appear"]; ok {
		t.Error("prerunReplacedAdvice pulled replaces_advice from a non-prerun config bead")
	}
}

// TestPrerunReplacedAdvice_FailOpenOnFetchError checks the fail-open contract:
// a daemon error yields nil (exempt nothing), so the undeliverable-capability
// check behaves exactly as it did before the exemption — a fetch failure can
// only re-add findings an operator can see, never silently drop one.
func TestPrerunReplacedAdvice_FailOpenOnFetchError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	if got := prerunReplacedAdvice(context.Background()); got != nil {
		t.Errorf("prerunReplacedAdvice returned %v on a fetch error, want nil (fail-open)", got)
	}
}
