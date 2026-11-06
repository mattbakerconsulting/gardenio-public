package main

// Unit tests for isBedrockBackedAccount — the predicate that filters
// bedrock-fronted accounts out of the Anthropic rate-limit probe loop.
//
// Regression coverage for ka-0000003306: the litellm-bedrock account
// emitted a WARN every probe interval ("unexpected status 401") because
// its secret token is not a valid api.anthropic.com OAuth token.

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestIsBedrockBackedAccount(t *testing.T) {
	cases := []struct {
		name string
		acct beadsapi.AccountInfo
		want bool
	}{
		{
			name: "bedrock model field",
			acct: beadsapi.AccountInfo{
				Model:  "bedrock-claude-opus-4-7",
				Fields: map[string]string{},
			},
			want: true,
		},
		{
			name: "bedrock probe_model field",
			acct: beadsapi.AccountInfo{
				Model:  "",
				Fields: map[string]string{"probe_model": "bedrock-claude-haiku-4-5"},
			},
			want: true,
		},
		{
			name: "anthropic-native model",
			acct: beadsapi.AccountInfo{
				Model:  "claude-opus-4-7-oauth",
				Fields: map[string]string{"probe_model": "anthropic/claude-haiku-4-5"},
			},
			want: false,
		},
		{
			name: "empty model and fields",
			acct: beadsapi.AccountInfo{
				Fields: map[string]string{},
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBedrockBackedAccount(tc.acct); got != tc.want {
				t.Errorf("isBedrockBackedAccount = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsDisabledAccount covers the predicate that filters status=disabled
// account beads out of the probe loop. Regression coverage for ka-0000005148:
// two account beads that alias the same upstream OAuth credential probed
// the upstream account twice every interval; the disabled alias was meant
// to be inert but the probe loop ignored its status.
func TestIsDisabledAccount(t *testing.T) {
	cases := []struct {
		name string
		acct beadsapi.AccountInfo
		want bool
	}{
		{
			name: "status=disabled is skipped",
			acct: beadsapi.AccountInfo{Status: "disabled"},
			want: true,
		},
		{
			name: "status=active is probed",
			acct: beadsapi.AccountInfo{Status: "active"},
			want: false,
		},
		{
			name: "empty status defaults to probed",
			acct: beadsapi.AccountInfo{},
			want: false,
		},
		{
			name: "status=exhausted still probed (not disabled)",
			acct: beadsapi.AccountInfo{Status: "exhausted"},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDisabledAccount(tc.acct); got != tc.want {
				t.Errorf("isDisabledAccount = %v, want %v", got, tc.want)
			}
		})
	}
}
