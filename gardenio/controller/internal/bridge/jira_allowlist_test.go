package bridge

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// allowlistTestLogger returns a TextHandler logger writing to buf at Warn level
// so tests can assert that a rejection was logged with role+field (the
// TextHandler renders attrs as key=value, which the assertions match on).
func allowlistTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// testAllowlist is a small fixed policy used across the table tests: one broad
// role (all fields, internal only), one role that may also post public replies,
// one internal-note-only role, and (implicitly) every other role unknown.
func testAllowlist() FieldAllowlist {
	return FieldAllowlist{
		"crew": allFieldsPolicy(),
		"support-public": {
			Fields:             map[JiraWriteField]bool{FieldComment: true},
			AllowPublicComment: true,
		},
		"restricted": internalNoteOnlyPolicy(),
	}
}

func TestAllowlistGate_Field(t *testing.T) {
	tests := []struct {
		name       string
		mode       AllowlistMode
		role       string
		field      JiraWriteField
		wantAllow  bool   // gate's proceed/drop result
		wantReason string // pure Decision.Reason
		wantLog    bool   // whether a denial line should be logged
	}{
		{
			name:       "allowed field passes",
			mode:       AllowlistModeEnforce,
			role:       "crew",
			field:      FieldTransition,
			wantAllow:  true,
			wantReason: "allowed",
			wantLog:    false,
		},
		{
			name:       "disallowed field dropped and logged",
			mode:       AllowlistModeEnforce,
			role:       "restricted",
			field:      FieldTransition,
			wantAllow:  false,
			wantReason: "field not in role allowlist",
			wantLog:    true,
		},
		{
			name:       "unknown role fails closed",
			mode:       AllowlistModeEnforce,
			role:       "intruder",
			field:      FieldComment,
			wantAllow:  false,
			wantReason: "unknown role",
			wantLog:    true,
		},
		{
			name:       "empty role fails closed",
			mode:       AllowlistModeEnforce,
			role:       "",
			field:      FieldComment,
			wantAllow:  false,
			wantReason: "empty role",
			wantLog:    true,
		},
		{
			name:       "unknown field fails closed",
			mode:       AllowlistModeEnforce,
			role:       "crew",
			field:      JiraWriteField("priority"),
			wantAllow:  false,
			wantReason: "unknown field",
			wantLog:    true,
		},
		{
			name:       "shadow mode allows but logs the denial",
			mode:       AllowlistModeShadow,
			role:       "restricted",
			field:      FieldAssignee,
			wantAllow:  true, // shadow lets it through
			wantReason: "field not in role allowlist",
			wantLog:    true,
		},
		{
			name:       "shadow mode allowed field does not log",
			mode:       AllowlistModeShadow,
			role:       "crew",
			field:      FieldLabels,
			wantAllow:  true,
			wantReason: "allowed",
			wantLog:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			g := NewAllowlistGate(testAllowlist(), tt.mode, allowlistTestLogger(&buf))

			// Pure decision: reason + fail-closed correctness, mode-independent.
			d := g.decideField(tt.role, tt.field)
			if d.Reason != tt.wantReason {
				t.Errorf("decideField reason = %q, want %q", d.Reason, tt.wantReason)
			}
			wantDecisionAllowed := tt.wantReason == "allowed"
			if d.Allowed != wantDecisionAllowed {
				t.Errorf("decideField allowed = %v, want %v", d.Allowed, wantDecisionAllowed)
			}

			// Mode-applied gate result.
			got := g.AllowField(tt.role, tt.field)
			if got != tt.wantAllow {
				t.Errorf("AllowField = %v, want %v", got, tt.wantAllow)
			}

			logged := buf.String()
			if tt.wantLog {
				if logged == "" {
					t.Fatalf("expected a denial log line, got none")
				}
				// Criterion 2: rejection must be logged with role+field.
				if !strings.Contains(logged, "role="+loggedRole(tt.role)) {
					t.Errorf("log missing role: %s", logged)
				}
				if !strings.Contains(logged, "field="+string(tt.field)) {
					t.Errorf("log missing field: %s", logged)
				}
			} else if logged != "" {
				t.Errorf("expected no denial log, got: %s", logged)
			}
		})
	}
}

// loggedRole renders the role the way slog's TextHandler does (empty becomes a
// quoted empty string) so the assertion matches the emitted line.
func loggedRole(role string) string {
	if role == "" {
		return `""`
	}
	return role
}

func TestAllowlistGate_Comment_InternalVsPublic(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		vis        CommentVisibility
		wantAllow  bool
		wantReason string
	}{
		{
			name:       "broad role internal note allowed",
			role:       "crew",
			vis:        CommentInternal,
			wantAllow:  true,
			wantReason: "allowed",
		},
		{
			name:       "broad role public reply denied (no public grant)",
			role:       "crew",
			vis:        CommentPublic,
			wantAllow:  false,
			wantReason: "public comment not permitted for role",
		},
		{
			name:       "public-allowed role internal note allowed",
			role:       "support-public",
			vis:        CommentInternal,
			wantAllow:  true,
			wantReason: "allowed",
		},
		{
			name:       "public-allowed role public reply allowed",
			role:       "support-public",
			vis:        CommentPublic,
			wantAllow:  true,
			wantReason: "allowed",
		},
		{
			name:       "restricted role internal note allowed",
			role:       "restricted",
			vis:        CommentInternal,
			wantAllow:  true,
			wantReason: "allowed",
		},
		{
			name:       "restricted role public reply denied",
			role:       "restricted",
			vis:        CommentPublic,
			wantAllow:  false,
			wantReason: "public comment not permitted for role",
		},
		{
			name:       "unknown role any comment fails closed",
			role:       "intruder",
			vis:        CommentInternal,
			wantAllow:  false,
			wantReason: "unknown role",
		},
		{
			name:       "unknown visibility fails closed",
			role:       "crew",
			vis:        CommentVisibility("semi-public"),
			wantAllow:  false,
			wantReason: "unknown comment visibility",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			g := NewAllowlistGate(testAllowlist(), AllowlistModeEnforce, allowlistTestLogger(&buf))

			d := g.decideComment(tt.role, tt.vis)
			if d.Reason != tt.wantReason {
				t.Errorf("decideComment reason = %q, want %q", d.Reason, tt.wantReason)
			}
			if got := g.AllowComment(tt.role, tt.vis); got != tt.wantAllow {
				t.Errorf("AllowComment = %v, want %v", got, tt.wantAllow)
			}

			// Every public-comment denial must surface in the log with the role.
			if !tt.wantAllow {
				if logged := buf.String(); logged == "" || !strings.Contains(logged, "role="+tt.role) {
					t.Errorf("expected denial log with role, got: %q", logged)
				}
			}
		})
	}
}

// TestDefaultFieldAllowlist asserts the shipped default policy is conservative:
// broad work roles get all governed writes but internal-only comments, the
// restricted tier is internal-note-only, no role gets public comments, and
// unconfigured roles fail closed.
func TestDefaultFieldAllowlist(t *testing.T) {
	g := DefaultAllowlistGate(slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})))

	// crew (broad) may do all five governed writes...
	for _, f := range []JiraWriteField{FieldComment, FieldTransition, FieldLabels, FieldAssignee, FieldRemoteLink} {
		if !g.AllowField("crew", f) {
			t.Errorf("crew should be allowed field %q in default allowlist", f)
		}
	}
	// ...but never a public reply.
	if g.AllowComment("crew", CommentPublic) {
		t.Error("crew must NOT be allowed public comments by default")
	}
	if !g.AllowComment("crew", CommentInternal) {
		t.Error("crew should be allowed internal comments by default")
	}

	// restricted tier: internal note only, nothing else.
	if !g.AllowComment("restricted", CommentInternal) {
		t.Error("restricted should be allowed internal comments")
	}
	for _, f := range []JiraWriteField{FieldTransition, FieldLabels, FieldAssignee, FieldRemoteLink} {
		if g.AllowField("restricted", f) {
			t.Errorf("restricted must NOT be allowed field %q", f)
		}
	}
	if g.AllowComment("restricted", CommentPublic) {
		t.Error("restricted must NOT be allowed public comments")
	}

	// fsm-triage is the prod FSM triage role (ka-0000003103 / operator Option A).
	// Its writes route through the bridge as gardenio2, so it sits in the triage
	// tier: labels + versions ONLY — no comment (internal OR public), transition,
	// assignee, or remotelink.
	if !g.AllowField("fsm-triage", FieldLabels) {
		t.Error("fsm-triage should be allowed labels (triage tier)")
	}
	if !g.AllowField("fsm-triage", FieldVersions) {
		t.Error("fsm-triage should be allowed versions (triage tier)")
	}
	for _, f := range []JiraWriteField{FieldComment, FieldTransition, FieldAssignee, FieldRemoteLink} {
		if g.AllowField("fsm-triage", f) {
			t.Errorf("fsm-triage must NOT be allowed field %q (labels+versions only)", f)
		}
	}
	if g.AllowComment("fsm-triage", CommentInternal) {
		t.Error("fsm-triage must NOT be allowed comments (labels+versions only, no note surface)")
	}
	if g.AllowComment("fsm-triage", CommentPublic) {
		t.Error("fsm-triage must NEVER be allowed public comments")
	}

	// No default role may post a public comment (mirror contract).
	for _, role := range g.Roles() {
		if g.AllowComment(role, CommentPublic) {
			t.Errorf("default role %q must not be allowed public comments", role)
		}
	}

	// An unconfigured role fails closed on every field.
	for _, f := range []JiraWriteField{FieldComment, FieldTransition, FieldLabels, FieldAssignee, FieldRemoteLink} {
		if g.AllowField("nonexistent-role", f) {
			t.Errorf("unknown role must fail closed for field %q", f)
		}
	}
}

func TestAllowlistModeFromString(t *testing.T) {
	tests := []struct {
		in   string
		want AllowlistMode
	}{
		{"enforce", AllowlistModeEnforce},
		{"ENFORCE", AllowlistModeEnforce},
		{" enforce ", AllowlistModeEnforce},
		{"enforced", AllowlistModeEnforce},
		{"block", AllowlistModeEnforce},
		{"reject", AllowlistModeEnforce},
		{"shadow", AllowlistModeShadow},
		{"", AllowlistModeShadow},        // default is the safe rollout mode
		{"garbage", AllowlistModeShadow}, // a typo must NOT silently enforce
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := AllowlistModeFromString(tt.in); got != tt.want {
				t.Errorf("AllowlistModeFromString(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestAllowlistGate_NilAllowlistFailsClosed verifies a gate built over a nil
// allowlist denies everything in enforce mode (the safest failure).
func TestAllowlistGate_NilAllowlistFailsClosed(t *testing.T) {
	g := NewAllowlistGate(nil, AllowlistModeEnforce, allowlistTestLogger(&bytes.Buffer{}))
	if g.AllowField("crew", FieldComment) {
		t.Error("nil allowlist must deny all fields")
	}
	if g.AllowComment("crew", CommentInternal) {
		t.Error("nil allowlist must deny all comments")
	}
}
