package server

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"
)

// A tenant caller reads no account bead (gd-q8jegjvKI3). Provisioning writes a
// tenant land's lands-<land> account bead with the label project:<land>, and
// it carries the land's raw LiteLLM virtual key (litellm_key) and its at-cost
// USD (total_cost_usd, purse_*). The project and taint gates let a member read
// any bead of its own land, so this is a type gate on top of them: a tenant
// caller reads no bead of a tenantHiddenTypes type, in its land or any other.
// A member sees its purse in credits only, through Lands.Budget, which reads
// the account bead on the daemon's side and is untouched by this gate.

// tenantHiddenTypes are the bead types a tenant caller may not read.
var tenantHiddenTypes = []model.BeadType{model.TypeAccount}

// A scoped principal (a person, a land principal) also reads no
// gatehouse_request: every gatehouse guest would read every other's GitHub
// account, outcome and draft (gd-R7meWqNxqr). The steward's per-agent token
// still reads the requests it files.
var scopedHiddenTypes = append(slices.Clone(tenantHiddenTypes), typeGatehouseRequest)

// isTenantHiddenType reports whether t is one of tenantHiddenTypes.
func isTenantHiddenType(t model.BeadType) bool {
	return slices.Contains(tenantHiddenTypes, t)
}

// callerIsTenant reports whether the caller is a tenant: a scoped principal (a
// land member's OIDC token, a token minted for a land, a land principal), or
// a per-agent token bound to a tenant land. A failed land lookup reads as a
// tenant, so the gate fails closed.
func (s *BeadsServer) callerIsTenant(ctx context.Context) bool {
	if isScopedPrincipal(ctx) {
		return true
	}
	if _, ok := agentBeadIDFromContext(ctx); !ok {
		return false
	}
	proj := TokenProjectFromContext(ctx)
	if proj == "" {
		return false
	}
	tenant, err := s.tenantLands.isTenant(ctx, s, proj)
	if err != nil {
		slog.Warn("tenant read gate: land lookup failed, hiding tenant types", "project", proj, "error", err)
		return true
	}
	return tenant
}

// tenantHiddenReadTypes returns the bead types the caller may not read:
// scopedHiddenTypes or tenantHiddenTypes for a tenant, nil for anyone else. A
// list path appends it to BeadFilter.ExcludeTypes.
func (s *BeadsServer) tenantHiddenReadTypes(ctx context.Context) []model.BeadType {
	if isScopedPrincipal(ctx) {
		return scopedHiddenTypes
	}
	if !s.callerIsTenant(ctx) {
		return nil
	}
	return tenantHiddenTypes
}

// readHidesBead reports whether b is hidden from the caller: a bead of a type
// tenantHiddenReadTypes names, or a label-less advice or config bead the
// tenant may not read (tenantHidesLabelLess). It resolves the caller only for
// such a bead, so a read of any other costs nothing.
func (s *BeadsServer) readHidesBead(ctx context.Context, b *model.Bead) bool {
	return (b.Type == typeGatehouseRequest && isScopedPrincipal(ctx)) ||
		(isTenantHiddenType(b.Type) || tenantHidesLabelLess(b)) && s.callerIsTenant(ctx)
}

// A per-agent token reads label-less advice and config (tokenReadsLabelLessBead):
// the fleet's Global and role-only operating instructions. A tenant land's
// agent reads only the tenant-safe ones (gd-c1DOWNYy7W): a label-less advice
// or config bead an operator labelled tenantSafeLabel. The set chosen with it
// is what a land agent (steward, lands-gardener) reads at gb prime: the global
// claude-instructions, prerun and nudge-prompts config beads. Global advice
// and every role:<fleet role> bead stay hidden. The list half is in
// scopeListFilter.
const tenantSafeLabel = "tenant-safe"

// tenantHidesLabelLess reports whether b is a label-less advice or config bead
// without tenantSafeLabel.
func tenantHidesLabelLess(b *model.Bead) bool {
	return labelLessReadableType(b.Type) && !hasProjectLabel(b.Labels) && !slices.Contains(b.Labels, tenantSafeLabel)
}

// tenantLandCache memoises whether a project is a tenant land
// (land_kind == "tenant") for identityCacheTTL, so a per-agent token's every
// list does not look its project up. Its zero value is ready to use.
type tenantLandCache struct {
	mu      sync.Mutex
	entries map[string]tenantLandEntry
}

type tenantLandEntry struct {
	tenant   bool
	cachedAt time.Time
}

func (c *tenantLandCache) isTenant(ctx context.Context, s *BeadsServer, land string) (bool, error) {
	c.mu.Lock()
	e, ok := c.entries[land]
	c.mu.Unlock()
	if ok && time.Since(e.cachedAt) < identityCacheTTL {
		return e.tenant, nil
	}
	tenant, err := s.isTenantLand(ctx, land)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]tenantLandEntry{}
	}
	c.entries[land] = tenantLandEntry{tenant: tenant, cachedAt: time.Now()}
	c.mu.Unlock()
	return tenant, nil
}
