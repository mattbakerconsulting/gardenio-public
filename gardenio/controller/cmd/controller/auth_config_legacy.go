package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// The controller's canonical failover state is the in-memory failoverRuntime.
// Historical fixtures (and some live data) represent the per-project primary
// account on a legacy config:auth bead — a type=config bead with title="auth",
// a "project:<name>" label, and a JSON description of the form
// {"account":"<primary-title>"}. A one-shot migration CLI moves project labels
// onto account beads, but deployments that have not yet migrated — and every
// E2E fixture that exercises the failover path — still rely on the legacy
// shape. The helpers below let the failover path discover projects from legacy
// config:auth beads without reintroducing config:auth as the source of truth.
// The controller only reads them: it runs as a machine principal that is not
// granted config:auth writes (credential routing), and the in-memory
// failoverRuntime plus the account beads carry the failover state.

// authConfigDescription is the JSON persisted in a legacy config:auth bead's
// description. Only `Account` is read; other keys (primary_account,
// failover_reason, failed_over_at) may be present on old beads and are ignored.
type authConfigDescription struct {
	Account string `json:"account"`
}

// findAuthConfigForProject returns the legacy config:auth bead for the given
// project, or nil if none exists. When multiple open config:auth beads carry
// the same project label (common in E2E fixtures that reuse "project:e2e-test"
// across runs), the most-recently-updated bead wins — stale fixtures from
// prior runs must not mask the current run's config bead.
func findAuthConfigForProject(ctx context.Context, daemon *beadsapi.Client, project string) (*beadsapi.BeadDetail, error) {
	if project == "" {
		return nil, nil
	}
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Labels:   []string{"project:" + project},
		Limit:    50,
	})
	if err != nil {
		return nil, fmt.Errorf("listing config beads for project %s: %w", project, err)
	}
	var best *beadsapi.BeadDetail
	for _, b := range result.Beads {
		if b.Title != "auth" {
			continue
		}
		if best == nil || b.UpdatedAt.After(best.UpdatedAt) {
			best = b
		}
	}
	return best, nil
}

// parseAuthConfigAccount extracts the "account" field from a config:auth
// description. Returns "" if the description is not valid JSON or lacks the
// field.
func parseAuthConfigAccount(description string) string {
	if description == "" {
		return ""
	}
	var desc authConfigDescription
	if err := json.Unmarshal([]byte(description), &desc); err != nil {
		return ""
	}
	return desc.Account
}

// listLegacyAuthConfigs returns every open config:auth bead that carries a
// "project:" label. The map key is the project name derived from the label.
// When multiple config:auth beads share a project label, the most-recently-
// updated bead wins. E2E fixtures reuse the same project label (e.g.
// "project:e2e-test") across runs — first-wins semantics let stale beads from
// prior runs mask the current run's config and silence the controller-driven
// failover path.
func listLegacyAuthConfigs(ctx context.Context, daemon *beadsapi.Client) (map[string]*beadsapi.BeadDetail, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing config beads: %w", err)
	}
	out := map[string]*beadsapi.BeadDetail{}
	for _, b := range result.Beads {
		if b.Title != "auth" {
			continue
		}
		for _, lbl := range b.Labels {
			p, ok := strings.CutPrefix(lbl, "project:")
			if !ok || p == "" {
				continue
			}
			if existing, exists := out[p]; !exists || b.UpdatedAt.After(existing.UpdatedAt) {
				out[p] = b
			}
		}
	}
	return out, nil
}

// walkChainFromPrimary walks a fallback chain starting at `primaryName` across
// the provided pool of accounts, returning the accounts traversed in chain
// order. Stops on a missing bead or a cycle. The returned slice is suitable
// for feeding into the failover helpers (projectPrimary, findByTitle,
// findHealthyFallback).
func walkChainFromPrimary(accounts []*beadsapi.BeadDetail, primaryName string) []*beadsapi.BeadDetail {
	if primaryName == "" {
		return nil
	}
	visited := map[string]bool{}
	var chain []*beadsapi.BeadDetail
	next := primaryName
	for next != "" {
		if visited[next] {
			break
		}
		visited[next] = true
		bead := findByTitle(accounts, next)
		if bead == nil {
			break
		}
		chain = append(chain, bead)
		next = bead.Fields["fallback"]
	}
	return chain
}

// resolveLegacyProjectChain looks up the legacy config:auth bead for a project
// and walks the fallback chain from its configured primary. Returns the chain
// of account beads (which projectPrimary/findHealthyFallback can operate on)
// and the primary account name. Returns (nil, "", nil) when no legacy
// config:auth exists for the project.
func resolveLegacyProjectChain(ctx context.Context, daemon *beadsapi.Client, project string) ([]*beadsapi.BeadDetail, string, error) {
	cfg, err := findAuthConfigForProject(ctx, daemon, project)
	if err != nil {
		return nil, "", err
	}
	if cfg == nil {
		return nil, "", nil
	}
	primary := parseAuthConfigAccount(cfg.Description)
	if primary == "" {
		return nil, "", nil
	}
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Limit:    200,
	})
	if err != nil {
		return nil, "", err
	}
	return walkChainFromPrimary(result.Beads, primary), primary, nil
}
