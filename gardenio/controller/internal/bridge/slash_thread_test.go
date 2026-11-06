package bridge

import (
	"context"
	"encoding/json"
	"testing"
)

func TestParseSlashThreadTS(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"empty payload", "", ""},
		{"no thread_ts", `{"command":"/formula","channel_id":"C1"}`, ""},
		{"with thread_ts", `{"command":"/formula","thread_ts":"1700000000.123456"}`, "1700000000.123456"},
		{"malformed json", `{not json`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSlashThreadTS(json.RawMessage(tt.payload))
			if got != tt.want {
				t.Errorf("parseSlashThreadTS(%q) = %q, want %q", tt.payload, got, tt.want)
			}
		})
	}
}

func TestSlashThreadTSContext_Roundtrip(t *testing.T) {
	ctx := context.Background()
	if got := SlashThreadTSFromContext(ctx); got != "" {
		t.Errorf("empty context should return empty thread_ts, got %q", got)
	}
	ctx = WithSlashThreadTS(ctx, "1700000000.111")
	if got := SlashThreadTSFromContext(ctx); got != "1700000000.111" {
		t.Errorf("expected 1700000000.111, got %q", got)
	}
}
