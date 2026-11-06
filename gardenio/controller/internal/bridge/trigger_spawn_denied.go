// A trigger whose fire the daemon refuses as a privileged spawn raises one
// operator escalation (gd-c4Xz4i95F7, gd-5RcYImJ0Et).
//
// The trigger-engine may spawn a privileged role (plan-gardener, herder, ...)
// only for a trigger that carries a valid spawn-lineage seal
// (kbeads/internal/server/spawn_lineage_seal.go). A trigger that was never
// sealed, or whose seal went stale because a non-admin edited a spawn-relevant
// field (cron, prompt, triggers, ...), gets `HTTP 403: spawn denied: role X is
// privileged; a non-human caller must escalate` on every fire. stampFireError
// records that on the trigger and, for a gardener, the consecutive-failure
// brake closes the trigger after a few fires. Until now nothing else said so:
// plan gd-r4GCQRjEei went without its gardener for two and a half hours, and
// gd-5RcYImJ0Et lists four more lanes lost the same way.
//
// So the first refused fire of a streak files an operator escalation naming the
// trigger and the reseal that fixes it, and posts one notifier line. It is
// deduped twice: a fire whose trigger already shows a spawn denial as its
// last_fire_error is the same streak (the persisted field survives a restart,
// and a successful fire clears it), and an escalation still open for the same
// trigger is not filed again.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

const (
	// spawnDeniedMarkerLabel tags the escalation so the next refused fire finds
	// it open and does not file another.
	spawnDeniedMarkerLabel = "trigger-spawn-denied"

	// spawnDeniedActor is the CreatedBy of the escalation and the
	// X-System-Operator source label, so the daemon attributes this
	// machine-origin write (ka-0000007257) as the sibling sweeps do.
	spawnDeniedActor = "trigger-spawn-denied"

	// spawnDeniedPrefix starts every daemon spawn refusal (kbeads spawn_authz.go).
	spawnDeniedPrefix = "spawn denied: "
)

// isPrivilegedSpawnDenial reports whether err is the daemon refusing an agent
// spawn with 403. A fake daemon or an extra wrap may hide the typed APIError,
// so the rendered text is matched too.
func isPrivilegedSpawnDenial(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusForbidden && strings.HasPrefix(apiErr.Message, spawnDeniedPrefix)
	}
	return strings.Contains(err.Error(), fmt.Sprintf("HTTP %d: %s", http.StatusForbidden, spawnDeniedPrefix))
}

// raiseSpawnDenied files the operator escalation for a fire the daemon refused
// as a privileged spawn. prevFireError is the trigger's last_fire_error from
// before this fire was stamped. Best-effort: every failure is logged, and the
// fire error stays recorded on the trigger either way.
func (e *TriggerEngine) raiseSpawnDenied(ctx context.Context, trig TriggerDef, prevFireError string, fireErr error) {
	if !isPrivilegedSpawnDenial(fireErr) || strings.Contains(prevFireError, spawnDeniedPrefix) {
		return
	}
	ctx = beadsapi.WithSystemOperator(ctx, spawnDeniedActor)

	open, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{spawnDeniedMarkerLabel},
		Limit:    100,
	})
	if err != nil {
		// Without the dedup read a duplicate could be filed; the next streak
		// tries again.
		e.logger.Warn("triggers: spawn denied, failed to check for an open escalation",
			"trigger", trig.ID, "error", err)
		return
	}
	for _, b := range open.Beads {
		if b != nil && spawnDeniedEscalationTrigger(b) == trig.ID {
			return
		}
	}

	errStr := truncateText(fireErr.Error(), maxFireErrorLen)
	body := renderSpawnDenied(trig, errStr)
	spec, err := json.Marshal(map[string]any{"action": "reseal_trigger", "trigger_id": trig.ID})
	if err != nil {
		e.logger.Warn("triggers: spawn denied, marshal operation_spec failed", "trigger", trig.ID, "error", err)
		return
	}
	fields, err := json.Marshal(map[string]any{
		"operation_type": "trigger_reseal",
		"operation_spec": json.RawMessage(spec),
		"context":        body,
		"audience":       "operator",
		"execution_script": "#!/usr/bin/env bash\nset -euo pipefail\n" +
			"# Run signed in as a gardenio-admin OIDC user (gb login). The reseal prints the\n" +
			"# content it adopts as yours; read it before you trust it.\n" +
			"gb trigger reseal " + trig.ID + "\n",
	})
	if err != nil {
		e.logger.Warn("triggers: spawn denied, marshal fields failed", "trigger", trig.ID, "error", err)
		return
	}
	project := trig.Project
	if project == "" {
		project = "gardenio"
	}
	id, err := e.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       fmt.Sprintf("Trigger %s cannot spawn its %s: every fire is refused (403), reseal it", trig.ID, defaultIfEmptyStr(trig.Role, "role")),
		ShortTitle:  "reseal a trigger refused at spawn",
		Type:        "escalation",
		Description: body,
		Labels:      []string{spawnDeniedMarkerLabel, "trigger-engine", "project:" + project},
		Priority:    1,
		CreatedBy:   spawnDeniedActor,
		Fields:      fields,
	})
	if err != nil {
		e.logger.Warn("triggers: spawn denied, failed to file escalation", "trigger", trig.ID, "error", err)
		return
	}
	e.logger.Warn("triggers: spawn denied, filed escalation",
		"trigger", trig.ID, "role", trig.Role, "escalation", id)
	if e.notifier != nil {
		e.notifier(WithNoticeProject(ctx, project), spawnDeniedActor,
			fmt.Sprintf("trigger %s (%s) cannot spawn its %s: the daemon refuses every fire as a privileged spawn, so its seal is missing or stale. See %s.",
				trig.ID, trig.Title, defaultIfEmptyStr(trig.Role, "role"), id),
			"")
	}
}

// spawnDeniedEscalationTrigger reads back the trigger id an open escalation
// names (operation_spec.trigger_id), or "".
func spawnDeniedEscalationTrigger(esc *beadsapi.BeadDetail) string {
	var spec struct {
		TriggerID string `json:"trigger_id"`
	}
	if json.Unmarshal([]byte(esc.Fields["operation_spec"]), &spec) != nil {
		return ""
	}
	return spec.TriggerID
}

// renderSpawnDenied builds the escalation body.
func renderSpawnDenied(trig TriggerDef, errStr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trigger %s (%q) fired, and the daemon refused to spawn its %s:\n\n    %s\n\n",
		trig.ID, trig.Title, defaultIfEmptyStr(trig.Role, "role"), errStr)
	b.WriteString("The trigger-engine may spawn a privileged role only for a trigger sealed by a gardenio-admin " +
		"OIDC session (spawn_lineage). This trigger carries no valid seal: it was never sealed, or someone " +
		"other than an admin changed a spawn-relevant field (cron, prompt, triggers, role, ...) after it was, " +
		"for example `gb gardener cadence` run by an agent (gd-c4Xz4i95F7).\n\n")
	if trig.Gardener != nil {
		b.WriteString("It is a gardener, so its consecutive-failure brake will close it after a few more refused fires. " +
			"If it is already closed, reopening or re-enabling it as a gardenio-admin seals it too.\n\n")
	}
	fmt.Fprintf(&b, "Fix: as a gardenio-admin, run `gb trigger reseal %s`. It shows the content you are adopting; "+
		"read it, because the reseal vouches for whoever last edited it.\n", trig.ID)
	return b.String()
}
