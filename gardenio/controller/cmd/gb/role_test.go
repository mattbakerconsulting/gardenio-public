package main

import (
	"context"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeRoleData is an in-memory roleDataSource / roleScaffoldWriter for the role
// command tests. roles is keyed by role name; configBeads is the set of config
// beads ListBeadsFiltered returns (only Title + Labels matter here).
type fakeRoleData struct {
	roles       map[string]beadsapi.RoleInfo
	configBeads []*beadsapi.BeadDetail
	listErr     error
	cfgErr      error

	created []beadsapi.CreateBeadRequest
}

func (f *fakeRoleData) ListRoleBeads(_ context.Context) (map[string]beadsapi.RoleInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.roles, nil
}

func (f *fakeRoleData) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.cfgErr != nil {
		return nil, f.cfgErr
	}
	return &beadsapi.ListBeadsResult{Beads: f.configBeads}, nil
}

func (f *fakeRoleData) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.created = append(f.created, req)
	return "gd-NEW" + req.Type, nil
}

func instrBead(role string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{Title: "claude-instructions", Labels: []string{"role:" + role}}
}

// TestBuiltinRolesParity is the drift guard: gb's builtinRoles copy MUST equal
// the controller's authoritative map (cmd/controller/project_role_cache.go). The controller's
// map lives in its own package main and cannot be imported, so we parse the role
// keys straight out of its source and compare. If someone adds a builtin role to
// the controller without updating gb, `gb role list` / the spawn warning would
// silently disagree with the controller — this test fails first
// (ka-0000001425).
func TestBuiltinRolesParity(t *testing.T) {
	src, err := os.ReadFile("../controller/project_role_cache.go")
	if err != nil {
		t.Fatalf("reading controller project_role_cache.go: %v", err)
	}
	block := regexp.MustCompile(`(?s)var builtinRoles = map\[string\]bool\{(.*?)\}`).FindSubmatch(src)
	if block == nil {
		t.Fatal("could not find builtinRoles map in controller project_role_cache.go")
	}
	keyRe := regexp.MustCompile(`"([^"]+)"\s*:\s*true`)
	want := map[string]bool{}
	for _, m := range keyRe.FindAllStringSubmatch(string(block[1]), -1) {
		want[m[1]] = true
	}
	if len(want) == 0 {
		t.Fatal("parsed zero builtin roles from controller source — regex drift?")
	}
	if len(want) != len(builtinRoles) {
		t.Fatalf("builtin role count drift: controller=%v gb=%v", keys(want), keys(builtinRoles))
	}
	for r := range want {
		if !builtinRoles[r] {
			t.Errorf("controller builtin role %q missing from gb builtinRoles", r)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestCollectRoleRows_MergesSourcesAndInstructions(t *testing.T) {
	f := &fakeRoleData{
		roles: map[string]beadsapi.RoleInfo{
			// builtin name + bead → "both".
			"gardener": {Name: "gardener", Model: "claude-opus-4-8"},
			// bead-only → "bead".
			"fsm-triage": {Name: "fsm-triage", Account: "fsm-anthropic", Image: "img:1"},
		},
		configBeads: []*beadsapi.BeadDetail{
			instrBead("fsm-triage"),               // fsm-triage has custom instructions
			{Title: "nudge-prompts", Labels: nil}, // unrelated config bead, ignored
		},
	}

	rows, err := collectRoleRows(context.Background(), f)
	if err != nil {
		t.Fatalf("collectRoleRows: %v", err)
	}

	byName := map[string]roleRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	// gardener: builtin + bead → both, no instructions bead → global default.
	if got := byName["gardener"]; got.Source != "both" || got.HasInstructions {
		t.Errorf("gardener: got source=%q hasInstr=%v, want both/false", got.Source, got.HasInstructions)
	}
	// crew: pure builtin (no bead) → builtin, no instructions.
	if got := byName["crew"]; got.Source != "builtin" || got.HasInstructions {
		t.Errorf("crew: got source=%q hasInstr=%v, want builtin/false", got.Source, got.HasInstructions)
	}
	// fsm-triage: bead-only + instructions bead present, overrides surfaced.
	got := byName["fsm-triage"]
	if got.Source != "bead" || !got.HasInstructions {
		t.Errorf("fsm-triage: got source=%q hasInstr=%v, want bead/true", got.Source, got.HasInstructions)
	}
	if !strings.Contains(got.Overrides, "account=fsm-anthropic") || !strings.Contains(got.Overrides, "image=img:1") {
		t.Errorf("fsm-triage overrides missing fields: %q", got.Overrides)
	}

	// Every builtin must appear even with no bead.
	for b := range builtinRoles {
		if _, ok := byName[b]; !ok {
			t.Errorf("builtin role %q missing from rows", b)
		}
	}

	// Rows must be sorted by name.
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Name > rows[i].Name {
			t.Errorf("rows not sorted: %q before %q", rows[i-1].Name, rows[i].Name)
		}
	}
}

// A daemon error listing config beads must NOT fail the whole command — roles
// still enumerate, just reported as instructions-unknown.
func TestCollectRoleRows_ConfigListErrorDegrades(t *testing.T) {
	f := &fakeRoleData{
		roles:  map[string]beadsapi.RoleInfo{"fsm-triage": {Name: "fsm-triage"}},
		cfgErr: context.DeadlineExceeded,
	}
	rows, err := collectRoleRows(context.Background(), f)
	if err != nil {
		t.Fatalf("collectRoleRows should degrade, got error: %v", err)
	}
	for _, r := range rows {
		if r.HasInstructions {
			t.Errorf("role %q reported instructions despite config-list error", r.Name)
		}
	}
}

func TestRoleWarning(t *testing.T) {
	f := &fakeRoleData{roles: map[string]beadsapi.RoleInfo{"fsm-triage": {Name: "fsm-triage"}}}

	tests := []struct {
		role     string
		wantWarn bool
	}{
		{"gardener", false},   // builtin
		{"crew", false},       // builtin
		{"fsm-triage", false}, // has type:role bead
		{"", false},           // empty role: no warning
		{"typo-role", true},   // unknown: warn
	}
	for _, tt := range tests {
		w := roleWarning(context.Background(), f, tt.role)
		if (w != "") != tt.wantWarn {
			t.Errorf("roleWarning(%q) = %q; wantWarn=%v", tt.role, w, tt.wantWarn)
		}
		if tt.wantWarn && !strings.Contains(w, "gb role scaffold") {
			t.Errorf("warning for %q should point at scaffold: %q", tt.role, w)
		}
	}
}

// roleWarning must fail open: a daemon error means "no warning", never a block.
func TestRoleWarning_FailsOpenOnError(t *testing.T) {
	f := &fakeRoleData{listErr: context.DeadlineExceeded}
	if w := roleWarning(context.Background(), f, "anything"); w != "" {
		t.Errorf("expected no warning on daemon error, got %q", w)
	}
}

// TestCollectRoleRows_ConfigOnlyRoleIsListed is the regression test for
// ka-0000006836: a role defined ONLY by a role:<name> label on config beads was
// absent from `gb role list` entirely. It still spawns pods (roles are
// free-string validated), so the omission hid live roles — the audit found 22,
// the busiest having spawned 1729 agents. It must now appear, flagged
// config-only, and a config bead that is NOT claude-instructions must still
// contribute the NAME while leaving HasInstructions false.
func TestCollectRoleRows_ConfigOnlyRoleIsListed(t *testing.T) {
	f := &fakeRoleData{
		roles: map[string]beadsapi.RoleInfo{"fsm-triage": {Name: "fsm-triage"}},
		configBeads: []*beadsapi.BeadDetail{
			instrBead("k6-perf"), // config-only, WITH instructions
			{Title: "context", Labels: []string{"role:ctx-only"}}, // config-only, no instructions
			{Title: "nudge-prompts", Labels: nil},                 // unscoped: contributes no role
		},
	}

	rows, err := collectRoleRows(context.Background(), f)
	if err != nil {
		t.Fatalf("collectRoleRows: %v", err)
	}
	byName := map[string]roleRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	got, ok := byName["k6-perf"]
	if !ok {
		t.Fatal("config-only role k6-perf missing from rows — the ka-0000006836 blind spot")
	}
	if got.Source != "config-only" || !got.HasInstructions {
		t.Errorf("k6-perf: got source=%q hasInstr=%v, want config-only/true", got.Source, got.HasInstructions)
	}
	if got.Overrides != "" {
		t.Errorf("config-only role must carry no pod overrides, got %q", got.Overrides)
	}

	// A non-instructions config bead names the role but must not claim it has
	// an instruction override — otherwise the INSTRUCTIONS column lies.
	ctxOnly, ok := byName["ctx-only"]
	if !ok {
		t.Fatal("role named only by a non-instructions config bead is missing")
	}
	if ctxOnly.Source != "config-only" || ctxOnly.HasInstructions {
		t.Errorf("ctx-only: got source=%q hasInstr=%v, want config-only/false", ctxOnly.Source, ctxOnly.HasInstructions)
	}

	// The pre-existing sources must not be reclassified by the new layer.
	if got := byName["fsm-triage"]; got.Source != "bead" {
		t.Errorf("fsm-triage: got source=%q, want bead", got.Source)
	}
	if got := byName["gardener"]; got.Source != "builtin" {
		t.Errorf("gardener: got source=%q, want builtin", got.Source)
	}
}

// A builtin or type:role bead must win over the config-only classification when
// the same name is ALSO carried by a config bead — the common case, since most
// real roles have both.
func TestCollectRoleRows_ConfigLabelDoesNotDowngradeKnownRoles(t *testing.T) {
	f := &fakeRoleData{
		roles: map[string]beadsapi.RoleInfo{"fsm-triage": {Name: "fsm-triage", Model: "m"}},
		configBeads: []*beadsapi.BeadDetail{
			instrBead("fsm-triage"), // bead role that also has instructions
			instrBead("gardener"),   // builtin that also has instructions
		},
	}
	rows, err := collectRoleRows(context.Background(), f)
	if err != nil {
		t.Fatalf("collectRoleRows: %v", err)
	}
	for _, r := range rows {
		switch r.Name {
		case "fsm-triage":
			if r.Source != "bead" {
				t.Errorf("fsm-triage downgraded to %q by its config bead", r.Source)
			}
		case "gardener":
			if r.Source != "builtin" {
				t.Errorf("gardener downgraded to %q by its config bead", r.Source)
			}
		}
	}
}

// A config-only role is REAL, not a typo, so roleWarning must say so
// distinctly. Reporting it as "unknown role" trains operators to ignore the
// warning, which then hides the actual typos it exists to catch.
func TestRoleWarning_DistinguishesConfigOnlyFromTypo(t *testing.T) {
	f := &fakeRoleData{
		roles:       map[string]beadsapi.RoleInfo{"fsm-triage": {Name: "fsm-triage"}},
		configBeads: []*beadsapi.BeadDetail{instrBead("k6-perf")},
	}

	configOnly := roleWarning(context.Background(), f, "k6-perf")
	if configOnly == "" {
		t.Fatal("config-only role should still produce a note (it sets no pod overrides)")
	}
	if strings.Contains(configOnly, "unknown role") {
		t.Errorf("config-only role must not be reported as unknown: %q", configOnly)
	}
	if !strings.Contains(configOnly, "config-only") {
		t.Errorf("config-only note should name the shape: %q", configOnly)
	}

	typo := roleWarning(context.Background(), f, "k6-prf")
	if !strings.Contains(typo, "unknown role") {
		t.Errorf("a genuine typo must still warn as unknown: %q", typo)
	}
}

// printRoleRows pads SOURCE wide enough for "config-only"; a too-narrow column
// would shunt INSTRUCTIONS/OVERRIDES out of alignment on exactly the rows the
// operator most needs to read.
func TestPrintRoleRows_ConfigOnlyColumnAlignment(t *testing.T) {
	var buf strings.Builder
	printRoleRows(&buf, []roleRow{
		{Name: "aaa", Source: "builtin", HasInstructions: true},
		{Name: "bbb", Source: "config-only", HasInstructions: false},
	})
	out := buf.String()

	lines := strings.Split(out, "\n")
	var header, configLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "ROLE") {
			header = l
		}
		if strings.HasPrefix(l, "bbb") {
			configLine = l
		}
	}
	if header == "" || configLine == "" {
		t.Fatalf("missing header or config-only row in output:\n%s", out)
	}
	if got, want := strings.Index(configLine, "global-default"), strings.Index(header, "INSTRUCTIONS"); got != want {
		t.Errorf("INSTRUCTIONS column misaligned: header col %d, row col %d\n%s", want, got, out)
	}
	if !strings.Contains(out, "1 role(s) are config-only") {
		t.Errorf("expected a config-only explainer footer:\n%s", out)
	}
}

func TestExecuteRoleScaffold_CreatesBothBeads(t *testing.T) {
	f := &fakeRoleData{roles: map[string]beadsapi.RoleInfo{}}
	plan := roleScaffoldPlan{
		Name:           "fsm-triage",
		Project:        "gardenio",
		RoleFields:     map[string]string{"model": "claude-haiku-4-5"},
		WithInstr:      true,
		InstrLabel:     "role:fsm-triage",
		InstrValueJSON: defaultRoleInstructionsJSON("fsm-triage"),
	}
	if err := executeRoleScaffold(context.Background(), f, plan); err != nil {
		t.Fatalf("executeRoleScaffold: %v", err)
	}
	if len(f.created) != 2 {
		t.Fatalf("expected 2 beads created, got %d", len(f.created))
	}

	var roleReq, instrReq *beadsapi.CreateBeadRequest
	for i := range f.created {
		switch f.created[i].Type {
		case "role":
			roleReq = &f.created[i]
		case "config":
			instrReq = &f.created[i]
		}
	}
	if roleReq == nil || instrReq == nil {
		t.Fatalf("expected one role and one config bead, got %+v", f.created)
	}
	if roleReq.Title != "fsm-triage" || roleReq.Kind != "config" {
		t.Errorf("role bead shape wrong: %+v", roleReq)
	}
	if !slices.Contains(roleReq.Labels, "project:gardenio") {
		t.Errorf("role bead missing project label: %v", roleReq.Labels)
	}
	if !strings.Contains(string(roleReq.Fields), "claude-haiku-4-5") {
		t.Errorf("role bead missing model field: %s", roleReq.Fields)
	}
	if instrReq.Title != "claude-instructions" || !slices.Contains(instrReq.Labels, "role:fsm-triage") {
		t.Errorf("instructions bead shape wrong: %+v", instrReq)
	}
	if !strings.Contains(instrReq.Description, "lifecycle") {
		t.Errorf("instructions bead body missing seed content: %s", instrReq.Description)
	}
}

// Idempotency: an existing role + existing instructions bead → zero creates.
func TestExecuteRoleScaffold_Idempotent(t *testing.T) {
	f := &fakeRoleData{
		roles:       map[string]beadsapi.RoleInfo{"fsm-triage": {Name: "fsm-triage"}},
		configBeads: []*beadsapi.BeadDetail{instrBead("fsm-triage")},
	}
	plan := roleScaffoldPlan{
		Name:           "fsm-triage",
		WithInstr:      true,
		InstrLabel:     "role:fsm-triage",
		InstrValueJSON: defaultRoleInstructionsJSON("fsm-triage"),
	}
	if err := executeRoleScaffold(context.Background(), f, plan); err != nil {
		t.Fatalf("executeRoleScaffold: %v", err)
	}
	if len(f.created) != 0 {
		t.Fatalf("expected 0 beads created (idempotent), got %d: %+v", len(f.created), f.created)
	}
}

// --no-instructions creates only the role bead.
func TestExecuteRoleScaffold_NoInstructions(t *testing.T) {
	f := &fakeRoleData{roles: map[string]beadsapi.RoleInfo{}}
	plan := roleScaffoldPlan{Name: "fsm-triage", WithInstr: false}
	if err := executeRoleScaffold(context.Background(), f, plan); err != nil {
		t.Fatalf("executeRoleScaffold: %v", err)
	}
	if len(f.created) != 1 || f.created[0].Type != "role" {
		t.Fatalf("expected only the role bead, got %+v", f.created)
	}
}

func TestIsValidRoleName(t *testing.T) {
	valid := []string{"crew", "fsm-triage", "auto-inspector", "haiku4-5"}
	invalid := []string{"", "Crew", "fsm_triage", "role name", "x/y", "Über"}
	for _, s := range valid {
		if !isValidRoleName(s) {
			t.Errorf("isValidRoleName(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if isValidRoleName(s) {
			t.Errorf("isValidRoleName(%q) = true, want false", s)
		}
	}
}

func TestApplyRoleSource_ExcludesSecrets(t *testing.T) {
	fields := map[string]string{}
	applyRoleSource(fields, beadsapi.RoleInfo{
		Image:   "img:1",
		Model:   "m",
		Account: "a",
		Secrets: []beadsapi.SecretEntry{{Env: "GITLAB_TOKEN", Secret: "s", Key: "k"}},
	})
	if fields["image"] != "img:1" || fields["model"] != "m" || fields["account"] != "a" {
		t.Errorf("applyRoleSource missing overrides: %v", fields)
	}
	// Secrets must never be cloned.
	for k := range fields {
		if strings.Contains(strings.ToLower(k), "secret") || strings.Contains(strings.ToLower(k), "token") {
			t.Errorf("applyRoleSource leaked a credential field: %q", k)
		}
	}
}

// applyRoleSource clones the per-role skill toggle (not a credential) as
// comma-separated fields, so `gb role scaffold --from` inherits the source
// role's enabled/disabled skill lists (Adam feedback #1, epic ka-0000004989).
func TestApplyRoleSource_ClonesSkillToggle(t *testing.T) {
	fields := map[string]string{}
	applyRoleSource(fields, beadsapi.RoleInfo{
		EnabledSkills:  []string{"onboarding", "video"},
		DisabledSkills: []string{"deploy"},
		EnabledAgents:  []string{"reviewer.md"},
		DisabledAgents: []string{"helm-deployer.md"},
	})
	if fields["enabled_skills"] != "onboarding,video" {
		t.Errorf("enabled_skills = %q, want %q", fields["enabled_skills"], "onboarding,video")
	}
	if fields["disabled_skills"] != "deploy" {
		t.Errorf("disabled_skills = %q, want %q", fields["disabled_skills"], "deploy")
	}
	// The sub-agent toggle clones the same way (ka-0000006908).
	if fields["enabled_agents"] != "reviewer.md" {
		t.Errorf("enabled_agents = %q, want %q", fields["enabled_agents"], "reviewer.md")
	}
	if fields["disabled_agents"] != "helm-deployer.md" {
		t.Errorf("disabled_agents = %q, want %q", fields["disabled_agents"], "helm-deployer.md")
	}
	// An unset toggle must not write an (empty) field.
	empty := map[string]string{}
	applyRoleSource(empty, beadsapi.RoleInfo{Image: "img"})
	for _, k := range []string{"enabled_skills", "disabled_skills", "enabled_agents", "disabled_agents"} {
		if _, ok := empty[k]; ok {
			t.Errorf("%s written for a role with no toggle", k)
		}
	}
}

// The scaffold plan carries enabled_skills/disabled_skills through to the
// created type:role bead's fields when the flags are set.
func TestExecuteRoleScaffold_WritesSkillToggle(t *testing.T) {
	f := &fakeRoleData{roles: map[string]beadsapi.RoleInfo{}}
	plan := roleScaffoldPlan{
		Name:    "fsm-triage",
		Project: "gardenio",
		RoleFields: map[string]string{
			"enabled_skills":  "onboarding,video",
			"disabled_skills": "deploy",
		},
		WithInstr: false,
	}
	if err := executeRoleScaffold(context.Background(), f, plan); err != nil {
		t.Fatalf("executeRoleScaffold: %v", err)
	}
	if len(f.created) != 1 || f.created[0].Type != "role" {
		t.Fatalf("expected one role bead, got %+v", f.created)
	}
	got := string(f.created[0].Fields)
	if !strings.Contains(got, "enabled_skills") || !strings.Contains(got, "onboarding,video") {
		t.Errorf("role bead fields missing enabled_skills: %s", got)
	}
	if !strings.Contains(got, "disabled_skills") || !strings.Contains(got, "deploy") {
		t.Errorf("role bead fields missing disabled_skills: %s", got)
	}
}

func TestPrintRoleRows(t *testing.T) {
	var b strings.Builder
	printRoleRows(&b, []roleRow{
		{Name: "gardener", Source: "both", HasInstructions: true, Overrides: "model=m"},
		{Name: "typo", Source: "bead", HasInstructions: false},
	})
	out := b.String()
	if !strings.Contains(out, "ROLE") || !strings.Contains(out, "gardener") || !strings.Contains(out, "custom") {
		t.Errorf("missing header/role/custom: %q", out)
	}
	if !strings.Contains(out, "global-default") {
		t.Errorf("a role with no instructions should render global-default: %q", out)
	}
	// Empty input renders the no-roles line, not a bare header.
	var e strings.Builder
	printRoleRows(&e, nil)
	if !strings.Contains(e.String(), "no roles") {
		t.Errorf("empty rows should say 'no roles', got %q", e.String())
	}
}

func TestPrintRoleScaffoldPlan(t *testing.T) {
	var b strings.Builder
	printRoleScaffoldPlan(&b, roleScaffoldPlan{
		Name:       "fsm-triage",
		Project:    "gardenio",
		RoleFields: map[string]string{"model": "m", "account": "a"},
		WithInstr:  true,
		InstrLabel: "role:fsm-triage",
	})
	out := b.String()
	if !strings.Contains(out, "fsm-triage") || !strings.Contains(out, "account=a model=m") {
		t.Errorf("plan output missing fields (overrides must be sorted): %q", out)
	}
	if !strings.Contains(out, "config:claude-instructions:role:fsm-triage") {
		t.Errorf("plan should show the instructions bead key: %q", out)
	}
	// --no-instructions path renders the global-defaults note.
	var n strings.Builder
	printRoleScaffoldPlan(&n, roleScaffoldPlan{Name: "x", WithInstr: false})
	if !strings.Contains(n.String(), "global defaults") {
		t.Errorf("no-instructions plan should note global defaults: %q", n.String())
	}
}

func TestSetOrDelete(t *testing.T) {
	m := map[string]string{"model": "old"}
	setOrDelete(m, "model", "")
	if _, ok := m["model"]; ok {
		t.Error("setOrDelete with empty value should delete the key")
	}
	setOrDelete(m, "model", "new")
	if m["model"] != "new" {
		t.Errorf("setOrDelete should set value, got %q", m["model"])
	}
}
