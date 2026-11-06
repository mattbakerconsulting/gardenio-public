package bridge

import "testing"

func TestDeriveCardSubject_PriorityOrder(t *testing.T) {
	cases := []struct {
		name string
		in   cardSubjectInputs
		want string
	}{
		{
			name: "tier1 ticket key in trigger text wins over everything",
			in: cardSubjectInputs{
				TriggerText: "Please triage FSM-1234 from the service desk",
				JiraKey:     "PE-9999",
				BeadID:      "gd-abc123",
				Slug:        "thread-1780064144-940679",
			},
			want: "FSM-1234",
		},
		{
			name: "tier1 ticket key in prompt when trigger text empty",
			in: cardSubjectInputs{
				Prompt: "Fix the bug described in PE-7083 and open an MR",
				BeadID: "gd-abc123",
				Slug:   "thread-1",
			},
			want: "PE-7083",
		},
		{
			name: "tier1 trigger text beats prompt",
			in: cardSubjectInputs{
				TriggerText: "DEVOPS-42 deploy",
				Prompt:      "see PE-7083",
			},
			want: "DEVOPS-42",
		},
		{
			name: "tier2 jira key when no ticket key in text",
			in: cardSubjectInputs{
				TriggerText: "no ticket here",
				Prompt:      "generic prompt with no key",
				JiraKey:     "PE-7083",
				BeadID:      "gd-abc123",
				Slug:        "thread-1",
			},
			want: "PE-7083",
		},
		{
			name: "tier3 bead id when no ticket key or jira key",
			in: cardSubjectInputs{
				Prompt: "do some work",
				BeadID: "gd-bniGXhp6BB",
				Slug:   "thread-1780064144-940679",
			},
			want: "gd-bniGXhp6BB",
		},
		{
			name: "tier4 slug fallback when nothing else present",
			in: cardSubjectInputs{
				Prompt: "do some work with no identifiers",
				Slug:   "thread-1780064144-940679",
			},
			want: "thread-1780064144-940679",
		},
		{
			name: "jira key whitespace trimmed",
			in: cardSubjectInputs{
				JiraKey: "  PE-100  ",
				Slug:    "agent-x",
			},
			want: "PE-100",
		},
		{
			name: "empty everything yields empty",
			in:   cardSubjectInputs{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveCardSubject(tc.in); got != tc.want {
				t.Errorf("deriveCardSubject(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFirstTicketKey(t *testing.T) {
	cases := []struct {
		name  string
		texts []string
		want  string
	}{
		{"FSM key", []string{"triage FSM-1234 now"}, "FSM-1234"},
		{"PE key", []string{"bug fix PE-7083"}, "PE-7083"},
		{"DEVOPS key", []string{"DEVOPS-42 pipeline"}, "DEVOPS-42"},
		{"first of multiple", []string{"PE-1 then FSM-2"}, "PE-1"},
		{"scans texts in order", []string{"no key here", "found PE-9"}, "PE-9"},
		{"no key", []string{"just words, no tickets"}, ""},
		{"lowercase prefix not matched", []string{"pe-7083 lowercase"}, ""},
		{"single-letter prefix not matched", []string{"A-1 too short"}, ""},
		{"key embedded in URL", []string{"https://x.atlassian.net/browse/PE-7083"}, "PE-7083"},
		{"empty input", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstTicketKey(tc.texts...); got != tc.want {
				t.Errorf("firstTicketKey(%v) = %q, want %q", tc.texts, got, tc.want)
			}
		})
	}
}
