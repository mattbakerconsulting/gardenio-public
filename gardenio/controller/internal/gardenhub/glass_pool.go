package gardenhub

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
)

// The Glass credential pool (Lands workbench-glass spec GL-15, §3.5, step 6.9, gd-zM1oXXxlYp): GET
// /garden/glass/credentials is coopmux's credential pool, read only, for an admin. It is the same snapshot mux.html's
// CredentialPanel.tsx polls (coop/crates/mux/src/transport/http_cred.rs credentials_pool): each account's health, its
// sessions now and in all, its 5 h and 7 d utilisation from the controller's rate-limit probe, and how the broker picks
// an account for a new session. The pool is fleet-wide (no land owns an account), so the route is admin-only and the
// panel opens from the operator land's header (the project panel of a land that is not a tenant's, project.go).
//
// The relay asks with the service bearer only, as GetScreen does: coopmux verifies the relay key on the watch upgrades
// alone (gd-9ZGh01W5ub), so the admin tier is checked here, with the ported table coopmux itself applies to
// /api/v1/credentials/*. Nothing in the snapshot is a secret: has_api_key only says whether a key is set.

// GlassPool reads coopmux's credential pool; *coopmuxapi.Client is one.
type GlassPool interface {
	CredentialPool(ctx context.Context) (*coopmuxapi.CredentialPool, error)
}

const glassPoolTimeout = 5 * time.Second

// glassPoolTier is who may read the pool: coopmux's own tier for GET /api/v1/credentials/pool (admin).
var glassPoolTier = glass.RouteTier(http.MethodGet, "/api/v1/credentials/pool")

// GlassPoolAccount is one account as the panel shows it.
type GlassPoolAccount struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	// Status is the broker's health: healthy, missing (no key), warming (near its limit; new sessions steered away) or
	// throttled (rate-limited or out of budget).
	Status   string `json:"status"`
	Sessions uint32 `json:"sessions"`
	Assigned uint64 `json:"assigned"` // sessions given it since coopmux started
	HasKey   bool   `json:"has_key"`
	// ExpiresAt is when its credential expires (unix seconds); 0 for a static key.
	ExpiresAt uint64 `json:"expires_at,omitempty"`
	// The rate-limit probe, absent until the account is probed: its throttle status, the 5 h and 7 d windows used as
	// fractions (0.42 is 42%), when the throttle resets (RFC 3339) and which window binds.
	Throttle string   `json:"throttle,omitempty"`
	Util5h   *float64 `json:"util_5h,omitempty"`
	Util7d   *float64 `json:"util_7d,omitempty"`
	ResetAt  string   `json:"reset_at,omitempty"`
	Claim    string   `json:"claim,omitempty"`
}

// GlassPoolDoc is GET /garden/glass/credentials.
type GlassPoolDoc struct {
	Accounts []GlassPoolAccount `json:"accounts"`
	Sessions uint32             `json:"sessions"`
	Healthy  int                `json:"healthy"`
	Total    int                `json:"total"`
	// Strategy is how the broker picks an account for a new session: least_loaded or round_robin. Next is the healthy
	// account a round robin gives next (its index among the healthy ones), when there is one.
	Strategy string  `json:"strategy,omitempty"`
	Next     *uint64 `json:"next,omitempty"`
	// Uncounted: auto-assign is off (COOPMUX_DISABLE_AUTO_ASSIGN, the default in prod), so the broker counts no
	// sessions and every count reads 0; the panel says so instead of showing the zeros (ka-0000005557).
	Uncounted bool  `json:"uncounted,omitempty"`
	ReadAt    int64 `json:"read_at"` // unix seconds
}

// mayReadPool reports whether this viewer may open the credential pool: it is wired and they hold its tier.
func (h *Handler) mayReadPool(r *http.Request) bool {
	if h.Glass == nil || h.Glass.Pool == nil || h.Glass.Who == nil {
		return false
	}
	return glassPoolTier.Allows(h.Glass.Who(r).Roles)
}

// glassPool serves GET /garden/glass/credentials.
func (h *Handler) glassPool(w http.ResponseWriter, r *http.Request) {
	if h.Glass == nil {
		http.Error(w, "the glass is not enabled", http.StatusServiceUnavailable)
		return
	}
	var who coopmuxapi.Identity
	if h.Glass.Who != nil {
		who = h.Glass.Who(r)
	}
	if !glassPoolTier.Allows(who.Roles) {
		http.Error(w, "the credential pool needs "+glassPoolTier.String(), http.StatusForbidden)
		return
	}
	if h.Glass.Pool == nil {
		http.Error(w, "the credential pool is not wired here", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), glassPoolTimeout)
	defer cancel()
	p, err := h.Glass.Pool.CredentialPool(ctx)
	switch {
	case errors.Is(err, coopmuxapi.ErrDisabled):
		http.Error(w, "coopmux is not deployed here", http.StatusServiceUnavailable)
		return
	case err != nil || p == nil:
		http.Error(w, "coopmux did not answer", http.StatusBadGateway)
		return
	}
	doc := glassPoolDoc(p)
	now := time.Now
	if h.Glass.now != nil {
		now = h.Glass.now
	}
	doc.ReadAt = now().Unix()
	writeDoc(w, "glass", doc)
}

// glassPoolDoc is the panel's view of coopmux's snapshot, the accounts in the broker's own order.
func glassPoolDoc(p *coopmuxapi.CredentialPool) *GlassPoolDoc {
	d := &GlassPoolDoc{Accounts: make([]GlassPoolAccount, 0, len(p.Accounts)), Sessions: p.TotalSessions, Healthy: p.HealthyAccounts,
		Total: p.TotalAccounts, Strategy: p.Strategy, Next: p.RotatorNextIndex, Uncounted: p.AutoAssignDisabled}
	for _, a := range p.Accounts {
		d.Accounts = append(d.Accounts, GlassPoolAccount{
			Name: a.Name, Provider: a.Provider, Status: strings.ToLower(strings.TrimSpace(a.Status)), Sessions: a.SessionCount,
			Assigned: a.CumulativeAssignments, HasKey: a.HasAPIKey, ExpiresAt: a.ExpiresAt, Throttle: strings.TrimSpace(a.ThrottleStatus),
			Util5h: fraction(a.Utilization5h), Util7d: fraction(a.Utilization7d), ResetAt: strings.TrimSpace(a.ThrottleResetAt),
			Claim: strings.TrimSpace(a.RepresentativeClaim),
		})
	}
	if d.Total == 0 {
		d.Total = len(d.Accounts)
	}
	if d.Uncounted {
		d.Sessions = 0
		for i := range d.Accounts {
			d.Accounts[i].Sessions, d.Accounts[i].Assigned = 0, 0
		}
	}
	return d
}

// fraction parses a probe's utilisation ("0.4200"); nil when it is not probed or not a number.
func fraction(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}
