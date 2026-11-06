package server

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// restrictTaintFleetTypes are the bead types an agent carrying a restrict
// taint may not write (opTypeWrite, restrictTaintOps): they configure agents,
// people or the fleet, or a bridge or the controller acts on them. The tenant
// tier is an allow-list (landsTenantWrites); this is the taint's deny-list for
// fleet agents, whose ordinary work stays open (gd-gaGdksntAG). The config
// key-value store (PUT /v1/configs) is refused as type config.
var restrictTaintFleetTypes = map[model.BeadType]bool{
	"config":               true,
	"user":                 true,
	"role":                 true,
	"credential":           true,
	"gate":                 true,
	"trigger":              true,
	"jira-poller":          true,
	"reaction-rule":        true,
	"widget-type":          true,
	"dashboard":            true,
	"schedule":             true,
	"script":               true,
	"alert_rule":           true,
	"alert_mute":           true,
	"spawn_policy":         true,
	"escalation_policy":    true,
	model.TypeAdvice:       true,
	model.TypeFormula:      true,
	model.TypeProject:      true,
	model.TypeAccount:      true,
	model.TypeTeam:         true,
	model.TypeFleet:        true,
	model.TypeScrubRequest: true,
}

// typeWriteDenial is opTypeWrite for the write gate: the lands-tenant tier,
// then the restrict taint.
func (s *BeadsServer) typeWriteDenial(ctx context.Context, action string, beadType model.BeadType) *AuthzDenial {
	if d := landsTenantTypeDenial(ctx, s.store, action, beadType); d != nil {
		return d
	}
	return s.restrictTaintOpDenial(ctx, restrictedOpRequest{Op: opTypeWrite, Action: action, BeadType: beadType})
}
