// Package main — the spawn-seal guard for `gb gardener cadence` and `gb gardener
// mode` (gd-c4Xz4i95F7).
//
// Both verbs rewrite `cron` / `triggers`, which are spawn-lineage digest keys
// (kbeads/internal/server/spawn_lineage_seal.go spawnLineageDigestKeys). On a
// trigger sealed to an owner's approval, a write from anything but a
// gardenio-admin OIDC session leaves the stored seal digest stale: the next fire
// of the privileged role is refused (`HTTP 403: spawn denied: role plan-gardener
// is privileged; a non-human caller must escalate`), and after a few refusals
// the hard_stop closes the trigger. That is what happened on plan gd-r4GCQRjEei
// when a plan gardener run applied the owner's "slow" choice with `gb gardener
// cadence --cron`. An OIDC admin write keeps the seal (the daemon re-stamps a
// bead that was validly sealed before the patch), so the guard refuses only the
// other callers, and tells them to escalate an edit-and-reseal instead.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// whoamiSourceOIDC is the /v1/whoami source of an OIDC session (kbeads
// whoami.go whoamiSourceAuth); static, fleet and machine tokens report "token".
const whoamiSourceOIDC = "auth"

// spawnSealValid reports whether b, as GetBead returns it, carries a spawn
// seal that still covers its current content: the seal's digest equals the
// digest the daemon computes over the bead now. It mirrors the digest half of
// the daemon's validSpawnSeal and ignores the seal's age.
func spawnSealValid(b *beadsapi.BeadDetail) bool {
	if b == nil || b.SpawnLineageDigest == "" {
		return false
	}
	raw := strings.TrimSpace(b.Fields["spawn_lineage"])
	if raw == "" {
		return false
	}
	var seal struct {
		Author string `json:"author"`
		Digest string `json:"digest"`
	}
	if json.Unmarshal([]byte(raw), &seal) != nil {
		return false
	}
	return strings.HasPrefix(seal.Author, "oidc:") && seal.Digest != "" && seal.Digest == b.SpawnLineageDigest
}

// spawnSealAuthor returns the author named by b's seal, or "".
func spawnSealAuthor(b *beadsapi.BeadDetail) string {
	var seal struct {
		Author string `json:"author"`
	}
	_ = json.Unmarshal([]byte(b.Fields["spawn_lineage"]), &seal)
	return seal.Author
}

// guardSealedGardenerEdit runs before a cadence or mode write to gardener
// trigger id. It reports whether the trigger is validly sealed, and refuses the
// edit when it is and the caller is not an OIDC session, because the write
// would void the seal. retry is the command that makes the edit, for the
// escalation recipe the refusal prints. An OIDC session is let through; whether
// it was a gardenio-admin one is checked after the write by
// checkGardenerSealKept.
func guardSealedGardenerEdit(ctx context.Context, c *beadsapi.Client, id, retry string) (bool, error) {
	// GET by id: a list result (findExistingGardenerSchedule) does not carry the
	// daemon's spawn_lineage_digest.
	b, err := c.GetBead(ctx, id)
	if err != nil {
		return false, fmt.Errorf("reading %s to check its spawn seal: %w", id, err)
	}
	if !spawnSealValid(b) {
		return false, nil
	}
	who, err := c.Whoami(ctx)
	if err == nil && who.Source == whoamiSourceOIDC {
		return true, nil
	}
	caller := "this credential is not an OIDC session"
	if err != nil {
		caller = fmt.Sprintf("could not tell who this credential is (%v)", err)
	}
	return true, fmt.Errorf(`refusing to edit gardener %s: it is sealed to %s, and %s.
A cadence or mode change here would void the seal: every later fire of its %s role is then
refused with HTTP 403 until the trigger hard-stops, and nothing else says so.

Only a gardenio-admin OIDC session (gb login) keeps the seal through this edit. From an agent,
file an escalation that makes the change as an admin, so the seal is re-stamped in the same pass:

  gb escalation create --operation-type gardener_cadence_reseal \
    --requesting-task <your-task> \
    --title "Change gardener %s cadence and keep its seal" \
    --context "<why: the owner's decision that asked for it>" \
    --script '%s && gb trigger reseal %s'

(gb trigger reseal shows the content it adopts and fails if the seal did not take.)`,
		id, defaultIfEmpty(spawnSealAuthor(b), "an owner"), caller,
		defaultIfEmpty(b.Fields["role"], "privileged"), id, retry, id)
}

// checkGardenerSealKept runs after a cadence or mode write to a trigger that
// was validly sealed before it. The daemon re-stamps the seal only for a
// gardenio-admin OIDC session; any other OIDC user's write leaves it stale. It
// returns an error naming the fix when the seal no longer covers the trigger.
func checkGardenerSealKept(ctx context.Context, c *beadsapi.Client, id string) error {
	b, err := c.GetBead(ctx, id)
	if err != nil {
		return fmt.Errorf("gardener %s was edited, but reading it back to check its spawn seal failed: %w", id, err)
	}
	if spawnSealValid(b) {
		return nil
	}
	return fmt.Errorf("gardener %s was edited, but it is no longer sealed: this session is probably not a gardenio-admin. "+
		"Its next fire will be refused with HTTP 403 until a gardenio-admin runs `gb trigger reseal %s`", id, id)
}
