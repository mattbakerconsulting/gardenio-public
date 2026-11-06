package beadsapi

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestChannelMode_Concierge(t *testing.T) {
	p := ProjectInfo{
		ChannelModes: map[string]string{
			"C111": "concierge",
			"C222": "mention",
		},
	}
	if got := p.ChannelMode("C111"); got != "concierge" {
		t.Errorf("ChannelMode(C111) = %q, want %q", got, "concierge")
	}
	if got := p.ChannelMode("C222"); got != "mention" {
		t.Errorf("ChannelMode(C222) = %q, want %q", got, "mention")
	}
}

func TestChannelMode_DefaultMention(t *testing.T) {
	p := ProjectInfo{
		ChannelModes: map[string]string{"C111": "concierge"},
	}
	if got := p.ChannelMode("C999"); got != "mention" {
		t.Errorf("ChannelMode(C999) = %q, want %q", got, "mention")
	}
}

func TestChannelMode_NilMap(t *testing.T) {
	p := ProjectInfo{}
	if got := p.ChannelMode("C111"); got != "mention" {
		t.Errorf("ChannelMode(C111) = %q, want %q", got, "mention")
	}
}

func TestParseSlackChannels(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"single", "C123", []string{"C123"}},
		{"multi", "C123,C456, C789", []string{"C123", "C456", "C789"}},
		{"trailing comma", "C123,", []string{"C123"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSlackChannels(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("parseSlackChannels(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseSlackChannels(%q)[%d] = %q, want %q", tt.raw, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestHasChannel(t *testing.T) {
	p := ProjectInfo{SlackChannels: []string{"C111", "C222"}}
	if !p.HasChannel("C111") {
		t.Error("HasChannel(C111) = false, want true")
	}
	if p.HasChannel("C999") {
		t.Error("HasChannel(C999) = true, want false")
	}
}

func TestChannelRole(t *testing.T) {
	p := ProjectInfo{ChannelRoles: map[string][]string{"C111": {"ops"}}}
	if got := p.ChannelRole("C111"); got != "ops" {
		t.Errorf("ChannelRole(C111) = %q, want %q", got, "ops")
	}
	if got := p.ChannelRole("C999"); got != "" {
		t.Errorf("ChannelRole(C999) = %q, want %q", got, "")
	}
	// Multi-role channel: ChannelRole returns the FIRST role (for /spawn
	// callers that need a single default).
	multi := ProjectInfo{ChannelRoles: map[string][]string{"C222": {"first", "second"}}}
	if got := multi.ChannelRole("C222"); got != "first" {
		t.Errorf("ChannelRole(C222) multi-role = %q, want %q", got, "first")
	}
}

func TestChannelRuntimeClass(t *testing.T) {
	// gb-turbo channel pinned to runsc; other channels inherit (empty).
	p := ProjectInfo{ChannelRuntimeClasses: map[string]string{"C-turbo": "runsc"}}
	if got := p.ChannelRuntimeClass("C-turbo"); got != "runsc" {
		t.Errorf("ChannelRuntimeClass(C-turbo) = %q, want %q", got, "runsc")
	}
	if got := p.ChannelRuntimeClass("C-other"); got != "" {
		t.Errorf("ChannelRuntimeClass(C-other) = %q, want %q", got, "")
	}
	// Nil map must not panic and returns empty.
	empty := ProjectInfo{}
	if got := empty.ChannelRuntimeClass("C-turbo"); got != "" {
		t.Errorf("ChannelRuntimeClass on empty project = %q, want empty", got)
	}
}

func TestChannelForRole(t *testing.T) {
	p := ProjectInfo{ChannelRoles: map[string][]string{
		"C111": {"ops"},
		"C222": {"crew"},
		"C333": {"ops"}, // duplicate role → deterministic pick
	}}
	if got := p.ChannelForRole("crew"); got != "C222" {
		t.Errorf("ChannelForRole(crew) = %q, want C222", got)
	}
	// "ops" appears on C111 and C333; lexicographic first must win.
	if got := p.ChannelForRole("ops"); got != "C111" {
		t.Errorf("ChannelForRole(ops) = %q, want C111", got)
	}
	if got := p.ChannelForRole("missing"); got != "" {
		t.Errorf("ChannelForRole(missing) = %q, want empty", got)
	}
	if got := p.ChannelForRole(""); got != "" {
		t.Errorf("ChannelForRole(empty) = %q, want empty", got)
	}
	empty := ProjectInfo{}
	if got := empty.ChannelForRole("ops"); got != "" {
		t.Errorf("ChannelForRole on empty project = %q, want empty", got)
	}

	// Many-role-per-channel (ka-0000006075): each role in a channel's list
	// must be discoverable via ChannelForRole.
	multi := ProjectInfo{ChannelRoles: map[string][]string{
		"CTROUBLE": {"monorepo-repro", "monorepo-fix", "monorepo-verify"},
		"COTHER":   {"monorepo-publish"},
	}}
	if got := multi.ChannelForRole("monorepo-repro"); got != "CTROUBLE" {
		t.Errorf("ChannelForRole(monorepo-repro) = %q, want CTROUBLE", got)
	}
	if got := multi.ChannelForRole("monorepo-fix"); got != "CTROUBLE" {
		t.Errorf("ChannelForRole(monorepo-fix) = %q, want CTROUBLE", got)
	}
	if got := multi.ChannelForRole("monorepo-verify"); got != "CTROUBLE" {
		t.Errorf("ChannelForRole(monorepo-verify) = %q, want CTROUBLE", got)
	}
	if got := multi.ChannelForRole("monorepo-publish"); got != "COTHER" {
		t.Errorf("ChannelForRole(monorepo-publish) = %q, want COTHER", got)
	}
}

func TestParseChannelRolesMap(t *testing.T) {
	got := ParseChannelRolesMap(map[string]string{
		"C1": "role-a",
		"C2": "role-a,role-b,role-c",
		"C3": " role-x , role-y ",
		"C4": "",
		"C5": ",,",
		"":   "role-z",
	})
	want := map[string][]string{
		"C1": {"role-a"},
		"C2": {"role-a", "role-b", "role-c"},
		"C3": {"role-x", "role-y"},
	}
	if len(got) != len(want) {
		t.Fatalf("ParseChannelRolesMap len = %d want %d, got %#v", len(got), len(want), got)
	}
	for k, v := range want {
		gv := got[k]
		if len(gv) != len(v) {
			t.Errorf("ParseChannelRolesMap[%q] len = %d want %d (%#v)", k, len(gv), len(v), gv)
			continue
		}
		for i := range v {
			if gv[i] != v[i] {
				t.Errorf("ParseChannelRolesMap[%q][%d] = %q want %q", k, i, gv[i], v[i])
			}
		}
	}
	if got := ParseChannelRolesMap(nil); got != nil {
		t.Errorf("ParseChannelRolesMap(nil) = %#v want nil", got)
	}
	if got := ParseChannelRolesMap(map[string]string{"": ""}); got != nil {
		t.Errorf("ParseChannelRolesMap(empty-values) = %#v want nil", got)
	}
}

func TestChannelConflicts_NoConflict(t *testing.T) {
	projects := map[string]ProjectInfo{
		"alpha": {SlackChannels: []string{"C111"}},
		"beta":  {SlackChannels: []string{"C222"}},
	}
	conflicts := ChannelConflicts(projects)
	if len(conflicts) != 0 {
		t.Errorf("expected no conflicts, got %v", conflicts)
	}
}

func TestChannelConflicts_Duplicate(t *testing.T) {
	projects := map[string]ProjectInfo{
		"alpha": {SlackChannels: []string{"C111", "C222"}},
		"beta":  {SlackChannels: []string{"C222", "C333"}},
	}
	conflicts := ChannelConflicts(projects)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %v", len(conflicts), conflicts)
	}
	names, ok := conflicts["C222"]
	if !ok {
		t.Fatal("expected conflict on C222")
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("conflict projects = %v, want [alpha beta]", names)
	}
}

func TestChannelConflicts_Empty(t *testing.T) {
	conflicts := ChannelConflicts(nil)
	if conflicts != nil {
		t.Errorf("expected nil, got %v", conflicts)
	}
}

func TestGitURLConflicts_NoConflict(t *testing.T) {
	projects := map[string]ProjectInfo{
		"alpha": {GitURL: "https://gitlab.com/org/alpha"},
		"beta":  {GitURL: "https://gitlab.com/org/beta"},
	}
	conflicts := GitURLConflicts(projects)
	if len(conflicts) != 0 {
		t.Errorf("expected no conflicts, got %v", conflicts)
	}
}

func TestGitURLConflicts_Duplicate(t *testing.T) {
	// Two projects claim the same repo, spelled with a .git suffix and a
	// trailing slash respectively; normalization must collapse them so the
	// collision is detected (this is the aws-cost-management/pooltest→gardenio
	// latent case, ka-0000002164).
	projects := map[string]ProjectInfo{
		"alpha": {GitURL: "https://gitlab.com/org/shared.git"},
		"beta":  {GitURL: "https://gitlab.com/org/shared/"},
	}
	conflicts := GitURLConflicts(projects)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %v", len(conflicts), conflicts)
	}
	names, ok := conflicts["https://gitlab.com/org/shared"]
	if !ok {
		t.Fatalf("expected conflict on normalized git_url, got %v", conflicts)
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("conflict projects = %v, want [alpha beta]", names)
	}
}

func TestGitURLConflicts_IgnoresEmpty(t *testing.T) {
	// Two satellite projects with empty git_url are NOT a conflict — they
	// legitimately own no repo and only hold repos[] references.
	projects := map[string]ProjectInfo{
		"sat1": {GitURL: ""},
		"sat2": {GitURL: ""},
	}
	conflicts := GitURLConflicts(projects)
	if conflicts != nil {
		t.Errorf("expected nil (empty git_urls ignored), got %v", conflicts)
	}
}

func TestGitURLConflicts_Empty(t *testing.T) {
	conflicts := GitURLConflicts(nil)
	if conflicts != nil {
		t.Errorf("expected nil, got %v", conflicts)
	}
}

func TestReposConflicts_NoConflict(t *testing.T) {
	projects := map[string]ProjectInfo{
		"alpha": {Repos: []RepoEntry{{URL: "https://gitlab.com/org/a"}}},
		"beta":  {Repos: []RepoEntry{{URL: "https://gitlab.com/org/b"}}},
	}
	conflicts := ReposConflicts(projects)
	if len(conflicts) != 0 {
		t.Errorf("expected no conflicts, got %v", conflicts)
	}
}

func TestReposConflicts_Duplicate(t *testing.T) {
	// devsecops + monorepo both list a shared reference (the pre-trim state,
	// ka-0000002164). The detector must flag it so a future re-add is caught.
	projects := map[string]ProjectInfo{
		"monorepo": {Repos: []RepoEntry{
			{URL: "https://gitlab.com/org/site-deployment.git"},
			{URL: "https://gitlab.com/org/monorepo"},
		}},
		"devsecops": {Repos: []RepoEntry{
			{URL: "https://gitlab.com/org/site-deployment/"},
		}},
	}
	conflicts := ReposConflicts(projects)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %v", len(conflicts), conflicts)
	}
	names, ok := conflicts["https://gitlab.com/org/site-deployment"]
	if !ok {
		t.Fatalf("expected conflict on site-deployment, got %v", conflicts)
	}
	if len(names) != 2 || names[0] != "devsecops" || names[1] != "monorepo" {
		t.Errorf("conflict projects = %v, want [devsecops monorepo]", names)
	}
}

func TestReposConflicts_SameRepoTwiceInOneProject(t *testing.T) {
	// A repo listed twice within a single project is not a cross-project
	// conflict — it counts once for that project.
	projects := map[string]ProjectInfo{
		"alpha": {Repos: []RepoEntry{
			{URL: "https://gitlab.com/org/dup"},
			{URL: "https://gitlab.com/org/dup.git"},
		}},
	}
	conflicts := ReposConflicts(projects)
	if conflicts != nil {
		t.Errorf("expected nil (same repo twice in one project), got %v", conflicts)
	}
}

func TestReposConflicts_Empty(t *testing.T) {
	conflicts := ReposConflicts(nil)
	if conflicts != nil {
		t.Errorf("expected nil, got %v", conflicts)
	}
}

func TestNormalizeGitURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "https://gitlab.com/org/repo", "https://gitlab.com/org/repo"},
		{"trailing slash", "https://gitlab.com/org/repo/", "https://gitlab.com/org/repo"},
		{"git suffix", "https://gitlab.com/org/repo.git", "https://gitlab.com/org/repo"},
		{"uppercase", "https://GitLab.com/Org/Repo", "https://gitlab.com/org/repo"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeGitURL(tt.in); got != tt.want {
				t.Errorf("NormalizeGitURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestProjectForChannel_Unique(t *testing.T) {
	projects := map[string]ProjectInfo{
		"alpha": {SlackChannels: []string{"C111"}},
		"beta":  {SlackChannels: []string{"C222"}},
	}
	project, conflicts := ProjectForChannel(projects, "C111")
	if project != "alpha" || conflicts != nil {
		t.Errorf("ProjectForChannel(C111) = %q, %v; want alpha, nil", project, conflicts)
	}
}

func TestProjectForChannel_Ambiguous(t *testing.T) {
	projects := map[string]ProjectInfo{
		"alpha": {SlackChannels: []string{"C111"}},
		"beta":  {SlackChannels: []string{"C111"}},
	}
	project, conflicts := ProjectForChannel(projects, "C111")
	if project != "" {
		t.Errorf("expected empty project for ambiguous channel, got %q", project)
	}
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d: %v", len(conflicts), conflicts)
	}
	// Sorted order.
	if conflicts[0] != "alpha" || conflicts[1] != "beta" {
		t.Errorf("conflicts = %v, want [alpha beta]", conflicts)
	}
}

func TestProjectForChannel_NoMatch(t *testing.T) {
	projects := map[string]ProjectInfo{
		"alpha": {SlackChannels: []string{"C111"}},
	}
	project, conflicts := ProjectForChannel(projects, "C999")
	if project != "" || conflicts != nil {
		t.Errorf("ProjectForChannel(C999) = %q, %v; want empty, nil", project, conflicts)
	}
}

func TestEffectivePools_NilOrDisabled(t *testing.T) {
	var nilCfg *PrewarmedPoolConfig
	if got := nilCfg.EffectivePools(); got != nil {
		t.Errorf("nil config: expected nil, got %v", got)
	}
	disabled := &PrewarmedPoolConfig{Enabled: false, Role: "thread"}
	if got := disabled.EffectivePools(); got != nil {
		t.Errorf("disabled config: expected nil, got %v", got)
	}
}

func TestEffectivePools_SingleRoleDefaults(t *testing.T) {
	// Top-level-only config with all fields blank → one pool with defaults.
	cfg := &PrewarmedPoolConfig{Enabled: true}
	pools := cfg.EffectivePools()
	if len(pools) != 1 {
		t.Fatalf("expected 1 pool, got %d: %v", len(pools), pools)
	}
	p := pools[0]
	if p.Role != "thread" || p.Mode != "crew" || p.MinSize != 2 || p.MaxSize != 5 {
		t.Errorf("defaults wrong: %+v", p)
	}
}

func TestEffectivePools_SingleRoleExplicit(t *testing.T) {
	cfg := &PrewarmedPoolConfig{
		Enabled: true, Role: "ops", Mode: "job", MinSize: 3, MaxSize: 7,
	}
	pools := cfg.EffectivePools()
	if len(pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(pools))
	}
	want := PrewarmedRolePool{Role: "ops", Mode: "job", MinSize: 3, MaxSize: 7}
	if pools[0] != want {
		t.Errorf("got %+v, want %+v", pools[0], want)
	}
}

func TestEffectivePools_MultiRoleInheritsDefaults(t *testing.T) {
	// Roles list takes precedence; each entry inherits unset fields from the
	// top-level defaults.
	cfg := &PrewarmedPoolConfig{
		Enabled: true, MinSize: 4, MaxSize: 9, Mode: "crew", Role: "thread",
		Roles: []PrewarmedRolePool{
			{Role: "thread"}, // inherits 4/9/crew
			{Role: "test-role", MinSize: 1, MaxSize: 2}, // overrides sizes
			{Role: "ops", Mode: "job"},                  // overrides mode
		},
	}
	pools := cfg.EffectivePools()
	if len(pools) != 3 {
		t.Fatalf("expected 3 pools, got %d: %v", len(pools), pools)
	}
	byRole := map[string]PrewarmedRolePool{}
	for _, p := range pools {
		byRole[p.Role] = p
	}
	if p := byRole["thread"]; p.MinSize != 4 || p.MaxSize != 9 || p.Mode != "crew" {
		t.Errorf("thread pool wrong: %+v", p)
	}
	if p := byRole["test-role"]; p.MinSize != 1 || p.MaxSize != 2 || p.Mode != "crew" {
		t.Errorf("test-role pool wrong: %+v", p)
	}
	if p := byRole["ops"]; p.Mode != "job" || p.MinSize != 4 || p.MaxSize != 9 {
		t.Errorf("ops pool wrong: %+v", p)
	}
}

func TestEffectivePools_DuplicateRolesCollapse(t *testing.T) {
	cfg := &PrewarmedPoolConfig{
		Enabled: true, Role: "thread", Mode: "crew", MinSize: 2, MaxSize: 5,
		Roles: []PrewarmedRolePool{
			{Role: "dup", MinSize: 1},
			{Role: "dup", MinSize: 8}, // dropped — first wins
		},
	}
	pools := cfg.EffectivePools()
	if len(pools) != 1 {
		t.Fatalf("expected 1 deduped pool, got %d: %v", len(pools), pools)
	}
	if pools[0].MinSize != 1 {
		t.Errorf("expected first entry to win (MinSize 1), got %d", pools[0].MinSize)
	}
}

func TestEffectivePools_EmptyRoleInListUsesDefault(t *testing.T) {
	cfg := &PrewarmedPoolConfig{
		Enabled: true, Role: "fallback", Mode: "crew", MinSize: 2, MaxSize: 5,
		Roles: []PrewarmedRolePool{{Role: ""}},
	}
	pools := cfg.EffectivePools()
	if len(pools) != 1 || pools[0].Role != "fallback" {
		t.Fatalf("expected single pool with default role 'fallback', got %v", pools)
	}
}

// ConfiguredMinSize returns the RAW operator-set min (pre-K8s-default), the
// opt-in signal a lambda_microvm pool is gated on. Unlike EffectivePools it
// must NOT fill the default of 2 — an unset min is 0 (not opted in).
func TestConfiguredMinSize(t *testing.T) {
	tests := []struct {
		name string
		cfg  *PrewarmedPoolConfig
		role string
		want int
	}{
		{"nil config", nil, "thread", 0},
		{"disabled", &PrewarmedPoolConfig{Enabled: false, MinSize: 3, Role: "thread"}, "thread", 0},
		{"single-role unset min → 0 (NOT defaulted to 2)",
			&PrewarmedPoolConfig{Enabled: true, Role: "thread"}, "thread", 0},
		{"single-role explicit min",
			&PrewarmedPoolConfig{Enabled: true, Role: "thread", MinSize: 1}, "thread", 1},
		{"single-role default role name when unset",
			&PrewarmedPoolConfig{Enabled: true, MinSize: 4}, "thread", 4},
		{"single-role role mismatch → 0",
			&PrewarmedPoolConfig{Enabled: true, Role: "thread", MinSize: 4}, "ops", 0},
		{"multi-role explicit entry min",
			&PrewarmedPoolConfig{Enabled: true, Role: "thread", MinSize: 9,
				Roles: []PrewarmedRolePool{{Role: "ops", MinSize: 2}}}, "ops", 2},
		{"multi-role entry inherits top-level raw min",
			&PrewarmedPoolConfig{Enabled: true, Role: "thread", MinSize: 3,
				Roles: []PrewarmedRolePool{{Role: "ops"}}}, "ops", 3},
		{"multi-role entry inherits top-level UNSET min → 0",
			&PrewarmedPoolConfig{Enabled: true, Role: "thread",
				Roles: []PrewarmedRolePool{{Role: "ops"}}}, "ops", 0},
		{"multi-role dup: first entry wins",
			&PrewarmedPoolConfig{Enabled: true, Role: "thread",
				Roles: []PrewarmedRolePool{{Role: "dup", MinSize: 1}, {Role: "dup", MinSize: 8}}}, "dup", 1},
		{"multi-role empty role uses default role",
			&PrewarmedPoolConfig{Enabled: true, Role: "fallback", MinSize: 5,
				Roles: []PrewarmedRolePool{{Role: ""}}}, "fallback", 5},
		{"multi-role role not served → 0",
			&PrewarmedPoolConfig{Enabled: true, Role: "thread", MinSize: 2,
				Roles: []PrewarmedRolePool{{Role: "ops", MinSize: 1}}}, "thread", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.ConfiguredMinSize(tt.role); got != tt.want {
				t.Errorf("ConfiguredMinSize(%q) = %d, want %d", tt.role, got, tt.want)
			}
		})
	}
}

// The opt-in signal must agree with EffectivePools on WHICH role each pool
// serves: for every pool EffectivePools produces, ConfiguredMinSize(role) is a
// non-defaulted view of that same pool's min (0 when the operator left it unset,
// the pool's explicit/inherited value otherwise).
func TestConfiguredMinSize_TracksEffectivePools(t *testing.T) {
	cfg := &PrewarmedPoolConfig{
		Enabled: true, Role: "thread", MinSize: 4, MaxSize: 9,
		Roles: []PrewarmedRolePool{
			{Role: "thread"},                            // inherits raw 4
			{Role: "sized", MinSize: 1},                 // explicit 1
			{Role: "inherit"},                           // inherits raw 4
		},
	}
	for _, p := range cfg.EffectivePools() {
		got := cfg.ConfiguredMinSize(p.Role)
		// EffectivePools fills unset→4 (top-level default); ConfiguredMinSize
		// returns that same inherited raw value here because the top level IS set.
		if got != p.MinSize {
			t.Errorf("role %q: ConfiguredMinSize=%d, EffectivePools MinSize=%d", p.Role, got, p.MinSize)
		}
	}
}

// withCapturedSlog redirects the default slog logger to a buffer for the
// duration of fn and returns whatever it logged, restoring the prior logger.
func withCapturedSlog(fn func()) string {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestUnmarshalProjectField_Empty(t *testing.T) {
	var out map[string]string
	logs := withCapturedSlog(func() {
		if unmarshalProjectField("proj", "channel_roles", "", &out) {
			t.Fatal("empty raw should report false (nothing parsed)")
		}
	})
	if out != nil {
		t.Fatalf("expected out untouched on empty raw, got %v", out)
	}
	if logs != "" {
		t.Fatalf("empty raw must not warn, got: %q", logs)
	}
}

func TestUnmarshalProjectField_Valid(t *testing.T) {
	var out map[string]string
	logs := withCapturedSlog(func() {
		if !unmarshalProjectField("proj", "channel_roles", `{"C1":"crew"}`, &out) {
			t.Fatal("valid JSON should report true")
		}
	})
	if out["C1"] != "crew" {
		t.Fatalf("expected parsed value, got %v", out)
	}
	if logs != "" {
		t.Fatalf("valid JSON must not warn, got: %q", logs)
	}
}

func TestUnmarshalProjectField_MalformedWarnsAndSkips(t *testing.T) {
	var out []SecretEntry
	logs := withCapturedSlog(func() {
		if unmarshalProjectField("myproj", "secrets", `{not json`, &out) {
			t.Fatal("malformed JSON should report false")
		}
	})
	if out != nil {
		t.Fatalf("malformed JSON must leave out at zero value, got %v", out)
	}
	// The warning must carry the structured project+field context so the
	// failure is greppable and surfaceable, not silently swallowed.
	for _, want := range []string{"level=WARN", "malformed project JSON field", "project=myproj", "field=secrets"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("warning missing %q; got: %q", want, logs)
		}
	}
}

func TestUnmarshalProjectField_WrongShapeWarns(t *testing.T) {
	// Well-formed JSON of the wrong shape (array where a map is expected)
	// must also warn, not silently blank the feature.
	var out map[string]string
	logs := withCapturedSlog(func() {
		if unmarshalProjectField("p", "env_json", `[1,2,3]`, &out) {
			t.Fatal("wrong-shape JSON should report false")
		}
	})
	if !strings.Contains(logs, "field=env_json") || !strings.Contains(logs, "level=WARN") {
		t.Fatalf("expected a structured warning for wrong-shape JSON, got: %q", logs)
	}
}
