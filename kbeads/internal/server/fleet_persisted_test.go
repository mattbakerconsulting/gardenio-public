package server

import (
	"testing"
	"time"
)

func TestFleetConfig_RegisterPersistedToken(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{"tok-held": "held-fleet"})
	exp := time.Now().Add(time.Hour)

	if !fc.RegisterPersistedToken("tok-new", "agent-gd-x", "gardenio", nil, nil, exp, "agent:x", false) {
		t.Fatal("an unknown persisted token must be added")
	}
	if !fc.HasToken("tok-new") || fc.ResolveProject("tok-new") != "gardenio" || fc.ResolveSubject("tok-new") != "agent:x" {
		t.Fatal("added token must carry its persisted fleet, project and subject")
	}
	if !fc.ResolveExpiresAt("tok-new").Equal(exp) {
		t.Fatal("added token must keep its persisted expiry")
	}

	// A held token keeps what it was registered with.
	if fc.RegisterPersistedToken("tok-held", "other", "other-project", nil, nil, time.Time{}, "", false) {
		t.Fatal("a token already held must not be re-registered")
	}
	if fc.ResolveFleet("tok-held") != "held-fleet" || fc.ResolveProject("tok-held") != "" {
		t.Fatal("re-sync must not overwrite a held token's registration")
	}
}

func TestFleetConfig_RegisterPersistedToken_SkipsRemoved(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull("tok-revoked", "agent-gd-a", "gardenio", nil, nil, time.Time{}, "", false)
	fc.RegisterTokenFull("tok-cascaded", "agent-gd-b", "gardenio", nil, nil, time.Time{}, "", false)

	fc.RevokeBridgeToken("agent-gd-a")
	fc.removeToken("tok-cascaded")

	// Their config rows may outlive the revoke (the delete is best-effort):
	// the re-sync must not bring them back.
	for _, tok := range []string{"tok-revoked", "tok-cascaded"} {
		if fc.RegisterPersistedToken(tok, "agent-gd-a", "gardenio", nil, nil, time.Time{}, "", false) {
			t.Fatalf("%s was removed on this daemon and must not be re-added", tok)
		}
		if fc.HasToken(tok) {
			t.Fatalf("%s must stay invalid", tok)
		}
	}

	// An explicit registration (the register endpoint) clears the mark.
	fc.RegisterTokenFull("tok-revoked", "agent-gd-a", "gardenio", nil, nil, time.Time{}, "", false)
	if !fc.HasToken("tok-revoked") {
		t.Fatal("an explicit registration must make the token valid again")
	}
}

func TestFleetConfig_RegisterPersistedToken_PrivilegedFleet(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.SetPrivilegedFleets([]string{"gitlab-bridge"})
	if !fc.RegisterPersistedToken("tok-gl", "gitlab-bridge", "", nil, nil, time.Time{}, "", false) {
		t.Fatal("token must be added")
	}
	if !fc.ResolvePrivileged("tok-gl") {
		t.Fatal("a re-synced token of a privileged fleet must be marked privileged")
	}
}
