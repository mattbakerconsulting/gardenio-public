package provision

import (
	"context"
	"fmt"

	"gardenio/controller/internal/keycloakadmin"
)

// The gatehouse (step 9.12, gd-vXokVcVUf8; decision D1 gd-B5HQhmG1cJ): one
// tenant-shaped land, provisioned from a plot application like any other,
// that every signed-in person with no land is a guest of. Its steward is a
// plain tenant steward whose playbook reads land_gatehouse and plays the
// front door (internal/steward).
//
// Being a guest is sitting in the land's Keycloak guests group, and in
// lands-tenants, which is what the viewer's edge admits to the Commons
// (joinLand): kbeads builds a person's lands from their token's groups only,
// so a guest list kept on a bead would grant nothing. The person picks the
// land up on their next token refresh.
//
//   - claim: a claimed invite that names no land makes its person a gatehouse
//     guest when they hold no land (email_invites.go).
//   - backfill: a claimed invite is terminal, so the people who were already
//     in invited with no land when the gatehouse opened never pass the claim
//     again. Once per gatehouse project bead, the pass makes each of them a
//     guest and stamps land_gatehouse_backfilled_at. The pass never runs
//     again for that bead, so taking someone out of the gatehouse in
//     Keycloak sticks.
//   - mark: the gatehouse's project bead carries land_gatehouse=true. The
//     project step stamps it on a land provisioned as the gatehouse; the pass
//     stamps it on one that was open before Gatehouse named it.
//
// Nobody is taken out of the gatehouse when they later get a land of their
// own; they stay a guest at the front door.

// Gatehouse project bead fields (kbeads type:project).
const (
	GatehouseField           = "land_gatehouse"
	GatehouseBackfilledField = "land_gatehouse_backfilled_at"
)

// GatehouseGuests runs the gatehouse pass: mark and backfill the gatehouse
// once it is open. It does nothing when no gatehouse is configured or it is
// not open yet; a failure is retried on the next pass.
func (p *Provisioner) GatehouseGuests(ctx context.Context) error {
	if p.Gatehouse == "" {
		return nil
	}
	open, err := p.landOpen(ctx, p.Gatehouse)
	if err != nil || !open {
		return err
	}
	proj, err := p.project(ctx, p.Gatehouse)
	if err != nil || proj == nil {
		return err
	}
	if proj.Fields[GatehouseField] != "true" {
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, proj.ID, map[string]any{GatehouseField: true}); err != nil {
			return fmt.Errorf("marking %s the gatehouse: %w", p.Gatehouse, err)
		}
	}
	if proj.Fields[GatehouseBackfilledField] != "" {
		return nil
	}
	people, err := p.Groups.InvitedMembers(ctx)
	if err != nil {
		return fmt.Errorf("reading the invited group: %w", err)
	}
	joined := 0
	for _, u := range people {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ok, err := p.joinGatehouse(ctx, u.ID)
		if err != nil {
			// Nothing is stamped, so the next pass runs the backfill again;
			// those already in are skipped as holding a land.
			return fmt.Errorf("backfilling the gatehouse: %s: %w", u.ID, err)
		}
		if ok {
			joined++
		}
	}
	if err := p.Daemon.UpdateBeadFieldsTyped(ctx, proj.ID, map[string]any{GatehouseBackfilledField: p.stamp()}); err != nil {
		return fmt.Errorf("stamping the gatehouse backfill: %w", err)
	}
	p.logger().Info("lands gatehouse: backfilled", "land", p.Gatehouse, "invited", len(people), "joined", joined)
	return nil
}

// joinGatehouse makes userID a guest of the gatehouse when they hold no land,
// and reports whether it did. The caller has checked the gatehouse is open.
func (p *Provisioner) joinGatehouse(ctx context.Context, userID string) (bool, error) {
	lands, err := p.Groups.UserLands(ctx, userID)
	if err != nil || len(lands) > 0 {
		return false, err
	}
	if _, err := p.joinLand(ctx, p.Gatehouse, keycloakadmin.RoleGuest, userID); err != nil {
		return false, err
	}
	return true, nil
}
