package server

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Ownership-denial MESSAGE tests (ka-0000005214, motivated by ka-0000000798).
//
// The old 403 named only the roles that would satisfy the gate — "requires
// created_by, assignee, owner, or admin role" — and never the resource's
// actual created_by / operated_by. That opacity produced a full wasted
// investigation and a bug report that had to be retracted: the guard had
// refused CORRECTLY, because the decision under test was addressed to a
// different human, but nothing in the response said so, so a correct refusal
// was indistinguishable from a broken guard.
//
// These tests pin the fix: an ownership denial must name who the caller was
// resolved to and who the bead actually belongs to.

func TestOwnershipDenial_NamesResourceOwnerAndCaller(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})

	b := ownedBead("gd-msg1", "alice@example.com")
	b.OperatedBy = "slack:U0THEBOSS"

	d := srv.authorizeOwnership(ctx, "close", b)
	if d == nil {
		t.Fatal("non-owner should be denied")
	}

	// The human-readable message must name both sides.
	for _, want := range []string{
		"ownership denied",    // preserved prefix — other tests and runbooks match on it
		"mallory@example.com", // who you are
		"alice@example.com",   // who created it
		"slack:U0THEBOSS",     // who it is addressed to — the ka-0000000798 miss
		"created_by=",
		"operated_by=",
	} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("denial message must contain %q, got: %s", want, d.Message)
		}
	}

	// The advisory text an agent is told to read must name them too.
	if !strings.Contains(d.AdvisoryText, "alice@example.com") ||
		!strings.Contains(d.AdvisoryText, "slack:U0THEBOSS") {
		t.Errorf("advisory text must name the actual owner, got: %s", d.AdvisoryText)
	}

	// Structured fields, for machine consumers of the 403 body.
	if d.CallerSubject != "mallory@example.com" {
		t.Errorf("CallerSubject = %q, want mallory@example.com", d.CallerSubject)
	}
	if d.ResourceCreatedBy != "alice@example.com" {
		t.Errorf("ResourceCreatedBy = %q, want alice@example.com", d.ResourceCreatedBy)
	}
	if d.ResourceOperatedBy != "slack:U0THEBOSS" {
		t.Errorf("ResourceOperatedBy = %q, want slack:U0THEBOSS", d.ResourceOperatedBy)
	}
}

// TestOwnershipDenial_DistinguishesUnsetFromMismatched is the specific
// confusion ka-0000000798 hit: "you are not the owner" reads the same whether
// the bead has a different owner or no owner at all. The message must say
// which fields are unset.
func TestOwnershipDenial_DistinguishesUnsetFromMismatched(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})

	b := ownedBead("gd-msg2", "alice@example.com") // assignee/owner/operated_by all empty
	d := srv.authorizeOwnership(ctx, "update", b)
	if d == nil {
		t.Fatal("non-owner should be denied")
	}
	if !strings.Contains(d.Message, "unset") {
		t.Errorf("message should mark the empty ownership fields as unset, got: %s", d.Message)
	}
	if strings.Contains(d.Message, `operated_by=""`) {
		t.Errorf("empty fields must not be rendered as empty-string values, got: %s", d.Message)
	}
}

// TestDescribeBeadOwnership covers the renderer directly, including the
// all-empty case. A bead with no ownership metadata at all is normally skipped
// by authorizeOwnership (the legacy carve-out), so this branch is only
// reachable through the helper — pin it so the message never degrades to a row
// of empty quotes.
func TestDescribeBeadOwnership(t *testing.T) {
	tests := []struct {
		name string
		bead *model.Bead
		want []string
	}{
		{
			name: "all set",
			bead: &model.Bead{
				CreatedBy: "a", Assignee: "b", Owner: "c", OperatedBy: "d",
			},
			want: []string{`created_by="a"`, `assignee="b"`, `owner="c"`, `operated_by="d"`},
		},
		{
			name: "none set",
			bead: &model.Bead{},
			want: []string{"owned by nobody"},
		},
		{
			name: "partial",
			bead: &model.Bead{CreatedBy: "a"},
			want: []string{`created_by="a"`, "assignee, owner, operated_by unset"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := describeBeadOwnership(tc.bead)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("describeBeadOwnership() = %q, want it to contain %q", got, w)
				}
			}
		})
	}
}

// TestQuoteIdentity pins the no-identity rendering so an anonymous caller does
// not produce the confusing `you are ""`.
func TestQuoteIdentity(t *testing.T) {
	if got := quoteIdentity(""); got != "(no identity)" {
		t.Errorf("quoteIdentity(\"\") = %q, want (no identity)", got)
	}
	if got := quoteIdentity("bridge:jira"); got != `"bridge:jira"` {
		t.Errorf("quoteIdentity = %q", got)
	}
}
