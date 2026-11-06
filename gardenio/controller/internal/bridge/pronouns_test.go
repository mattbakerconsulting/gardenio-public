package bridge

import "testing"

func TestParsePronouns(t *testing.T) {
	tests := []struct {
		input string
		want  PronounSet
	}{
		{"he/him", PronounSet{"he", "him", "his"}},
		{"she/her", PronounSet{"she", "her", "her"}},
		{"they/them", PronounSet{"they", "them", "their"}},
		{"ze/zir", PronounSet{"ze", "zir", "zir"}},
		{"xe/xem", PronounSet{"xe", "xem", "xyr"}},

		// Compound — use first listed.
		{"he/they", PronounSet{"he", "him", "his"}},
		{"she/they", PronounSet{"she", "her", "her"}},

		// Three-part format.
		{"she/her/hers", PronounSet{"she", "her", "her"}},

		// Case insensitive.
		{"He/Him", PronounSet{"he", "him", "his"}},
		{"SHE/HER", PronounSet{"she", "her", "her"}},
		{"They/Them", PronounSet{"they", "them", "their"}},

		// Whitespace tolerance.
		{"  he/him  ", PronounSet{"he", "him", "his"}},

		// Empty or unknown → default they/them.
		{"", PronounSet{"they", "them", "their"}},
		{"unknown", PronounSet{"they", "them", "their"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParsePronouns(tt.input)
			if got != tt.want {
				t.Errorf("ParsePronouns(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestUserIdentity_PronounMethods(t *testing.T) {
	u := &UserIdentity{Pronouns: "she/her"}
	if got := u.SubjectPronoun(); got != "she" {
		t.Errorf("SubjectPronoun() = %q, want %q", got, "she")
	}
	if got := u.ObjectPronoun(); got != "her" {
		t.Errorf("ObjectPronoun() = %q, want %q", got, "her")
	}
	if got := u.PossessivePronoun(); got != "her" {
		t.Errorf("PossessivePronoun() = %q, want %q", got, "her")
	}
}

func TestUserIdentity_PronounMethods_Empty(t *testing.T) {
	u := &UserIdentity{}
	if got := u.SubjectPronoun(); got != "they" {
		t.Errorf("SubjectPronoun() = %q, want %q", got, "they")
	}
	if got := u.ObjectPronoun(); got != "them" {
		t.Errorf("ObjectPronoun() = %q, want %q", got, "them")
	}
	if got := u.PossessivePronoun(); got != "their" {
		t.Errorf("PossessivePronoun() = %q, want %q", got, "their")
	}
}

func TestAppendPronouns(t *testing.T) {
	tests := []struct {
		name    string
		nameArg string
		ident   *UserIdentity
		want    string
	}{
		{"nil identity returns name unchanged", "Sam", nil, "Sam"},
		{"empty pronouns returns name unchanged", "Sam", &UserIdentity{}, "Sam"},
		{"whitespace pronouns returns name unchanged", "Sam", &UserIdentity{Pronouns: "   "}, "Sam"},
		{"appends pronouns when set", "Sam", &UserIdentity{Pronouns: "she/her"}, "Sam (she/her)"},
		{"trims pronouns whitespace", "Sam", &UserIdentity{Pronouns: "  they/them  "}, "Sam (they/them)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appendPronouns(tt.nameArg, tt.ident); got != tt.want {
				t.Errorf("appendPronouns(%q, %+v) = %q, want %q", tt.nameArg, tt.ident, got, tt.want)
			}
		})
	}
}
