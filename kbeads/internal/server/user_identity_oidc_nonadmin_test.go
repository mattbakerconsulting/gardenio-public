package server

import (
	"net/http"
	"testing"
)

// gd-YnrhRV9PJa security precondition for the Slack link pin rules: the
// bridge accepts a link when the signed-in person's user bead pins the Slack
// id or lists the Slack email in email_aliases, so those fields must not be
// writable by anyone but an admin (or the link endpoint on the caller's own
// bead). A signed-in non-admin person editing SOMEONE ELSE's bead gets 403 on
// every route, and the bead is unchanged. (Static/fleet tokens are covered by
// TestFieldMutationEndpoints_UserIdentityGate.)
func TestFieldMutationEndpoints_UserIdentityGate_NonAdminOIDCDenied(t *testing.T) {
	for _, role := range []string{RoleOperator, RoleViewer} {
		for _, c := range fieldWriteCases() {
			if !c.denied {
				continue
			}
			t.Run(role+"/"+c.name, func(t *testing.T) {
				_, ms, h := newTaintTestServer(map[string][]string{"mallory@example.com": {role}})
				seedIdentityUserBead(t, ms, "gd-user1", userBeadFieldsJSON)
				before := string(ms.beads["gd-user1"].Fields)
				nBefore := countUserBeads(ms)

				rec := fieldWriteRequest(t, h, c, "mallory@example.com", "")
				if rec.Code != http.StatusForbidden {
					t.Fatalf("code = %d, want 403; body %s", rec.Code, rec.Body.String())
				}
				if after := string(ms.beads["gd-user1"].Fields); after != before {
					t.Errorf("fields changed despite 403:\n before %s\n after  %s", before, after)
				}
				if countUserBeads(ms) != nBefore {
					t.Errorf("a user bead was created despite 403")
				}
			})
		}
	}
}
