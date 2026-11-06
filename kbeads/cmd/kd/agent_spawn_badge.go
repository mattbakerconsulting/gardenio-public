package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/ui"
)

// printAgentSpawnBadge renders the `kd show` spawn-provenance line for an
// agent bead: "policy-approved" (spawn_approval_mode=auto), "human-approved"
// (spawn_approval_mode=human, naming spawn_approved_by when set), or — for a
// privileged role with neither field recorded — a red "unreviewed" call-out.
// The three cases are mutually exclusive and render distinctly (gd-jbY5UiWlN4).
// Renders nothing for a non-agent bead or an agent whose role isn't
// privileged and carries no approval yet — the badge is a problem signal,
// never decoration on an ordinary spawn.
func printAgentSpawnBadge(ctx context.Context, bead *model.Bead) {
	if string(bead.Type) != "agent" || len(bead.Fields) == 0 {
		return
	}
	var fields map[string]any
	if json.Unmarshal(bead.Fields, &fields) != nil {
		return
	}
	label, danger := spawnBadgeLabel(ctx, fields)
	if label == "" {
		return
	}
	if danger && ui.ShouldUseColor() {
		label = ui.RenderDanger(label)
	}
	fmt.Printf("Spawn:       %s\n", label)
}

// spawnBadgeLabel returns the badge text for an agent bead's field map and
// whether it is the danger ("unreviewed") case. An empty label means there is
// nothing worth surfacing.
func spawnBadgeLabel(ctx context.Context, fields map[string]any) (label string, danger bool) {
	mode, _ := fields["spawn_approval_mode"].(string)
	switch mode {
	case "auto":
		return "policy-approved", false
	case "human":
		if by, _ := fields["spawn_approved_by"].(string); by != "" {
			return fmt.Sprintf("human-approved (%s)", by), false
		}
		return "human-approved", false
	}
	role, _ := fields["role"].(string)
	if role == "" || !roleIsPrivilegedForShow(ctx, role) {
		return "", false
	}
	return "unreviewed — privileged role, no spawn approval on record", true
}

// roleIsPrivilegedForShow is the read-side, best-effort mirror of the
// daemon's spawn-authz check (roleIsPrivileged, internal/server/spawn_authz.go):
// does any type:role bead titled role set privileged=true? A lookup error or
// a role with no matching bead is treated as not privileged, so `kd show`
// never fails or blocks on this display-only enrichment.
//
// "privileged" is schema-typed boolean (kbeads/internal/server/config.go
// type:role), so a bead written through validation carries a real JSON
// boolean here. But both other readers of this field — the daemon's own
// roleIsPrivileged (parseFieldsToStringMap + strconv.ParseBool) and the
// controller's RoleInfo parser (fields["privileged"] == "true") — go through
// a string-normalizing field decode that also tolerates a legacy/foreign
// string value. fieldBoolValue mirrors that tolerance here instead of
// type-asserting a Go bool directly, which would silently read as
// "unprivileged" for any bead holding the string form.
func roleIsPrivilegedForShow(ctx context.Context, role string) bool {
	if beadsClient == nil {
		return false
	}
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Type:  []string{"role"},
		Kind:  []string{"config"},
		Limit: 500,
	})
	if err != nil || resp == nil {
		return false
	}
	// Any element of a comma-joined role counts (split on commas, trim): the
	// same parse the daemon's gate and the controller's pod spec apply.
	var elems []string
	for _, r := range strings.Split(role, ",") {
		elems = append(elems, strings.TrimSpace(r))
	}
	for _, b := range resp.Beads {
		if !slices.Contains(elems, b.Title) || len(b.Fields) == 0 {
			continue
		}
		var f map[string]any
		if json.Unmarshal(b.Fields, &f) != nil {
			continue
		}
		if fieldBoolValue(f["privileged"]) {
			return true
		}
	}
	return false
}

// fieldBoolValue reports whether a decoded bead field value is truthy,
// accepting either a real JSON boolean (the schema-validated representation
// for a boolean-typed field) or its string form ("true"/"1"), which other
// readers of role-bead fields (parseFieldsToStringMap-style decoding)
// normalize to.
func fieldBoolValue(v any) bool {
	switch vv := v.(type) {
	case bool:
		return vv
	case string:
		b, _ := strconv.ParseBool(vv)
		return b
	default:
		return false
	}
}
