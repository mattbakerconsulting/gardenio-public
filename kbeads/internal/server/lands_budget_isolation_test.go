package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestLandsAPI_BudgetOtherLandRefusedByAdmit: another land's budget and budget history read as absent to a land's
// member and to its land principal, while that land is a real tenant land with a stamped purse that an operator reads.
// The admin's 200 is the positive control: the 404 is the land check's, not a missing project or account bead (the
// "other land" asserts in TestLandsAPI_Budget and TestLandsAPI_BudgetHistory name a land that does not exist). Security
// review of the tenant charts, step 3.5 (gd-E7FphpZNMI).
func TestLandsAPI_BudgetOtherLandRefusedByAdmit(t *testing.T) {
	f := newScopedFixture(t)
	f.seedLandProject(t, "false")
	f.seedTyped("proj-other", "project", "")
	proj := f.ms.beads["proj-other"]
	proj.Title, proj.Kind, proj.Fields = "other", "config", []byte(`{"land_kind":"tenant"}`)
	f.seedTyped("acct-other", "account", "project:other")
	acct := f.ms.beads["acct-other"]
	acct.Title, acct.Kind = "lands-other", "config"
	acct.Fields, _ = json.Marshal(map[string]string{
		"purse_granted_credits": "100", "purse_used_credits": "30", "purse_state": "ok",
		"purse_read_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), "total_cost_usd": "12",
	})

	for _, path := range []string{"/other/budget", "/other/budget/history"} {
		if code, _, body := f.landsDo(t, tokOwnerAdmin, http.MethodGet, path, nil); code != http.StatusOK || strings.Contains(body, "usd") {
			t.Fatalf("admin GET %s: %d %s, want 200 in credits (the positive control)", path, code, body)
		}
		for _, tok := range []string{tokScopedWriter, tokScopedOwner, tokScopedGuest, tokLandClaimlayer} {
			code, _, body := f.landsDo(t, tok, http.MethodGet, path, nil)
			assertLandsProblem(t, tok+" GET "+path, code, body, http.StatusNotFound, "not_found")
		}
	}
	// Shapes that are not the land's name never reach another land.
	for _, path := range []string{"/Other/budget", "/other%2F..%2Fclaimlayer/budget", "/claimlayer%2F..%2Fother/budget/history"} {
		if code, _, body := f.landsDo(t, tokScopedWriter, http.MethodGet, path, nil); code == http.StatusOK {
			t.Errorf("member GET %s: 200 %s", path, body)
		}
	}
}
