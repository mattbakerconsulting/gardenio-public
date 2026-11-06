package provision

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// Re-binding a land's steward (gd-UgZueN6Nk7): no open land without an open
// steward. A steward is standing, so trigger-engine renews its wall_clock in
// place and age never closes it (ka-0000004573). What closes it is a
// hard_stop (its consecutive_failures brake, or the credential crash-loop
// gate) or an operator. Bind runs only at provisioning, so each pass looks at
// every provisioned land that is not closing or frozen and binds a fresh
// steward for one whose steward closed; Bind adopts only an open one, so it
// mints. A freeze (gb lands freeze) is how an operator stops a land's
// steward: one closed by hand is bound again on the next pass.
//
// A steward that closed on a hard_stop crash-looped, and binding it again
// every pass would loop the crash. For StewardRebindBackoff after such a close
// the provisioner files one operator escalation, keyed on the closed steward,
// and binds a fresh one as soon as the operator closes that escalation, or
// when the back-off is over.
//
// An open steward bound before a step added a wake entry (the person-joined
// wake, step 9.14) is upgraded in place on the pass (Stewards.Upgrade), so it
// hears what a fresh bind would.
//
// The fresh steward is not woken: it wakes on the land's next event or its
// daily digest, and the land's steward-notes bead carries what the old one
// knew. A land with a close requested, or closed, is never bound one: the
// close's steward step retires its steward (close.go).

// StewardRebindBackoff is how long after a crash-loop close the provisioner
// waits for the operator before it binds the land a fresh steward anyway.
const StewardRebindBackoff = 24 * time.Hour

// rebindSteward binds a fresh steward for app's land when its steward closed.
// It is best effort, like backfillLandScope: a failure is logged and retried
// on the next pass, and never touches the application.
func (p *Provisioner) rebindSteward(ctx context.Context, app *Application) {
	if p.Stewards == nil {
		return
	}
	log := p.logger().With("application", app.ID, "land", app.LandName)
	open, last, err := p.Stewards.Find(ctx, app.LandName)
	if err != nil {
		log.Warn("lands provisioner: could not read the land's steward", "error", err)
		return
	}
	if open != nil {
		if err := p.Stewards.Upgrade(ctx, app.LandName, open); err != nil {
			log.Warn("lands provisioner: could not upgrade the land's steward wakes", "steward", open.ID, "error", err)
		}
		return
	}
	proj, err := p.landProject(ctx, app)
	if err != nil {
		log.Warn("lands provisioner: could not read the land for its steward", "error", err)
		return
	}
	if proj.Fields["land_frozen"] == "true" {
		return
	}
	why := "it had none"
	if last != nil {
		why = fmt.Sprintf("its steward %s %s", last.ID, closedHow(last))
		if crashLooped(last) && p.now().Sub(closedAt(last)) < StewardRebindBackoff {
			esc, err := p.escalateSteward(ctx, app, last)
			if err != nil {
				log.Warn("lands provisioner: could not file the steward escalation", "steward", last.ID, "error", err)
				return
			}
			if esc.Status != "closed" {
				return
			}
			why += fmt.Sprintf(", and the operator closed escalation %s", esc.ID)
		}
	}
	trig, err := p.Stewards.Bind(ctx, app.LandName, stewardOperator(app))
	if err != nil {
		log.Warn("lands provisioner: could not bind the land a fresh steward", "error", err)
		return
	}
	log.Info("lands provisioner: bound the land a fresh steward", "steward", trig.ID, "why", why)
	if err := p.Daemon.AddComment(ctx, proj.ID, Author,
		fmt.Sprintf("Bound steward %s to land %s: %s.", trig.ID, app.LandName, why)); err != nil {
		log.Warn("lands provisioner: could not comment the fresh steward on the land", "error", err)
	}
}

// escalateSteward files (or finds) the operator escalation for a steward that
// crash-looped. It is keyed on the steward, so a pass files it once.
func (p *Provisioner) escalateSteward(ctx context.Context, app *Application, last *beadsapi.BeadDetail) (*beadsapi.BeadDetail, error) {
	closed := closedAt(last).UTC()
	return p.escalate(ctx, app, "lands_steward",
		fmt.Sprintf("Lands steward crash-looped: %s", app.LandName),
		"lands-steward:"+last.ID,
		fmt.Sprintf("The steward of land %q (trigger %s) %s, so its runs were failing. The Lands provisioner binds "+
			"a fresh steward for an open land whose steward closed, but waits %s after a crash so a broken steward "+
			"is not made again every minute.\n\n"+
			"Read the closed steward's last runs for the cause and fix it, then close this escalation: the next pass "+
			"binds a fresh steward. Left open, a fresh one is bound at %s anyway. To leave the land without a "+
			"steward, freeze it (`gb lands freeze %s`).",
			app.LandName, last.ID, closedHow(last), StewardRebindBackoff,
			closed.Add(StewardRebindBackoff).Format(time.RFC3339), app.LandName),
		map[string]any{"application": app.ID, "land": app.LandName, "steward": last.ID,
			"stop": last.Fields["gardener_terminated_kind"]})
}

// crashLooped reports whether the engine closed the steward on a failure
// brake: a hard_stop other than its wall_clock, which the engine renews in
// place (a wall_clock close is no crash, so it is bound again at once).
func crashLooped(b *beadsapi.BeadDetail) bool {
	return strings.TrimSpace(b.Fields["gardener_terminated_slot"]) == scheduler.StopSlotHardStop &&
		strings.TrimSpace(b.Fields["gardener_terminated_kind"]) != scheduler.StopKindWallClock
}

// closedHow says how and when a steward closed.
func closedHow(b *beadsapi.BeadDetail) string {
	at := closedAt(b).UTC().Format(time.RFC3339)
	slot, kind := strings.TrimSpace(b.Fields["gardener_terminated_slot"]), strings.TrimSpace(b.Fields["gardener_terminated_kind"])
	if slot == "" {
		return "was closed at " + at + " with no gardener stop recorded"
	}
	return fmt.Sprintf("closed at %s on its %s %s", at, slot, kind)
}

// closedAt is when b closed: closed_at, else its last update.
func closedAt(b *beadsapi.BeadDetail) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(b.ClosedAt)); err == nil {
		return t
	}
	return b.UpdatedAt
}

// stewardOperator is the approver as an operated-by alias, or "".
func stewardOperator(app *Application) string {
	if strings.Contains(app.DecidedBy, "@") {
		return "oidc:" + app.DecidedBy
	}
	return ""
}
