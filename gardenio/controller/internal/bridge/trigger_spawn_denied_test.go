package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// privilegedSpawnDenial is the daemon's refusal of a trigger-engine spawn of a
// privileged role for a trigger with no valid seal (kbeads spawn_authz.go).
var privilegedSpawnDenial = &beadsapi.APIError{StatusCode: 403, Message: "spawn denied: role plan-gardener is privileged; a non-human caller must escalate"}

// spawnDeniedEscalations returns the escalations the engine filed.
func spawnDeniedEscalations(daemon *mockTriggerDaemon) []beadsapi.CreateBeadRequest {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var out []beadsapi.CreateBeadRequest
	for _, req := range daemon.createdBeads {
		if req.Type == "escalation" && slices.Contains(req.Labels, spawnDeniedMarkerLabel) {
			out = append(out, req)
		}
	}
	return out
}

// TestEvalOne_SpawnDenied_FilesOneEscalation is the gd-c4Xz4i95F7 signal: a
// gardener whose fire the daemon refuses as a privileged spawn must raise an
// operator escalation naming the trigger and the reseal, and one notifier line,
// before its consecutive-failure brake quietly closes it.
func TestEvalOne_SpawnDenied_FilesOneEscalation(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{RunBudget: 5})
	daemon.spawnErr = privilegedSpawnDenial
	notif := &recordingNotifier{}
	engine.notifier = notif.notify

	engine.Evaluate(context.Background())

	escs := spawnDeniedEscalations(daemon)
	if len(escs) != 1 {
		t.Fatalf("filed %d spawn-denied escalations, want 1 (created: %+v)", len(escs), daemon.createdBeads)
	}
	esc := escs[0]
	if !strings.Contains(esc.Title, "gd-cap-1") || !strings.Contains(esc.Description, "gb trigger reseal gd-cap-1") {
		t.Errorf("escalation does not name the trigger and its reseal: title=%q\n%s", esc.Title, esc.Description)
	}
	if !strings.Contains(esc.Description, "consecutive-failure brake") {
		t.Errorf("a gardener's escalation should warn about the hard_stop:\n%s", esc.Description)
	}
	if !slices.Contains(esc.Labels, "project:monorepo") {
		t.Errorf("labels = %v, want the trigger's project", esc.Labels)
	}
	var fields map[string]any
	if err := json.Unmarshal(esc.Fields, &fields); err != nil {
		t.Fatalf("fields: %v", err)
	}
	if fields["operation_type"] != "trigger_reseal" {
		t.Errorf("operation_type = %v, want trigger_reseal", fields["operation_type"])
	}
	if script, _ := fields["execution_script"].(string); !strings.Contains(script, "gb trigger reseal gd-cap-1") {
		t.Errorf("execution_script = %q, want the reseal", script)
	}
	if spec, _ := fields["operation_spec"].(map[string]any); spec["trigger_id"] != "gd-cap-1" {
		t.Errorf("operation_spec = %v, want trigger_id gd-cap-1", fields["operation_spec"])
	}
	daemon.mu.Lock()
	sysOp := daemon.createSystemOperators[len(daemon.createSystemOperators)-1]
	daemon.mu.Unlock()
	if sysOp != spawnDeniedActor {
		t.Errorf("escalation create carried system operator %q, want %q", sysOp, spawnDeniedActor)
	}
	if notif.count() != 1 || !strings.Contains(notif.calls[0], "gd-cap-1") {
		t.Errorf("notifier calls = %v, want one naming the trigger", notif.calls)
	}
	// The fire error is still recorded as before.
	if got, _ := fieldValueFor(daemon, "gd-cap-1", "last_fire_error"); !strings.Contains(got, "spawn denied") {
		t.Errorf("last_fire_error = %q, want the denial", got)
	}
}

// TestEvalOne_SpawnDenied_SameStreakDoesNotRefile: a trigger whose last fire
// was already a spawn denial is the same streak; it files nothing more.
func TestEvalOne_SpawnDenied_SameStreakDoesNotRefile(t *testing.T) {
	daemon, engine, src, _ := gardenerTrigger(t, GardenerSpec{RunBudget: 5})
	daemon.spawnErr = privilegedSpawnDenial
	src.triggers[0].LastFireError = "spawning agent: " + privilegedSpawnDenial.Error()

	engine.Evaluate(context.Background())

	if escs := spawnDeniedEscalations(daemon); len(escs) != 0 {
		t.Errorf("filed %d escalations on a continuing streak, want 0", len(escs))
	}
}

// TestEvalOne_SpawnDenied_OpenEscalationDedups: an escalation still open for the
// same trigger (say, from before a restart that lost last_fire_error) is not
// filed again.
func TestEvalOne_SpawnDenied_OpenEscalationDedups(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{RunBudget: 5})
	daemon.spawnErr = privilegedSpawnDenial
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID: "gd-esc-old", Type: "escalation", Status: "open",
		Labels: []string{spawnDeniedMarkerLabel},
		Fields: map[string]string{"operation_spec": `{"action":"reseal_trigger","trigger_id":"gd-cap-1"}`},
	})

	engine.Evaluate(context.Background())

	if escs := spawnDeniedEscalations(daemon); len(escs) != 0 {
		t.Errorf("filed %d escalations with one already open, want 0", len(escs))
	}
}

// TestEvalOne_OtherFireErrorFilesNothing: only the privileged-spawn 403 means a
// missing seal; any other fire error stays a last_fire_error stamp.
func TestEvalOne_OtherFireErrorFilesNothing(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{RunBudget: 5})
	daemon.spawnErr = errors.New("HTTP 400: operator could not be resolved")

	engine.Evaluate(context.Background())

	if escs := spawnDeniedEscalations(daemon); len(escs) != 0 {
		t.Errorf("filed %d escalations for a 400, want 0", len(escs))
	}
}

func TestIsPrivilegedSpawnDenial(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"typed, wrapped", fmt.Errorf("spawning agent: %w", privilegedSpawnDenial), true},
		{"rendered text only", errors.New(`spawning agent "x": HTTP 403: spawn denied: role herder is privileged; a non-human caller must escalate`), true},
		{"403 but not a spawn denial", &beadsapi.APIError{StatusCode: 403, Message: "forbidden"}, false},
		{"spawn denied as a 400", &beadsapi.APIError{StatusCode: 400, Message: "spawn denied: not authorized for project p"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isPrivilegedSpawnDenial(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
