package bridge

import (
	"encoding/json"
	"testing"
)

func TestNotificationConfig_Defaults(t *testing.T) {
	cfg := DefaultNotificationConfig()
	if cfg.SuppressBeadActivity {
		t.Error("SuppressBeadActivity should default to false")
	}
	if cfg.SuppressSpawnConfirmation {
		t.Error("SuppressSpawnConfirmation should default to false")
	}
	if cfg.SuppressToolStream {
		t.Error("SuppressToolStream should default to false")
	}
	if cfg.SuppressSquawk {
		t.Error("SuppressSquawk should default to false")
	}
	if !cfg.SuppressMentionHint {
		t.Error("SuppressMentionHint should default to true (native Slack assistant handles thread interactions)")
	}
}

func TestNotificationConfig_JSONRoundTrip(t *testing.T) {
	cfg := NotificationConfig{
		SuppressBeadActivity:      true,
		SuppressSpawnConfirmation: false,
		SuppressToolStream:        true,
		SuppressSquawk:            false,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var decoded NotificationConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.SuppressBeadActivity != true {
		t.Error("SuppressBeadActivity should be true after roundtrip")
	}
	if decoded.SuppressSpawnConfirmation != false {
		t.Error("SuppressSpawnConfirmation should be false after roundtrip")
	}
	if decoded.SuppressToolStream != true {
		t.Error("SuppressToolStream should be true after roundtrip")
	}
	if decoded.SuppressSquawk != false {
		t.Error("SuppressSquawk should be false after roundtrip")
	}
}

func TestNotificationConfig_PartialJSON(t *testing.T) {
	// Partial JSON should only set the specified fields (rest stay default false).
	data := []byte(`{"suppress_bead_activity": true}`)
	var cfg NotificationConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.SuppressBeadActivity {
		t.Error("SuppressBeadActivity should be true")
	}
	if cfg.SuppressSquawk {
		t.Error("SuppressSquawk should remain false")
	}
}
