package main

import (
	"strings"
	"testing"
)

func TestValidateSlackUserID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     string
		wantErr   bool
		wantInErr string
	}{
		{name: "empty is ok", value: "", wantErr: false},
		{name: "whitespace-only is ok", value: "   ", wantErr: false},
		{name: "U prefix accepted", value: "U0PERSON013", wantErr: false},
		{name: "W prefix accepted (Enterprise Grid)", value: "W02QK1BKFTL", wantErr: false},
		{name: "leading whitespace accepted", value: "  U0PERSON013  ", wantErr: false},

		{name: "D prefix (DM channel) rejected with hint", value: "D07KA0J8QLX", wantErr: true, wantInErr: "DM channel"},
		{name: "C prefix (public channel) rejected", value: "C07KA0J8QLX", wantErr: true, wantInErr: "public-channel"},
		{name: "G prefix (private group) rejected", value: "G07KA0J8QLX", wantErr: true, wantInErr: "private-group"},
		{name: "B prefix (bot) rejected", value: "B07KA0J8QLX", wantErr: true, wantInErr: "bot/app"},
		{name: "lowercase u rejected", value: "u02QK1BKFTL", wantErr: true, wantInErr: "expected U… or W…"},
		{name: "garbage rejected", value: "not-an-id", wantErr: true, wantInErr: "expected U… or W…"},
		{name: "single char rejected", value: "U", wantErr: true, wantInErr: "valid Slack user ID"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateSlackUserID("--slack-id", tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateSlackUserID(%q) = nil, want error", tc.value)
				}
				if tc.wantInErr != "" && !strings.Contains(err.Error(), tc.wantInErr) {
					t.Fatalf("validateSlackUserID(%q) error = %q, want substring %q", tc.value, err.Error(), tc.wantInErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSlackUserID(%q) = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestValidateSlackUserID_DPrefixHintMentionsBothFlags(t *testing.T) {
	t.Parallel()
	err := validateSlackUserID("--slack-id", "D07KA0J8QLX")
	if err == nil {
		t.Fatal("expected error for D-prefix id")
	}
	msg := err.Error()
	for _, want := range []string{"--slack-channel-id", "--slack-id U", "D07KA0J8QLX"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\nfull message:\n%s", want, msg)
		}
	}
}

func TestValidateOperatedBy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     string
		wantErr   bool
		wantInErr string
	}{
		{name: "empty is ok", value: "", wantErr: false},
		{name: "slack:U accepted", value: "slack:U0PERSON013", wantErr: false},
		{name: "slack:W accepted", value: "slack:W02QK1BKFTL", wantErr: false},
		{name: "oidc passes through", value: "oidc:matt@acme.example", wantErr: false},
		{name: "system passes through", value: "system:scheduler", wantErr: false},
		{name: "fleet passes through", value: "fleet:gardenio", wantErr: false},

		{name: "slack:D rejected with hint", value: "slack:D07KA0J8QLX", wantErr: true, wantInErr: "DM channel"},
		{name: "slack:C rejected", value: "slack:C07KA0J8QLX", wantErr: true, wantInErr: "public-channel"},
		{name: "slack:G rejected", value: "slack:G07KA0J8QLX", wantErr: true, wantInErr: "private-group"},
		{name: "slack:B rejected", value: "slack:B07KA0J8QLX", wantErr: true, wantInErr: "bot/app"},
		{name: "slack:garbage rejected", value: "slack:not-an-id", wantErr: true, wantInErr: "expected U… or W…"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateOperatedBy(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateOperatedBy(%q) = nil, want error", tc.value)
				}
				if tc.wantInErr != "" && !strings.Contains(err.Error(), tc.wantInErr) {
					t.Fatalf("validateOperatedBy(%q) error = %q, want substring %q", tc.value, err.Error(), tc.wantInErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateOperatedBy(%q) = %v, want nil", tc.value, err)
			}
		})
	}
}
