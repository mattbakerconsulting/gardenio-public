package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetOperatedByBeadMemo clears the per-process memo so tests exercising the
// step-4 agent-bead fallback are hermetic regardless of ordering.
func resetOperatedByBeadMemo() {
	operatedByBeadMu.Lock()
	defer operatedByBeadMu.Unlock()
	operatedByBeadCached = ""
}

// beadFallbackServer spins up a stub daemon that returns a single bead at
// GET /v1/beads/{id} whose fields.operated_by is opVal.
func beadFallbackServer(t *testing.T, beadID, opVal string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			http.NotFound(w, r)
			return
		}
		fields := map[string]string{}
		if opVal != "" {
			fields["operated_by"] = opVal
		}
		fb, _ := json.Marshal(fields)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     beadID,
			"title":  "agent bead",
			"kind":   "config",
			"type":   "agent",
			"fields": json.RawMessage(fb),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// clearTokenEnv neutralizes the bearer-token env vars so defaultToken() does
// not pick up an ambient token in CI and so the stub daemon (which ignores
// auth) is the only thing the lookup talks to.
func clearTokenEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GARDENIO_TOKEN", "KBEADS_TOKEN", "BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN"} {
		t.Setenv(k, "")
	}
}

// TestDefaultOperatedBy_BeadFallback verifies the 4th-precedence fallback: when
// BEADS_OPERATED_BY and the credstore are both empty, kd reads the agent bead's
// fields.operated_by and returns it ONLY when it is a well-formed human shape,
// rejecting agent-shaped names and system:/fleet: sentinels.
func TestDefaultOperatedBy_BeadFallback(t *testing.T) {
	cases := []struct {
		name    string
		beadVal string
		want    string
	}{
		{"oidc human accepted", "oidc:matthew.baker@acme.example", "oidc:matthew.baker@acme.example"},
		{"slack human accepted", "slack:U0OPERATOR1", "slack:U0OPERATOR1"},
		{"agent-shaped rejected", "gardenio-some-agent-abc", ""},
		{"system sentinel rejected", "system:legacy-pre-attribution", ""},
		{"fleet sentinel rejected", "fleet:gardenio", ""},
		{"oidc without domain dot rejected", "oidc:user@localhost", ""},
		{"oidc empty local rejected", "oidc:@acme.example", ""},
		{"slack lowercase rejected", "slack:u0operator1", ""},
		{"slack non-U rejected", "slack:T123", ""},
		{"empty field rejected", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", tmp) // no credstore present
			t.Setenv("BEADS_OPERATED_BY", "")
			clearTokenEnv(t)
			t.Setenv("KD_AGENT_ID", "gd-AGENT1")
			t.Setenv("BOAT_AGENT_BEAD_ID", "")

			srv := beadFallbackServer(t, "gd-AGENT1", tc.beadVal)
			t.Setenv("BEADS_HTTP_URL", srv.URL)

			if got := resolveOperatedByFromAgentBead(); got != tc.want {
				t.Errorf("resolveOperatedByFromAgentBead() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultOperatedBy_BeadFallback_NoAgentID verifies the fallback is a no-op
// when no agent bead id is present (e.g. a direct human CLI invocation).
func TestDefaultOperatedBy_BeadFallback_NoAgentID(t *testing.T) {
	t.Setenv("BEADS_OPERATED_BY", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	if got := resolveOperatedByFromAgentBead(); got != "" {
		t.Errorf("resolveOperatedByFromAgentBead() = %q, want empty", got)
	}
}

// TestDefaultOperatedBy_BeadFallback_BoatAgentID verifies BOAT_AGENT_BEAD_ID is
// honored when KD_AGENT_ID is unset.
func TestDefaultOperatedBy_BeadFallback_BoatAgentID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BEADS_OPERATED_BY", "")
	clearTokenEnv(t)
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-AGENT2")

	srv := beadFallbackServer(t, "gd-AGENT2", "oidc:human@acme.example")
	t.Setenv("BEADS_HTTP_URL", srv.URL)

	if got, want := resolveOperatedByFromAgentBead(), "oidc:human@acme.example"; got != want {
		t.Errorf("resolveOperatedByFromAgentBead() = %q, want %q", got, want)
	}
}

// TestOperatedByFromAgentBead_Memoized verifies the lookup is performed at most
// once per process.
func TestOperatedByFromAgentBead_Memoized(t *testing.T) {
	resetOperatedByBeadMemo()

	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BEADS_OPERATED_BY", "")
	clearTokenEnv(t)
	t.Setenv("KD_AGENT_ID", "gd-AGENT3")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	srv := beadFallbackServer(t, "gd-AGENT3", "oidc:first@acme.example")
	t.Setenv("BEADS_HTTP_URL", srv.URL)

	first := operatedByFromAgentBead()
	if first != "oidc:first@acme.example" {
		t.Fatalf("first lookup = %q, want oidc:first@acme.example", first)
	}

	srv2 := beadFallbackServer(t, "gd-AGENT3", "oidc:second@acme.example")
	t.Setenv("BEADS_HTTP_URL", srv2.URL)

	if got := operatedByFromAgentBead(); got != first {
		t.Errorf("memoized lookup = %q, want cached %q", got, first)
	}
}

// TestOperatedByFromAgentBead_EmptyNotCached is the resumed-pool-attribution
// regression (ka-0000002362). A warm pool member boots into standby with an
// EMPTY operated_by on its own agent bead; the human operator is stamped only
// AFTER the pool assigns it. So a resolution that runs BEFORE assignment sees
// "" — and must NOT cache it, or a process outliving that transition would 400
// every attributed write for the rest of its life. Once the bead carries the
// operator, the next call must re-resolve and succeed.
func TestOperatedByFromAgentBead_EmptyNotCached(t *testing.T) {
	resetOperatedByBeadMemo()

	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BEADS_OPERATED_BY", "")
	clearTokenEnv(t)
	t.Setenv("KD_AGENT_ID", "gd-WARM1")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	// Pre-assignment: the standby bead carries no operated_by yet.
	preSrv := beadFallbackServer(t, "gd-WARM1", "")
	t.Setenv("BEADS_HTTP_URL", preSrv.URL)

	if got := operatedByFromAgentBead(); got != "" {
		t.Fatalf("pre-assignment lookup = %q, want empty", got)
	}

	// Post-assignment: the pool stamps the human operator on the SAME bead.
	postSrv := beadFallbackServer(t, "gd-WARM1", "oidc:matthew.baker@acme.example")
	t.Setenv("BEADS_HTTP_URL", postSrv.URL)

	if got, want := operatedByFromAgentBead(), "oidc:matthew.baker@acme.example"; got != want {
		t.Errorf("post-assignment lookup = %q, want %q (empty result must not be cached)", got, want)
	}

	// And now that a human resolved, it IS memoized: a third call returns the
	// cached value even if the bead changes again.
	changedSrv := beadFallbackServer(t, "gd-WARM1", "slack:U0OPERATOR1")
	t.Setenv("BEADS_HTTP_URL", changedSrv.URL)
	if got, want := operatedByFromAgentBead(), "oidc:matthew.baker@acme.example"; got != want {
		t.Errorf("cached lookup = %q, want %q", got, want)
	}
}

// TestOperatedByFieldFromBead_MixedTypeSiblings is the regression test for the
// prewarmed-pod squawk failure (gd-HEO8TC9w5q): a real agent bead's fields blob
// carries non-string siblings (e.g. litellm_spend_baseline is a JSON number).
// A strict map[string]string unmarshal errors on the first non-string value and
// drops EVERY field, so operated_by came back "" and the fallback 400'd with
// "operator could not be resolved" despite a valid human operator on the bead.
func TestOperatedByFieldFromBead_MixedTypeSiblings(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "numeric sibling does not drop operated_by",
			// Mirrors a live prewarmed agent bead: litellm_spend_baseline is
			// a JSON number, the rest are strings.
			raw:  `{"operated_by":"oidc:matthew.baker@acme.example","litellm_spend_baseline":0,"agent":"prewarmed-1","mode":"crew"}`,
			want: "oidc:matthew.baker@acme.example",
		},
		{
			name: "object sibling does not drop operated_by",
			raw:  `{"operated_by":"slack:U0OPERATOR1","meta":{"k":"v"}}`,
			want: "slack:U0OPERATOR1",
		},
		{
			name: "all-string fields still work",
			raw:  `{"operated_by":"oidc:a@b.io","role":"thread"}`,
			want: "oidc:a@b.io",
		},
		{
			name: "operated_by absent",
			raw:  `{"litellm_spend_baseline":0,"agent":"x"}`,
			want: "",
		},
		{
			name: "operated_by non-string value rejected",
			raw:  `{"operated_by":42}`,
			want: "",
		},
		{
			name: "empty blob",
			raw:  ``,
			want: "",
		},
		{
			name: "malformed blob",
			raw:  `{not json`,
			want: "",
		},
		{
			name: "value is trimmed",
			raw:  `{"operated_by":"  oidc:a@b.io  ","n":1}`,
			want: "oidc:a@b.io",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := operatedByFieldFromBead(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("operatedByFieldFromBead(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestDefaultOperatedBy_BeadFallback_NumericSibling drives the full
// resolveOperatedByFromAgentBead path against a stub daemon whose bead fields
// include a numeric sibling, exercising the HTTP client + parse end to end.
// This is the coverage the original bead-fallback test lacked: it only ever
// emitted string-valued fields, so it never reproduced the live 400.
func TestDefaultOperatedBy_BeadFallback_NumericSibling(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BEADS_OPERATED_BY", "")
	clearTokenEnv(t)
	t.Setenv("KD_AGENT_ID", "gd-AGENTN")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			http.NotFound(w, r)
			return
		}
		// A realistic prewarmed agent bead: operated_by alongside a numeric
		// litellm_spend_baseline field.
		fb := json.RawMessage(`{"operated_by":"oidc:matthew.baker@acme.example","litellm_spend_baseline":0,"agent":"prewarmed-1","mode":"crew"}`)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-AGENTN",
			"title":  "agent bead",
			"kind":   "config",
			"type":   "agent",
			"fields": fb,
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BEADS_HTTP_URL", srv.URL)

	if got, want := resolveOperatedByFromAgentBead(), "oidc:matthew.baker@acme.example"; got != want {
		t.Errorf("resolveOperatedByFromAgentBead() = %q, want %q", got, want)
	}
}

func TestIsHumanOperatorID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"oidc:matthew.baker@acme.example", true},
		{"slack:U0OPERATOR1", true},
		{"", false},
		{"gardenio-some-agent", false},
		{"system:legacy", false},
		{"fleet:gardenio", false},
		{"oidc:user@localhost", false},
		{"oidc:@acme.example", false},
		{"oidc:user@", false},
		{"slack:u0operator1", false},
		{"slack:T123", false},
		{"slack:", false},
	}
	for _, tc := range cases {
		if got := isHumanOperatorID(tc.in); got != tc.want {
			t.Errorf("isHumanOperatorID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
