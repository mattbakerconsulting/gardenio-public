package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestSanitizeOperatedByHeader pins the trust-boundary contract: only
// non-empty, length-bounded, control-char-free strings are accepted. A bad
// header on a static-token call must NOT escape into the operator field.
func TestSanitizeOperatedByHeader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"empty", "", "", false},
		{"whitespace only", "   \t\n  ", "", false},
		{"slack id", "slack:U0OPERATOR2", "slack:U0OPERATOR2", true},
		{"oidc subject", "oidc:matt@acme.example", "oidc:matt@acme.example", true},
		{"trims surrounding ws", "  slack:U123  ", "slack:U123", true},
		{"too long", strings.Repeat("a", 129), "", false},
		{"max length junk rejected as non-human", strings.Repeat("a", 128), "", false},
		{"embedded newline", "slack:U123\nINJECTED", "", false},
		{"embedded CR", "slack:U123\rINJECTED", "", false},
		{"embedded tab", "slack:U\t123", "", false},
		{"DEL char", "slack:U\x7f123", "", false},
		// ka-0000002601: header must be a human shape. Agent ids,
		// sentinels, and bare strings are rejected at the boundary.
		{"agent id rejected", "sched-gardenio-gardener-release-qjj", "", false},
		{"agent id with thread- prefix", "thread-1779239441-920949", "", false},
		{"system sentinel rejected", "system:legacy-pre-attribution", "", false},
		{"fleet sentinel rejected", "fleet:gardenio", "", false},
		{"slack with non-U prefix rejected", "slack:bogus", "", false},
		{"oidc without email shape rejected", "oidc:notanemail", "", false},
		{"oidc with valid email accepted", "oidc:matt@acme.example", "oidc:matt@acme.example", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitizeOperatedByHeader(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("sanitizeOperatedByHeader(%q) = (%q, %v); want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestOperatedByFromContext_NoFlag pins the R2 inversion (ka-0000005772):
// the resolver no longer gates on an env flag, so every supported identity
// shape resolves to its expected operator string with no setup. Fleet
// tokens attribute to the "fleet:<subject>" sentinel; anonymous callers
// still resolve to "" — that's a property of the resolver, not of a kill
// switch.
func TestOperatedByFromContext_NoFlag(t *testing.T) {
	bead := &model.Bead{ID: "agent-x", Fields: json.RawMessage(`{"slack_user_id":"U123"}`)}
	srv := newTestServerWithAgentBead(bead)

	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"oidc", WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matt@acme.example"}), "oidc:matt@acme.example"},
		{"agent-with-header", WithOperatedByHeader(withTestAgent(context.Background(), "agent-x"), "slack:U999"), "slack:U999"},
		{"agent-fallback", withTestAgent(context.Background(), "agent-x"), "slack:U123"},
		{"fleet", WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "gardenio"}), "fleet:gardenio"},
		{"anonymous", context.Background(), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := srv.operatedByFromContext(tc.ctx); got != tc.want {
				t.Errorf("resolver returned %q; want %q", got, tc.want)
			}
		})
	}
}

// TestOperatedByFromContext_OIDC pins step 1 of the resolver ladder: OIDC
// callers always resolve to "oidc:<subject>" — no header, no fallback,
// no trust escalation.
func TestOperatedByFromContext_OIDC(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matt@acme.example"})
	if got := srv.operatedByFromContext(ctx); got != "oidc:matt@acme.example" {
		t.Errorf("got %q; want oidc:matt@acme.example", got)
	}

	// OIDC with empty subject must not produce "oidc:" — that's a useless
	// half-record. Resolve to "" instead.
	ctx = WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: ""})
	if got := srv.operatedByFromContext(ctx); got != "" {
		t.Errorf("OIDC with empty subject got %q; want empty", got)
	}

	// An X-Operated-By header on an OIDC call must NOT override the subject
	// (header is only honored for static-token callers — R3 §4).
	ctx = WithOperatedByHeader(
		WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matt@acme.example"}),
		"slack:UMALICIOUS",
	)
	if got := srv.operatedByFromContext(ctx); got != "oidc:matt@acme.example" {
		t.Errorf("OIDC + header got %q; header must not override subject", got)
	}
}

// TestOperatedByFromContext_StaticHeader pins step 2: a fleet-token caller
// with a sanitized X-Operated-By header gets the header verbatim. Agents
// already format the value (e.g. "slack:Uxxx") so the resolver does not
// re-prefix.
func TestOperatedByFromContext_StaticHeader(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	ctx := WithOperatedByHeader(
		WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"}),
		"slack:U0OPERATOR2",
	)
	if got := srv.operatedByFromContext(ctx); got != "slack:U0OPERATOR2" {
		t.Errorf("got %q; want slack:U0OPERATOR2", got)
	}
}

// TestOperatedByFromContext_StaticFallback pins step 3: when a static-token
// call lacks the header, fall back to the agent's bead — read its
// slack_user_id field and synthesize "slack:<id>". This lets long-lived
// agent pods inherit the human who originally spawned them.
func TestOperatedByFromContext_StaticFallback(t *testing.T) {
	bead := &model.Bead{
		ID:     "gardenio-p2-agent",
		Fields: json.RawMessage(`{"slack_user_id":"U0OPERATOR2","other":"ignored"}`),
	}
	srv := newTestServerWithAgentBead(bead)

	ctx := withTestAgent(context.Background(), "gardenio-p2-agent")
	if got := srv.operatedByFromContext(ctx); got != "slack:U0OPERATOR2" {
		t.Errorf("got %q; want slack:U0OPERATOR2", got)
	}

	// Subject pointing at a non-existent bead → "".
	if got := srv.operatedByFromAgentBead(context.Background(), "no-such-agent"); got != "" {
		t.Errorf("missing-bead got %q; want empty", got)
	}

	// The legacy "static" subject (rows written under the retired shared
	// token) must not attempt a bead lookup keyed by the literal string.
	if got := srv.operatedByFromAgentBead(context.Background(), "static"); got != "" {
		t.Errorf("static-sentinel got %q; want empty", got)
	}
}

// TestOperatedByFromContext_StaticFallbackMalformed pins the failure modes
// of the bead-fields parse path: a bead with no Fields, malformed JSON, a
// non-string slack_user_id, or an empty slack_user_id all resolve to "".
// We refuse to fabricate an attribution we can't justify.
func TestOperatedByFromContext_StaticFallbackMalformed(t *testing.T) {
	cases := []struct {
		name   string
		fields json.RawMessage
	}{
		{"nil fields", nil},
		{"empty fields", json.RawMessage(``)},
		{"malformed json", json.RawMessage(`{not json`)},
		{"missing slack_user_id", json.RawMessage(`{"other":"x"}`)},
		{"non-string slack_user_id", json.RawMessage(`{"slack_user_id":42}`)},
		{"empty slack_user_id", json.RawMessage(`{"slack_user_id":""}`)},
		{"whitespace slack_user_id", json.RawMessage(`{"slack_user_id":"   "}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := &model.Bead{ID: "agent-x", Fields: tc.fields}
			srv := newTestServerWithAgentBead(bead)
			if got := srv.operatedByFromAgentBead(context.Background(), "agent-x"); got != "" {
				t.Errorf("got %q; want empty", got)
			}
		})
	}
}

// TestOperatedByFromContext_Anonymous pins the unsupported branches:
// anonymous / none / refresh identities resolve to "". Fleet tokens now
// resolve to "fleet:<subject>" via the resolver's fleet branch (R2,
// ka-0000005772) — see TestOperatedByFromContext_NoFlag.
func TestOperatedByFromContext_Anonymous(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"anonymous", context.Background()},
		{"none", WithAuthIdentity(context.Background(), AuthIdentity{Type: "none"})},
		{"refresh", WithAuthIdentity(context.Background(), AuthIdentity{Type: "refresh"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := srv.operatedByFromContext(tc.ctx); got != "" {
				t.Errorf("got %q; want empty", got)
			}
		})
	}
}

// TestOperatedByFromContext_StaticFallback_RejectsAgentShape pins the
// ka-0000002601 fix: when the spawn chain pushed a non-human value
// (agent id, "thread-…", "sched-…") into fields.operated_by, the
// resolver MUST NOT round-trip that as the recorded operator. It falls
// through to slack_user_id; if that is absent the resolver returns ""
// and the live-state caller fails the write.
func TestOperatedByFromContext_StaticFallback_RejectsAgentShape(t *testing.T) {
	cases := []struct {
		name    string
		fields  string
		want    string
		comment string
	}{
		{
			name:    "operated_by is gardener agent id, slack_user_id absent → empty",
			fields:  `{"operated_by":"sched-gardenio-gardener-release-qjj"}`,
			want:    "",
			comment: "gardener spawner pushed its own id; no slack fallback ⇒ refuse the write",
		},
		{
			name:    "operated_by is thread-* id, slack_user_id present → slack wins",
			fields:  `{"operated_by":"thread-1779239441-920949","slack_user_id":"U0OPERATOR1"}`,
			want:    "slack:U0OPERATOR1",
			comment: "non-human operated_by is skipped; slack_user_id is the human ancestor",
		},
		{
			name:    "operated_by is bare gardenio-* agent id → fall through to slack",
			fields:  `{"operated_by":"gardenio-p1-migration-015-r36","slack_user_id":"U0OPERATOR1"}`,
			want:    "slack:U0OPERATOR1",
			comment: "agent id never resolves as the operator",
		},
		{
			name:    "operated_by is slack:U… (human) → propagates",
			fields:  `{"operated_by":"slack:U0OPERATOR1"}`,
			want:    "slack:U0OPERATOR1",
			comment: "well-formed human shape round-trips as before",
		},
		{
			name:    "operated_by is oidc:user@host (human) → propagates",
			fields:  `{"operated_by":"oidc:matt@acme.example"}`,
			want:    "oidc:matt@acme.example",
			comment: "oidc human shape is accepted",
		},
		{
			name:    "operated_by is system:legacy-pre-attribution → fall through",
			fields:  `{"operated_by":"system:legacy-pre-attribution","slack_user_id":"U0OPERATOR1"}`,
			want:    "slack:U0OPERATOR1",
			comment: "sentinels are not humans; per-invariant they cannot stamp new writes via inheritance",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := &model.Bead{ID: "agent-x", Fields: json.RawMessage(tc.fields)}
			srv := newTestServerWithAgentBead(bead)
			if got := srv.operatedByFromAgentBead(context.Background(), "agent-x"); got != tc.want {
				t.Errorf("%s: got %q; want %q (%s)", tc.name, got, tc.want, tc.comment)
			}
		})
	}
}

// TestIsHumanOperator pins the shape predicate that drives the header
// sanitizer and the agent-bead fallback filter.
func TestIsHumanOperator(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"slack:U0OPERATOR2", true},
		{"slack:UABC123", true},
		{"slack:bogus", false},       // not U-prefixed
		{"slack:", false},            // empty subject
		{"slack:U0oPERATOR1", false}, // lowercase rejected by extractSlackUserID
		{"oidc:matt@acme.example", true},
		{"oidc:alice@example.com", true},
		{"oidc:notanemail", false},
		{"oidc:@host.com", false},
		{"oidc:matt@", false},
		{"oidc:matt@host", false}, // no dot in domain
		{"system:legacy-pre-attribution", false},
		{"fleet:gardenio", false},
		{"sched-gardenio-gardener-release-qjj", false},
		{"thread-1779239441-920949", false},
		{"gardenio-p1-migration-015-r36", false},
		{"static", false},
	}
	for _, tc := range cases {
		if got := isHumanOperator(tc.in); got != tc.want {
			t.Errorf("isHumanOperator(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// TestIsAgentOperator pins the counter-increment predicate.
func TestIsAgentOperator(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"slack:U0OPERATOR1", false},
		{"oidc:matt@acme.example", false},
		{"system:legacy-pre-attribution", false},
		{"fleet:gardenio", false},
		{"sched-gardenio-gardener-release-qjj", true},
		{"thread-1779239441-920949", true},
		{"gardenio-p1-migration-015-r36", true},
		{"static", true},
		{"unknown-prefix:foo", true},
	}
	for _, tc := range cases {
		if got := isAgentOperator(tc.in); got != tc.want {
			t.Errorf("isAgentOperator(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// newTestServerWithAgentBead constructs a BeadsServer whose store contains
// the given agent bead (or none, when nil). Used by resolver tests that
// exercise the agent-bead fallback path.
func newTestServerWithAgentBead(b *model.Bead) *BeadsServer {
	ms := &mockStore{beads: map[string]*model.Bead{}}
	if b != nil {
		ms.beads[b.ID] = b
	}
	return &BeadsServer{store: ms}
}
