package server

import (
	"context"
	"testing"
)

// TestCanonicalizeHumanOperator pins the canonicalization contract
// (ka-0000002687): oidc emails collapse to lower case so a case variant can
// never fork a duplicate operator, while slack ids, sentinels, and every
// non-oidc value pass through byte-for-byte. Idempotency is asserted so the
// helper is safe to call at both the header boundary and the resolver wrapper.
func TestCanonicalizeHumanOperator(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"oidc mixed case → lower", "oidc:Matthew.Baker@acme.example", "oidc:matthew.baker@acme.example"},
		{"oidc all upper → lower", "oidc:MATT@acme.example", "oidc:matt@acme.example"},
		{"oidc already canonical", "oidc:matthew.baker@acme.example", "oidc:matthew.baker@acme.example"},
		{"slack untouched (uppercase id preserved)", "slack:U0OPERATOR1", "slack:U0OPERATOR1"},
		{"system sentinel untouched", "system:machine", "system:machine"},
		{"fleet sentinel untouched", "fleet:slack-bridge", "fleet:slack-bridge"},
		{"empty untouched", "", ""},
		{"bare value without prefix untouched", "matthew.baker@acme.example", "matthew.baker@acme.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalizeHumanOperator(tc.in)
			if got != tc.want {
				t.Fatalf("canonicalizeHumanOperator(%q) = %q; want %q", tc.in, got, tc.want)
			}
			// Idempotent: a second pass must not change an already-canonical value.
			if again := canonicalizeHumanOperator(got); again != got {
				t.Fatalf("not idempotent: canonicalizeHumanOperator(%q) = %q", got, again)
			}
		})
	}
}

// TestSanitizeOperatedByHeader_CanonicalizesOIDC proves the sanitize region —
// the ROOT CAUSE site named in ka-0000002687 — hands back the canonical form,
// so a case-variant oidc header lands on one operator instead of forking a
// duplicate. Slack headers are accepted verbatim.
func TestSanitizeOperatedByHeader_CanonicalizesOIDC(t *testing.T) {
	if got, ok := sanitizeOperatedByHeader("oidc:Matthew.Baker@acme.example"); !ok || got != "oidc:matthew.baker@acme.example" {
		t.Fatalf("sanitize oidc = (%q, %v); want (oidc:matthew.baker@acme.example, true)", got, ok)
	}
	if got, ok := sanitizeOperatedByHeader("slack:U0OPERATOR1"); !ok || got != "slack:U0OPERATOR1" {
		t.Fatalf("sanitize slack = (%q, %v); want (slack:U0OPERATOR1, true)", got, ok)
	}
	// Non-human shapes are still rejected — canonicalization runs only after
	// the isHumanOperator gate, so it cannot smuggle a sentinel through.
	if _, ok := sanitizeOperatedByHeader("system:machine"); ok {
		t.Fatalf("sanitize accepted a sentinel; want rejected")
	}
}

// TestResolveOperator_CaseVariantOIDC_CollapsesToOneOperator is the alias
// resolution stub case (ka-0000002687 criterion 3): once the write path
// canonicalizes, two case-variant oidc identities resolve to the SAME operator
// id rather than cold-path-forking two rows — the fragmentation this task ends.
func TestResolveOperator_CaseVariantOIDC_CollapsesToOneOperator(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)
	ctx := context.Background()

	idA, okA := srv.resolveOperator(ctx, canonicalizeHumanOperator("oidc:Matthew.Baker@acme.example"))
	idB, okB := srv.resolveOperator(ctx, canonicalizeHumanOperator("oidc:matthew.baker@acme.example"))
	if !okA || !okB {
		t.Fatalf("resolveOperator ok flags: A=%v B=%v", okA, okB)
	}
	if idA == 0 || idB == 0 {
		t.Fatalf("expected non-zero ids; got idA=%d idB=%d", idA, idB)
	}
	if idA != idB {
		t.Fatalf("case-variant oidc identities forked distinct operators %d != %d", idA, idB)
	}
}

// TestResolveOperator_CaseVariantGitLab_CollapsesToOneOperator: the bridge's
// "gitlab:<username>" fallback carries the login as GitLab spelled it, while a
// user bead links the lowercased alias. resolveOperator folds the subject, so
// both spellings land on one operator (gd-9AXizUgtzG).
func TestResolveOperator_CaseVariantGitLab_CollapsesToOneOperator(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)
	ctx := context.Background()

	idA, okA := srv.resolveOperator(ctx, "gitlab:JoyousAversion")
	idB, okB := srv.resolveOperator(ctx, "gitlab:joyousaversion")
	if !okA || !okB || idA == 0 {
		t.Fatalf("resolveOperator: A=(%d,%v) B=(%d,%v)", idA, okA, idB, okB)
	}
	if idA != idB {
		t.Fatalf("case-variant gitlab logins forked distinct operators %d != %d", idA, idB)
	}
}
