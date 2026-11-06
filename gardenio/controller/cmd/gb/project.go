package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// resolveProject is the ONE project resolver for cmd/gb: an explicit flag, then
// KD_PROJECT, then BOAT_PROJECT. Before this, three call sites disagreed —
// notably buildSubscriptions, which decided WHICH config beads a pod receives,
// ignored KD_PROJECT and honoured the dead BOAT_RIG alias.
func resolveProject(flag string) string {
	if p := strings.TrimSpace(flag); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("KD_PROJECT")); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv("BOAT_PROJECT"))
}

// defaultGBProject returns the project context for gb commands.
// Precedence: the explicit `gb prime --project` flag > KD_PROJECT > BOAT_PROJECT > "".
//
// The --project flag lets callers (notably the advice-supersession E2E and
// operators inspecting prime output for another project) scope the rendered
// identity header, project-scoped advice, and auto-assign without mutating the
// pod's KD_PROJECT/BOAT_PROJECT env. When the flag is unset it is the empty
// string and falls through to the env precedence, preserving prior behavior.
func defaultGBProject() string { return resolveProject(primeProject) }

// projectCmd is the parent for project-config orchestration: scaffolding a new
// type:project bead (`gb project create`) and wiring its Slack channels
// (`gb project channel`, project_channel.go); other project edits still happen
// through the beads-viewer hub and `kd`.
var projectCmd = &cobra.Command{
	Use:     "project",
	Short:   "Manage gardenio project config beads",
	GroupID: "orchestration",
}

// project-create flags.
var (
	projectGitURL        string
	projectAccount       string
	projectFrom          string
	projectPrefix        string
	projectDefaultBranch string
	projectRepoRole      string
	projectDryRun        bool
)

var projectCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Scaffold a PR-capable type:project config bead",
	Long: `Create a new project config bead (type:project) with the minimal field set an
agent needs to clone a repo and open PRs: git_url, prefix, default_branch,
account, and a primary repos[] entry.

The scaffold is validated against the controller's ProjectInfo struct set
(beadsapi/projects.go) BEFORE the bead is created, so a malformed repos[]/JSON
field or a dangling account reference is reported up front rather than being
silently dropped at spawn time. Nothing is written until validation passes.

  gb project create acme \
    --git-url https://gitlab.com/example-org/widgets.git \
    --account acme-anthropic

Clone the field set from an existing project bead and override what differs:

  gb project create acme-staging --from acme \
    --git-url https://gitlab.com/example-org/widgets.git

This command writes ONLY the project config bead. Git/GitLab credential secrets,
the type:account bead, and any custom role beads are separate steps — the
success output prints exactly what remains to wire (see the onboarding runbook,
ka-0000005921).`,
	Args: cobra.ExactArgs(1),
	RunE: runProjectCreate,
}

// projectScaffold is the minimal, PR-capable field set for a new type:project
// bead. It is the in-memory model the command builds, validates, and then
// serialises into the bead's fields map. Keeping it a struct (rather than
// poking a map directly) lets validateProjectScaffold reason about the whole
// shape — required fields, the repos[] entry, the account reference — in one
// place, pre-POST.
type projectScaffold struct {
	Name          string
	Prefix        string
	GitURL        string
	DefaultBranch string
	Account       string
	Repos         []beadsapi.RepoEntry
}

func runProjectCreate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	name := strings.TrimSpace(args[0])

	sc := projectScaffold{
		Name:          name,
		Prefix:        projectPrefix,
		GitURL:        strings.TrimSpace(projectGitURL),
		DefaultBranch: projectDefaultBranch,
		Account:       strings.TrimSpace(projectAccount),
	}

	// --from clones the field set from an existing project bead, then the
	// explicit flags below override whatever the operator wants to change.
	// Cloning happens BEFORE the flag overrides so `--from acme --git-url X`
	// inherits acme's prefix/account/branch but points at a new repo.
	if projectFrom != "" {
		src, err := daemon.GetProjectBead(ctx, projectFrom)
		if err != nil {
			return fmt.Errorf("cloning from project %q: %w", projectFrom, err)
		}
		applyProjectSource(&sc, src)
		// Re-apply explicit flag overrides on top of the cloned values. Only
		// flags the operator actually set should win; an unset flag must not
		// clobber a cloned value with its empty/default string.
		if cmd.Flags().Changed("prefix") {
			sc.Prefix = projectPrefix
		}
		if cmd.Flags().Changed("git-url") {
			sc.GitURL = strings.TrimSpace(projectGitURL)
		}
		if cmd.Flags().Changed("default-branch") {
			sc.DefaultBranch = projectDefaultBranch
		}
		if cmd.Flags().Changed("account") {
			sc.Account = strings.TrimSpace(projectAccount)
		}
	}

	// Keep the primary repos[] entry consistent with git_url. init-clone reads
	// repos[] BEFORE falling back to git_url, so a primary entry that disagrees
	// with git_url silently wins — which would make a `--from X --git-url Y`
	// clone clone X's repo, not Y. ensurePrimaryRepo materialises a primary
	// entry when none exists and retargets a stale one to git_url, leaving any
	// reference repos untouched.
	ensurePrimaryRepo(&sc, projectRepoRole)

	// Validate the whole scaffold against the projects.go struct set BEFORE any
	// write. Account resolution needs the live account list; a fetch failure is
	// non-fatal (we degrade to skipping only the dangling-reference check), so
	// the command still works when the daemon can't list accounts.
	var knownAccounts map[string]struct{}
	if accounts, err := daemon.ListAccountBeads(ctx); err == nil {
		knownAccounts = make(map[string]struct{}, len(accounts))
		for accName := range accounts {
			knownAccounts[accName] = struct{}{}
		}
	} else {
		fmt.Fprintf(os.Stderr, "warning: could not list accounts to validate --account reference: %v\n", err)
	}

	if problems := validateProjectScaffold(sc, knownAccounts); len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "cannot create project %q — fix these first:\n", name)
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		return fmt.Errorf("%d validation error(s); no bead created", len(problems))
	}

	fields, err := sc.fields()
	if err != nil {
		return fmt.Errorf("building project fields: %w", err)
	}

	if projectDryRun {
		fmt.Fprintf(os.Stdout, "dry-run: would create project %q (no bead written)\n", name)
		printProjectScaffold(os.Stdout, sc)
		return nil
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshalling project fields: %w", err)
	}

	// Label scope: a project bead is scoped under the CURRENT project (matching
	// the documented gardenio convention — pooltest carries project:gardenio, not
	// project:pooltest; the new project's *name* is its title). We emit exactly
	// one project:<current> label and let CreateBead pass it through verbatim,
	// sidestepping the [project:gardenio, project:gardenio] double-label papercut
	// that `kd create --type=project -l project:gardenio` hits.
	labels := projectScopeLabels()

	beadID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     name,
		Type:      "project",
		CreatedBy: actor,
		Labels:    labels,
		Fields:    json.RawMessage(fieldsJSON),
	})
	if err != nil {
		return fmt.Errorf("creating project bead: %w", err)
	}

	printProjectCreateSuccess(os.Stdout, beadID, sc)
	return nil
}

// applyProjectSource copies the PR-capable field set from an existing
// ProjectInfo into the scaffold. It does NOT copy the source's name (the new
// project gets its own) and only carries the minimal fields this command owns;
// richer config (secrets, channels, pools) is intentionally left for the
// operator to wire on the new bead so a clone never silently inherits another
// project's credentials.
func applyProjectSource(sc *projectScaffold, src beadsapi.ProjectInfo) {
	sc.Prefix = src.Prefix
	sc.GitURL = src.GitURL
	sc.DefaultBranch = src.DefaultBranch
	sc.Account = src.Account
	if len(src.Repos) > 0 {
		sc.Repos = append([]beadsapi.RepoEntry(nil), src.Repos...)
	}
}

// ensurePrimaryRepo normalises the scaffold's repos[] so it carries EXACTLY ONE
// primary entry: git_url on default_branch. The command owns a single primary
// repo (the one cloned into the workspace's work/ checkout), so all
// primary-eligible entries (role "" or "primary") collapse into one canonical
// entry built fresh from git_url. This is what makes the git_url/default_branch
// values authoritative for the primary:
//
//   - no repos[] at all → the canonical primary is the only entry.
//   - a cloned (--from) primary whose URL/branch/name disagree with git_url →
//     discarded in favour of the canonical entry, so a `--from X --git-url Y`
//     clone clones Y on the new default_branch, not X's stale url/branch/name.
//     init-clone reads repos[] before falling back to git_url, so a stale
//     primary here would silently win — the exact footgun this prevents.
//   - two primary-eligible entries → folded into one (two primaries would both
//     claim the work/ checkout).
//
// Reference (and any other explicitly-roled) entries are preserved in order.
// With an empty git_url it is a no-op (validation rejects the scaffold).
func ensurePrimaryRepo(sc *projectScaffold, role string) {
	if sc.GitURL == "" {
		return
	}
	if role == "" {
		role = "primary"
	}
	rebuilt := []beadsapi.RepoEntry{{
		URL:    sc.GitURL,
		Role:   role,
		Branch: sc.DefaultBranch,
	}}
	for _, r := range sc.Repos {
		// Skip primary-eligible entries — they are subsumed by the canonical
		// primary above. Keep reference and custom-role entries untouched.
		if r.Role == "" || r.Role == "primary" {
			continue
		}
		rebuilt = append(rebuilt, r)
	}
	sc.Repos = rebuilt
}

// fields renders the scaffold into the bead's string field map, matching the
// daemon type:project schema (kbeads config.go) and the controller parser key
// names (projects.go). Only non-empty values are emitted so the bead stays
// minimal and legacy-compatible.
func (sc projectScaffold) fields() (map[string]string, error) {
	fields := map[string]string{}
	if sc.Prefix != "" {
		fields["prefix"] = sc.Prefix
	}
	if sc.GitURL != "" {
		fields["git_url"] = sc.GitURL
	}
	if sc.DefaultBranch != "" {
		fields["default_branch"] = sc.DefaultBranch
	}
	if sc.Account != "" {
		fields["account"] = sc.Account
	}
	if len(sc.Repos) > 0 {
		reposJSON, err := json.Marshal(sc.Repos)
		if err != nil {
			return nil, fmt.Errorf("marshalling repos: %w", err)
		}
		fields["repos"] = string(reposJSON)
	}
	return fields, nil
}

// validateProjectScaffold checks the scaffold against the controller's
// ProjectInfo struct set BEFORE any bead is written. It returns a sorted list
// of human-readable problems; an empty slice means the scaffold is safe to
// create. The daemon accepts a project bead with missing/empty fields and the
// controller silently drops malformed JSON — both produce a pod that spawns but
// cannot make PRs with NO operator-visible error. This function front-loads
// those failures.
//
// knownAccounts is the set of live type:account bead names; pass nil to skip
// the dangling-account check (e.g. when the account list could not be fetched).
func validateProjectScaffold(sc projectScaffold, knownAccounts map[string]struct{}) []string {
	var problems []string

	if strings.TrimSpace(sc.Name) == "" {
		problems = append(problems, "project name (the bead title) is required")
	} else if strings.HasPrefix(sc.Name, "Project: ") {
		// The controller strips a "Project: " title prefix on read, so a name
		// carrying it resolves to something other than what the operator typed.
		problems = append(problems, `project name must not start with "Project: " (the controller strips that prefix on read)`)
	}

	// git_url is required in practice: an empty git_url makes init-clone a
	// no-op and the agent spawns into an empty workspace with no error.
	if sc.GitURL == "" {
		problems = append(problems, "--git-url is required (without it agents clone an empty workspace and cannot open PRs)")
	} else if !looksLikeGitURL(sc.GitURL) {
		problems = append(problems, fmt.Sprintf("--git-url %q does not look like a git remote URL (expected https://host/group/repo.git or git@host:group/repo.git)", sc.GitURL))
	}

	if sc.Prefix == "" {
		problems = append(problems, "--prefix is required (the bead-ID prefix for this project's issues, e.g. kd)")
	}

	// default_branch is part of the minimal field set: init-clone clones it, so
	// an empty value (e.g. a --from clone of a source bead lacking the field)
	// would clone a nonexistent branch. The flag defaults to "main", but a clone
	// path can still empty it, so guard the resolved value.
	if sc.DefaultBranch == "" {
		problems = append(problems, "--default-branch is required (init-clone clones this branch; empty would clone a nonexistent ref)")
	}

	// Round-trip repos[] through the SAME struct the controller parser uses, so
	// a shape the controller would silently drop is caught here. Then assert
	// the workspace will actually have a primary repo to clone, and that every
	// entry's URL is a plausible git remote (a non-empty but malformed URL —
	// e.g. carried in via --from — fails at spawn with no up-front signal).
	if len(sc.Repos) > 0 {
		raw, err := json.Marshal(sc.Repos)
		if err != nil {
			problems = append(problems, fmt.Sprintf("repos[] cannot be serialised: %v", err))
		} else if err := shapeCheckRepos(string(raw)); err != nil {
			problems = append(problems, fmt.Sprintf("repos[] has a shape the controller would silently drop: %v", err))
		}
		hasPrimary := false
		for i, r := range sc.Repos {
			if strings.TrimSpace(r.URL) == "" {
				problems = append(problems, fmt.Sprintf("repos[%d] has an empty url", i))
			} else if !looksLikeGitURL(r.URL) {
				problems = append(problems, fmt.Sprintf("repos[%d] url %q does not look like a git remote URL", i, r.URL))
			}
			if r.Role == "" || r.Role == "primary" {
				hasPrimary = true
			}
		}
		if !hasPrimary {
			problems = append(problems, `repos[] has no primary entry (role:"primary" or unset) — the workspace would have no checkout to push from`)
		}
	}

	// Dangling account reference: a non-empty account that doesn't resolve to a
	// live type:account bead routes pods to a credential that doesn't exist —
	// they "look alive, do nothing". Only checked when the account list loaded.
	if knownAccounts != nil && sc.Account != "" {
		if _, ok := knownAccounts[sc.Account]; !ok {
			problems = append(problems, fmt.Sprintf("--account %q does not resolve to a live type:account bead (create the account bead first, or fix the name)", sc.Account))
		}
	}

	sort.Strings(problems)
	return problems
}

// shapeCheckRepos round-trips a raw repos[] JSON value through the exact Go
// type the controller parser unmarshals into ([]RepoEntry), mirroring its lax
// json.Unmarshal. A decode error here is precisely the `json.Unmarshal(...) ==
// nil` guard the controller uses to silently skip the field, so flagging it
// pre-POST surfaces the otherwise-invisible drop. Trailing data after the first
// JSON value is rejected because json.Unmarshal rejects it too.
func shapeCheckRepos(raw string) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	var repos []beadsapi.RepoEntry
	if err := dec.Decode(&repos); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("unexpected trailing data after JSON array")
	}
	return nil
}

// looksLikeGitURL is a lightweight sanity check, NOT a full URL validator. It
// rejects the obvious mistakes (a bare repo name, a local path) that would make
// init-clone fail, while accepting both https and scp-style git remotes on any
// host (the host is intentionally NOT pinned to gitlab.com).
func looksLikeGitURL(u string) bool {
	u = strings.TrimSpace(u)
	switch {
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "ssh://"):
		// Need a host and a path component, e.g. https://host/group/repo.
		rest := u[strings.Index(u, "://")+3:]
		return strings.Contains(rest, "/") && !strings.HasPrefix(rest, "/")
	case strings.Contains(u, "@") && strings.Contains(u, ":"):
		// scp-style: git@host:group/repo.git
		return true
	default:
		return false
	}
}

// projectScopeLabels returns the deduplicated label set for a new project bead.
// A project bead is scoped under the CURRENT project (gardenio convention), so
// it carries a single project:<current> label. Returns nil when no project is
// resolvable, leaving the bead unlabelled rather than emitting "project:".
func projectScopeLabels() []string {
	cur := defaultGBProject()
	if cur == "" {
		return nil
	}
	return []string{"project:" + cur}
}

// printProjectScaffold renders the resolved scaffold for --dry-run.
func printProjectScaffold(w io.Writer, sc projectScaffold) {
	fmt.Fprintf(w, "  name:           %s\n", sc.Name)
	fmt.Fprintf(w, "  prefix:         %s\n", sc.Prefix)
	fmt.Fprintf(w, "  git_url:        %s\n", sc.GitURL)
	fmt.Fprintf(w, "  default_branch: %s\n", sc.DefaultBranch)
	fmt.Fprintf(w, "  account:        %s\n", orDash(sc.Account))
	if len(sc.Repos) > 0 {
		reposJSON, _ := json.Marshal(sc.Repos)
		fmt.Fprintf(w, "  repos:          %s\n", string(reposJSON))
	}
}

// printProjectCreateSuccess prints the new bead id plus the concrete steps that
// remain — credential wiring and (optional) role beads. The project bead alone
// does not make an agent PR-capable; these are the silent-failure steps the
// onboarding audit (epic ka-0000003121) flagged, so we spell them out.
func printProjectCreateSuccess(w io.Writer, beadID string, sc projectScaffold) {
	secretPrefix := sc.Name + "-"
	fmt.Fprintf(w, "created project %q\n", sc.Name)
	fmt.Fprintf(w, "  bead:    %s\n", beadID)
	fmt.Fprintf(w, "  git_url: %s\n", sc.GitURL)
	if sc.Account != "" {
		fmt.Fprintf(w, "  account: %s\n", sc.Account)
	}
	fmt.Fprintln(w, "\nNext steps (the project bead alone does NOT make agents PR-capable):")
	fmt.Fprintf(w, "  1. Wire git/GitLab credentials. Pre-create the AWS Secrets Manager value, then\n")
	fmt.Fprintf(w, "     reference it on this bead. The K8s Secret name MUST be prefixed %q or the\n", secretPrefix)
	fmt.Fprintf(w, "     controller silently drops it:\n")
	fmt.Fprintf(w, "       kd update %s -f secrets='[\n", beadID)
	fmt.Fprintf(w, "         {\"env\":\"GIT_USERNAME\",\"secret\":\"%sgit-credentials\",\"key\":\"username\"},\n", secretPrefix)
	fmt.Fprintf(w, "         {\"env\":\"GIT_TOKEN\",\"secret\":\"%sgit-credentials\",\"key\":\"token\"},\n", secretPrefix)
	fmt.Fprintf(w, "         {\"env\":\"GITLAB_TOKEN\",\"secret\":\"%sgitlab-token\",\"key\":\"token\"}]'\n", secretPrefix)
	step := 2
	if sc.Account == "" {
		fmt.Fprintf(w, "  %d. Create the LLM account bead and set --account (agents need a credential to run):\n", step)
		fmt.Fprintf(w, "       kd create '<account-name>' --type=account -f provider=anthropic -f credential=secret:%sclaude-oauth\n", secretPrefix)
		fmt.Fprintf(w, "       kd update %s -f account=<account-name>\n", beadID)
		step++
	}
	fmt.Fprintf(w, "  %d. (Optional) Create a custom role bead:\n", step)
	fmt.Fprintf(w, "       kd create '<role-name>' --type=role -l project:%s -f model=<model>\n", sc.Name)
	// `gb spawn` takes no --project flag — project comes from BOAT_PROJECT or a
	// task's project: label. Teach the env form, or `gb start` which takes the
	// project positionally. (A stale `gb spawn --project` hint here is what
	// re-seeds gardener trigger prompts that `gb lint trigger-prompts` then flags
	// as drift — ka-0000006674.)
	fmt.Fprintf(w, "  Then spawn an agent: BOAT_PROJECT=%s gb spawn --role <role>\n", sc.Name)
	fmt.Fprintf(w, "     (or with an explicit name: gb start <name> %s --role <role>)\n", sc.Name)
	fmt.Fprintf(w, "\nReference runbook: ka-0000005921 (onboarding a new project + role).\n")
}

// orDash returns "-" for an empty string, for table-ish output.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	projectCreateCmd.Flags().StringVar(&projectGitURL, "git-url", "", "git remote URL for the project's primary repo (required)")
	projectCreateCmd.Flags().StringVar(&projectAccount, "account", "", "type:account bead name for LLM credentials")
	projectCreateCmd.Flags().StringVar(&projectFrom, "from", "", "clone the field set from an existing project bead (by name)")
	projectCreateCmd.Flags().StringVar(&projectPrefix, "prefix", "kd", "bead-ID prefix for this project's issues")
	projectCreateCmd.Flags().StringVar(&projectDefaultBranch, "default-branch", "main", "default branch agents clone and branch from")
	projectCreateCmd.Flags().StringVar(&projectRepoRole, "repo-role", "primary", "role for the derived repos[] entry (primary|reference)")
	projectCreateCmd.Flags().BoolVar(&projectDryRun, "dry-run", false, "validate and print the scaffold without creating the bead")

	projectCmd.AddCommand(projectCreateCmd)
	rootCmd.AddCommand(projectCmd)
}
