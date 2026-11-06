package main

import "testing"

func TestBridgeTokenName(t *testing.T) {
	cases := []struct {
		name      string
		key       string
		canonical map[string]bool
		wantName  string
		wantOK    bool
	}{
		{
			name:     "canonical bridge key",
			key:      "bridge_token:slack-bridge",
			wantName: "slack-bridge",
			wantOK:   true,
		},
		{
			name:     "legacy fleet key with no canonical row loads",
			key:      "fleet_token:old-bridge",
			wantName: "old-bridge",
			wantOK:   true,
		},
		{
			name:      "legacy fleet key shadowed by canonical row is skipped",
			key:       "fleet_token:dup",
			canonical: map[string]bool{"dup": true},
			wantOK:    false,
		},
		{
			name:     "unrelated config key ignored",
			key:      "type:agent",
			wantOK:   false,
			wantName: "",
		},
		{
			name:   "empty canonical name ignored",
			key:    "bridge_token:",
			wantOK: false,
		},
		{
			name:   "empty legacy name ignored",
			key:    "fleet_token:",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bridgeTokenName(tc.key, tc.canonical)
			if ok != tc.wantOK || got != tc.wantName {
				t.Fatalf("bridgeTokenName(%q) = (%q, %v), want (%q, %v)", tc.key, got, ok, tc.wantName, tc.wantOK)
			}
		})
	}
}
