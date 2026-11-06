package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// autoInheritTypeConfig is the permissive type config used by the unit-level
// applyAutoInherit tests — declares both inherited fields so the type-aware
// filter doesn't strip them. The end-to-end createBead tests below pick a
// real builtin type (agent) that already declares both.
var autoInheritTypeConfig = &model.TypeConfig{
	Kind: model.KindIssue,
	Fields: []model.FieldDef{
		{Name: "operated_by", Type: model.FieldTypeString},
		{Name: "user_bead_id", Type: model.FieldTypeString},
		{Name: "x", Type: model.FieldTypeString},
	},
}

// autoInheritSlackTypeConfig adds slack_user_id to the permissive type config
// so the unit-level applyAutoInherit tests can exercise the
// slack_user_id-derivation branch (ka-0000005990). The existing tests above
// deliberately use a type config WITHOUT slack_user_id so the filter strips
// the derived value — that pins the type-aware-filter contract.
var autoInheritSlackTypeConfig = &model.TypeConfig{
	Kind: model.KindIssue,
	Fields: []model.FieldDef{
		{Name: "operated_by", Type: model.FieldTypeString},
		{Name: "user_bead_id", Type: model.FieldTypeString},
		{Name: "slack_user_id", Type: model.FieldTypeString},
		{Name: "x", Type: model.FieldTypeString},
	},
}

// TestSanitizeUserBeadIDHeader pins the trust-boundary contract for the new
// X-User-Bead-Id request header. Same defensive posture as
// sanitizeOperatedByHeader / sanitizeActorHeader: reject empty, overlong,
// control-character, or non-bead-id-shaped values so a malformed header can
// never smuggle log-line breaks or junk values into Fields[user_bead_id].
func TestSanitizeUserBeadIDHeader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"empty", "", "", false},
		{"whitespace only", "   \t  ", "", false},
		{"kd id", "ka-0000002859", "ka-0000002859", true},
		{"trims surrounding ws", "  ka-0000002859  ", "ka-0000002859", true},
		{"too long", strings.Repeat("a", 65), "", false},
		{"max length", strings.Repeat("a", 64), strings.Repeat("a", 64), true},
		{"embedded newline", "gd-abc\nINJECT", "", false},
		{"embedded space", "gd-abc INJECT", "", false},
		{"colon (rejected — not a bead id)", "slack:U123", "", false},
		{"slash (rejected)", "gd-/etc/passwd", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitizeUserBeadIDHeader(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("sanitizeUserBeadIDHeader(%q) = (%q, %v); want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestApplyAutoInherit covers the create-handler defaulting matrix: the
// helper fills in Fields[operated_by] and Fields[user_bead_id] from
// request context when the caller omitted them, leaves caller-supplied
// values untouched (idempotency), and short-circuits on X-No-Auto-Inherit.
// The unit-level tests pin the contract independently of the HTTP/gRPC
// header pipeline so refactoring the auth layer cannot silently regress
// the spawn-attribution chain.
func TestApplyAutoInherit(t *testing.T) {
	const op = "slack:U0OPERATOR2"
	const ub = "gd-ZZjnn3QScW"

	tests := []struct {
		name        string
		ctx         func() context.Context
		fields      string
		wantOp      string
		wantUB      string
		wantUnchged bool
	}{
		{
			name:   "fills both when caller omits and ctx has both",
			ctx:    func() context.Context { return setBoth(context.Background(), op, ub) },
			fields: ``,
			wantOp: op,
			wantUB: ub,
		},
		{
			name:   "fills both with empty object input",
			ctx:    func() context.Context { return setBoth(context.Background(), op, ub) },
			fields: `{}`,
			wantOp: op,
			wantUB: ub,
		},
		{
			name:        "idempotent when both already set",
			ctx:         func() context.Context { return setBoth(context.Background(), op, ub) },
			fields:      `{"operated_by":"slack:UPRESET","user_bead_id":"gd-preset"}`,
			wantOp:      "slack:UPRESET",
			wantUB:      "gd-preset",
			wantUnchged: true,
		},
		{
			name:   "fills only operated_by when user_bead_id already set",
			ctx:    func() context.Context { return setBoth(context.Background(), op, ub) },
			fields: `{"user_bead_id":"gd-preset"}`,
			wantOp: op,
			wantUB: "gd-preset",
		},
		{
			name:   "fills only user_bead_id when operated_by already set",
			ctx:    func() context.Context { return setBoth(context.Background(), op, ub) },
			fields: `{"operated_by":"slack:UPRESET"}`,
			wantOp: "slack:UPRESET",
			wantUB: ub,
		},
		{
			name:   "treats empty string operated_by as absent",
			ctx:    func() context.Context { return setBoth(context.Background(), op, ub) },
			fields: `{"operated_by":"","user_bead_id":""}`,
			wantOp: op,
			wantUB: ub,
		},
		{
			name:        "opt-out skips defaulting entirely",
			ctx:         func() context.Context { return WithNoAutoInherit(setBoth(context.Background(), op, ub), true) },
			fields:      `{}`,
			wantOp:      "",
			wantUB:      "",
			wantUnchged: true,
		},
		{
			name:        "no-op when ctx has no operated_by or user_bead_id",
			ctx:         func() context.Context { return context.Background() },
			fields:      `{"x":"y"}`,
			wantUnchged: true,
		},
		{
			name:        "leaves non-object fields untouched",
			ctx:         func() context.Context { return setBoth(context.Background(), op, ub) },
			fields:      `["array","is","not","object"]`,
			wantUnchged: true,
		},
		{
			name:   "fills only operated_by when ctx user_bead_id missing",
			ctx:    func() context.Context { return WithOperatedByHeader(context.Background(), op) },
			fields: ``,
			wantOp: op,
			wantUB: "",
		},
		{
			name:   "fills only user_bead_id when ctx operated_by missing",
			ctx:    func() context.Context { return WithUserBeadID(context.Background(), ub) },
			fields: ``,
			wantOp: "",
			wantUB: ub,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in json.RawMessage
			if tt.fields != "" {
				in = json.RawMessage(tt.fields)
			}
			out := applyAutoInherit(tt.ctx(), in, autoInheritTypeConfig)

			if tt.wantUnchged {
				if string(out) != string(in) {
					t.Errorf("expected unchanged fields; got %q, want %q", string(out), string(in))
				}
				// For the idempotent path, we still assert the field values
				// to guard against accidental rewrites that happen to
				// re-marshal to the same bytes.
			}

			got := decodeFields(t, out)
			if v, _ := got["operated_by"].(string); v != tt.wantOp {
				t.Errorf("operated_by = %q; want %q", v, tt.wantOp)
			}
			if v, _ := got["user_bead_id"].(string); v != tt.wantUB {
				t.Errorf("user_bead_id = %q; want %q", v, tt.wantUB)
			}
		})
	}
}

// TestApplyAutoInherit_DerivesSlackUserID pins the ka-0000005990 contract:
// the create handler auto-extracts the U… portion of operated_by="slack:U…"
// and stamps Fields[slack_user_id] when empty. Covers the three breaks
// research ka-0000007523 identified — trigger-fired beads, gardener→crew
// agent beads, and the Slack /spawn task bead — plus the don't-derive
// shape negative cases (oidc:, system:, malformed slack:) and the
// type-aware filter (slack_user_id absent from type config → skip derive).
func TestApplyAutoInherit_DerivesSlackUserID(t *testing.T) {
	tests := []struct {
		name      string
		ctx       func() context.Context
		fields    string
		tc        *model.TypeConfig
		wantSlack string // "" when no derivation expected
		wantOp    string
	}{
		{
			name:      "derives from ctx-inherited slack: operated_by",
			ctx:       func() context.Context { return WithOperatedByHeader(context.Background(), "slack:U0OPERATOR2") },
			fields:    ``,
			tc:        autoInheritSlackTypeConfig,
			wantSlack: "U0OPERATOR2",
			wantOp:    "slack:U0OPERATOR2",
		},
		{
			name:      "derives from caller-supplied slack: operated_by (Slack /spawn task bead)",
			ctx:       func() context.Context { return context.Background() },
			fields:    `{"operated_by":"slack:U0OPERATOR1"}`,
			tc:        autoInheritSlackTypeConfig,
			wantSlack: "U0OPERATOR1",
			wantOp:    "slack:U0OPERATOR1",
		},
		{
			name:      "idempotent — caller-supplied slack_user_id wins",
			ctx:       func() context.Context { return WithOperatedByHeader(context.Background(), "slack:U0OPERATOR2") },
			fields:    `{"slack_user_id":"UPRESET"}`,
			tc:        autoInheritSlackTypeConfig,
			wantSlack: "UPRESET",
			wantOp:    "slack:U0OPERATOR2",
		},
		{
			name:      "treats empty string slack_user_id as absent (derives)",
			ctx:       func() context.Context { return WithOperatedByHeader(context.Background(), "slack:U0OPERATOR2") },
			fields:    `{"slack_user_id":""}`,
			tc:        autoInheritSlackTypeConfig,
			wantSlack: "U0OPERATOR2",
			wantOp:    "slack:U0OPERATOR2",
		},
		{
			name:      "no derivation for oidc: operated_by",
			ctx:       func() context.Context { return WithOperatedByHeader(context.Background(), "oidc:matt@acme.example") },
			fields:    ``,
			tc:        autoInheritSlackTypeConfig,
			wantSlack: "",
			wantOp:    "oidc:matt@acme.example",
		},
		{
			name: "no derivation for system: sentinel",
			ctx: func() context.Context {
				return WithOperatedByHeader(context.Background(), "system:legacy-pre-attribution")
			},
			fields:    ``,
			tc:        autoInheritSlackTypeConfig,
			wantSlack: "",
			wantOp:    "system:legacy-pre-attribution",
		},
		{
			name:      "no derivation for slack: without U prefix",
			ctx:       func() context.Context { return WithOperatedByHeader(context.Background(), "slack:bogus") },
			fields:    ``,
			tc:        autoInheritSlackTypeConfig,
			wantSlack: "",
			wantOp:    "slack:bogus",
		},
		{
			name:      "no derivation for empty slack: subject",
			ctx:       func() context.Context { return WithOperatedByHeader(context.Background(), "slack:") },
			fields:    ``,
			tc:        autoInheritSlackTypeConfig,
			wantSlack: "",
			wantOp:    "slack:",
		},
		{
			name:      "no derivation when type config rejects slack_user_id",
			ctx:       func() context.Context { return WithOperatedByHeader(context.Background(), "slack:U0OPERATOR2") },
			fields:    ``,
			tc:        autoInheritTypeConfig, // no slack_user_id field
			wantSlack: "",
			wantOp:    "slack:U0OPERATOR2",
		},
		{
			name: "opt-out skips slack derivation too",
			ctx: func() context.Context {
				return WithNoAutoInherit(WithOperatedByHeader(context.Background(), "slack:U0OPERATOR2"), true)
			},
			fields:    `{"operated_by":"slack:U0OPERATOR1"}`,
			tc:        autoInheritSlackTypeConfig,
			wantSlack: "",
			wantOp:    "slack:U0OPERATOR1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in json.RawMessage
			if tt.fields != "" {
				in = json.RawMessage(tt.fields)
			}
			out := applyAutoInherit(tt.ctx(), in, tt.tc)
			got := decodeFields(t, out)
			if v, _ := got["slack_user_id"].(string); v != tt.wantSlack {
				t.Errorf("slack_user_id = %q; want %q (out=%s)", v, tt.wantSlack, string(out))
			}
			if v, _ := got["operated_by"].(string); v != tt.wantOp {
				t.Errorf("operated_by = %q; want %q (out=%s)", v, tt.wantOp, string(out))
			}
		})
	}
}

// TestExtractSlackUserID pins the trust-boundary contract for the
// operated_by → slack_user_id derivation. Rejects anything outside the
// "slack:U<alnum>" shape so a malformed operated_by header that slipped
// past sanitization cannot smuggle junk values into Fields[slack_user_id].
func TestExtractSlackUserID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"happy path", "slack:U0OPERATOR2", "U0OPERATOR2", true},
		{"workspace-prefix W rejected", "slack:W0AGMP95H54", "", false},
		{"bot-prefix B rejected", "slack:B0AGMP95H54", "", false},
		{"oidc rejected", "oidc:matt@acme.example", "", false},
		{"system sentinel rejected", "system:legacy-pre-attribution", "", false},
		{"empty rejected", "", "", false},
		{"slack: with empty subject", "slack:", "", false},
		{"slack:U with empty after U", "slack:U", "U", true},
		{"lowercase rejected", "slack:Uabc123", "", false},
		{"colon-in-subject rejected", "slack:U123:extra", "", false},
		{"whitespace-in-subject rejected", "slack:U123 inj", "", false},
		{"newline-in-subject rejected", "slack:U123\nINJ", "", false},
		{"hyphen rejected (not Slack id charset)", "slack:U-ABC", "", false},
		{"overlong rejected", "slack:U" + strings.Repeat("A", 64), "", false},
		{"max length accepted", "slack:" + "U" + strings.Repeat("A", 63), "U" + strings.Repeat("A", 63), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractSlackUserID(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("extractSlackUserID(%q) = (%q, %v); want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestCreateBead_AutoInheritsSlackUserID covers the slack_user_id derivation
// end-to-end through s.createBead. The "agent" builtin type declares
// slack_user_id, so the persisted bead carries the derived value alongside
// the inherited operated_by — exactly what bot-DM notification targeting
// (bot_decisions.go:48) needs to find the originating Slack user.
func TestCreateBead_AutoInheritsSlackUserID(t *testing.T) {
	s, ms, _ := newTestServer()

	ctx := setBoth(context.Background(), "slack:U0OPERATOR2", "")
	in := createBeadInput{Title: "trigger-spawned task", Type: "task"}

	bead, err := s.createBead(ctx, in)
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	fields := decodeFields(t, stored.Fields)
	if v, _ := fields["operated_by"].(string); v != "slack:U0OPERATOR2" {
		t.Errorf("Fields[operated_by] = %q; want %q", v, "slack:U0OPERATOR2")
	}
	if v, _ := fields["slack_user_id"].(string); v != "U0OPERATOR2" {
		t.Errorf("Fields[slack_user_id] = %q; want %q", v, "U0OPERATOR2")
	}
}

// TestCreateBead_AutoInherits exercises the auto-inherit path end-to-end
// through s.createBead so the wiring in beads.go is covered. The mock store
// captures the persisted bead and the test inspects Fields[] on it.
func TestCreateBead_AutoInherits(t *testing.T) {
	s, ms, _ := newTestServer()

	ctx := setBoth(context.Background(), "slack:U0OPERATOR2", "gd-qrssr0oqys")
	in := createBeadInput{Title: "auto-inherit me", Type: "task"}

	bead, err := s.createBead(ctx, in)
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}

	fields := decodeFields(t, stored.Fields)
	if v, _ := fields["operated_by"].(string); v != "slack:U0OPERATOR2" {
		t.Errorf("Fields[operated_by] = %q; want %q", v, "slack:U0OPERATOR2")
	}
	if v, _ := fields["user_bead_id"].(string); v != "gd-qrssr0oqys" {
		t.Errorf("Fields[user_bead_id] = %q; want %q", v, "gd-qrssr0oqys")
	}
}

// TestCreateBead_AutoInheritIdempotent confirms the per-site spawn handlers
// in gardenio (triggers.go, bot_commands.go, scheduler.go) still win — the
// auto-inherit code only fills in when the caller omits the field, so
// migrating those handlers to the server-side default is a clean cleanup
// rather than a breaking change.
func TestCreateBead_AutoInheritIdempotent(t *testing.T) {
	s, ms, _ := newTestServer()

	ctx := setBoth(context.Background(), "slack:U_CTX", "gd-ctxUserBd")
	in := createBeadInput{
		Title:  "explicit",
		Type:   "task",
		Fields: json.RawMessage(`{"operated_by":"slack:U_EXPLICIT","user_bead_id":"gd-352PjEMKXH"}`),
	}

	bead, err := s.createBead(ctx, in)
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	stored := ms.beads[bead.ID]
	fields := decodeFields(t, stored.Fields)
	if v, _ := fields["operated_by"].(string); v != "slack:U_EXPLICIT" {
		t.Errorf("Fields[operated_by] = %q; want %q (caller-supplied must win)", v, "slack:U_EXPLICIT")
	}
	if v, _ := fields["user_bead_id"].(string); v != "gd-352PjEMKXH" {
		t.Errorf("Fields[user_bead_id] = %q; want %q (caller-supplied must win)", v, "gd-352PjEMKXH")
	}
}

// TestCreateBead_OptOutHeader confirms X-No-Auto-Inherit short-circuits
// the defaulting so legacy/backfill clients can preserve the previous
// "Fields empty unless explicitly set" semantics.
func TestCreateBead_OptOutHeader(t *testing.T) {
	s, ms, _ := newTestServer()

	ctx := setBoth(context.Background(), "slack:U_CTX", "gd-ctxUserBd")
	ctx = WithNoAutoInherit(ctx, true)
	in := createBeadInput{Title: "no inherit", Type: "task"}

	bead, err := s.createBead(ctx, in)
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	stored := ms.beads[bead.ID]
	if len(stored.Fields) == 0 {
		return // empty Fields — expected: no defaults applied
	}
	fields := decodeFields(t, stored.Fields)
	if _, ok := fields["operated_by"]; ok {
		t.Errorf("Fields[operated_by] unexpectedly populated under opt-out: %v", fields)
	}
	if _, ok := fields["user_bead_id"]; ok {
		t.Errorf("Fields[user_bead_id] unexpectedly populated under opt-out: %v", fields)
	}
}

// TestHTTP_StaticTokenHeadersAutoInherit covers the full HTTP path: the
// static-token auth interceptor stamps ctx from the X-Operated-By and
// X-User-Bead-Id request headers, and the create handler auto-fills the
// corresponding Fields entries. This is the contract gardenio agent pods
// rely on once the per-site Fields[] pushes are removed.
func TestHTTP_StaticTokenHeadersAutoInherit(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(srv, "primary-token", nil)

	body, _ := json.Marshal(createBeadInput{Title: "http auto-inherit", Type: "task"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	req.Header.Set("X-User-Bead-Id", "gd-ZZjnn3QScW")
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
	if id == "" {
		t.Fatalf("missing id in response: %v", created)
	}
	stored := ms.beads[id]
	if stored == nil {
		t.Fatalf("bead %s not persisted", id)
	}
	fields := decodeFields(t, stored.Fields)
	if v, _ := fields["operated_by"].(string); v != "slack:U0OPERATOR2" {
		t.Errorf("Fields[operated_by] = %q; want %q", v, "slack:U0OPERATOR2")
	}
	if v, _ := fields["user_bead_id"].(string); v != "gd-ZZjnn3QScW" {
		t.Errorf("Fields[user_bead_id] = %q; want %q", v, "gd-ZZjnn3QScW")
	}
}

// TestHTTP_NoAutoInheritHeader pins the opt-out path through the HTTP
// interceptor: the header sets ctx, the create handler short-circuits, and
// the persisted bead has no Fields entries even though the X-Operated-By
// header is present.
func TestHTTP_NoAutoInheritHeader(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(srv, "primary-token", nil)

	body, _ := json.Marshal(createBeadInput{Title: "opt out", Type: "task"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	req.Header.Set("X-User-Bead-Id", "gd-ZZjnn3QScW")
	req.Header.Set("X-No-Auto-Inherit", "1")
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
	if len(stored.Fields) > 0 {
		fields := decodeFields(t, stored.Fields)
		if _, ok := fields["operated_by"]; ok {
			t.Errorf("Fields[operated_by] unexpectedly populated under X-No-Auto-Inherit: %v", fields)
		}
		if _, ok := fields["user_bead_id"]; ok {
			t.Errorf("Fields[user_bead_id] unexpectedly populated under X-No-Auto-Inherit: %v", fields)
		}
	}
}

// setBoth seeds operated_by + user_bead_id on a context — the typical
// shape an HTTP request ends up with after the interceptor stamps them
// from request headers. Also stamps a static AuthIdentity so the post-R2
// resolver (ka-0000005772) enters the static branch and reads the
// X-Operated-By header value — without this the resolver short-circuits
// on the anonymous identity and createBead fail-closes.
func setBoth(ctx context.Context, op, ub string) context.Context {
	ctx = withTestAgent(ctx, "test-static-agent")
	if op != "" {
		ctx = WithOperatedByHeader(ctx, op)
	}
	if ub != "" {
		ctx = WithUserBeadID(ctx, ub)
	}
	return ctx
}

// decodeFields decodes a Fields JSON blob, tolerating nil/empty/non-object
// input. Non-object payloads (arrays, scalars, null) return an empty map so
// assertions can use zero-value semantics for absent fields.
func decodeFields(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	out := make(map[string]any)
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return out
	}
	if trimmed[0] != '{' {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode Fields: %v (raw=%s)", err, string(raw))
	}
	return out
}
