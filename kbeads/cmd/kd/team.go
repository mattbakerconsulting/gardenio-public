package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// teamCmd is the `kd team` command group, backing type:team config beads —
// the principal kind that taints are owned by, grants are given to, and
// scrub policies name as approvers (ADR docs/adr/taints.md §8, §9.2). Write
// policy (min_role: gardenio-admin, ADR §8.2) is enforced entirely
// server-side by authorizeWriteTitled; these commands never check the caller's
// role themselves and just surface the resulting 403.
var teamCmd = &cobra.Command{
	Use:     "team",
	Short:   "Manage teams (type:team config beads)",
	GroupID: "system",
}

func init() {
	teamCmd.AddCommand(teamCreateCmd)
	teamCmd.AddCommand(teamAddCmd)
	teamCmd.AddCommand(teamRemoveCmd)
	teamCmd.AddCommand(teamShowCmd)
	teamCmd.AddCommand(teamListCmd)
}

// teamFieldsRaw is the parsed shape of a type:team bead's Fields JSON, kept
// as json.RawMessage for members/owners so re-marshaling a member/owner list
// preserves untouched entries byte-for-byte instead of round-tripping them
// through a Go type that might reformat them.
type teamFieldsRaw struct {
	Team       string            `json:"team"`
	Display    string            `json:"display,omitempty"`
	Members    []json.RawMessage `json:"members,omitempty"`
	OIDCGroups []string          `json:"oidc_groups,omitempty"`
	Owners     []json.RawMessage `json:"owners,omitempty"`
	Projects   []string          `json:"projects,omitempty"`
}

// parseTeamFields unmarshals a bead's Fields into teamFieldsRaw, tolerating
// an empty/absent Fields blob (an empty team has no members/owners yet).
func parseTeamFields(fields json.RawMessage) (teamFieldsRaw, error) {
	var tf teamFieldsRaw
	if len(fields) == 0 {
		return tf, nil
	}
	if err := json.Unmarshal(fields, &tf); err != nil {
		return tf, fmt.Errorf("parsing team fields: %w", err)
	}
	return tf, nil
}

// findTeamByName looks up a type:team bead by its `team` field — NOT the
// bead Title/ID, which is just the bead's own generated id (see
// internal/server/team_membership_test.go's seedTeamBead: Title is set to
// the bead id while the human-facing name lives in Fields.team). Mirrors the
// client-side scan pattern in resolveConfigBead (config_bead.go), since the
// mock/list surface does not implement server-side FieldFilters matching.
func findTeamByName(ctx context.Context, name string) (*model.Bead, error) {
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Type:  []string{"team"},
		Kind:  []string{"config"},
		Limit: 1000,
	})
	if err != nil {
		return nil, fmt.Errorf("listing teams: %w", err)
	}
	for _, b := range resp.Beads {
		tf, err := parseTeamFields(b.Fields)
		if err != nil {
			continue
		}
		if tf.Team == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("team %q not found", name)
}

// parseOperatorRef splits a CLI-supplied --member/--owner value into either
// an already-resolved numeric operator id, or a (kind, subject) alias pair —
// a bare value defaults to kind "oidc" (an email), matching the same default
// operatorAliasForIdentity/teamMatchesMember apply server-side.
func parseOperatorRef(ref string) (kind, subject string, id int64, isNumber bool) {
	ref = strings.TrimSpace(ref)
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return "", "", n, true
	}
	k, s, ok := strings.Cut(ref, ":")
	if !ok {
		return "oidc", ref, 0, false
	}
	return k, s, 0, false
}

// resolveOperatorRef resolves a CLI-supplied member/owner reference to the
// value stored in a team's members/owners array, per ADR docs/adr/taints.md
// §8.1: "members is a list of operator ids (or emails, resolved through
// operator_aliases at write time and stored as ids)". A ref that does not
// resolve to a known operator yet (nobody has authenticated as them) is
// stored as its scheme-prefixed alias string instead — teamMatchesMember
// resolves that lazily at read time once the person shows up.
func resolveOperatorRef(ctx context.Context, ref string) (json.RawMessage, error) {
	kind, subject, id, isNumber := parseOperatorRef(ref)
	if isNumber {
		return json.Marshal(id)
	}
	resp, err := beadsClient.LookupOperatorByAlias(ctx, kind, subject)
	if err != nil {
		return nil, fmt.Errorf("resolving operator alias %s:%s: %w", kind, subject, err)
	}
	if resp.Found {
		return json.Marshal(resp.OperatorID)
	}
	fmt.Fprintf(os.Stderr, "warning: %s:%s has no operator record yet; storing as a pending alias (resolves once they authenticate)\n", kind, subject)
	return json.Marshal(kind + ":" + subject)
}

// resolveOperatorRefs resolves a batch of member/owner refs in order.
func resolveOperatorRefs(ctx context.Context, refs []string) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(refs))
	for _, ref := range refs {
		resolved, err := resolveOperatorRef(ctx, ref)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

// resolvedOperatorID mirrors teamMatchesMember's own resolution of one
// members/owners entry: a JSON number is already an operator id; a JSON
// string is a "kind:subject" alias (bare strings default to "oidc") resolved
// through LookupOperatorByAlias. resolved=false with a non-empty alias means
// the entry has no operator row yet, so `kd team remove` falls back to
// comparing the normalized alias string directly.
func resolvedOperatorID(ctx context.Context, raw json.RawMessage) (id int64, alias string, resolved bool) {
	var asNumber int64
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		return asNumber, "", true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil || asString == "" {
		return 0, "", false
	}
	kind, subject, ok := strings.Cut(asString, ":")
	if !ok {
		kind, subject = "oidc", asString
	}
	resp, err := beadsClient.LookupOperatorByAlias(ctx, kind, subject)
	if err == nil && resp.Found {
		return resp.OperatorID, "", true
	}
	return 0, kind + ":" + subject, false
}

// operatorRefMatchesEntry reports whether a CLI-supplied --member/--owner ref
// (as passed to `kd team remove`) identifies the same operator as an existing
// members/owners array entry, resolving both sides through operator_aliases
// so a Slack alias in the array can be removed by passing the person's email
// (and vice versa) — the same equivalence teamMatchesMember relies on for
// matching.
func operatorRefMatchesEntry(ctx context.Context, ref string, entry json.RawMessage) bool {
	kind, subject, id, isNumber := parseOperatorRef(ref)
	entryID, entryAlias, entryResolved := resolvedOperatorID(ctx, entry)
	if isNumber {
		return entryResolved && entryID == id
	}
	resp, err := beadsClient.LookupOperatorByAlias(ctx, kind, subject)
	if err == nil && resp.Found {
		return entryResolved && entryID == resp.OperatorID
	}
	return !entryResolved && entryAlias == kind+":"+subject
}

// containsRawJSON reports whether raw already appears in existing, compared
// by exact encoded bytes (the cheap, non-alias-resolving dedup check used by
// `kd team add` to avoid appending an identical entry twice).
func containsRawJSON(existing []json.RawMessage, raw json.RawMessage) bool {
	for _, e := range existing {
		if string(e) == string(raw) {
			return true
		}
	}
	return false
}
