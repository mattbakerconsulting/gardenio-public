package bridge

// prerun_config_test.go — guards the per-role prerun config beads seeded by
// configBeadEntries() and the retraction contract (ka-0000007894): a prerun
// entry MUST ship with the removal of the instruction that mandated the command,
// so the result is relocated into the cached prefix rather than duplicated.

import (
	"encoding/json"
	"strings"
	"testing"
)

// getPrerunEntries extracts the prerun entry list for a role's config bead.
func getPrerunEntries(t *testing.T, role string) []map[string]any {
	t.Helper()
	entries := configBeadEntries()
	raw, ok := entries["config:prerun:role:"+role]
	if !ok {
		t.Fatalf("configBeadEntries missing config:prerun:role:%s", role)
	}
	// The value round-trips through JSON exactly as EnsureConfigs marshals it.
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal prerun config for %s: %v", role, err)
	}
	var decoded struct {
		Prerun []map[string]any `json:"prerun"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal prerun config for %s: %v", role, err)
	}
	if len(decoded.Prerun) == 0 {
		t.Fatalf("role %s prerun list is empty", role)
	}
	return decoded.Prerun
}

// prerunMandatingVerbs are the same state-mutating fragments prerun.go rejects
// at runtime; duplicated here (separate package) so the seeded beads are also
// checked at authoring time — a mutating prerun would fire on every resume.
var prerunMandatingVerbs = []string{
	"kd create", "kd claim", "kd close", "kd update", "kd comment",
	"kd dep", "kd label", "kd delete",
	"gb spawn", "gb start", "gb decision", "gb escalation", "gb squawk",
	"gb stop", "gb yield", "gb mail", "gb done", "gb mr",
}

func TestPrerunConfig_GardenerAndPlanGardenerSeeded(t *testing.T) {
	for _, role := range []string{"gardener", "plan-gardener"} {
		entries := getPrerunEntries(t, role)

		var haveShow, haveNews bool
		for _, e := range entries {
			cmd, _ := e["cmd"].(string)
			if cmd == "" {
				t.Errorf("role %s: prerun entry missing cmd: %+v", role, e)
			}
			if id, _ := e["id"].(string); id == "" {
				t.Errorf("role %s: prerun entry missing id: %+v", role, e)
			}
			if strings.Contains(cmd, "kd show") {
				haveShow = true
			}
			if strings.Contains(cmd, "gb news") {
				haveNews = true
			}
			// Every entry must be read-only.
			for _, verb := range prerunMandatingVerbs {
				if cmd == verb || strings.HasPrefix(cmd, verb+" ") {
					t.Errorf("role %s: prerun cmd %q is a mutating verb — prerun re-fires on every resume", role, cmd)
				}
			}
			// Every entry must declare what it retracts (advice id and/or section).
			_, hasSections := e["retracts_sections"]
			_, hasAdvice := e["replaces_advice"]
			if !hasSections && !hasAdvice {
				t.Errorf("role %s: prerun entry %q declares no retraction (retracts_sections/replaces_advice) — injection without retraction is pure additive cost", role, cmd)
			}
		}
		if !haveShow {
			t.Errorf("role %s: prerun should pre-run `kd show <assigned>` (11/11 pods ran it — ka-0000007971)", role)
		}
		if !haveNews {
			t.Errorf("role %s: prerun should pre-run `gb news` (11/11 pods ran it — ka-0000007971)", role)
		}
	}
}

// getGlobalPrerunEntries extracts the prerun entry list for the global config
// bead (config:prerun:global) — the fleet-wide capability probe.
func getGlobalPrerunEntries(t *testing.T) []map[string]any {
	t.Helper()
	entries := configBeadEntries()
	raw, ok := entries["config:prerun:global"]
	if !ok {
		t.Fatal("configBeadEntries missing config:prerun:global")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal global prerun config: %v", err)
	}
	var decoded struct {
		Prerun []map[string]any `json:"prerun"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal global prerun config: %v", err)
	}
	if len(decoded.Prerun) == 0 {
		t.Fatal("global prerun list is empty")
	}
	return decoded.Prerun
}

// TestPrerunConfig_GlobalCapabilityProbeSeeded verifies the fleet-wide
// capability probe (ka-0000003174, escalation deep-dive ka-0000002006 §3) is
// seeded as a GLOBAL prerun so thread/crew/gardener all boot with a live view of
// what they can already do — the cheapest lever against stale-mental-model
// escalations.
func TestPrerunConfig_GlobalCapabilityProbeSeeded(t *testing.T) {
	entries := getGlobalPrerunEntries(t)

	var probe map[string]any
	for _, e := range entries {
		if id, _ := e["id"].(string); id == "capability-probe" {
			probe = e
		}
	}
	if probe == nil {
		t.Fatal("global prerun must include a `capability-probe` entry (ka-0000003174)")
	}

	cmd, _ := probe["cmd"].(string)
	if cmd == "" {
		t.Fatal("capability-probe entry missing cmd")
	}

	// Must be read-only — a prerun re-fires on every resume.
	for _, verb := range prerunMandatingVerbs {
		for _, seg := range strings.FieldsFunc(cmd, func(r rune) bool {
			return r == ';' || r == '|' || r == '&'
		}) {
			seg = strings.TrimSpace(seg)
			if seg == verb || strings.HasPrefix(seg, verb+" ") {
				t.Errorf("capability-probe cmd segment %q is a mutating verb — prerun re-fires on every resume", seg)
			}
		}
	}

	// Must probe the three identity surfaces the deep-dive named (AWS/K8s/GitLab)
	// via identity-scoped reads, so an agent can see what it holds before
	// escalating.
	for _, needle := range []string{"aws sts get-caller-identity", "kubectl auth", "glab api user"} {
		if !strings.Contains(cmd, needle) {
			t.Errorf("capability-probe cmd should probe %q; got: %q", needle, cmd)
		}
	}

	// CARE (ka-0000003174): output rides EVERY cached prompt, so the probe must
	// print identities/verdicts ONLY — never a secret VALUE. Guard against the
	// obvious value-dumping commands sneaking in.
	secretLeaks := []string{
		"get-secret-value", "get secret", "aws configure get",
		"printenv", "cat ", "kubectl get secret", "secretsmanager",
		"describe-secret", "--show-secret",
	}
	for _, leak := range secretLeaks {
		if strings.Contains(cmd, leak) {
			t.Errorf("capability-probe cmd must not print secret values (found %q): %q", leak, cmd)
		}
	}

	// Must declare a retraction (documentation guard shared with the per-role
	// entries): injection without retraction is pure additive cost.
	_, hasSections := probe["retracts_sections"]
	_, hasAdvice := probe["replaces_advice"]
	if !hasSections && !hasAdvice {
		t.Error("capability-probe entry declares no retraction (retracts_sections/replaces_advice)")
	}
}

// TestPrerunConfig_AssignedBeadPlaceholder verifies the kd show entry uses the
// {{.AssignedBead}} placeholder (resolved from BOAT_TASK_ID at runtime) rather
// than a hardcoded id.
func TestPrerunConfig_AssignedBeadPlaceholder(t *testing.T) {
	entries := getPrerunEntries(t, "gardener")
	for _, e := range entries {
		cmd, _ := e["cmd"].(string)
		if strings.Contains(cmd, "kd show") && !strings.Contains(cmd, "{{.AssignedBead}}") {
			t.Errorf("kd show prerun must use the {{.AssignedBead}} placeholder, got %q", cmd)
		}
	}
}

// TestPrerunConfig_GardenerInstructionsRetractMandates guards the retraction
// half of the contract: the gardener claude-instructions must NOT still command
// the agent to run `kd show` on turn 1 or list `gb news` as an unconditional
// ASSESS step, since those are now pre-injected. It must instead point the
// agent at the ## Prerun results section.
func TestPrerunConfig_GardenerInstructionsRetractMandates(t *testing.T) {
	entries := configBeadEntries()
	raw, ok := entries["config:claude-instructions:role:gardener"]
	if !ok {
		t.Fatal("missing config:claude-instructions:role:gardener")
	}
	sections, ok := raw.(map[string]string)
	if !ok {
		t.Fatal("gardener claude-instructions is not map[string]string")
	}

	lifecycle := sections["lifecycle"]
	if lifecycle == "" {
		t.Fatal("gardener lifecycle section is empty")
	}
	// The old step-1 "Read your task bead ..." mandate must be gone; the new
	// text must reference the prerun injection instead.
	if strings.Contains(lifecycle, "Read your task bead to understand your mission") {
		t.Error("gardener lifecycle still mandates `Read your task bead` on turn 1 — kd show is now pre-injected via prerun; retract the mandate")
	}
	if !strings.Contains(lifecycle, "Prerun results") {
		t.Error("gardener lifecycle should point the agent at the pre-injected ## Prerun results section")
	}

	workflows := sections["workflows"]
	if workflows == "" {
		t.Fatal("gardener workflows section is empty")
	}
	// The ASSESS block must no longer list a bare `gb news` command line as an
	// unconditional step (it's pre-injected); it should mention Prerun results.
	if strings.Contains(workflows, "gb news              (all in-progress work, shows spawner; splits live vs stale)") {
		t.Error("gardener ASSESS block still lists the bare `gb news` boot step — retract it (now pre-injected via prerun)")
	}
	if !strings.Contains(workflows, "Prerun results") {
		t.Error("gardener ASSESS block should reference the pre-injected ## Prerun results snapshot")
	}
}
