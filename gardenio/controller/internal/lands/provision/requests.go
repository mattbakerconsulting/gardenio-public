package provision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The operator's two requests, written by gb lands provision --retry and gb
// lands close. Both only write the application; the controller's next pass
// does the work, so the Keycloak and LiteLLM credentials stay in the
// controller.

// ErrNoApplication is returned by Find when nothing matches.
var ErrNoApplication = errors.New("no plot application matches")

// Find returns the application ref names: a plot_application id, or a land
// name, which matches its newest application that is not denied or
// withdrawn.
func Find(ctx context.Context, d Daemon, ref string) (*Application, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, ErrNoApplication
	}
	if b, err := d.GetBead(ctx, ref); err == nil {
		if app, ok := ApplicationFromBead(b); ok {
			return app, nil
		}
	}
	beads, err := listAll(ctx, d, beadsapi.ListBeadsQuery{
		Types: []string{ApplicationType}, Labels: []string{IntakeLabel},
		Fields: map[string]string{"land_name": ref}, Statuses: allStatuses,
	})
	if err != nil {
		return nil, fmt.Errorf("reading applications: %w", err)
	}
	var newest *beadsapi.BeadDetail
	for _, b := range beads {
		app, ok := ApplicationFromBead(b)
		if !ok || app.LandName != ref || app.State == "denied" || app.State == "withdrawn" {
			continue
		}
		if newest == nil || b.CreatedAt.After(newest.CreatedAt) {
			newest = b
		}
	}
	if newest == nil {
		return nil, fmt.Errorf("%w %q (an application id or a land name)", ErrNoApplication, ref)
	}
	app, _ := ApplicationFromBead(newest)
	return app, nil
}

// Retry moves a failed provisioning back to provisioning as a new attempt;
// the next pass resumes after provision_step.
func Retry(ctx context.Context, d Daemon, app *Application) error {
	switch {
	case app.CloseRequestedAt != "":
		return fmt.Errorf("application %s has a close requested; run gb lands close %s again to retry the close", app.ID, app.LandName)
	case app.State != StateFailed:
		return fmt.Errorf("application %s is %s; only a failed provisioning can be retried", app.ID, app.State)
	}
	attempt := app.Attempt + 1
	if err := d.UpdateBeadFieldsTyped(ctx, app.ID, map[string]any{
		"state": StateProvisioning, "provision_attempt": attempt, "provision_error": "",
	}); err != nil {
		return err
	}
	app.State, app.Attempt, app.Error = StateProvisioning, attempt, ""
	return nil
}

// RequestClose asks the provisioner to close app's land. Asking again after a
// failed close clears the failure and retries from close_step.
func RequestClose(ctx context.Context, d Daemon, app *Application, by, reason string, now time.Time) error {
	switch {
	case strings.TrimSpace(by) == "":
		return errors.New("no operator identity to record as close_requested_by (set --operated-by or BEADS_OPERATED_BY)")
	case app.State == StateClosed:
		return fmt.Errorf("application %s is already closed", app.ID)
	case app.State == StateProvisioning:
		return fmt.Errorf("application %s is being provisioned; close it once it is provisioned or failed", app.ID)
	case !slices.Contains(closableStates, app.State):
		return fmt.Errorf("application %s is %s; only an approved, provisioned or failed application has a land to close", app.ID, app.State)
	}
	at := now.UTC().Format(time.RFC3339)
	fields := map[string]any{
		"close_requested_at": at, "close_requested_by": by, "close_reason": strings.TrimSpace(reason),
		"provision_error": "",
	}
	if err := d.UpdateBeadFieldsTyped(ctx, app.ID, fields); err != nil {
		return err
	}
	app.CloseRequestedAt, app.CloseRequestedBy, app.CloseReason, app.Error = at, by, strings.TrimSpace(reason), ""
	return nil
}
