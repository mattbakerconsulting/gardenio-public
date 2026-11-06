// Cred-exhaustion alerting — emit no_fallback as gardenio alert
// beads instead of bespoke Slack posts (epic ka-0000003084, task ka-0000005341).
//
// Matt's ask (Slack thread ka-0000001084): "could we stop with the No fallback
// account ... manual intervention required everywhere? probably just eliminate
// that whole category of messages and file something to use gardenio alerts for
// that kind of thing". Part 1 (ka-0000001061) suppressed the bespoke posts by
// default; this is Part 2 — routing the signal through gardenio alerts.
//
// Rather than post to Slack, NotifyNoFallback now
// file a firing-alert-shaped bead in the shape the (since-deleted, 2026-10-31,
// gd-Y4ZCGr2m2t) alertmanager-bridge AlertRouter used for Alertmanager-sourced
// alerts: labels source:alert + severity:<sev>, fields alert_severity +
// webhook_event_key (the dedup + resolved-lookup key). So a cred-exhaustion
// alert lands in the operator surface (gb ready, beads-viewer alert nodes)
// with none of the Slack-spam framing.
//
// Dedup + auto-resolve:
//   - dedup: one OPEN alert bead per project+account. A repeat exhaustion event
//     while the alert is still open is a no-op (the fallback chain being broken
//     for minutes fires the same event every reconcile) — exactly the churn the
//     old in-memory 15-minute cooldown existed to damp, now enforced by the
//     bead's open state instead of process-local memory that dies on restart.
//   - auto-resolve: account_recovered closes every open cred-exhaustion alert
//     for the recovered account — the "auto-resolves on account_recovered" the
//     task asks for.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// credExhaustionActor stamps created_by / comment author on the cred-exhaustion
// alert bead's writes so the provenance is visible in the viewer.
const credExhaustionActor = "cred-exhaustion-alert"

// credExhaustionAlertLabel marks the alert beads this emitter files so the
// recovery-close path and any operator query can find them by kind, distinct
// from other source:alert beads.
const credExhaustionAlertLabel = "cred-exhaustion"

// credExhaustionSeverity is the alert severity for a cred-exhaustion event. warn
// (not page) because the fallback machinery is designed to ride out an exhausted
// account: routing to a bug bead (the warn→bug alert convention) puts it in the
// operator queue without paging, and — unlike page→decision — needs no options
// payload, so it clears the daemon's ack-only decision guard. Recovery is
// automatic (SweepStaleExhausted + retryStuckFailovers), so a page would be
// noise the moment the billing window resets.
const credExhaustionSeverity = "warn"

// credExhaustionKindNoFallback labels the exhaustion condition on the bead
// ("no fallback configured").
const credExhaustionKindNoFallback = "no_fallback"

// credExhaustionEventKey builds the webhook_event_key for a cred-exhaustion
// alert: a stable per-group identity stored in the webhook_event_key field
// (the token:groupKey convention of Alertmanager-sourced alert beads), so the
// open-bead lookup finds it and one project+account exhaustion maps to
// exactly one open alert regardless of how many times the event refires.
func credExhaustionEventKey(project, account string) string {
	return "cred-exhaustion:" + project + ":" + account
}

// emitCredExhaustionAlert files (or dedups to an existing open) firing-alert
// bead for a cred-exhaustion condition. kind is one of the credExhaustionKind*
// constants. It is idempotent while an alert for the same project+account is
// open: a repeat call finds the open bead and returns without filing a
// duplicate. A nil daemon (unconfigured bridge) is a silent no-op.
func (b *Bot) emitCredExhaustionAlert(ctx context.Context, kind string, evt RotationEvent) error {
	if b.daemon == nil {
		b.logger.Debug("cred-exhaustion alert: no daemon configured, skipping",
			"kind", kind, "project", evt.Project, "account", evt.FromAccount)
		return nil
	}

	key := credExhaustionEventKey(evt.Project, evt.FromAccount)

	// Dedup: an open alert for this project+account already covers the
	// condition. The fallback chain being broken re-emits the same event every
	// reconcile; the open bead is the dedup gate (replacing the old in-memory
	// cooldown), so a repeat is a no-op until recovery closes it.
	existing, err := b.findOpenCredExhaustionAlert(ctx, key)
	if err != nil {
		return err
	}
	if existing != "" {
		b.logger.Debug("cred-exhaustion alert already open, not refiling",
			"kind", kind, "project", evt.Project, "account", evt.FromAccount, "bead", existing)
		return nil
	}

	title := credExhaustionAlertTitle(evt)

	labels := []string{
		"source:alert",
		"severity:" + credExhaustionSeverity,
		credExhaustionAlertLabel,
	}
	if evt.Project != "" {
		labels = append(labels, "project:"+evt.Project)
	}

	fields := map[string]string{
		"alert_severity":          credExhaustionSeverity,
		"webhook_event_key":       key,
		"cred_exhaustion_kind":    kind,
		"cred_exhaustion_account": evt.FromAccount,
	}
	if evt.Project != "" {
		fields["cred_exhaustion_project"] = evt.Project
	}
	if evt.ToAccount != "" {
		fields["cred_exhaustion_fallback"] = evt.ToAccount
	}
	if evt.ResetAt != "" {
		fields["reset_at"] = evt.ResetAt
	}
	fieldsJSON, _ := json.Marshal(fields)

	// Dedup is the OPEN-bead lookup above, not an IdempotencyKey: the daemon's
	// idempotency lookup-or-return matches any non-deleted bead (a CLOSED alert
	// still carries the key), so an idempotency key would return the resolved
	// bead after recovery and suppress the next genuine exhaustion. Scoping the
	// lookup to open beads instead releases the gate the moment the alert
	// auto-closes.
	beadID, err := b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "bug", // warn severity → bug bead
		Description: credExhaustionAlertDescription(evt),
		Labels:      labels,
		Fields:      json.RawMessage(fieldsJSON),
		CreatedBy:   credExhaustionActor,
	})
	if err != nil {
		return fmt.Errorf("cred-exhaustion alert: create bead: %w", err)
	}

	b.logger.Info("filed cred-exhaustion alert bead",
		"kind", kind, "project", evt.Project, "account", evt.FromAccount,
		"bead", beadID, "key", key)
	return nil
}

// resolveCredExhaustionAlerts closes every open cred-exhaustion alert bead for
// the recovered account, annotating each with the auto-resolve provenance —
// the account_recovered counterpart to an Alertmanager status=resolved. A nil daemon
// is a silent no-op. Errors closing an individual bead are logged and do not
// abort the sweep (best-effort recovery — a stuck close must not wedge the
// probe-event listener).
func (b *Bot) resolveCredExhaustionAlerts(ctx context.Context, account string) error {
	if b.daemon == nil || account == "" {
		return nil
	}

	// The event key embeds the account but not a stable project (an account may
	// serve several projects), so match on the account field across all open
	// cred-exhaustion alerts rather than a single event key.
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"bug"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{"source:alert", credExhaustionAlertLabel},
		Fields:   map[string]string{"cred_exhaustion_account": account},
		Limit:    50,
	})
	if err != nil {
		return fmt.Errorf("cred-exhaustion alert: lookup open alerts for %q: %w", account, err)
	}
	if res == nil || len(res.Beads) == 0 {
		return nil
	}

	for _, bead := range res.Beads {
		if cerr := b.daemon.AddComment(ctx, bead.ID, credExhaustionActor,
			fmt.Sprintf("Account `%s` recovered — auto-closing cred-exhaustion alert (probe reported rejected→allowed).", account)); cerr != nil {
			b.logger.Warn("cred-exhaustion alert: resolve comment failed (continuing to close)",
				"bead", bead.ID, "account", account, "error", cerr)
		}
		if cerr := b.daemon.CloseBead(ctx, bead.ID, nil); cerr != nil {
			b.logger.Warn("cred-exhaustion alert: close on recovery failed",
				"bead", bead.ID, "account", account, "error", cerr)
			continue
		}
		b.logger.Info("closed cred-exhaustion alert on account recovery",
			"bead", bead.ID, "account", account)
	}
	return nil
}

// findOpenCredExhaustionAlert returns the ID of an open cred-exhaustion alert
// bead carrying the given webhook_event_key, or "" if none is open.
func (b *Bot) findOpenCredExhaustionAlert(ctx context.Context, key string) (string, error) {
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"bug"},
		Statuses: []string{"open", "in_progress"},
		Fields:   map[string]string{"webhook_event_key": key},
		Limit:    5,
	})
	if err != nil {
		return "", fmt.Errorf("cred-exhaustion alert: lookup open alert %q: %w", key, err)
	}
	if res == nil || len(res.Beads) == 0 {
		return "", nil
	}
	return res.Beads[0].ID, nil
}

// credExhaustionAlertTitle renders the alert bead title, bracket-prefixed with
// the event key so it reads like other alert beads ("[<key>] <what>").
func credExhaustionAlertTitle(evt RotationEvent) string {
	key := credExhaustionEventKey(evt.Project, evt.FromAccount)
	return fmt.Sprintf("[%s] No fallback account for %s (%s)", key, evt.FromAccount, projectOrUnknown(evt.Project))
}

// credExhaustionAlertDescription renders the alert body: what happened, the
// affected account/fallback, the recovery ETA (folded in from the old Slack
// block), and that recovery is automatic.
func credExhaustionAlertDescription(evt RotationEvent) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Account `%s` is exhausted for project `%s` and no healthy fallback account is configured in its chain.\n",
		evt.FromAccount, projectOrUnknown(evt.Project))
	if eta := formatRecoveryETA(evt.ResetAt); eta != "" {
		sb.WriteString(eta)
	}
	sb.WriteString("\nRecovery is automatic: this alert auto-closes when the account's rate-limit clears (account_recovered). ")
	sb.WriteString("To recover sooner, configure a healthy fallback on the account bead or wait for the billing window to reset.")
	return sb.String()
}

// projectOrUnknown renders a project name, falling back to a placeholder for
// account-scoped events that carry no project.
func projectOrUnknown(project string) string {
	if project == "" {
		return "(account-scoped)"
	}
	return project
}
