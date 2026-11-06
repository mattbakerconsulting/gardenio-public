package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// seedOwnerAgentBead inserts an agent-type bead into the mock store with the
// supplied fields map. Returns the bead id.
func seedOwnerAgentBead(t *testing.T, ms *mockStore, id string, fields map[string]any) {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	ms.beads[id] = &model.Bead{
		ID:        id,
		Kind:      model.KindIssue,
		Type:      "agent",
		Title:     id,
		Status:    model.StatusOpen,
		Fields:    raw,
		UpdatedAt: time.Now(),
	}
}

// seedUserBead inserts a user-type bead into the mock store with the
// supplied fields map. Returns the bead id.
func seedUserBead(t *testing.T, ms *mockStore, id string, fields map[string]any) {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	ms.beads[id] = &model.Bead{
		ID:        id,
		Kind:      model.KindIssue,
		Type:      "user",
		Title:     id,
		Status:    model.StatusOpen,
		Fields:    raw,
		UpdatedAt: time.Now(),
	}
}

// staticAuthCtx returns a ctx whose AuthIdentity is a static-token caller
// authenticated as the named agent — mirrors what the static-token
// interceptor stamps in production.
func staticAuthCtx(agent string) context.Context {
	return withTestAgent(context.Background(), agent)
}

// TestApplyOwnerDefault_NonAgent_HumanHeader pins the ka-0000000269 happy
// path under the keep-header-but-tighten policy: a non-agent static-token
// caller (e.g. bridge or admin using the primary token, Subject == "static")
// with X-Operated-By = human-with-email gets Owner = header.
func TestApplyOwnerDefault_NonAgent_HumanHeader(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, _, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	got := s.applyOwnerDefault(ctx, "")
	if got != "slack:U0OPERATOR1" {
		t.Errorf("applyOwnerDefault = %q; want %q", got, "slack:U0OPERATOR1")
	}
}

// TestApplyOwnerDefault_NonAgent_OIDCHeader covers an oidc:<email> header
// for a non-agent caller — also human-with-email; also accepted.
func TestApplyOwnerDefault_NonAgent_OIDCHeader(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, _, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	ctx = WithOperatedByHeader(ctx, "oidc:matt@example.com")
	got := s.applyOwnerDefault(ctx, "")
	if got != "oidc:matt@example.com" {
		t.Errorf("applyOwnerDefault = %q; want %q", got, "oidc:matt@example.com")
	}
}

// TestApplyOwnerDefault_FleetSentinelRejected confirms a non-human shape
// (fleet:slack-bridge) leaves Owner empty even when the caller managed to
// stash it in ctx. The sanitizer at the interceptor already rejects this
// shape, but applyOwnerDefault re-checks for defence in depth.
func TestApplyOwnerDefault_FleetSentinelRejected(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, _, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	ctx = WithOperatedByHeader(ctx, "fleet:slack-bridge")
	got := s.applyOwnerDefault(ctx, "")
	if got != "" {
		t.Errorf("applyOwnerDefault = %q; want \"\" (fleet sentinel rejected)", got)
	}
}

// TestApplyOwnerDefault_SystemSentinelRejected mirrors the fleet case for
// the system:* prefix.
func TestApplyOwnerDefault_SystemSentinelRejected(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, _, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	ctx = WithOperatedByHeader(ctx, "system:cron")
	got := s.applyOwnerDefault(ctx, "")
	if got != "" {
		t.Errorf("applyOwnerDefault = %q; want \"\" (system sentinel rejected)", got)
	}
}

// TestApplyOwnerDefault_BareAgentShapeRejected pins that an agent-name
// header shape (no slack:/oidc: prefix) is refused.
func TestApplyOwnerDefault_BareAgentShapeRejected(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, _, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	ctx = WithOperatedByHeader(ctx, "gardenio-some-agent-name")
	got := s.applyOwnerDefault(ctx, "")
	if got != "" {
		t.Errorf("applyOwnerDefault = %q; want \"\" (bare-agent rejected)", got)
	}
}

// TestApplyOwnerDefault_AgentCaller_ChainRootMatches covers the agent-
// driven happy path: caller is a real agent, X-Operated-By matches the
// resolved spawn-chain root → Owner = header.
func TestApplyOwnerDefault_AgentCaller_ChainRootMatches(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	seedOwnerAgentBead(t, ms, "root-gardener", map[string]any{
		"slack_user_id": "U0OPERATOR1",
	})
	seedOwnerAgentBead(t, ms, "leaf-crew", map[string]any{
		"spawned_by": "root-gardener",
	})

	ctx := staticAuthCtx("leaf-crew")
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	got := s.applyOwnerDefault(ctx, "")
	if got != "slack:U0OPERATOR1" {
		t.Errorf("applyOwnerDefault = %q; want %q", got, "slack:U0OPERATOR1")
	}
}

// TestApplyOwnerDefault_AgentCaller_ChainRootMatches_UserBead exercises
// the user_bead_id dereference: the root agent points at a user bead with
// an email, the header is the corresponding oidc:<email>, and the match
// goes through.
func TestApplyOwnerDefault_AgentCaller_ChainRootMatches_UserBead(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	seedUserBead(t, ms, "user-mb", map[string]any{
		"email":         "matt@example.com",
		"slack_user_id": "U0OPERATOR1",
	})
	seedOwnerAgentBead(t, ms, "root", map[string]any{
		"user_bead_id": "user-mb",
	})
	seedOwnerAgentBead(t, ms, "leaf", map[string]any{
		"spawned_by": "root",
	})

	ctx := staticAuthCtx("leaf")
	ctx = WithOperatedByHeader(ctx, "oidc:matt@example.com")
	got := s.applyOwnerDefault(ctx, "")
	if got != "oidc:matt@example.com" {
		t.Errorf("applyOwnerDefault = %q; want %q", got, "oidc:matt@example.com")
	}
}

// TestApplyOwnerDefault_AgentCaller_ChainRootMismatch is the Matt-disaster
// shape: caller is a real agent, X-Operated-By is a relayed header that
// does NOT match the resolved spawn-chain root → Owner empty.
func TestApplyOwnerDefault_AgentCaller_ChainRootMismatch(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	// Spawn chain is rooted at Alice (oidc:alice), but the relayed header
	// carries Matt's slack id — exactly the disaster shape we want to
	// block.
	seedOwnerAgentBead(t, ms, "alice-root", map[string]any{
		"operated_by": "oidc:alice@example.com",
	})
	seedOwnerAgentBead(t, ms, "leaf-crew", map[string]any{
		"spawned_by": "alice-root",
	})

	ctx := staticAuthCtx("leaf-crew")
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	got := s.applyOwnerDefault(ctx, "")
	if got != "" {
		t.Errorf("applyOwnerDefault = %q; want \"\" (mismatch must reject)", got)
	}
}

// TestApplyOwnerDefault_AgentCaller_NoChainRoot covers the
// "chain unresolvable" failure: the caller agent's bead has no
// spawned_by and no human fields, so resolveSpawnChainRoot returns
// ("", false). The header alone must NOT default Owner for an agent
// caller.
func TestApplyOwnerDefault_AgentCaller_NoChainRoot(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	seedOwnerAgentBead(t, ms, "lonely-agent", map[string]any{})

	ctx := staticAuthCtx("lonely-agent")
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	got := s.applyOwnerDefault(ctx, "")
	if got != "" {
		t.Errorf("applyOwnerDefault = %q; want \"\" (no chain root)", got)
	}
}

// TestApplyOwnerDefault_NoHeader pins the "header missing" case: even with
// a flag-on, valid chain-root, applyOwnerDefault returns "" when no
// X-Operated-By header is set.
func TestApplyOwnerDefault_NoHeader(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	seedOwnerAgentBead(t, ms, "root", map[string]any{
		"slack_user_id": "U0OPERATOR1",
	})
	seedOwnerAgentBead(t, ms, "leaf", map[string]any{
		"spawned_by": "root",
	})

	ctx := staticAuthCtx("leaf")
	got := s.applyOwnerDefault(ctx, "")
	if got != "" {
		t.Errorf("applyOwnerDefault = %q; want \"\" (no header)", got)
	}
}

// TestApplyOwnerDefault_OwnerOverride pins the X-Owner-Override escape
// hatch: when ownerOverrideFromContext returns a value, applyOwnerDefault
// returns it — even with the flag off, no auth identity, no header.
func TestApplyOwnerDefault_OwnerOverride(t *testing.T) {
	s, _, _ := newTestServer()

	ctx := WithOwnerOverride(context.Background(), "slack:U0BRIDGE001")
	got := s.applyOwnerDefault(ctx, "")
	if got != "slack:U0BRIDGE001" {
		t.Errorf("applyOwnerDefault = %q; want %q", got, "slack:U0BRIDGE001")
	}
}

// TestApplyOwnerDefault_OwnerOverride_BeatsHeader proves the override
// short-circuits the header path: even when the header would otherwise
// default, the override wins.
func TestApplyOwnerDefault_OwnerOverride_BeatsHeader(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	seedOwnerAgentBead(t, ms, "root", map[string]any{
		"slack_user_id": "U0OPERATOR1",
	})
	seedOwnerAgentBead(t, ms, "leaf", map[string]any{
		"spawned_by": "root",
	})

	ctx := staticAuthCtx("leaf")
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	ctx = WithOwnerOverride(ctx, "oidc:bridge@example.com")
	got := s.applyOwnerDefault(ctx, "")
	if got != "oidc:bridge@example.com" {
		t.Errorf("applyOwnerDefault = %q; want %q (override must beat header)",
			got, "oidc:bridge@example.com")
	}
}

// TestApplyOwnerDefault_ExplicitOwnerWins pins the caller-supplied case:
// req.Owner != "" beats override, beats header.
func TestApplyOwnerDefault_ExplicitOwnerWins(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	seedOwnerAgentBead(t, ms, "root", map[string]any{
		"slack_user_id": "U0OPERATOR1",
	})
	ctx := staticAuthCtx("root")
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	ctx = WithOwnerOverride(ctx, "slack:U0BRIDGE001")
	got := s.applyOwnerDefault(ctx, "explicit-owner")
	if got != "explicit-owner" {
		t.Errorf("applyOwnerDefault = %q; want %q (explicit must win)",
			got, "explicit-owner")
	}
}

// TestApplyOwnerDefault_DashSentinel preserves the per-bead opt-out: "-"
// produces "" even when header + override would otherwise default.
func TestApplyOwnerDefault_DashSentinel(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	seedOwnerAgentBead(t, ms, "root", map[string]any{
		"slack_user_id": "U0OPERATOR1",
	})
	ctx := staticAuthCtx("root")
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	ctx = WithOwnerOverride(ctx, "slack:U0BRIDGE001")
	got := s.applyOwnerDefault(ctx, "-")
	if got != "" {
		t.Errorf("applyOwnerDefault(\"-\") = %q; want \"\"", got)
	}
}

// TestApplyOwnerDefault_FlagOff guards the rollout: with the flag unset,
// the header default does not fire. Override + caller-supplied still
// work.
func TestApplyOwnerDefault_FlagOff(t *testing.T) {
	// No t.Setenv — flag is unset.
	s, _, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	got := s.applyOwnerDefault(ctx, "")
	if got != "" {
		t.Errorf("applyOwnerDefault with flag off = %q; want \"\"", got)
	}
}

// TestApplyOwnerDefault_OptOutHeader covers X-No-Owner-Default: with the
// opt-out flag in ctx, the header default does not fire even when the
// flag is on and the header is well-formed.
func TestApplyOwnerDefault_OptOutHeader(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, _, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	ctx = WithNoOwnerDefault(ctx)
	got := s.applyOwnerDefault(ctx, "")
	if got != "" {
		t.Errorf("applyOwnerDefault(opt-out ctx) = %q; want \"\"", got)
	}
}

// TestApplyOwnerDefault_OIDCAuth covers OIDC-authenticated callers: they
// don't go through the X-Operated-By header sanitizer (the static-token
// interceptor is the only one that stamps the ctx key), so no header is
// present and Owner stays empty even with the flag on.
func TestApplyOwnerDefault_OIDCAuth(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, _, _ := newTestServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matt@example.com"})
	got := s.applyOwnerDefault(ctx, "")
	if got != "" {
		t.Errorf("applyOwnerDefault(oidc ctx) = %q; want \"\"", got)
	}
}

// TestParseNoOwnerDefaultHeader is unchanged from ka-0000000269 — the
// truthy-value matrix and header semantics are preserved.
func TestParseNoOwnerDefaultHeader(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{"TRUE", true},
		{"  1  ", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"junk", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseNoOwnerDefaultHeader(tt.in); got != tt.want {
				t.Errorf("parseNoOwnerDefaultHeader(%q) = %v; want %v",
					tt.in, got, tt.want)
			}
		})
	}
}

// TestSanitizeOwnerOverrideHeader pins the sanitizer contract: trims, caps
// length at 128, rejects control characters and empty/whitespace-only.
func TestSanitizeOwnerOverrideHeader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"slack id", "slack:U0OPERATOR1", "slack:U0OPERATOR1", true},
		{"oidc email", "oidc:matt@example.com", "oidc:matt@example.com", true},
		{"trimmed", "  slack:U0X  ", "slack:U0X", true},
		{"empty", "", "", false},
		{"whitespace", "   ", "", false},
		{"too long", strings.Repeat("x", 129), "", false},
		{"trailing newline stripped", "slack:U0X\n", "slack:U0X", true},
		{"tab", "slack:\tU0X", "", false},
		{"embedded newline", "slack:U\n0X", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitizeOwnerOverrideHeader(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("sanitizeOwnerOverrideHeader(%q) = (%q, %v); want (%q, %v)",
					tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestCreateBead_OwnerDefault_AgentChainRootMatches exercises the full
// createBead path: the persisted bead.Owner picks up the header when the
// caller is an agent and the header matches the spawn-chain root.
func TestCreateBead_OwnerDefault_AgentChainRootMatches(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	seedOwnerAgentBead(t, ms, "root", map[string]any{
		"slack_user_id": "U0OPERATOR1",
	})
	seedOwnerAgentBead(t, ms, "crew", map[string]any{
		"spawned_by": "root",
	})

	ctx := staticAuthCtx("crew")
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	in := createBeadInput{Title: "default owner", Type: "task"}

	bead, err := s.createBead(ctx, in)
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if stored.Owner != "slack:U0OPERATOR1" {
		t.Errorf("bead.Owner = %q; want %q", stored.Owner, "slack:U0OPERATOR1")
	}
}

// TestCreateBead_OwnerDefault_AgentChainRootMismatchEmpty pins the
// disaster guard: an agent write whose X-Operated-By is a relayed header
// that doesn't match the spawn-chain root persists Owner = "" (not
// Matt-as-default).
func TestCreateBead_OwnerDefault_AgentChainRootMismatchEmpty(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	// Root is Alice; header carries Matt — classic relay forward.
	seedOwnerAgentBead(t, ms, "alice-root", map[string]any{
		"operated_by": "oidc:alice@example.com",
	})
	seedOwnerAgentBead(t, ms, "crew", map[string]any{
		"spawned_by": "alice-root",
	})

	ctx := staticAuthCtx("crew")
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	in := createBeadInput{Title: "mismatch bead", Type: "task"}

	bead, err := s.createBead(ctx, in)
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if stored.Owner != "" {
		t.Errorf("bead.Owner = %q; want \"\" (mismatch must reject)", stored.Owner)
	}
}

// TestCreateBead_OwnerDefault_ExplicitWins confirms an explicit Owner is
// preserved through createBead even when the flag is on, a chain exists,
// and an override is present.
func TestCreateBead_OwnerDefault_ExplicitWins(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")
	s, ms, _ := newTestServer()

	seedOwnerAgentBead(t, ms, "root", map[string]any{
		"slack_user_id": "U0OPERATOR1",
	})

	ctx := staticAuthCtx("root")
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	ctx = WithOwnerOverride(ctx, "slack:U0BRIDGE001")
	in := createBeadInput{Title: "explicit owner", Type: "task", Owner: "human-owner"}

	bead, err := s.createBead(ctx, in)
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored.Owner != "human-owner" {
		t.Errorf("bead.Owner = %q; want %q (explicit must win)",
			stored.Owner, "human-owner")
	}
}

// TestHTTP_OwnerOverride covers the full HTTP wiring for the
// X-Owner-Override header: a static-token POST with X-Owner-Override but
// no Owner in the body persists the bead with Owner == override value.
func TestHTTP_OwnerOverride(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(srv, "primary-token", nil)

	body, _ := json.Marshal(createBeadInput{Title: "http override", Type: "task"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	// X-Operated-By is required by the audit resolver (R2 fail-closed).
	// X-Owner-Override is the orthogonal new header under test.
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR1")
	req.Header.Set("X-Owner-Override", "slack:U0BRIDGE001")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	id, _ := created["id"].(string)
	stored := ms.beads[id]
	if stored == nil {
		t.Fatalf("bead %s not persisted", id)
	}
	if stored.Owner != "slack:U0BRIDGE001" {
		t.Errorf("bead.Owner = %q; want %q", stored.Owner, "slack:U0BRIDGE001")
	}
}

// TestHTTP_OwnerDefault_OptOutHeader preserves the HTTP opt-out under the
// new policy: X-No-Owner-Default: 1 persists Owner = "" even when a
// header would otherwise default.
func TestHTTP_OwnerDefault_OptOutHeader(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")

	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(srv, "primary-token", nil)

	body, _ := json.Marshal(createBeadInput{Title: "http opt out", Type: "task"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR1")
	req.Header.Set("X-No-Owner-Default", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	id, _ := created["id"].(string)
	stored := ms.beads[id]
	if stored == nil {
		t.Fatalf("bead %s not persisted", id)
	}
	if stored.Owner != "" {
		t.Errorf("bead.Owner = %q; want \"\" (opt-out)", stored.Owner)
	}
}

// TestHTTP_OwnerDefault_NonAgentHumanHeader exercises the ka-0000000269
// happy path through HTTP: a primary-token caller (Subject == "static",
// non-agent) with X-Operated-By = human gets Owner = header.
func TestHTTP_OwnerDefault_NonAgentHumanHeader(t *testing.T) {
	t.Setenv("BEADS_OWNER_DEFAULT_ENABLED", "1")

	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(srv, "primary-token", nil)

	body, _ := json.Marshal(createBeadInput{Title: "http happy", Type: "task"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	id, _ := created["id"].(string)
	stored := ms.beads[id]
	if stored == nil {
		t.Fatalf("bead %s not persisted", id)
	}
	if stored.Owner != "slack:U0OPERATOR1" {
		t.Errorf("bead.Owner = %q; want %q", stored.Owner, "slack:U0OPERATOR1")
	}
}
