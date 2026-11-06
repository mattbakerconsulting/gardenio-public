package references

import (
	"reflect"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestExtractTyped(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []TypedReference
	}{
		{
			name: "empty string",
			in:   "",
			want: nil,
		},
		{
			name: "plain text no matches",
			in:   "this is some prose with no bead references at all",
			want: nil,
		},
		{
			name: "bare gd-id without verb is not returned",
			in:   "see gd-iMrpT42pGz for more",
			want: nil,
		},
		{
			name: "call form parent",
			in:   "fixes parent(gd-iMrpT42pGz) today",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent(gd-iMrpT42pGz)", Position: 6},
			},
		},
		{
			name: "colon form on its own line",
			in:   "parent: gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "colon form with hyphen bullet",
			in:   "- parent: gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "- parent: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "colon form with asterisk bullet",
			in:   "* parent: gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "* parent: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "multiple call forms in one text",
			in:   "parent(gd-iMrpT42pGz) and blocks(gd-n5F2I5gzEp) too",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent(gd-iMrpT42pGz)", Position: 0},
				{TargetID: "gd-n5F2I5gzEp", Verb: "blocks", Raw: "blocks(gd-n5F2I5gzEp)", Position: 26},
			},
		},
		{
			name: "uppercase canonical verb in call form lowercased",
			in:   "Parent(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "Parent(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "uppercase canonical verb in colon form lowercased",
			in:   "Parent: gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "Parent: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "inverse verb child-of",
			in:   "child-of(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "child-of", Raw: "child-of(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "inverse verb blocked-by",
			in:   "blocked-by(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "blocked-by", Raw: "blocked-by(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "inverse verb duplicated-by",
			in:   "duplicated-by(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "duplicated-by", Raw: "duplicated-by(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "inverse verb superseded-by",
			in:   "superseded-by(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "superseded-by", Raw: "superseded-by(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "verb related",
			in:   "related(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "related", Raw: "related(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "verb duplicates",
			in:   "duplicates(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "duplicates", Raw: "duplicates(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "verb supersedes",
			in:   "supersedes(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "supersedes", Raw: "supersedes(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "verb triggered-by call form",
			in:   "triggered-by(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "triggered-by", Raw: "triggered-by(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "verb triggered-by colon form bulleted",
			in:   "- triggered-by: gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "triggered-by", Raw: "- triggered-by: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "inverse verb triggers",
			in:   "triggers(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "triggers", Raw: "triggers(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "chains_to forward verb (snake_case) parses",
			in:   "chains_to(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "chains_to", Raw: "chains_to(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "chains-to forward verb (hyphen alias) parses",
			in:   "chains-to(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "chains-to", Raw: "chains-to(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "succeeds inverse verb in colon form parses (provenance stamp)",
			in:   "- succeeds: gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "succeeds", Raw: "- succeeds: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "unknown verb in call form degrades to flat with raw_verb",
			in:   "closes(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "", RawVerb: "closes", Raw: "closes(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "unknown verb in colon form degrades to flat with raw_verb",
			in:   "fixes: gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "", RawVerb: "fixes", Raw: "fixes: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "unknown verb preserves original case in raw_verb",
			in:   "Tracks(gd-iMrpT42pGz)",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "", RawVerb: "Tracks", Raw: "Tracks(gd-iMrpT42pGz)", Position: 0},
			},
		},
		{
			name: "whitespace inside parens accepted",
			in:   "parent( gd-iMrpT42pGz )",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent( gd-iMrpT42pGz )", Position: 0},
			},
		},
		{
			name: "mixed call and colon in one document",
			in:   "intro parent(gd-iMrpT42pGz) text\n- blocks: gd-n5F2I5gzEp\nend",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent(gd-iMrpT42pGz)", Position: 6},
				{TargetID: "gd-n5F2I5gzEp", Verb: "blocks", Raw: "- blocks: gd-n5F2I5gzEp", Position: 33},
			},
		},
		{
			name: "mid-sentence colon does not match verb-colon form",
			in:   "we will eventually parent: gd-iMrpT42pGz this task",
			want: nil,
		},
		{
			name: "markdown header does not match colon form",
			in:   "## Parent: gd-iMrpT42pGz",
			want: nil,
		},
		{
			name: "trailing punctuation after colon-form id is fine",
			in:   "parent: gd-iMrpT42pGz.",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "parens with no verb prefix do not match",
			in:   "see (gd-iMrpT42pGz) here",
			want: nil,
		},
		{
			name: "very long verb (>32 chars) does not match",
			in:   "thisisaveryveryveryverylongverbtokenname(gd-iMrpT42pGz)",
			want: nil,
		},
		{
			name: "duplicate matches preserved with separate positions",
			in:   "parent(gd-iMrpT42pGz) and parent(gd-iMrpT42pGz) again",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent(gd-iMrpT42pGz)", Position: 0},
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent(gd-iMrpT42pGz)", Position: 26},
			},
		},
		{
			name: "multiline list of relations all parsed",
			in:   "- parent: gd-iMrpT42pGz\n- blocks: gd-n5F2I5gzEp\n- related: gd-xfshRmkdC5",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "- parent: gd-iMrpT42pGz", Position: 0},
				{TargetID: "gd-n5F2I5gzEp", Verb: "blocks", Raw: "- blocks: gd-n5F2I5gzEp", Position: 24},
				{TargetID: "gd-xfshRmkdC5", Verb: "related", Raw: "- related: gd-xfshRmkdC5", Position: 48},
			},
		},
		{
			name: "no-space-after-bullet does not match",
			in:   "-parent: gd-iMrpT42pGz",
			want: nil,
		},
		{
			name: "CRLF line endings parsed",
			in:   "intro\r\nparent: gd-iMrpT42pGz\r\nend",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent: gd-iMrpT42pGz", Position: 7},
			},
		},
		{
			name: "multi-target call form not supported (only one id per parens)",
			in:   "parent(gd-iMrpT42pGz, gd-n5F2I5gzEp)",
			want: nil,
		},
		{
			name: "empty parens do not match",
			in:   "parent()",
			want: nil,
		},
		{
			name: "unclosed parens do not match",
			in:   "parent(gd-iMrpT42pGz",
			want: nil,
		},
		{
			name: "all-digit target id captured",
			in:   "parent(gd-UxaO9Bh6Zc)",
			want: []TypedReference{
				{TargetID: "gd-UxaO9Bh6Zc", Verb: "parent", Raw: "parent(gd-UxaO9Bh6Zc)", Position: 0},
			},
		},
		{
			name: "case in target id preserved",
			in:   "parent(ka-0000004973)",
			want: []TypedReference{
				{TargetID: "ka-0000004973", Verb: "parent", Raw: "parent(ka-0000004973)", Position: 0},
			},
		},
		{
			name: "inverse verb child-of in colon form",
			in:   "- child-of: gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "child-of", Raw: "- child-of: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "verb without space before colon",
			in:   "parent:gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "parent:gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "indented colon form",
			in:   "  parent: gd-iMrpT42pGz",
			want: []TypedReference{
				{TargetID: "gd-iMrpT42pGz", Verb: "parent", Raw: "  parent: gd-iMrpT42pGz", Position: 0},
			},
		},
		{
			name: "colon form requires gd-id immediately after colon",
			in:   "parent: see gd-iMrpT42pGz",
			want: nil,
		},
		{
			name: "call and colon at same offset prefer document order",
			in:   "blocks(gd-HGdJK5HmG2)\nparent: gd-N0lFD1jzHZ",
			want: []TypedReference{
				{TargetID: "gd-HGdJK5HmG2", Verb: "blocks", Raw: "blocks(gd-HGdJK5HmG2)", Position: 0},
				{TargetID: "gd-N0lFD1jzHZ", Verb: "parent", Raw: "parent: gd-N0lFD1jzHZ", Position: 22},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractTyped(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractTyped(%q)\n got: %#v\nwant: %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonicalVerb(t *testing.T) {
	tests := []struct {
		verb    string
		want    model.DependencyType
		wantOK  bool
		comment string
	}{
		{"parent", model.DepParentChild, true, "forward parent"},
		{"child-of", model.DepParentChild, true, "inverse parent"},
		{"blocks", model.DepBlocks, true, "forward blocks"},
		{"blocked-by", model.DepBlocks, true, "inverse blocks"},
		{"related", model.DepRelated, true, "related"},
		{"duplicates", model.DepDuplicates, true, "forward duplicates"},
		{"duplicated-by", model.DepDuplicates, true, "inverse duplicates"},
		{"supersedes", model.DepSupersedes, true, "forward supersedes"},
		{"superseded-by", model.DepSupersedes, true, "inverse supersedes"},
		{"triggered-by", model.DepTriggeredBy, true, "forward triggered-by"},
		{"triggers", model.DepTriggeredBy, true, "inverse triggers"},
		{"chains_to", model.DepChainsTo, true, "forward chains_to (snake_case)"},
		{"chains-to", model.DepChainsTo, true, "forward chains-to (hyphen alias)"},
		{"succeeds", model.DepChainsTo, true, "inverse succeeds"},
		{"Chains_To", model.DepChainsTo, true, "case insensitive chains_to"},
		{"Triggered-By", model.DepTriggeredBy, true, "case insensitive triggered-by"},
		{"Parent", model.DepParentChild, true, "case insensitive"},
		{"closes", "", false, "unknown verb"},
		{"fixes", "", false, "unknown verb"},
		{"", "", false, "empty verb"},
	}
	for _, tt := range tests {
		t.Run(tt.comment, func(t *testing.T) {
			got, ok := CanonicalVerb(tt.verb)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("CanonicalVerb(%q) = (%q, %v), want (%q, %v)",
					tt.verb, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
