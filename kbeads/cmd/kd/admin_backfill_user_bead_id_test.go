package main

import "testing"

func TestResolveOperatorLookup(t *testing.T) {
	tests := []struct {
		name        string
		operatedBy  string
		joinedEmail string
		wantEmail   string
		wantSlack   string
	}{
		{
			name:        "joined email wins",
			operatedBy:  "slack:U123",
			joinedEmail: "matt@acme.example",
			wantEmail:   "matt@acme.example",
		},
		{
			name:       "oidc → email",
			operatedBy: "oidc:matt@acme.example",
			wantEmail:  "matt@acme.example",
		},
		{
			name:       "gitlab → email",
			operatedBy: "gitlab:dev@acme.example",
			wantEmail:  "dev@acme.example",
		},
		{
			name:       "jira → email",
			operatedBy: "jira:dev@acme.example",
			wantEmail:  "dev@acme.example",
		},
		{
			name:       "slack → slack id",
			operatedBy: "slack:U12345",
			wantSlack:  "U12345",
		},
		{
			name:       "system sentinel → unresolvable",
			operatedBy: "system:scheduler",
		},
		{
			name:       "fleet sentinel → unresolvable",
			operatedBy: "fleet:gardenio",
		},
		{
			name:       "no prefix → unresolvable",
			operatedBy: "matt@acme.example",
		},
		{
			name:       "empty → unresolvable",
			operatedBy: "",
		},
		{
			name:       "empty subject → unresolvable",
			operatedBy: "oidc:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotEmail, gotSlack := resolveOperatorLookup(tt.operatedBy, tt.joinedEmail)
			if gotEmail != tt.wantEmail {
				t.Errorf("email = %q, want %q", gotEmail, tt.wantEmail)
			}
			if gotSlack != tt.wantSlack {
				t.Errorf("slack = %q, want %q", gotSlack, tt.wantSlack)
			}
		})
	}
}
