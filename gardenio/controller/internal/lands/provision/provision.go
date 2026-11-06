// Package provision is the Lands provisioner (Lands Track B spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md §3.3,
// step 8.6): the controller loop that turns an approved plot_application into
// a land, and takes a land apart again when the operator closes it.
//
// Provisioning runs §3.3 steps 1-7, in order:
//
//  1. project: the land's type:project bead (land_kind=tenant, the repo, the
//     pinned Lands App installation, Slack mirroring off).
//  2. purse: the lands-<land> account bead and its LiteLLM virtual key, the
//     initial credit_grant from the approval, the key's max_budget derived
//     from the whole ledger, and the project's account pinned to it.
//  3. keycloak: the group project-<land>, its owners subgroup with the
//     applicant in it, and the applicant in lands-tenants.
//  4. channels: the land's #general, #crew, #decisions, #escalations,
//     #support and #machines, all with mirror=off (B-D11), and
//     land_default_channel.
//  5. namespace: the land's namespace land-<land> from the §5.2 template
//     (internal/lands/landns, step 10.2), and the project's namespace and
//     runtime_class (RuntimeClass, empty for the node's default runtime),
//     which record where its pods run.
//  6. steward: the land-bound gardener (internal/steward, step 9.3), woken
//     once by a post in #machines so its first run greets #general.
//  7. mail: "your land is ready" to the applicant; the application becomes
//     provisioned and the project gets land_opened_at.
//
// The steward is the one tenant agent the spawn wall lets past tenant spawns
// off (LANDS_TENANT_SPAWNS; owner ruling gd-KOAoUELMWo, internal/lands/wall).
// Every other wall refusal (frozen, lands-pause, purse) still stops its wake,
// and then the greeting waits for the land's next wake.
//
// Every step is idempotent. Each one records itself in provision_step on the
// application when it is done, and a pass resumes after the last recorded
// step, so a controller that dies between any two steps finishes the land on
// its next pass. A step that fails sets the application failed with the error,
// files one operator escalation carrying the application and the step, and
// stops; gb lands provision --retry moves it back to provisioning, and the
// next pass resumes from provision_step.
//
// Closing (close.go) is the same machine in reverse, driven by gb lands close.
// A provisioned land whose steward closed is bound a fresh one (rebind.go).
// Member invites (invites.go, step 8.9) are settled on the same pass, after
// the applications, and so is the gatehouse (gatehouse.go, step 9.12).
package provision

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/mailer"
)

// Interval is how often the provisioner looks for work.
const Interval = time.Minute

// NamespaceReconcile is how often a provisioned land's namespace is applied
// again (reconcileNamespace), besides once on every controller start.
const NamespaceReconcile = time.Hour

// Application states (kbeads plot_application.go).
const (
	StateApproved     = "approved"
	StateProvisioning = "provisioning"
	StateProvisioned  = "provisioned"
	StateFailed       = "failed"
	StateClosed       = "closed"
)

// Provisioning steps, recorded in provision_step when done.
const (
	StepProject   = "project"
	StepPurse     = "purse"
	StepKeycloak  = "keycloak"
	StepChannels  = "channels"
	StepNamespace = "namespace"
	StepSteward   = "steward"
	StepMail      = "mail"
)

// Steps is the provisioning order.
var Steps = []string{StepProject, StepPurse, StepKeycloak, StepChannels, StepNamespace, StepSteward, StepMail}

const (
	// ApplicationType and IntakeLabel are where applications live (B-D4).
	ApplicationType = "plot_application"
	IntakeLabel     = "project:lands-intake"

	// Author signs the provisioner's comments and escalations.
	Author = "gardenio-controller"

	// DefaultMaxLiveAgents is a tenant land's max_live_agents (§3.2).
	DefaultMaxLiveAgents = 3

	// StewardSettle is how old the land's steward trigger must be before the
	// provisioner wakes it: trigger-engine loads triggers every 30s and drops
	// an event no loaded trigger matches, so a wake posted the moment the
	// trigger is made could be lost.
	StewardSettle = 2 * time.Minute
)

// DefaultChannels are a tenant land's channels and their channel_kind: the
// four Commons defaults (Commons step 2.3), #support (§6.1), whose kind is a
// user channel, and #machines, where the land's agents and the controller
// talk so #general is left to people and the steward (steward amendment
// 2026-10-23 §2). #general comes first; it is the land's default channel.
var DefaultChannels = []struct{ Name, Kind string }{
	{"general", "general"},
	{"crew", "crew"},
	{"decisions", "decisions"},
	{"escalations", "escalations"},
	{"support", "user"},
	{"machines", "machines"},
}

// Daemon is the slice of the daemon client the provisioner reads and writes
// through, as the controller's machine principal (kbeads machine_grants.go,
// gardenio-machine-lands-provisioner). *beadsapi.Client satisfies it.
type Daemon interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// Keys is the LiteLLM key admin the purse step needs (litellm.go).
type Keys interface {
	// Ensure makes sure the virtual key exists, creating it with alias,
	// max_budget and metadata when it does not. An existing key is left as
	// it is.
	Ensure(ctx context.Context, key, alias string, maxBudgetUSD float64, metadata map[string]string) error
	// SetBudget sets the key's max_budget.
	SetBudget(ctx context.Context, key string, maxBudgetUSD float64) error
	// Delete revokes the key; a key LiteLLM does not know is already gone.
	Delete(ctx context.Context, key string) error
}

// Groups is the Keycloak admin the keycloak step, the invites and close
// need. *keycloakadmin.Client satisfies it.
type Groups interface {
	EnsureProjectGroup(ctx context.Context, land string) (string, error)
	EnsureLandRoleGroup(ctx context.Context, land string, role keycloakadmin.LandRole) (string, error)
	AddLandMember(ctx context.Context, land string, role keycloakadmin.LandRole, userID string) error
	AddTenant(ctx context.Context, userID string) error
	LandMembers(ctx context.Context, land string) ([]keycloakadmin.LandMember, error)
	RemoveFromLand(ctx context.Context, land, userID string) error
	UserLands(ctx context.Context, userID string) ([]string, error)
	RemoveTenant(ctx context.Context, userID string) error
	FindUserByGitHubID(ctx context.Context, id int64) (keycloakadmin.User, error)
	FindUserByEmail(ctx context.Context, email string) (keycloakadmin.User, error)
	AddInvited(ctx context.Context, userID string) error
	InvitedMembers(ctx context.Context) ([]keycloakadmin.User, error)
}

// Repos reads a repository's default branch on github.com through the Lands
// App installation the applicant made.
type Repos interface {
	DefaultBranch(ctx context.Context, installationID int64, repo string) (string, error)
}

// Namespaces applies and deletes a land's namespace. *landns.Applier
// satisfies it.
type Namespaces interface {
	// Apply makes land's namespace from the template, or brings it back to
	// it, and returns its name.
	Apply(ctx context.Context, land string, maxLiveAgents int) (string, error)
	// Delete deletes land's namespace; a namespace that is gone is done.
	Delete(ctx context.Context, land string) error
}

// LandScopes makes a land's land-<land> client scope, whose token audience
// narrows a bearer to the land (gd-eAfptUBh9Z). *landscope.Applier satisfies
// it: it writes a KeycloakClientScope resource for the keycloak-operator, so
// the provisioner's Keycloak credential never needs manage-clients.
type LandScopes interface {
	Ensure(ctx context.Context, land string) error
}

// Stewards binds and wakes a land's steward (§3.3 step 6, Phase 9).
// steward.ProvisionHook satisfies it.
type Stewards interface {
	// Bind binds the land's one steward, or finds the one it has, and
	// returns its trigger bead. operator is the approver, as an operated-by
	// alias (oidc:<email>), or "".
	Bind(ctx context.Context, land, operator string) (*beadsapi.BeadDetail, error)
	// Wake wakes the land's steward once, with a post its land-opened wake
	// entry fires on. key makes the post idempotent.
	Wake(ctx context.Context, land, key string) error
	// Greet wakes the land's steward once to greet a person who just joined
	// it (step 9.14), with a post its person-joined wake entry fires on. name
	// is what the greeting calls them, never an email address ("" for
	// nothing), and role their land role. key makes the post idempotent.
	Greet(ctx context.Context, land, name, role, key string) error
	// Upgrade brings the land's open steward trigger up to the wake entries
	// a fresh bind writes; one already up to date is not written.
	Upgrade(ctx context.Context, land string, trig *beadsapi.BeadDetail) error
	// Retire disarms and closes the land's steward trigger, noting why on
	// it. A land with no open steward is done.
	Retire(ctx context.Context, land, why string) error
	// Find returns the land's open steward trigger, or, when it has none,
	// the one that closed last (nil when it never had one).
	Find(ctx context.Context, land string) (open, last *beadsapi.BeadDetail, err error)
}

// Provisioner is the provisioning and closing loop. Daemon, Keys, Groups and
// Repos are required; a nil or disabled Mailer sends no mail. A nil
// Namespaces fails the namespace step: a land with no fence never opens. A
// nil Stewards fails the steward step: a land opens with its steward. A nil
// LandScopes makes no land scope: the land opens, and its land door refuses
// every token until one exists.
type Provisioner struct {
	Daemon     Daemon
	Keys       Keys
	Groups     Groups
	Repos      Repos
	Namespaces Namespaces
	Stewards   Stewards
	LandScopes LandScopes
	Mailer     mailer.Mailer
	Templates  *mailer.Templates
	Logger     *slog.Logger

	// RuntimeClass is the RuntimeClass the namespace step stamps as the land
	// project's runtime_class (LANDS_RUNTIME_CLASS, the value the
	// controller's land fence puts tenant pods under). Empty, the default,
	// is the node's default runtime (gd-iyZom5u11A).
	RuntimeClass string

	// AccountTemplate names the account bead whose provider, base_url,
	// credential and model a land's lands-<land> account copies: what the
	// land's agents authenticate upstream with (the credential stays the
	// fleet's by owner ruling gd-NXTrUXD1Sq; an operator may repoint one land
	// at secret:lands-<land>). Its own LiteLLM key is never copied; the
	// land's key is its own.
	AccountTemplate string
	// ReservedLands are land names the provisioner refuses besides the ones
	// keycloakadmin and kbeads already refuse.
	ReservedLands []string
	// DemoLand is the land the viewer's /lands/demo door hands guest passes
	// for (LANDS_DEMO_LAND); "" for none. A joined guest there is taken out
	// DemoGuestTTL (default DefaultDemoGuestTTL) after joining (invites.go).
	DemoLand     string
	DemoGuestTTL time.Duration
	// Gatehouse is the land every signed-in person with no land is a guest
	// of (LANDS_GATEHOUSE_LAND, gatehouse.go); "" for none.
	Gatehouse string

	// Now defaults to time.Now; NewKey to a random sk- key.
	Now    func() time.Time
	NewKey func() string

	// landScopesDone records the provisioned lands whose client scope this
	// process has already reconciled (backfillLandScope).
	landScopesDone map[string]bool
	// namespacesApplied is when this process last applied each land's
	// namespace (stepNamespace, reconcileNamespace).
	namespacesApplied map[string]time.Time
	// inviteMailFailed is when each sign-in invite's last mail send failed,
	// so the pass retries it at most every InviteMailRetry (email_invites.go).
	inviteMailFailed map[string]time.Time
}

// Application is a plot_application as the provisioner reads it.
type Application struct {
	ID               string
	State            string
	LandName         string
	Repo             string
	ApplicantSub     string
	ApplicantEmail   string
	GitHubLogin      string
	InstallationID   int64
	CreditsGranted   float64
	Model            string
	DecisionID       string
	DecidedBy        string
	Step             string
	Attempt          int
	Error            string
	CloseRequestedAt string
	CloseRequestedBy string
	CloseReason      string
	CloseStep        string
}

// ApplicationFromBead reads b, or reports false when b is not a plot
// application in lands-intake.
func ApplicationFromBead(b *beadsapi.BeadDetail) (*Application, bool) {
	if b == nil || b.Type != ApplicationType || !slices.Contains(b.Labels, IntakeLabel) {
		return nil, false
	}
	f := b.Fields
	inst, _ := strconv.ParseInt(strings.TrimSpace(f["github_installation_id"]), 10, 64)
	return &Application{
		ID: b.ID, State: f["state"], LandName: f["land_name"], Repo: f["repo"],
		ApplicantSub: f["applicant_sub"], ApplicantEmail: f["applicant_email"], GitHubLogin: f["github_login"],
		InstallationID: inst, CreditsGranted: purse.FieldFloat(f, "credits_granted_usd"), Model: f["model"],
		DecisionID: f["decision_id"], DecidedBy: f["decided_by"],
		Step: f["provision_step"], Attempt: purse.FieldInt(f, "provision_attempt"), Error: f["provision_error"],
		CloseRequestedAt: f["close_requested_at"], CloseRequestedBy: f["close_requested_by"],
		CloseReason: f["close_reason"], CloseStep: f["close_step"],
	}, true
}

// AccountName is the land's LiteLLM account and key alias.
func AccountName(land string) string { return purse.AccountName(land) }

// Run passes every interval until ctx is cancelled, with one pass on start.
func (p *Provisioner) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = Interval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := p.Pass(ctx); err != nil {
			p.logger().Warn("lands provisioner: pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass provisions every approved or provisioning application, closes every
// land whose close was requested, re-applies a provisioned land's namespace
// when it is due (reconcileNamespace), binds a fresh steward for a
// provisioned land whose steward closed (rebind.go), then settles the member
// invites and the sign-in invites (email_invites.go). One application's
// failure never stops the others; it is recorded on that application.
func (p *Provisioner) Pass(ctx context.Context) error {
	apps, err := p.applications(ctx, StateApproved, StateProvisioning, StateProvisioned, StateFailed)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case app.CloseRequestedAt != "":
			// A provisioning application finishes (or fails) first; the
			// close runs on the pass after.
			if app.State != StateProvisioning {
				p.Close(ctx, app)
			}
		case app.State == StateApproved || app.State == StateProvisioning:
			p.Provision(ctx, app)
		case app.State == StateProvisioned:
			p.backfillLandScope(ctx, app)
			p.reconcileNamespace(ctx, app)
			p.rebindSteward(ctx, app)
		}
	}
	// Each invite pass runs whether or not the other could read its beads.
	// The gatehouse backfill runs before the sign-in invites: its people are
	// in invited already.
	return errors.Join(p.Invites(ctx), p.GatehouseGuests(ctx), p.SigninInvites(ctx))
}

// backfillLandScope gives a provisioned land its land-<land> client scope,
// once per process: a land provisioned before per-land audiences
// (gd-eAfptUBh9Z), or one whose Keycloak step could not make it. Ensure is
// idempotent, so the first pass after a controller start reconciles every
// provisioned land and later passes skip the ones done. A failure is logged
// and retried on the next pass; it never touches the application.
func (p *Provisioner) backfillLandScope(ctx context.Context, app *Application) {
	if p.LandScopes == nil {
		return
	}
	if p.landScopesDone == nil {
		p.landScopesDone = map[string]bool{}
	}
	if p.landScopesDone[app.LandName] {
		return
	}
	if err := p.LandScopes.Ensure(ctx, app.LandName); err != nil {
		p.logger().Warn("lands provisioner: land client scope backfill failed", "land", app.LandName, "application", app.ID, "error", err)
		return
	}
	p.landScopesDone[app.LandName] = true
}

// reconcileNamespace applies a provisioned land's namespace again on the
// first pass after a controller start and then every NamespaceReconcile, so
// a changed template (egress peers or except CIDRs, the quota), a changed
// max_live_agents or a hand edit to the land's fence objects reaches lands
// opened before it (gd-pXik57wZA9). Apply is idempotent. A failure is logged
// and retried on the next pass; it never touches the application.
func (p *Provisioner) reconcileNamespace(ctx context.Context, app *Application) {
	if p.Namespaces == nil {
		return
	}
	if last, ok := p.namespacesApplied[app.LandName]; ok && p.now().Sub(last) < NamespaceReconcile {
		return
	}
	if err := p.stepNamespace(ctx, app); err != nil {
		p.logger().Warn("lands provisioner: land namespace reconcile failed", "land", app.LandName, "application", app.ID, "error", err)
	}
}

// applications lists the plot applications in any of states, re-checked
// here so a daemon that ignored the filter cannot hand the loop anything else.
func (p *Provisioner) applications(ctx context.Context, states ...string) ([]*Application, error) {
	var out []*Application
	for _, state := range states {
		beads, err := listAll(ctx, p.Daemon, beadsapi.ListBeadsQuery{
			Types: []string{ApplicationType}, Labels: []string{IntakeLabel},
			Fields: map[string]string{"state": state}, Statuses: liveStatuses,
		})
		if err != nil {
			return nil, fmt.Errorf("reading %s applications: %w", state, err)
		}
		for _, b := range beads {
			if a, ok := ApplicationFromBead(b); ok && a.State == state {
				out = append(out, a)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Provision runs app's remaining steps. It returns the error the application
// failed on (already recorded on it), or nil.
func (p *Provisioner) Provision(ctx context.Context, app *Application) error {
	log := p.logger().With("application", app.ID, "land", app.LandName)
	if app.State == StateApproved {
		attempt := app.Attempt + 1
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, app.ID, map[string]any{
			"state": StateProvisioning, "provision_attempt": attempt, "provision_error": "",
		}); err != nil {
			log.Warn("lands provisioner: could not start", "error", err)
			return err
		}
		app.State, app.Attempt, app.Error = StateProvisioning, attempt, ""
		log.Info("lands provisioner: provisioning", "attempt", attempt)
	}
	if err := p.checkApplication(app); err != nil {
		return p.fail(ctx, app, nextStep(app.Step), err)
	}
	for _, step := range remaining(app.Step) {
		if err := p.runStep(ctx, app, step); errors.Is(err, errNotYet) {
			// Not a failure: the step is re-run on a later pass.
			log.Info("lands provisioner: waiting", "step", step, "reason", err)
			return nil
		} else if err != nil {
			return p.fail(ctx, app, step, err)
		}
		if step == StepMail {
			// The last step's record is the state change itself.
			break
		}
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, app.ID, map[string]any{"provision_step": step}); err != nil {
			if refusedRecord(err) {
				// kbeads refuses this record every time (a step its schema
				// lacks), so re-running the step each pass would hide the
				// failure: fail the attempt so it escalates.
				return p.fail(ctx, app, step, fmt.Errorf("recording provision_step=%s: %w", step, err))
			}
			log.Warn("lands provisioner: could not record a step; the next pass re-runs it", "step", step, "error", err)
			return err
		}
		app.Step = step
		log.Info("lands provisioner: step done", "step", step)
	}
	log.Info("lands provisioner: provisioned")
	return nil
}

// refusedRecord reports whether kbeads refused a write as invalid (HTTP 400 or
// 422), which repeating it will not change, as opposed to a transient error.
func refusedRecord(err error) bool {
	var apiErr *beadsapi.APIError
	return errors.As(err, &apiErr) &&
		(apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusUnprocessableEntity)
}

// remaining are the steps after done ("" = none done).
func remaining(done string) []string {
	i := slices.Index(Steps, done)
	return Steps[i+1:]
}

// nextStep is the step after done.
func nextStep(done string) string {
	if r := remaining(done); len(r) > 0 {
		return r[0]
	}
	return done
}

func (p *Provisioner) runStep(ctx context.Context, app *Application, step string) error {
	switch step {
	case StepProject:
		return p.stepProject(ctx, app)
	case StepPurse:
		return p.stepPurse(ctx, app)
	case StepKeycloak:
		return p.stepKeycloak(ctx, app)
	case StepChannels:
		return p.stepChannels(ctx, app)
	case StepNamespace:
		return p.stepNamespace(ctx, app)
	case StepSteward:
		return p.stepSteward(ctx, app)
	case StepMail:
		return p.stepMail(ctx, app)
	}
	return fmt.Errorf("unknown step %q", step)
}

// checkApplication refuses an application the provisioner cannot act on
// whatever the step: one that names no land or repo, or a reserved land.
func (p *Provisioner) checkApplication(app *Application) error {
	if _, err := keycloakadmin.GroupName(app.LandName); err != nil {
		return err
	}
	if slices.Contains(p.ReservedLands, app.LandName) {
		return fmt.Errorf("land %q is reserved on this deployment", app.LandName)
	}
	if owner, name, ok := strings.Cut(app.Repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("repo %q is not owner/name", app.Repo)
	}
	return nil
}

// fail records the failure on the application, files one escalation for it
// and returns err. The escalation is keyed by the attempt, so a pass that
// fails again before it could record the failure files nothing new.
func (p *Provisioner) fail(ctx context.Context, app *Application, step string, err error) error {
	log := p.logger().With("application", app.ID, "land", app.LandName, "step", step)
	log.Error("lands provisioner: step failed", "error", err)
	msg := truncate(err.Error(), 500)
	esc, escErr := p.escalate(ctx, app, "lands_provision",
		fmt.Sprintf("Lands provisioning failed: %s at step %s", app.LandName, step),
		fmt.Sprintf("lands-provision:%s:attempt:%d", app.ID, app.Attempt),
		fmt.Sprintf("The Lands provisioner stopped provisioning land %q (plot_application %s, attempt %d) at step %q: %s\n\n"+
			"Every step before it is done and recorded in provision_step (%q). Fix the cause, then run "+
			"`gb lands provision --retry %s`; it resumes from the failed step. Nothing is rolled back.",
			app.LandName, app.ID, app.Attempt, step, msg, app.Step, app.ID),
		map[string]any{"application": app.ID, "land": app.LandName, "step": step, "attempt": app.Attempt})
	if escErr != nil {
		log.Warn("lands provisioner: could not file the escalation", "error", escErr)
	}
	fields := map[string]any{
		"state": StateFailed, "provision_error": step + ": " + msg,
		"provision_failed_at": p.now().UTC().Format(time.RFC3339),
	}
	if esc != nil {
		fields["provision_escalation_id"] = esc.ID
	}
	if uerr := p.Daemon.UpdateBeadFieldsTyped(ctx, app.ID, fields); uerr != nil {
		log.Warn("lands provisioner: could not record the failure", "error", uerr)
	} else {
		app.State, app.Error = StateFailed, step+": "+msg
	}
	return err
}

// escalate files an operator escalation about app under key and returns it:
// the one filed before under key, closed or not, when there is one. It is
// labelled project:lands-intake, which no tenant can read.
func (p *Provisioner) escalate(ctx context.Context, app *Application, opType, title, key, text string, spec map[string]any) (*beadsapi.BeadDetail, error) {
	raw, err := json.Marshal(map[string]any{
		"operation_type": opType, "operation_spec": spec,
		"context": text, "requesting_task": app.ID,
	})
	if err != nil {
		return nil, err
	}
	return p.Daemon.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
		Title: title, Type: "escalation", Kind: "data", Priority: 1,
		Labels: []string{IntakeLabel}, Fields: raw, CreatedBy: Author, IdempotencyKey: key,
	})
}

// ---- Step 1: the project bead ----------------------------------------------

// project returns the live project bead named land, or nil. Two live beads of
// one name is an error: the daemon would read the lowest id, and the
// provisioner will not guess which is the land's.
func (p *Provisioner) project(ctx context.Context, land string) (*beadsapi.BeadDetail, error) {
	beads, err := listAll(ctx, p.Daemon, beadsapi.ListBeadsQuery{Types: []string{"project"}, Statuses: liveStatuses})
	if err != nil {
		return nil, fmt.Errorf("reading projects: %w", err)
	}
	var found []*beadsapi.BeadDetail
	for _, b := range beads {
		if projectName(b) == land {
			found = append(found, b)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("%d live project beads are named %q", len(found), land)
}

// projectName is a project bead's name: its title without "Project: ", as
// beadsapi.ListProjectBeads reads it.
func projectName(b *beadsapi.BeadDetail) string {
	return strings.TrimSpace(strings.TrimPrefix(b.Title, "Project: "))
}

// landProject returns the land's project bead, which must be the one this
// application made.
func (p *Provisioner) landProject(ctx context.Context, app *Application) (*beadsapi.BeadDetail, error) {
	b, err := p.project(ctx, app.LandName)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("land %q has no project bead", app.LandName)
	}
	if got := b.Fields["land_application_id"]; got != app.ID {
		return nil, fmt.Errorf("project %q (%s) belongs to application %q, not %s", app.LandName, b.ID, got, app.ID)
	}
	return b, nil
}

func (p *Provisioner) stepProject(ctx context.Context, app *Application) error {
	existing, err := p.project(ctx, app.LandName)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Fields["land_application_id"] != app.ID {
			return fmt.Errorf("the name %q is already project %s; it is not this application's land", app.LandName, existing.ID)
		}
		return nil
	}
	if app.InstallationID <= 0 {
		return errors.New("the application has no github_installation_id")
	}
	branch, err := p.Repos.DefaultBranch(ctx, app.InstallationID, app.Repo)
	if err != nil {
		return fmt.Errorf("reading %s's default branch: %w", app.Repo, err)
	}
	if branch == "" {
		return fmt.Errorf("GitHub named no default branch for %s", app.Repo)
	}
	prefix, err := p.prefix(ctx, app.LandName)
	if err != nil {
		return err
	}
	gitURL := "https://github.com/" + app.Repo + ".git"
	fields := map[string]any{
		"prefix": prefix, "git_url": gitURL, "default_branch": branch,
		"repos":     []map[string]string{{"url": gitURL, "role": "primary", "branch": branch}},
		"land_kind": "tenant", "land_owner_sub": app.ApplicantSub, "land_application_id": app.ID,
		"land_slack_mirror": "off", "github_app": "lands",
		"github_installation_id": strconv.FormatInt(app.InstallationID, 10),
		"max_live_agents":        DefaultMaxLiveAgents,
		// Triggers on: the steward is a trigger, and triggers_enabled=false
		// skips every trigger of the project, so a land born with it off
		// never woke its steward (gd-AQ9iCiKxML) and nothing turns it on. A
		// land's own controls are the spawn wall, the purse, land freeze and
		// lands-pause. Schedules stay off: they silence the land's plan
		// gardeners but not its steward (bridge scheduleGatedGardener).
		"triggers_enabled": true, "schedules_enabled": false,
	}
	if app.Model != "" {
		fields["model"] = app.Model
	}
	if p.Gatehouse != "" && app.LandName == p.Gatehouse {
		// Marked from birth, so the steward's first wake greets as the
		// gatehouse (gatehouse.go).
		fields[GatehouseField] = true
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	b, err := p.Daemon.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
		Title: app.LandName, Type: "project", Kind: "config",
		Description: fmt.Sprintf("Tenant land %s on github.com/%s, provisioned from plot_application %s.", app.LandName, app.Repo, app.ID),
		Labels:      []string{"project:" + app.LandName}, Fields: raw, CreatedBy: Author,
		IdempotencyKey: "lands-provision:" + app.ID + ":project",
	})
	if err != nil {
		return fmt.Errorf("creating project %s: %w", app.LandName, err)
	}
	if b.Type != "project" || projectName(b) != app.LandName || b.Fields["land_application_id"] != app.ID {
		return fmt.Errorf("the project create for %s returned bead %s (%s %q), not this land's project", app.LandName, b.ID, b.Type, b.Title)
	}
	return nil
}

// prefix derives the land's bead prefix from its name: the letters and digits
// of the name, with a number appended when another project already uses it.
func (p *Provisioner) prefix(ctx context.Context, land string) (string, error) {
	base := strings.ReplaceAll(land, "-", "")
	beads, err := listAll(ctx, p.Daemon, beadsapi.ListBeadsQuery{Types: []string{"project"}, Statuses: allStatuses})
	if err != nil {
		return "", fmt.Errorf("reading projects: %w", err)
	}
	taken := map[string]bool{}
	for _, b := range beads {
		taken[strings.ToLower(strings.TrimSpace(b.Fields["prefix"]))] = true
	}
	for n := 1; n < 100; n++ {
		cand := base
		if n > 1 {
			cand += strconv.Itoa(n)
		}
		if !taken[cand] {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no free bead prefix for %q", land)
}

// ---- Step 2: the purse -------------------------------------------------------

func (p *Provisioner) stepPurse(ctx context.Context, app *Application) error {
	proj, err := p.landProject(ctx, app)
	if err != nil {
		return err
	}
	rates, err := purse.LoadRates(ctx, p.Daemon)
	if err != nil {
		return err
	}
	rate := purse.InForce(rates)
	if rate == nil {
		return errors.New("no credit rate is set, so the land's grant cannot be priced: the operator sets one with `gb lands rates set --llm-multiplier <x>`")
	}
	acct, err := p.ensureAccount(ctx, app)
	if err != nil {
		return err
	}
	key := strings.TrimSpace(acct.Fields["litellm_key"])
	if key == "" {
		// The key is written to the account before LiteLLM learns it, so a
		// crash between the two leaves a known key to create, never an
		// orphan with a budget nobody can see.
		key = p.newKey()
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, acct.ID, map[string]any{"litellm_key": key}); err != nil {
			return fmt.Errorf("recording the land key on %s: %w", acct.Title, err)
		}
	}
	alias := AccountName(app.LandName)
	// A new key is born with max_budget 0, which LiteLLM refuses every call
	// on, and gets its real budget from the ledger below.
	if err := p.Keys.Ensure(ctx, key, alias, 0, map[string]string{
		"land": app.LandName, "plot_application": app.ID,
	}); err != nil {
		return fmt.Errorf("creating the LiteLLM key %s: %w", alias, err)
	}
	if app.CreditsGranted > 0 {
		if err := p.ensureGrant(ctx, app, *rate); err != nil {
			return err
		}
	}
	grants, err := purse.LoadGrants(ctx, p.Daemon, app.LandName)
	if err != nil {
		return err
	}
	if err := p.Keys.SetBudget(ctx, key, purse.KeyBudgetUSD(purse.SumCredits(grants), *rate)); err != nil {
		return fmt.Errorf("setting %s's max_budget: %w", alias, err)
	}
	if proj.Fields["account"] != alias {
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, proj.ID, map[string]any{"account": alias}); err != nil {
			return fmt.Errorf("pinning %s to account %s: %w", app.LandName, alias, err)
		}
	}
	return nil
}

// ensureAccount returns the land's lands-<land> account bead, creating it
// from the account template.
func (p *Provisioner) ensureAccount(ctx context.Context, app *Application) (*beadsapi.BeadDetail, error) {
	name := AccountName(app.LandName)
	accounts, err := listAll(ctx, p.Daemon, beadsapi.ListBeadsQuery{Types: []string{"account"}, Statuses: liveStatuses})
	if err != nil {
		return nil, fmt.Errorf("reading accounts: %w", err)
	}
	var tmpl *beadsapi.BeadDetail
	for _, b := range accounts {
		switch b.Title {
		case name:
			// kbeads refused this application if the land was a project
			// already, so lands-<land> is a previous attempt's.
			return b, nil
		case p.AccountTemplate:
			tmpl = b
		}
	}
	if p.AccountTemplate == "" || tmpl == nil {
		return nil, fmt.Errorf("account template %q not found: set LANDS_ACCOUNT_TEMPLATE to the account whose provider and credential tenant lands use", p.AccountTemplate)
	}
	fields := map[string]any{}
	for _, k := range []string{"provider", "base_url", "credential", "model"} {
		if v := strings.TrimSpace(tmpl.Fields[k]); v != "" {
			fields[k] = v
		}
	}
	if app.Model != "" {
		fields["model"] = app.Model
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	b, err := p.Daemon.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
		Title: name, Type: "account", Kind: "config",
		Description: fmt.Sprintf("The purse of tenant land %s: its own LiteLLM key, billed at the credit rate (Track B §3.4).", app.LandName),
		Labels:      []string{"project:" + app.LandName}, Fields: raw, CreatedBy: Author,
		IdempotencyKey: "lands-provision:" + app.ID + ":account",
	})
	if err != nil {
		return nil, fmt.Errorf("creating account %s: %w", name, err)
	}
	if b.Type != "account" || b.Title != name {
		return nil, fmt.Errorf("the account create for %s returned bead %s (%s %q)", name, b.ID, b.Type, b.Title)
	}
	return b, nil
}

// ensureGrant writes the approval's credit_grant once: the idempotency key
// makes a re-run return the grant already written.
func (p *Provisioner) ensureGrant(ctx context.Context, app *Application, rate purse.Rate) error {
	grantedBy := app.DecidedBy
	if grantedBy == "" {
		grantedBy = Author
	}
	fields := map[string]any{
		"land": app.LandName, "amount_credits": app.CreditsGranted, "granted_by": grantedBy,
		"rate_version": rate.Version, "reason": "initial grant on approval of plot_application " + app.ID,
	}
	if app.DecisionID != "" {
		fields["decision_id"] = app.DecisionID
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	b, err := p.Daemon.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
		Title: fmt.Sprintf("Credit grant: %s %+g credits", app.LandName, app.CreditsGranted),
		Type:  "credit_grant", Kind: "data", Status: "closed",
		Labels: []string{"project:" + app.LandName}, Fields: raw, CreatedBy: Author,
		IdempotencyKey: "lands-provision:" + app.ID + ":grant",
	})
	if err != nil {
		return fmt.Errorf("writing the initial credit_grant: %w", err)
	}
	if b.Type != "credit_grant" || b.Fields["land"] != app.LandName {
		return fmt.Errorf("the grant create for %s returned bead %s (%s)", app.LandName, b.ID, b.Type)
	}
	return nil
}

// ---- Step 3: Keycloak --------------------------------------------------------

func (p *Provisioner) stepKeycloak(ctx context.Context, app *Application) error {
	if _, err := p.Groups.EnsureProjectGroup(ctx, app.LandName); err != nil {
		return err
	}
	// The land scope only opens the land door; a land without one is fenced,
	// not exposed (its door refuses every token). So a failure here never
	// fails the land: backfillLandScope retries it once the land is open.
	if p.LandScopes == nil {
		p.logger().Warn("lands provisioner: no land scope writer; the land door refuses every token until one exists", "land", app.LandName)
	} else if err := p.LandScopes.Ensure(ctx, app.LandName); err != nil {
		p.logger().Warn("lands provisioner: land client scope not made; retried once the land is open", "land", app.LandName, "application", app.ID, "error", err)
	}
	if _, err := p.Groups.EnsureLandRoleGroup(ctx, app.LandName, keycloakadmin.RoleOwner); err != nil {
		return err
	}
	if err := p.Groups.AddLandMember(ctx, app.LandName, keycloakadmin.RoleOwner, app.ApplicantSub); err != nil {
		return err
	}
	return p.Groups.AddTenant(ctx, app.ApplicantSub)
}

// ---- Step 4: channels --------------------------------------------------------

func (p *Provisioner) stepChannels(ctx context.Context, app *Application) error {
	proj, err := p.landProject(ctx, app)
	if err != nil {
		return err
	}
	existing, err := listAll(ctx, p.Daemon, beadsapi.ListBeadsQuery{
		Types: []string{"channel"}, Labels: []string{"project:" + app.LandName}, Statuses: liveStatuses,
	})
	if err != nil {
		return fmt.Errorf("reading %s's channels: %w", app.LandName, err)
	}
	byName := map[string]*beadsapi.BeadDetail{}
	for _, b := range existing {
		if n := b.Fields["name"]; n != "" && byName[n] == nil {
			byName[n] = b
		}
	}
	var general string
	for _, ch := range DefaultChannels {
		b := byName[ch.Name]
		if b == nil {
			// No mirror field: unset is off, and kbeads refuses any Slack
			// binding field (mirror, slack_channel_id) on a create from a
			// non-admin principal, even "off" (channel_binding_authz.go).
			raw, err := json.Marshal(map[string]any{"name": ch.Name, "channel_kind": ch.Kind})
			if err != nil {
				return err
			}
			b, err = p.Daemon.CreateBeadDetail(ctx, beadsapi.CreateBeadRequest{
				Title: "#" + ch.Name, Type: "channel", Kind: "data",
				Description: fmt.Sprintf("Default #%s channel of land %s, made by the Lands provisioner. Not mirrored to Slack (B-D11).", ch.Name, app.LandName),
				Labels:      []string{"project:" + app.LandName}, Fields: raw, CreatedBy: Author,
				IdempotencyKey: "lands-provision:" + app.ID + ":channel:" + ch.Name,
			})
			if err != nil {
				return fmt.Errorf("creating #%s in %s: %w", ch.Name, app.LandName, err)
			}
		}
		if m := b.Fields["mirror"]; m != "" && m != "off" {
			return fmt.Errorf("#%s in %s (%s) mirrors to Slack (mirror=%s); a tenant land's channels never do", ch.Name, app.LandName, b.ID, m)
		}
		if ch.Name == "general" {
			general = b.ID
		}
	}
	if general != "" && proj.Fields["land_default_channel"] != general {
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, proj.ID, map[string]any{"land_default_channel": general}); err != nil {
			return fmt.Errorf("setting %s's default channel: %w", app.LandName, err)
		}
	}
	return nil
}

// ---- Step 5: namespace and runtime -------------------------------------------

// ErrNamespacesOff is the namespace step's error when the controller has no
// land namespaces configured.
var ErrNamespacesOff = errors.New("land namespaces are off on this controller (LANDS_NAMESPACES_ENABLED); " +
	"a tenant land does not open without its namespace and NetworkPolicy")

func (p *Provisioner) stepNamespace(ctx context.Context, app *Application) error {
	if p.Namespaces == nil {
		return ErrNamespacesOff
	}
	proj, err := p.landProject(ctx, app)
	if err != nil {
		return err
	}
	maxLive := purse.FieldInt(proj.Fields, "max_live_agents")
	if maxLive <= 0 {
		maxLive = DefaultMaxLiveAgents
	}
	ns, err := p.Namespaces.Apply(ctx, app.LandName, maxLive)
	if err != nil {
		return fmt.Errorf("applying %s's namespace: %w", app.LandName, err)
	}
	if p.namespacesApplied == nil {
		p.namespacesApplied = map[string]time.Time{}
	}
	p.namespacesApplied[app.LandName] = p.now()
	if proj.Fields["namespace"] != ns || proj.Fields["runtime_class"] != p.RuntimeClass {
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, proj.ID, map[string]any{
			"namespace": ns, "runtime_class": p.RuntimeClass,
		}); err != nil {
			return fmt.Errorf("placing %s's pods in %s: %w", app.LandName, ns, err)
		}
	}
	return nil
}

// ---- Step 6: steward ---------------------------------------------------------

// errNotYet ends a pass at a step that is not done and has not failed; a
// later pass runs it again.
var errNotYet = errors.New("not yet")

// ErrStewardsOff is the steward step's error when the controller has no
// steward hook.
var ErrStewardsOff = errors.New("the provisioner has no steward hook; a tenant land does not open without its steward")

// stepSteward binds the land's steward and, once trigger-engine has had time
// to load it, wakes it once. Bind adopts the steward an earlier pass made and
// the wake post is idempotent, so a re-run repeats neither.
func (p *Provisioner) stepSteward(ctx context.Context, app *Application) error {
	if p.Stewards == nil {
		return ErrStewardsOff
	}
	trig, err := p.Stewards.Bind(ctx, app.LandName, stewardOperator(app))
	if err != nil {
		return fmt.Errorf("binding %s's steward: %w", app.LandName, err)
	}
	if age := p.now().Sub(trig.CreatedAt); age < StewardSettle {
		return fmt.Errorf("%w: steward %s was bound %s ago; it is woken once it is %s old", errNotYet, trig.ID, age.Round(time.Second), StewardSettle)
	}
	if err := p.Stewards.Wake(ctx, app.LandName, "lands-provision:"+app.ID+":steward-wake"); err != nil {
		return fmt.Errorf("waking %s's steward %s: %w", app.LandName, trig.ID, err)
	}
	return nil
}

// ---- Step 7: mail and done ---------------------------------------------------

func (p *Provisioner) stepMail(ctx context.Context, app *Application) error {
	proj, err := p.landProject(ctx, app)
	if err != nil {
		return err
	}
	now := p.now().UTC().Format(time.RFC3339)
	if proj.Fields["land_opened_at"] == "" {
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, proj.ID, map[string]any{"land_opened_at": now}); err != nil {
			return fmt.Errorf("stamping land_opened_at: %w", err)
		}
	}
	// The mail goes before the state change: a crash between the two sends
	// it twice, which beats never sending it.
	p.sendMail(ctx, mailer.KindLandReady, app, "", app.CreditsGranted)
	if err := p.Daemon.UpdateBeadFieldsTyped(ctx, app.ID, map[string]any{
		"state": StateProvisioned, "provision_step": StepMail, "project": app.LandName, "provision_error": "",
	}); err != nil {
		return fmt.Errorf("marking the application provisioned: %w", err)
	}
	app.State, app.Step = StateProvisioned, StepMail
	return nil
}

// sendMail renders and sends one mail to the applicant, logging rather than
// failing: the change it reports has happened either way.
func (p *Provisioner) sendMail(ctx context.Context, kind mailer.Kind, app *Application, note string, credits float64) {
	if p.Templates == nil || p.Mailer == nil || !mailer.Enabled(p.Mailer) || app.ApplicantEmail == "" {
		return
	}
	m, err := p.Templates.Render(kind, app.ApplicantEmail, mailer.Data{
		Name: app.GitHubLogin, ApplicationID: app.ID, Land: app.LandName, Repo: app.Repo,
		Note: note, GrantedUSD: credits, RemainingUSD: credits,
	})
	if err != nil {
		p.logger().Warn("lands provisioner: mail render failed", "kind", kind, "application", app.ID, "error", err)
		return
	}
	if err := p.Mailer.Send(ctx, m); err != nil {
		p.logger().Warn("lands provisioner: mail send failed", "kind", kind, "application", app.ID, "error", err)
	}
}

// ---- helpers -----------------------------------------------------------------

var (
	liveStatuses = []string{"open", "in_progress", "blocked", "deferred"}
	allStatuses  = []string{"open", "in_progress", "blocked", "deferred", "closed"}
)

const (
	pageSize = 200
	maxPages = 50
)

// listAll pages a query to the end; past maxPages it is an error, never a
// silent cut.
func listAll(ctx context.Context, d interface {
	ListBeadsFiltered(context.Context, beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}, q beadsapi.ListBeadsQuery) ([]*beadsapi.BeadDetail, error) {
	q.Limit, q.Sort = pageSize, "created_at"
	var out []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for page := range maxPages {
		q.Offset = page * pageSize
		res, err := d.ListBeadsFiltered(ctx, q)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return out, nil
		}
		for _, b := range res.Beads {
			if b != nil && !seen[b.ID] {
				seen[b.ID] = true
				out = append(out, b)
			}
		}
		if len(res.Beads) < pageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("more than %d %v beads; refusing a partial read", maxPages*pageSize, q.Types)
}

func (p *Provisioner) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

func (p *Provisioner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provisioner) newKey() string {
	if p.NewKey != nil {
		return p.NewKey()
	}
	return "sk-lands-" + rand.Text()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
