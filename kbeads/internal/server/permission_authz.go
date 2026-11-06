package server

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// authorizePermissionFieldWrite is the type:permission write_policy for the
// single-field endpoints (increment, append, reorder, edit), which do not run
// authorizeWriteTitled. A permission bead's behavior and answered_by record the
// verdict the bridge delivered (gd-RuI4Ze6vAg), so any field write on one needs
// what PATCH needs: the bridge's machine grant or an OIDC admin. Other bead
// types are untouched.
func (s *BeadsServer) authorizePermissionFieldWrite(ctx context.Context, bead *model.Bead) *AuthzDenial {
	if bead == nil || bead.Type != typePermission {
		return nil
	}
	return s.authorizeWriteForBead(ctx, "update", bead)
}
