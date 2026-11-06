package provision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/mailer"
)

// Closing steps (§3.3 "Deprovision"), recorded in close_step when done. They
// run provisioning in reverse: freeze, retire the steward, stop agents, remove
// members, revoke the key, delete the land namespace (step 10.2), close the
// project, mail. Beads
// are kept; the purge sweep (internal/lands/purge, step 8.11) deletes them
// after the retention period.
const (
	CloseFreeze    = "freeze"
	CloseSteward   = "steward"
	CloseAgents    = "agents"
	CloseMembers   = "members"
	CloseKey       = "key"
	CloseNamespace = "namespace"
	CloseProject   = "project"
	CloseMail      = "mail"
)

// CloseSteps is the closing order.
var CloseSteps = []string{CloseFreeze, CloseSteward, CloseAgents, CloseMembers, CloseKey, CloseNamespace, CloseProject, CloseMail}

// closableStates are the application states gb lands close may act on: a
// land, one that failed on its way to being one, or an approval nothing has
// been built for yet.
var closableStates = []string{StateApproved, StateProvisioned, StateFailed}

// stoppedAgentStates are agent states that need no stop.
var stoppedAgentStates = map[string]bool{"stopping": true, "done": true, "failed": true}

// Close runs app's remaining closing steps. A close that failed (it carries
// provision_error) waits for gb lands close to ask again, which clears the
// error. It returns the error the close stopped on (already recorded), or nil.
func (p *Provisioner) Close(ctx context.Context, app *Application) error {
	if !slices.Contains(closableStates, app.State) || app.CloseRequestedAt == "" || app.Error != "" {
		return nil
	}
	log := p.logger().With("application", app.ID, "land", app.LandName)
	log.Info("lands provisioner: closing", "requested_by", app.CloseRequestedBy, "after", app.CloseStep)
	for _, step := range closeRemaining(app.CloseStep) {
		if err := p.runCloseStep(ctx, app, step); err != nil {
			return p.failClose(ctx, app, step, err)
		}
		if step == CloseMail {
			break
		}
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, app.ID, map[string]any{"close_step": step}); err != nil {
			log.Warn("lands provisioner: could not record a close step; the next pass re-runs it", "step", step, "error", err)
			return err
		}
		app.CloseStep = step
		log.Info("lands provisioner: close step done", "step", step)
	}
	log.Info("lands provisioner: closed")
	return nil
}

func closeRemaining(done string) []string {
	i := slices.Index(CloseSteps, done)
	return CloseSteps[i+1:]
}

func (p *Provisioner) runCloseStep(ctx context.Context, app *Application, step string) error {
	switch step {
	case CloseFreeze:
		return p.closeFreeze(ctx, app)
	case CloseSteward:
		return p.closeSteward(ctx, app)
	case CloseAgents:
		return p.closeAgents(ctx, app)
	case CloseMembers:
		return p.closeMembers(ctx, app)
	case CloseKey:
		return p.closeKey(ctx, app)
	case CloseNamespace:
		return p.closeNamespace(ctx, app)
	case CloseProject:
		return p.closeProject(ctx, app)
	case CloseMail:
		return p.closeMail(ctx, app)
	}
	return fmt.Errorf("unknown close step %q", step)
}

// failClose records a failed close and files one escalation for it. The key
// is the close request's time, so asking again files a new one.
func (p *Provisioner) failClose(ctx context.Context, app *Application, step string, err error) error {
	log := p.logger().With("application", app.ID, "land", app.LandName, "step", step)
	log.Error("lands provisioner: close step failed", "error", err)
	msg := truncate(err.Error(), 500)
	esc, escErr := p.escalate(ctx, app, "lands_close",
		fmt.Sprintf("Lands close failed: %s at step %s", app.LandName, step),
		fmt.Sprintf("lands-close:%s:%s", app.ID, app.CloseRequestedAt),
		fmt.Sprintf("The Lands provisioner stopped closing land %q (plot_application %s) at step %q: %s\n\n"+
			"Steps done so far are recorded in close_step (%q). Fix the cause, then run `gb lands close %s` again; "+
			"it resumes from the failed step.", app.LandName, app.ID, step, msg, app.CloseStep, app.LandName),
		map[string]any{"application": app.ID, "land": app.LandName, "step": step})
	if escErr != nil {
		log.Warn("lands provisioner: could not file the close escalation", "error", escErr)
	}
	fields := map[string]any{
		"provision_error":     "close " + step + ": " + msg,
		"provision_failed_at": p.now().UTC().Format(time.RFC3339),
	}
	if esc != nil {
		fields["provision_escalation_id"] = esc.ID
	}
	if uerr := p.Daemon.UpdateBeadFieldsTyped(ctx, app.ID, fields); uerr != nil {
		log.Warn("lands provisioner: could not record the close failure", "error", uerr)
	} else {
		app.Error = "close " + step + ": " + msg
	}
	return err
}

// closingProject is the land's project bead, or nil when provisioning never
// made it. A project of that name that is not this application's is an error:
// closing must never touch another land.
func (p *Provisioner) closingProject(ctx context.Context, app *Application) (*beadsapi.BeadDetail, error) {
	b, err := p.project(ctx, app.LandName)
	if err != nil || b == nil {
		return nil, err
	}
	if got := b.Fields["land_application_id"]; got != app.ID {
		return nil, fmt.Errorf("project %q (%s) belongs to application %q, not %s; nothing of it is closed", app.LandName, b.ID, got, app.ID)
	}
	return b, nil
}

// closeFreeze sets land_frozen, which the spawn wall refuses every spawn for
// (§3.5), so nothing new starts while the land comes apart.
func (p *Provisioner) closeFreeze(ctx context.Context, app *Application) error {
	proj, err := p.closingProject(ctx, app)
	if err != nil || proj == nil || proj.Fields["land_frozen"] == "true" {
		return err
	}
	if err := p.Daemon.UpdateBeadFieldsTyped(ctx, proj.ID, map[string]any{"land_frozen": true}); err != nil {
		return fmt.Errorf("freezing %s: %w", app.LandName, err)
	}
	return nil
}

// closeSteward retires the land's steward trigger, so its wakes and daily
// digest stop firing into a spawn wall that refuses every one. A controller
// with no steward hook can only skip a land whose provisioning never reached
// the steward step.
func (p *Provisioner) closeSteward(ctx context.Context, app *Application) error {
	proj, err := p.closingProject(ctx, app)
	if err != nil || proj == nil {
		return err
	}
	if p.Stewards == nil {
		if slices.Index(Steps, app.Step) < slices.Index(Steps, StepNamespace) {
			return nil // the steward step never ran
		}
		return fmt.Errorf("%s may have a steward to retire: %w", app.LandName, ErrStewardsOff)
	}
	if err := p.Stewards.Retire(ctx, app.LandName, fmt.Sprintf("Retired: the operator closed land %s.", app.LandName)); err != nil {
		return fmt.Errorf("retiring %s's steward: %w", app.LandName, err)
	}
	return nil
}

// closeAgents puts every live agent of the land on the stop path
// (agent_state=stopping, as gb stop and the purse watcher write it).
func (p *Provisioner) closeAgents(ctx context.Context, app *Application) error {
	proj, err := p.closingProject(ctx, app)
	if err != nil || proj == nil {
		return err
	}
	agents, err := listAll(ctx, p.Daemon, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + app.LandName}, Statuses: liveStatuses,
	})
	if err != nil {
		return fmt.Errorf("reading %s's agents: %w", app.LandName, err)
	}
	for _, a := range agents {
		if a.Type != "agent" || stoppedAgentStates[a.Fields["agent_state"]] {
			continue
		}
		if pr := strings.TrimSpace(a.Fields["project"]); pr != "" && pr != app.LandName {
			continue
		}
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, a.ID, map[string]any{"agent_state": "stopping"}); err != nil {
			return fmt.Errorf("stopping agent %s: %w", a.ID, err)
		}
		if err := p.Daemon.AddComment(ctx, a.ID, Author, fmt.Sprintf("Stopped: the operator closed land %s.", app.LandName)); err != nil {
			p.logger().Warn("lands provisioner: comment failed", "bead", a.ID, "error", err)
		}
	}
	return nil
}

// closeMembers takes everyone out of the land's groups, and out of
// lands-tenants when this was the last land they held. The group itself is
// kept (empty), so the land name stays taken in Keycloak too.
func (p *Provisioner) closeMembers(ctx context.Context, app *Application) error {
	members, err := p.Groups.LandMembers(ctx, app.LandName)
	if errors.Is(err, keycloakadmin.ErrGroupNotFound) {
		if slices.Index(Steps, app.Step) < slices.Index(Steps, StepKeycloak) {
			return nil // provisioning never reached Keycloak
		}
		// The group was made, so "not found" means the lookup cannot see it
		// (Keycloak list calls hidden, gd-7peNzGjJ3C) or someone deleted it.
		// Skipping would leave every member in place.
		return fmt.Errorf("provision_step %q records the Keycloak step, but %w; no member was removed. "+
			"Check that the provisioner's Keycloak permissions use a user policy (keycloak-provisioner.md); "+
			"if the group really was deleted, record close_step=%s by hand", app.Step, err, CloseMembers)
	}
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(members)+1)
	for _, m := range members {
		ids = append(ids, m.ID)
	}
	if app.ApplicantSub != "" && !slices.Contains(ids, app.ApplicantSub) {
		// A crash after the owner left the land but before they left
		// lands-tenants leaves them in no land group; finish them too.
		ids = append(ids, app.ApplicantSub)
	}
	for _, id := range ids {
		if err := p.Groups.RemoveFromLand(ctx, app.LandName, id); err != nil {
			return err
		}
		lands, err := p.Groups.UserLands(ctx, id)
		if err != nil {
			return err
		}
		if len(lands) == 0 {
			if err := p.Groups.RemoveTenant(ctx, id); err != nil && !errors.Is(err, keycloakadmin.ErrGroupNotFound) {
				return err
			}
		}
	}
	return nil
}

// closeKey revokes the land's LiteLLM key, which stops every call it could
// still make, then clears it from the account and closes the account.
func (p *Provisioner) closeKey(ctx context.Context, app *Application) error {
	name := AccountName(app.LandName)
	accounts, err := listAll(ctx, p.Daemon, beadsapi.ListBeadsQuery{Types: []string{"account"}, Statuses: liveStatuses})
	if err != nil {
		return fmt.Errorf("reading accounts: %w", err)
	}
	for _, a := range accounts {
		if a.Title != name {
			continue
		}
		if key := strings.TrimSpace(a.Fields["litellm_key"]); key != "" {
			if err := p.Keys.Delete(ctx, key); err != nil {
				return fmt.Errorf("revoking %s's LiteLLM key: %w", name, err)
			}
			if err := p.Daemon.UpdateBeadFieldsTyped(ctx, a.ID, map[string]any{"litellm_key": ""}); err != nil {
				return fmt.Errorf("clearing %s's key: %w", name, err)
			}
		}
		if err := p.Daemon.CloseBead(ctx, a.ID, nil); err != nil {
			return fmt.Errorf("closing account %s: %w", name, err)
		}
	}
	return nil
}

// closeNamespace deletes the land's namespace, and with it every pod, PVC and
// secret left in it. A controller with land namespaces off can only skip a
// land that never got one.
func (p *Provisioner) closeNamespace(ctx context.Context, app *Application) error {
	if p.Namespaces == nil {
		proj, err := p.closingProject(ctx, app)
		if err != nil {
			return err
		}
		if proj != nil && proj.Fields["namespace"] != "" {
			return fmt.Errorf("%s has namespace %s to delete: %w", app.LandName, proj.Fields["namespace"], ErrNamespacesOff)
		}
		return nil
	}
	return p.Namespaces.Delete(ctx, app.LandName)
}

// closeProject closes the land's project bead with land_closed_at.
func (p *Provisioner) closeProject(ctx context.Context, app *Application) error {
	proj, err := p.closingProject(ctx, app)
	if err != nil || proj == nil {
		return err
	}
	if proj.Fields["land_closed_at"] == "" {
		if err := p.Daemon.UpdateBeadFieldsTyped(ctx, proj.ID, map[string]any{
			"land_closed_at": p.now().UTC().Format(time.RFC3339),
		}); err != nil {
			return fmt.Errorf("stamping %s's land_closed_at: %w", app.LandName, err)
		}
	}
	if err := p.Daemon.CloseBead(ctx, proj.ID, nil); err != nil {
		return fmt.Errorf("closing project %s: %w", app.LandName, err)
	}
	return nil
}

// closeMail tells the tenant, then marks the application closed.
func (p *Provisioner) closeMail(ctx context.Context, app *Application) error {
	if app.State == StateProvisioned {
		p.sendMail(ctx, mailer.KindLandClosed, app, app.CloseReason, 0)
	}
	if err := p.Daemon.UpdateBeadFieldsTyped(ctx, app.ID, map[string]any{"state": StateClosed, "close_step": CloseMail}); err != nil {
		return fmt.Errorf("marking the application closed: %w", err)
	}
	app.State, app.CloseStep = StateClosed, CloseMail
	return nil
}
