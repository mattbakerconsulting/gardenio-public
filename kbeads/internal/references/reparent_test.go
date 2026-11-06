package references

import (
	"testing"
)

func TestRewriteParentMention(t *testing.T) {
	const (
		oldP = "gd-7q7xzPRSPX"
		newP = "gd-OvZUSDFSzm"
		self = "gd-gJuadOKhm0"
	)

	tests := []struct {
		name        string
		in          string
		wantText    string
		wantRewrote int
	}{
		{
			name:        "colon form rewritten",
			in:          "parent: " + oldP,
			wantText:    "parent: " + newP,
			wantRewrote: 1,
		},
		{
			name:        "call form rewritten",
			in:          "fixes parent(" + oldP + ") today",
			wantText:    "fixes parent(" + newP + ") today",
			wantRewrote: 1,
		},
		{
			name:        "bulleted colon form preserves bullet",
			in:          "- parent: " + oldP,
			wantText:    "- parent: " + newP,
			wantRewrote: 1,
		},
		{
			name:        "asterisk bullet preserved",
			in:          "  * parent:   " + oldP,
			wantText:    "  * parent:   " + newP,
			wantRewrote: 1,
		},
		{
			name:        "case-insensitive verb in call form",
			in:          "Parent(" + oldP + ")",
			wantText:    "Parent(" + newP + ")",
			wantRewrote: 1,
		},
		{
			name:        "inverse child-of colon form rewritten",
			in:          "child-of: " + oldP,
			wantText:    "child-of: " + newP,
			wantRewrote: 1,
		},
		{
			name:        "inverse child-of call form rewritten",
			in:          "note child-of(" + oldP + ") here",
			wantText:    "note child-of(" + newP + ") here",
			wantRewrote: 1,
		},
		{
			name:        "mixed parent and child-of of same old parent",
			in:          "parent: " + oldP + "\nchild-of(" + oldP + ")",
			wantText:    "parent: " + newP + "\nchild-of(" + newP + ")",
			wantRewrote: 2,
		},
		{
			name:        "both forms in one text",
			in:          "parent: " + oldP + "\nsee also parent(" + oldP + ")",
			wantText:    "parent: " + newP + "\nsee also parent(" + newP + ")",
			wantRewrote: 2,
		},
		{
			name:        "parent mention of a different bead untouched",
			in:          "parent: gd-oAAMSc5xDk",
			wantText:    "parent: gd-oAAMSc5xDk",
			wantRewrote: 0,
		},
		{
			name:        "related mention of old parent untouched",
			in:          "related: " + oldP,
			wantText:    "related: " + oldP,
			wantRewrote: 0,
		},
		{
			name:        "blocks mention of old parent untouched",
			in:          "blocks: " + oldP,
			wantText:    "blocks: " + oldP,
			wantRewrote: 0,
		},
		{
			name:        "bare mention untouched",
			in:          "see " + oldP + " for context",
			wantText:    "see " + oldP + " for context",
			wantRewrote: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RewriteParentMention(tt.in, oldP, newP)
			if got.Text != tt.wantText {
				t.Errorf("Text:\n got %q\nwant %q", got.Text, tt.wantText)
			}
			if got.Rewrote != tt.wantRewrote {
				t.Errorf("Rewrote = %d, want %d", got.Rewrote, tt.wantRewrote)
			}
		})
	}

	// Self-parent guard: a no-op is expected when old == new.
	if got := RewriteParentMention("parent: "+self, self, self); got.Rewrote != 0 {
		t.Errorf("old==new should be a no-op, got Rewrote=%d", got.Rewrote)
	}
}

func TestHasRelatedMention(t *testing.T) {
	const target = "gd-8ugSzVZImV"
	cases := []struct {
		in   string
		want bool
	}{
		{"related: " + target, true},
		{"see related(" + target + ") here", true},
		{"parent: " + target, false},      // parent, not related
		{"related: gd-oAAMSc5xDk", false}, // related but different target
		{"just prose " + target, false},   // bare mention, not typed
		{"", false},
	}
	for _, c := range cases {
		if got := HasRelatedMention(c.in, target); got != c.want {
			t.Errorf("HasRelatedMention(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestAppendRelatedMention(t *testing.T) {
	const target = "gd-8ugSzVZImV"

	// Appends onto non-empty text with a blank-line separator.
	if got := AppendRelatedMention("parent: gd-OvZUSDFSzm", target); got != "parent: gd-OvZUSDFSzm\n\nrelated: "+target {
		t.Errorf("append onto text: got %q", got)
	}
	// Empty text yields just the related line (no leading newlines).
	if got := AppendRelatedMention("", target); got != "related: "+target {
		t.Errorf("append onto empty: got %q", got)
	}
	// Trailing newlines are normalized, not stacked.
	if got := AppendRelatedMention("body\n\n", target); got != "body\n\nrelated: "+target {
		t.Errorf("append normalizes trailing newlines: got %q", got)
	}
	// The appended line round-trips through the parser as a related mention.
	if !HasRelatedMention(AppendRelatedMention("x", target), target) {
		t.Errorf("appended related line did not parse as a related mention")
	}
}

// TestRewriteParentMention_NoResurrection is the core acceptance test for
// ka-0000007756: after rewriting, ExtractTyped (the exact parser the write
// path runs on every update via replaceBeadReferences) must NOT surface ANY
// parent-EDGE typed mention of the old parent — neither `parent` NOR the
// inverse `child-of`, since both materialize the same parent-child dep row —
// so a subsequent description write cannot re-materialize the old edge.
func TestRewriteParentMention_NoResurrection(t *testing.T) {
	const (
		oldP = "gd-7q7xzPRSPX"
		newP = "gd-OvZUSDFSzm"
	)
	inputs := []string{
		"parent: " + oldP,
		"fixes parent(" + oldP + ")",
		"- parent: " + oldP + "\nsome prose\nparent(" + oldP + ") inline",
		"Root cause blah.\n\nparent: " + oldP + "\nrelated: gd-EAaC6mP1FC",
		// Inverse child-of forms — the resurrection footgun the child-of
		// generalization (ka-0000007756 review) closes. These materialize the
		// identical parent-child dep row as `parent`.
		"child-of: " + oldP,
		"was child-of(" + oldP + ") before",
		"parent: " + oldP + "\nchild-of(" + oldP + ")",
	}
	for _, in := range inputs {
		got := RewriteParentMention(in, oldP, newP)

		// The rewritten text must have NO parent-edge mention of the old parent.
		for _, ref := range ExtractTyped(got.Text) {
			if IsParentEdgeVerb(ref.Verb) && ref.TargetID == oldP {
				t.Errorf("resurrection risk: rewritten text still yields %s->%s\ninput:  %q\noutput: %q",
					ref.Verb, oldP, in, got.Text)
			}
		}

		// And it MUST yield a parent-edge mention of the new parent (proving the
		// edge was moved, not just deleted).
		var sawNewParent bool
		for _, ref := range ExtractTyped(got.Text) {
			if IsParentEdgeVerb(ref.Verb) && ref.TargetID == newP {
				sawNewParent = true
			}
		}
		if !sawNewParent {
			t.Errorf("expected a parent-edge mention of %s after rewrite\ninput:  %q\noutput: %q",
				newP, in, got.Text)
		}

		// Idempotency: a second pass changes nothing.
		second := RewriteParentMention(got.Text, oldP, newP)
		if second.Text != got.Text {
			t.Errorf("second pass not idempotent:\n first  %q\n second %q", got.Text, second.Text)
		}
		if second.Rewrote != 0 {
			t.Errorf("second pass Rewrote = %d, want 0", second.Rewrote)
		}
	}
}

// TestParentEdgeTargetsExcept covers the residual-scan helper that both drives
// the rewrite target list and backs `kd reparent`'s post-rewrite guard.
func TestParentEdgeTargetsExcept(t *testing.T) {
	const (
		a    = "gd-RlgZWBIFlj"
		b    = "gd-inDyGfsHhr"
		newP = "gd-OvZUSDFSzm"
	)
	got := ParentEdgeTargetsExcept(
		"parent: "+a+"\nchild-of("+b+")\nparent: "+newP+"\nrelated: gd-otherX00\nblocks: gd-blockY00",
		newP,
	)
	// Both a (parent) and b (child-of) are parent-edge targets; newP is
	// excluded; related/blocks are not parent edges.
	want := map[string]bool{a: true, b: true}
	if len(got) != 2 {
		t.Fatalf("got %v, want exactly {%s, %s}", got, a, b)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected target %s in %v", id, got)
		}
	}
	// No parent-edge targets in a related-only text.
	if r := ParentEdgeTargetsExcept("related: "+a, newP); len(r) != 0 {
		t.Errorf("related-only text yielded targets: %v", r)
	}
}
