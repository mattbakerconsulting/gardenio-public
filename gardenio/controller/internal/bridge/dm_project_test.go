package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseDMProjectFromPreferences(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"not json", "nope", ""},
		{"missing key", `{"vibe_mode":"true"}`, ""},
		{"set via gb prefs", `{"dm_project":"claimlayer"}`, "claimlayer"},
		{"trimmed", `{"dm_project":"  claimlayer "}`, "claimlayer"},
		{"non-string value", `{"dm_project":true}`, ""},
		{"null", `{"dm_project":null}`, ""},
		{"alongside other prefs", `{"vibe_mode":"true","dm_project":"claimlayer"}`, "claimlayer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDMProjectFromPreferences(tc.raw); got != tc.want {
				t.Errorf("parseDMProjectFromPreferences(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func newDMProjectTestBot(t *testing.T, daemon *mockDaemon) *Bot {
	t.Helper()
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(slackSrv.Close)
	return newTestBot(daemon, slackSrv)
}

// A user's dm_project preference outranks the fleet-wide DEFAULT_DM_PROJECT
// and a project bead's dm_default.
func TestResolveDMProject_UserPreferenceBeatsEnvDefault(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithDMDefault("gardenio")
	daemon.seedProject("claimlayer")
	daemon.seedUser("Bob", map[string]string{
		"slack_id":    "U-BOB",
		"preferences": `{"dm_project":"claimlayer"}`,
	})

	bot := newDMProjectTestBot(t, daemon)
	bot.defaultDMProject = "gardenio"

	if got := bot.resolveDMProject(context.Background(), "D-BOB", "U-BOB", "test"); got != "claimlayer" {
		t.Errorf("expected claimlayer (user dm_project), got %q", got)
	}
	// A different user with no preference still lands on the env default.
	if got := bot.resolveDMProject(context.Background(), "D-ALICE", "U-ALICE", "test"); got != "gardenio" {
		t.Errorf("expected gardenio (env default) for user without pref, got %q", got)
	}
}

// A dm_project that names a project that does not exist is ignored and the
// resolver falls through to the env default.
func TestResolveDMProject_UserPreferenceUnknownProjectFallsThrough(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")
	daemon.seedUser("Bob", map[string]string{
		"slack_id":    "U-BOB",
		"preferences": `{"dm_project":"does-not-exist"}`,
	})

	bot := newDMProjectTestBot(t, daemon)
	bot.defaultDMProject = "gardenio"

	if got := bot.resolveDMProject(context.Background(), "D-BOB", "U-BOB", "test"); got != "gardenio" {
		t.Errorf("expected gardenio (fall through), got %q", got)
	}
}

// With a single project the preference is moot: the sole project wins.
func TestResolveDMProject_SoleProjectIgnoresPreference(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedUser("Bob", map[string]string{
		"slack_id":    "U-BOB",
		"preferences": `{"dm_project":"claimlayer"}`,
	})

	bot := newDMProjectTestBot(t, daemon)

	if got := bot.resolveDMProject(context.Background(), "D-BOB", "U-BOB", "test"); got != "gardenio" {
		t.Errorf("expected gardenio (sole project), got %q", got)
	}
}
