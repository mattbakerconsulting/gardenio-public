// Package wall is the Lands spawn wall (Lands Track B spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md §3.5,
// step 8.3): the one decision the controller's AgentSpawn admission, the
// reconciler's create-missing twin and the bridge's gardenerPreFireCheck all
// take before anything new starts on a tenant land.
//
// A tenant land refuses a new spawn when it is frozen (land_frozen, the
// per-land kill switch of §5.5), when the all-lands kill switch is engaged
// (an open lands-pause gate, internal/pausegate; a gate that cannot be read
// refuses too, step 10.5), when tenant spawns are off (lands.tenantSpawns, the
// controller setting LANDS_TENANT_SPAWNS that defaults to off until Phase 10's
// fences ship), or when its purse (internal/lands/purse) is at the wall or
// unknown. Operator lands are never refused here.
//
// Tenant spawns off does not stop the land's steward (owner ruling
// gd-KOAoUELMWo, after gd-3Vx9UFk7Ad: "only the steward and you spawn"): with
// them off a tenant land runs its steward and nothing else. Every other
// refusal still applies to the steward, the purse included, since it spends
// credits like any agent.
//
// Past those, a tenant land also has a per-land spawn rate (Track B §5.4, step
// 10.4): Rate refuses a new spawn when the land has started its cap of agents
// in the last hour (LANDS_SPAWNS_PER_HOUR in the controller).
//
// A refused agent bead gets agent_state=budget_exceeded, the FieldCode and
// FieldReason fields below, and one comment saying which; gb spawn reads the
// fields back to print the reason.
package wall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/pausegate"
	"gardenio/controller/internal/spawnguard"
)

const (
	// EnvTenantSpawns is the setting that lets tenant lands spawn at all
	// (lands.tenantSpawns). Unset or unparseable is off.
	EnvTenantSpawns = "LANDS_TENANT_SPAWNS"

	// Refusal codes, written to FieldCode on a refused agent bead.
	CodeFrozen          = "land_frozen"
	CodeLandsPaused     = "lands_paused"
	CodeTenantSpawnsOff = "tenant_spawns_off"
	CodePurseWall       = "purse_wall"
	CodePurseUnknown    = "purse_unknown"
	// CodeLandGone: a known tenant land with no live project bead (closed,
	// deleted or renamed), which fails closed.
	CodeLandGone  = "land_gone"
	CodeSpawnRate = "spawn_rate"
	// CodeUnfenced: a tenant land the controller cannot yet place in its own
	// namespace (step 10.2): it has no namespace, or the
	// controller's project cache does not know it as a tenant land yet.
	CodeUnfenced = "land_unfenced"

	// AgentState is the agent_state a refused agent bead is given.
	AgentState = "budget_exceeded"
	// FieldCode and FieldReason record the refusal on the agent bead.
	FieldCode   = "spawn_wall"
	FieldReason = "spawn_wall_reason"
	// CommentAuthor signs the refusal comment.
	CommentAuthor = "gardenio-controller"
)

// Refusal is the wall's verdict. The zero value lets the spawn through.
type Refusal struct {
	Code   string
	Reason string
	// Tenant reports that Check read the project as a tenant land, whether it
	// refused or not: the per-land spawn rate (Rate) applies only to those.
	Tenant bool
}

// Refused reports whether the spawn is refused.
func (r Refusal) Refused() bool { return r.Code != "" }

// Fields are the agent bead fields a refusal writes.
func (r Refusal) Fields() map[string]string {
	return map[string]string{"agent_state": AgentState, FieldCode: r.Code, FieldReason: r.Reason}
}

// Comment is the one comment a refusal leaves on the agent bead.
func (r Refusal) Comment() string {
	return fmt.Sprintf("Spawn refused by the Lands spawn wall (%s): %s", r.Code, r.Reason)
}

// TenantSpawnsFromEnv reads EnvTenantSpawns.
func TenantSpawnsFromEnv() bool {
	on, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(EnvTenantSpawns)))
	return err == nil && on
}

// Unknown is the refusal for a tenant land whose state cannot be read: fail
// closed is the rule for a tenant's money.
func Unknown(project string, err error) Refusal {
	return Refusal{Code: CodePurseUnknown, Reason: fmt.Sprintf("land %q could not be read, so its purse is unknown: %v", project, err)}
}

// Unfenced refuses a tenant land's spawn that the controller cannot place in
// the land's namespace (step 10.2), for why.
func Unfenced(project, why string) Refusal {
	return Refusal{Code: CodeUnfenced, Reason: fmt.Sprintf("land %q cannot be fenced yet: %s", project, why), Tenant: true}
}

// Check decides whether a new spawn (or a gardener fire) of role into project
// may go ahead. An operator land passes, and so does a project with no live project
// bead unless it is known as a tenant land (purse.KnownTenant: a closed
// tenant project bead or a lands-<land> account), which is refused. A
// failed read of the land itself returns the error and no refusal, because it
// cannot tell a tenant land from an operator one: the caller decides (Unknown
// fails it closed). A tenant land whose purse cannot be read is refused as
// unknown, and so is every tenant land when the lands-pause gate is open or
// cannot be read: unlike gardeners-pause, that gate fails closed. With tenant
// spawns off only the steward (StewardSpawn) gets past to the purse.
func Check(ctx context.Context, src purse.Source, project, role string, tenantSpawns bool, now time.Time) (Refusal, error) {
	r, tenant, err := killSwitches(ctx, src, project)
	if err != nil || r.Refused() || !tenant {
		return r, err
	}
	if !tenantSpawns && !StewardSpawn(role) {
		return Refusal{Code: CodeTenantSpawnsOff, Reason: fmt.Sprintf("tenant spawns are off (%s), so no agent but the steward starts on tenant land %q", EnvTenantSpawns, project), Tenant: true}, nil
	}
	p, err := purse.Read(ctx, src, project, now)
	if err != nil {
		p = purse.Unreadable(project, err, now)
	}
	switch {
	case p.State == purse.StateWall:
		return Refusal{Code: CodePurseWall, Reason: fmt.Sprintf("land %q's purse is at the wall: %s credits remaining, margin %.2f", project, credits(p.RemainingCredits), p.MarginCredits), Tenant: true}, nil
	case p.Blocked:
		return Refusal{Code: CodePurseUnknown, Reason: fmt.Sprintf("land %q's purse is unknown: %s", project, p.Reason), Tenant: true}, nil
	}
	return Refusal{Tenant: true}, nil
}

// StewardSpawn reports whether role is exactly the steward's role, the one
// role Check lets through with tenant spawns off. A role list that names the
// steward beside another role (IsSteward's "crew,steward") is not the
// steward, and neither is any role that only starts with the name.
func StewardSpawn(role string) bool {
	return strings.TrimSpace(role) == StewardRole
}

// Halted is the kill-switch half of Check, for what keeps a running tenant
// agent going rather than starting a new one: a GitHub token's mint, reuse
// and refresh (gd-Ld5r7Ok2rK). It refuses a frozen land, a known tenant land
// with no live project bead, and every tenant land while the lands-pause gate
// is open or unreadable. A land that cannot be read is refused as Unknown,
// since it is not known to be the operator's. Tenant spawns and the purse are
// not its concern: running agents continue past the purse wall (§3.4).
func Halted(ctx context.Context, src purse.Source, project string) Refusal {
	r, _, err := killSwitches(ctx, src, project)
	if err != nil {
		return Unknown(project, err)
	}
	return r
}

// killSwitches reads project's land and the switches that stop everything on
// it. tenant reports a tenant land (a refused one included); an operator land
// or a project that is no land at all is never refused. A failed read returns
// the error and no refusal.
func killSwitches(ctx context.Context, src purse.Source, project string) (r Refusal, tenant bool, err error) {
	tenant, frozen, err := purse.LandOf(ctx, src, project)
	switch {
	case errors.Is(err, purse.ErrNotLand):
		why, kerr := purse.KnownTenant(ctx, src, project)
		switch {
		case kerr != nil:
			return Refusal{}, false, kerr
		case why != "":
			return Refusal{Code: CodeLandGone, Reason: fmt.Sprintf("land %q is a tenant land with no live project bead (%s), so nothing starts on it", project, why), Tenant: true}, true, nil
		}
		return Refusal{}, false, nil
	case err != nil:
		return Refusal{}, false, err
	case !tenant:
		return Refusal{}, false, nil
	case frozen:
		return Refusal{Code: CodeFrozen, Reason: fmt.Sprintf("land %q is frozen (land_frozen)", project), Tenant: true}, true, nil
	}
	return Paused(ctx, src), true, nil
}

// Paused is the all-lands kill switch (Track B §5.5): a refusal while a
// lands-pause gate is open, and while the gate cannot be read, because no
// tenant agent starts on a switch nobody can see. The zero Refusal when the
// gate is closed.
func Paused(ctx context.Context, src pausegate.Lister) Refusal {
	g, err := pausegate.Find(ctx, src, pausegate.LandsPauseTitle)
	switch {
	case err != nil:
		return Refusal{Code: CodeLandsPaused, Tenant: true, Reason: fmt.Sprintf(
			"the %s gate could not be read, and it fails closed, so no tenant agent starts: %v", pausegate.LandsPauseTitle, err)}
	case g != nil:
		return Refusal{Code: CodeLandsPaused, Tenant: true, Reason: fmt.Sprintf(
			"every tenant land is paused (%s gate %s); gb lands resume lifts it", pausegate.LandsPauseTitle, g.ID)}
	}
	return Refusal{}
}

// Rate is the per-land spawn rate (Track B §5.4, step 10.4), decided for one
// new spawn after Check let a tenant land's spawn through: limit is keyed by
// land, and beadID makes the agent's slot its own, so the event door and the
// reconciler deciding the same agent count it once. A land at its cap is
// refused with CodeSpawnRate; the bead stays open and starts once the oldest
// spawn in the window ages out. A nil or disabled limit admits.
func Rate(limit *spawnguard.Keyed, project, beadID string) Refusal {
	if ok, _ := limit.Allow(project, beadID); ok {
		return Refusal{Tenant: true}
	}
	return Refusal{Code: CodeSpawnRate, Tenant: true, Reason: fmt.Sprintf(
		"land %q started its cap of %d agents in the last %s; this one starts when the oldest leaves the window",
		project, limit.Max(), limit.Window())}
}

func credits(v *float64) string {
	if v == nil {
		return "?"
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

// NewSpawn reports whether an agent in agent_state has not started yet, so
// creating its pod is a new spawn the wall decides. Recreating the pod of an
// agent that already runs (spot loss, image drift) is not: running agents
// continue past the purse wall (§3.4), and the purse watcher (step 8.4) parks
// them. Such a recreate is asked of Halted alone (gd-Nc7yMpW7U6).
func NewSpawn(agentState string) bool {
	switch agentState {
	case "", "spawning", "queued", AgentState:
		return true
	}
	return false
}

// Recorder is the slice of the daemon client that records a refusal.
type Recorder interface {
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// Record writes a refusal onto the agent bead: the Fields and one Comment.
// fields are the bead's current fields; a bead already refused with the same
// code is left alone, so the reconciler re-deciding a refused bead every pass
// leaves one comment, not one per pass. wrote reports a write was made.
func Record(ctx context.Context, rec Recorder, beadID string, fields map[string]string, r Refusal) (wrote bool, err error) {
	if !r.Refused() || beadID == "" {
		return false, nil
	}
	if fields[FieldCode] == r.Code && fields["agent_state"] == AgentState {
		return false, nil
	}
	if err := rec.UpdateBeadFields(ctx, beadID, r.Fields()); err != nil {
		return false, fmt.Errorf("stamping the spawn wall refusal on %s: %w", beadID, err)
	}
	if err := rec.AddComment(ctx, beadID, CommentAuthor, r.Comment()); err != nil {
		return true, fmt.Errorf("commenting the spawn wall refusal on %s: %w", beadID, err)
	}
	return true, nil
}
