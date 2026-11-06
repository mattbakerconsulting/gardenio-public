// Package bridge — repo-less git-path guard.
//
// A project-less (repo-less) trigger spawns a pod with NO repo checked out:
// buildInitCloneContainer (internal/podmanager/initclone.go) returns nil — and
// so clones nothing — exactly when the pod spec carries no GitURL and no
// reference repos, which is the case whenever the trigger's `project` field is
// empty (there is no project bead to read git_url from). The empty `project`
// field is the ONLY repo-less lever: there is no role-level skip-clone flag
// (RoleCacheEntry in internal/config/config.go carries no repo opt-out).
//
// When such a repo-less trigger's spawn prompt names a git path
// (e.g. `gardenio/docs/gardener-first-mate-prompt.md`), that path is a phantom
// at runtime: the agent tries to Read it, gets "file does not exist", and burns
// reads recovering. That was the live first-mate 404 (bug ka-0000004796): the
// gardener pod ran repo-less while its own advice pointed the prompt at
// `gardenio/docs/*.md`. The durable fix is to inline the playbook into an advice
// body / bundle the agent reaches via `gb advice show` — never a git path.
//
// This file provides the detector shared by the trigger engine (warn-once at
// evaluation time) and the `gb trigger create` / `gb gardener start` CLIs (loud
// warning at creation time). It is WARN-only, never fail-closed: the CORRECTED
// first-mate prompt legitimately contains `gardenio/docs/*.md` inside a "you run
// repo-less — do NOT open these, they 404" disclaimer, so a fail-closed
// path-substring reject would refuse the very fix. See task ka-0000004167 and
// advice ka-0000002990 ("gardener/crew prompts live as advice beads, not git
// files"). Mirrors the gardener-gate footgun guard (gardener_gate_guard.go).
package bridge

import (
	"regexp"
	"sort"
	"strings"
)

// distinctiveTopLevelDirs are the monorepo top-level directories whose NAMES are
// not ordinary English words, so a `<dir>/<segment>` token starting with one is
// almost certainly a repo path even without a file extension (e.g. `gardenio/docs`,
// `coop/crates`, `kbeads/cmd`). The common-word top-level dirs `docs`, `scripts`,
// and `policy` are deliberately EXCLUDED here — `docs/pages`, `scripts/or`,
// `policy/procedure` collide with ordinary prose — they are still detected by the
// extension-anchored alternation below when they name an actual file
// (`docs/x.md`, `scripts/release.sh`). A drift here only weakens detection (misses
// a path), it never causes a false reject, since the guard merely warns.
var distinctiveTopLevelDirs = []string{
	"gardenio",
	"coop",
	"kbeads",
	"deploy-values",
	"observability",
}

// repoSourceExtensions are file extensions that, at the end of a slash-bearing
// path token, strongly signal "a specific file inside the repo tree" even when
// the leading directory is not a recognised top-level dir (e.g. a nested
// `internal/bridge/triggers.go`). Matching either signal keeps the detector
// robust without hand-listing every nested directory.
const repoSourceExtensions = `md|go|rs|ya?ml|json|toml|sh|txt|py|proto|tf|ipynb`

// gitPathRE matches a repo-relative path token in prompt text. Two alternations:
//
//  1. A path that STARTS with a DISTINCTIVE repo top-level dir and has at least
//     one more path segment: `gardenio/docs`, `gardenio/docs/foo.md`, `coop/crates`.
//  2. A nested path whose FIRST segment has NO dot (so domain/module paths like
//     `github.com/x/y/z.go` are excluded) and which ENDS in a source-file
//     extension: `internal/bridge/triggers.go`, `docs/x.md`, `a/b/c.yaml`.
//
// Segments are restricted to path-safe characters ([\w.*-]) so ordinary prose
// with a slash ("and/or", "read/write") does not match. The leading boundary
// class includes whitespace/quotes plus `= : , ;` so a path after `--file=`,
// `note:`, or a comma-joined list is caught — but deliberately NOT `/`, which
// would let a path embedded in a `https://…` URL match. URLs are additionally
// excluded by the caller (a token containing `://` is dropped).
var gitPathRE = regexp.MustCompile(
	`(?:^|[\s"'` + "`" + `(<=:,;])` +
		`(` +
		`(?:` + strings.Join(distinctiveTopLevelDirs, "|") + `)/[\w.*-]+(?:/[\w.*-]+)*` +
		`|` +
		`[\w-]+(?:/[\w.*-]+)+\.(?:` + repoSourceExtensions + `)` +
		`)`,
)

// repolessAckMarkers are case-insensitive substrings that show the prompt
// author already KNOWS the pod is repo-less and is deliberately naming a git
// path only to tell the agent NOT to open it (the corrected first-mate shape:
// "you run repo-less — do NOT try to open gardenio/docs/*.md, those 404 at
// runtime"). Their presence suppresses the warning so the fix is never flagged
// as the bug. Kept specific to the repo-less idiom — the bare "do not open" /
// "don't open" phrasings were dropped because they can appear in an unrelated
// instruction and wrongly suppress a genuinely-buggy prompt.
var repolessAckMarkers = []string{
	"repo-less",
	"repoless",
	"no repo",
	"without a repo",
	"without the repo",
	"404 at runtime",
	"those 404",
	"they 404",
	"do not try to open",
	"don't try to open",
}

// RepoPathsInPrompt returns the distinct repo-relative path tokens named in
// prompt, in first-seen order. URL tokens (containing `://`) are excluded. An
// empty result means the prompt names no git path. The result is the evidence a
// caller shows in its warning; it does NOT by itself mean the prompt is a
// problem — only a repo-LESS trigger's paths are unreachable (see
// UnreachableRepoPaths).
func RepoPathsInPrompt(prompt string) []string {
	if prompt == "" {
		return nil
	}
	matches := gitPathRE.FindAllStringSubmatch(prompt, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		// `.` is a valid path-segment char (main.go, Chart.yaml), so the regex
		// greedily swallows a trailing sentence period / comma. No real repo
		// path ends in punctuation, so trim a trailing run of it.
		p := strings.TrimRight(m[1], ".,;:!?)]}")
		if p == "" || strings.Contains(p, "://") {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// promptAcknowledgesRepoless reports whether the prompt already carries a
// repo-less-aware disclaimer, i.e. the author named the path only to warn the
// agent off it. Matching is case-insensitive.
func promptAcknowledgesRepoless(prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, m := range repolessAckMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// actionSpawnsAgent reports whether a trigger action spawns an agent pod whose
// prompt is expanded from the trigger's own `prompt` field. Both spawn_agent and
// apply_formula do: apply_formula pours the molecule then spawns a DRIVER crew
// with ExpandTemplate(trig.Prompt, …) via the same daemon.SpawnAgent call
// (triggers.go actionApplyFormula), so a repo-less apply_formula prompt naming a
// git path 404s exactly as a spawn_agent one does. Mirrors the `spawnsAgent`
// predicate already used at triggers.go:1651/1821 and trigger_permissions.go:95.
func actionSpawnsAgent(action string) bool {
	return action == string(TriggerActionSpawnAgent) || action == string(TriggerActionApplyFormula)
}

// UnreachableRepoPaths reports whether a repo-LESS agent-spawning trigger names
// git paths its pod cannot resolve at runtime. It returns the offending paths and
// unreachable=true only when ALL hold:
//
//   - the trigger spawns an agent (spawn_agent OR apply_formula), and
//   - the trigger is repo-less (projectField is empty → no clone), and
//   - the prompt names at least one repo-relative path, and
//   - the prompt carries NO repo-less-aware disclaimer.
//
// paths is always the set found by RepoPathsInPrompt (so a caller can name them
// even when unreachable is false, e.g. for debugging), but only the fully-gated
// case sets unreachable=true.
func UnreachableRepoPaths(prompt, projectField, action string) (paths []string, unreachable bool) {
	paths = RepoPathsInPrompt(prompt)
	if len(paths) == 0 {
		return nil, false
	}
	if !actionSpawnsAgent(action) {
		return paths, false
	}
	if strings.TrimSpace(projectField) != "" {
		// Has a project → pod clones its repo → paths resolve.
		return paths, false
	}
	if promptAcknowledgesRepoless(prompt) {
		return paths, false
	}
	return paths, true
}

// FieldsUnreachableRepoPaths applies UnreachableRepoPaths to a CLI-side fields
// map (the `map[string]any` assembled by `gb trigger create` / `gb gardener
// start`). Non-string entries are treated as empty.
func FieldsUnreachableRepoPaths(fields map[string]any) (paths []string, unreachable bool) {
	prompt, _ := fields["prompt"].(string)
	project, _ := fields["project"].(string)
	action, _ := fields["action"].(string)
	return UnreachableRepoPaths(prompt, project, action)
}

// RepoPathWarning renders the loud, actionable warning shown when a repo-less
// trigger's prompt names git paths the pod cannot resolve. paths should be the
// set returned by an *UnreachableRepoPaths helper. Paths are sorted for a
// stable message.
func RepoPathWarning(triggerID string, paths []string) string {
	id := triggerID
	if id == "" {
		id = "<new>"
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	return strings.Join([]string{
		"⚠️  trigger " + id + " is project-less (repo-less: no `project` → the pod clones no repo) " +
			"yet its spawn prompt names git path(s): " + strings.Join(sorted, ", ") + ".",
		"    A repo-less pod has NO working tree — those paths 404 at runtime (bug ka-0000004796).",
		"    Fix one of:",
		"      • Inline the referenced content into an advice body / bundle the agent reads via",
		"        `gb advice show <id>` / `gb bundle show <id>` — never a git path (advice ka-0000002990), or",
		"      • give the trigger a `project` so the pod clones the repo, or",
		"      • if you name the path only to tell the agent NOT to open it, say so explicitly",
		"        (\"you run repo-less — do NOT open <path>, it 404s\") to acknowledge the repo-less runtime.",
	}, "\n")
}
