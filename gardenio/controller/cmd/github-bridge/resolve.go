package main

// The mechanical conflict resolver (Task 3.2, gd-mBJXcHcBxb;
// docs/pr-conflicts.md): when the conflict scanner finds an open PR whose only
// conflicts with main are on MechanicalConflictPaths, it pushes one
// merge-of-main commit to the PR branch. It is deterministic: no LLM, and no
// worktree. The merge is `git merge-tree`'s, each conflicted file is rewritten
// by its rule into a temporary index of the blobless mirror (only conflicted
// blobs are fetched), and the commit is verified before it is pushed with a
// lease on the scanned head, so a crew push that raced it is never clobbered.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/githubapp"
)

// Results of a resolve attempt, the result label of
// github_bridge_mechanical_resolves_total.
const (
	resolveOK           = "ok"
	resolveAborted      = "aborted"
	resolveLeaseLost    = "lease_lost"
	resolveVerifyFailed = "verify_failed"
)

// Why an attempt aborted, the reason label of an aborted result (gd-qDrQYJB726:
// the log line was the only record, and it goes with the pod).
const (
	reasonMergeFailed  = "merge_failed"
	reasonAlreadyClean = "already_clean"
	reasonHandConflict = "hand_conflict"
	reasonBuildError   = "build_error"
	reasonPushRefused  = "push_refused"
)

const (
	// noBotMergeLabel opts a PR out of the resolver.
	noBotMergeLabel = "no-bot-merge"
	// defaultResolveAuthor is the gardenio-io App's bot user.
	defaultResolveAuthor = "gardenio-io[bot] <332327360+gardenio-io[bot]@users.noreply.github.com>"
	// maxSubjectFiles bounds the file list in the commit subject.
	maxSubjectFiles = 3
)

// resolveRule is how one class of mechanical file is resolved.
type resolveRule string

const (
	// ruleUnion: main's lines, then the PR's lines not already present, in
	// order. For one-file-per-PR entry dirs, where a conflict means both sides
	// edited the same entry file.
	ruleUnion resolveRule = "union of lines"
	// ruleTakeMain: main's version wins. Goldens never enter a feature PR
	// (gd-oxDhsqdFVP); bless-goldens.yml re-shoots them on main.
	ruleTakeMain resolveRule = "main's version"
)

// resolveRules gives each MechanicalConflictPaths entry its rule. The list
// stays the one source of what is mechanical; a pattern without a rule here
// aborts (and TestResolveEveryMechanicalPatternHasARule fails).
var resolveRules = map[string]resolveRule{
	"gardenio/controller/internal/gardenhub/testdata/golden/**":      ruleTakeMain,
	"gardenio/controller/internal/gardenhub/testdata/jsbudget/*.txt": ruleUnion,
	"scripts/ratchets/line-budget.d/*.tsv":                           ruleUnion,
}

// ruleFor is the rule for path p, or "" when p is not mechanical.
func ruleFor(p string) resolveRule {
	return resolveRules[mechanicalPattern(p)]
}

// unionLines is ruleUnion: main's lines, then those of pr not already present,
// newline-terminated.
func unionLines(main, pr []byte) []byte {
	split := func(b []byte) []string {
		s := strings.TrimSuffix(string(b), "\n")
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	out := split(main)
	seen := map[string]bool{}
	for _, l := range out {
		seen[l] = true
	}
	for _, l := range split(pr) {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// stageEntry is one index stage of a conflicted path in merge-tree output.
type stageEntry struct {
	mode string
	oid  string
}

// conflictEntry is a conflicted path with its stages: 1 the merge base, 2 main
// (merge-tree's first side), 3 the PR head. A nil stage is a side that does
// not have the path.
type conflictEntry struct {
	path   string
	stages [4]*stageEntry
}

// mergeTreeStages merges head into base like mergeTree but returns the
// written tree and every conflicted path's stages. rc 0 is clean (no entries).
func (m *gitMirror) mergeTreeStages(ctx context.Context, base, head string) (string, []conflictEntry, error) {
	out, rc, err := m.run(ctx, gitCommandTimeout, "merge-tree", "--write-tree", "--no-messages", "-z", base, head)
	if err != nil {
		return "", nil, err
	}
	if rc != 0 && rc != 1 {
		return "", nil, fmt.Errorf("git merge-tree: rc %d", rc)
	}
	// -z: the tree id, then "<mode> <oid> <stage>\t<path>" per stage.
	parts := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	tree := parts[0]
	if tree == "" {
		return "", nil, fmt.Errorf("git merge-tree: rc %d with no tree", rc)
	}
	byPath := map[string]*conflictEntry{}
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		meta, name, ok := strings.Cut(p, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || len(f[2]) != 1 || f[2][0] < '1' || f[2][0] > '3' {
			return "", nil, fmt.Errorf("git merge-tree: unparsed entry %q", p)
		}
		e := byPath[name]
		if e == nil {
			e = &conflictEntry{path: name}
			byPath[name] = e
		}
		e.stages[f[2][0]-'0'] = &stageEntry{mode: f[0], oid: f[1]}
	}
	var entries []conflictEntry
	for _, e := range byPath {
		entries = append(entries, *e)
	}
	slices.SortFunc(entries, func(a, b conflictEntry) int { return strings.Compare(a.path, b.path) })
	return tree, entries, nil
}

// prDetail is what the resolver reads of a PR before touching its branch.
type prDetail struct {
	State    string
	HeadRef  string
	HeadSHA  string
	HeadRepo string // owner/name; "" when the head repo is gone
	Labels   []string
}

type prDetailGetter interface {
	PRDetail(ctx context.Context, repo string, n int) (prDetail, error)
}

// PRDetail reads one pull request (GET pulls/N).
func (c *githubCommenter) PRDetail(ctx context.Context, repo string, n int) (prDetail, error) {
	var pr struct {
		State string `json:"state"`
		Head  struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := c.do(ctx, repo, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, http.StatusOK, &pr); err != nil {
		return prDetail{}, err
	}
	d := prDetail{State: pr.State, HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA}
	if pr.Head.Repo != nil {
		d.HeadRepo = pr.Head.Repo.FullName
	}
	for _, l := range pr.Labels {
		d.Labels = append(d.Labels, l.Name)
	}
	return d, nil
}

// resolvedFile is one conflicted path and the rule that resolved it.
type resolvedFile struct {
	path string
	rule resolveRule
}

// mechanicalResolver resolves mechanical-only conflicts. Resolve runs on the
// scanner's goroutine, so the mirror has one user at a time.
type mechanicalResolver struct {
	repo     string
	mirror   *gitMirror
	prs      prDetailGetter
	comments prCommenter
	// pushToken is the contents:write credential for the push only; nil uses
	// the mirror's (a local remote in tests).
	pushToken   func(ctx context.Context) (string, error)
	authorName  string
	authorEmail string
	logger      *slog.Logger
	results     *prometheus.CounterVec

	mu sync.Mutex
	// merged is the main sha each PR was last bot-merged with: at most one
	// bot merge per PR per main push.
	merged map[int]string
	// tried is the base+head pair last attempted per PR, so a scan every
	// interval does not redo an attempt that aborted.
	tried map[int]string
}

func newMechanicalResolver(repo string, mirror *gitMirror, prs prDetailGetter, comments prCommenter, author string, reg prometheus.Registerer, logger *slog.Logger) (*mechanicalResolver, error) {
	if author == "" {
		author = defaultResolveAuthor
	}
	// Not net/mail: it refuses the "[bot]" in an App's unquoted name.
	name, email, ok := strings.Cut(author, " <")
	name, email = strings.TrimSpace(name), strings.TrimSuffix(email, ">")
	if !ok || name == "" || !strings.HasSuffix(author, ">") || !strings.Contains(email, "@") || strings.ContainsAny(email, "<> ") {
		return nil, fmt.Errorf("CONFLICT_RESOLVER_AUTHOR %q is not \"Name <email>\"", author)
	}
	results := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "github_bridge_mechanical_resolves_total",
		Help: "Mechanical conflict resolve attempts by result (ok, aborted, lease_lost or verify_failed) and, for aborted, reason.",
	}, []string{"result", "reason"})
	for _, r := range []string{resolveOK, resolveLeaseLost, resolveVerifyFailed} {
		results.WithLabelValues(r, "")
	}
	for _, why := range []string{reasonMergeFailed, reasonAlreadyClean, reasonHandConflict, reasonBuildError, reasonPushRefused} {
		results.WithLabelValues(resolveAborted, why)
	}
	if reg != nil {
		reg.MustRegister(results)
	}
	return &mechanicalResolver{
		repo: repo, mirror: mirror, prs: prs, comments: comments,
		authorName: name, authorEmail: email, logger: logger, results: results,
		merged: map[int]string{}, tried: map[int]string{},
	}, nil
}

// Resolve tries one PR the scan found conflicting at base with only mechanical
// files. It returns the result, or "" when the PR was skipped before any
// attempt (already tried, a fork, opted out, closed, or its head moved).
func (r *mechanicalResolver) Resolve(ctx context.Context, pr openPR, base, head string) string {
	log := r.logger.With("repo", r.repo, "pr", pr.Number, "base", base, "head", head)
	r.mu.Lock()
	skip := r.merged[pr.Number] == base || r.tried[pr.Number] == base+" "+head
	if !skip {
		r.tried[pr.Number] = base + " " + head
	}
	r.mu.Unlock()
	if skip {
		return ""
	}
	d, err := r.prs.PRDetail(ctx, r.repo, pr.Number)
	if err != nil {
		log.Warn("mechanical resolve: PR not read", "error", err)
		r.forget(pr.Number)
		return ""
	}
	switch {
	case d.State != "open":
		return ""
	case !strings.EqualFold(d.HeadRepo, r.repo):
		log.Info("mechanical resolve: skipped, fork PR", "head_repo", d.HeadRepo)
		return ""
	case slices.Contains(d.Labels, noBotMergeLabel):
		log.Info("mechanical resolve: skipped, label " + noBotMergeLabel)
		return ""
	case d.HeadSHA != head:
		// The next scan, on the synchronize, sees the new head.
		log.Info("mechanical resolve: skipped, head moved since the scan", "now", d.HeadSHA)
		r.forget(pr.Number)
		return ""
	case d.HeadRef == "":
		return ""
	}
	result, reason, files, commit := r.attempt(ctx, log, base, head, d.HeadRef)
	r.results.WithLabelValues(result, reason).Inc()
	if result != resolveOK {
		return result
	}
	r.mu.Lock()
	r.merged[pr.Number] = base
	r.mu.Unlock()
	log.Info("mechanical resolve: pushed", "branch", d.HeadRef, "commit", commit, "files", len(files))
	if err := r.comments.Post(ctx, prRef{Repo: r.repo, Number: pr.Number}, resolveComment(base, commit, d.HeadRef, files)); err != nil {
		log.Warn("mechanical resolve: PR comment not posted", "error", err)
	}
	return result
}

// forget drops the attempt memo of PR n, so the next scan tries again.
func (r *mechanicalResolver) forget(n int) {
	r.mu.Lock()
	delete(r.tried, n)
	r.mu.Unlock()
}

// attempt builds, verifies and pushes the merge commit. reason is set only
// when the result is aborted.
func (r *mechanicalResolver) attempt(ctx context.Context, log *slog.Logger, base, head, branch string) (result, reason string, files []resolvedFile, commit string) {
	tree, entries, err := r.mirror.mergeTreeStages(ctx, base, head)
	if err != nil {
		log.Warn("mechanical resolve: aborted, merge failed", "reason", reasonMergeFailed, "error", err)
		return resolveAborted, reasonMergeFailed, nil, ""
	}
	if len(entries) == 0 {
		log.Info("mechanical resolve: aborted, the merge is already clean", "reason", reasonAlreadyClean)
		return resolveAborted, reasonAlreadyClean, nil, ""
	}
	for _, e := range entries {
		rule := ruleFor(e.path)
		if rule == "" {
			log.Info("mechanical resolve: aborted, a hand conflict", "reason", reasonHandConflict, "file", e.path)
			return resolveAborted, reasonHandConflict, nil, ""
		}
		files = append(files, resolvedFile{path: e.path, rule: rule})
	}
	commit, err = r.build(ctx, tree, entries, base, head)
	if err != nil {
		log.Warn("mechanical resolve: aborted, build failed", "reason", reasonBuildError, "error", err)
		return resolveAborted, reasonBuildError, nil, ""
	}
	if err := r.verify(ctx, base, head, commit, entries); err != nil {
		log.Warn("mechanical resolve: verify failed", "commit", commit, "error", err)
		return resolveVerifyFailed, "", nil, ""
	}
	switch res := r.push(ctx, log, branch, head, commit); res {
	case resolveOK:
		return resolveOK, "", files, commit
	case resolveAborted:
		return res, reasonPushRefused, nil, ""
	default:
		return res, "", nil, ""
	}
}

// build writes the resolved tree through a temporary index and commits it
// with the PR head as first parent and main as second, as `git merge` on the
// PR branch would.
func (r *mechanicalResolver) build(ctx context.Context, tree string, entries []conflictEntry, base, head string) (string, error) {
	// Under the mirror, not os.TempDir(): the pod's root filesystem is
	// read-only and the mirror's volume is its one writable path
	// (gd-qDrQYJB726). git ignores a stray directory in a bare repo.
	tmp, err := os.MkdirTemp(r.mirror.dir, "resolve-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	idx := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	raw := func(args ...string) ([]byte, error) {
		out, rc, err := r.mirror.runWith(ctx, gitCommandTimeout, idx, nil, args...)
		if err != nil {
			return nil, err
		}
		if rc != 0 {
			return nil, fmt.Errorf("git %s: rc %d", firstArg(args), rc)
		}
		return out, nil
	}
	git := func(args ...string) (string, error) {
		out, err := raw(args...)
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("read-tree", tree); err != nil {
		return "", err
	}
	for i, e := range entries {
		main, pr := e.stages[2], e.stages[3]
		switch ruleFor(e.path) {
		case ruleTakeMain:
			if main == nil {
				if _, err := git("update-index", "--force-remove", "--", e.path); err != nil {
					return "", err
				}
				continue
			}
			if _, err := git("update-index", "--add", "--cacheinfo", main.mode+","+main.oid+","+e.path); err != nil {
				return "", err
			}
		case ruleUnion:
			if main == nil || pr == nil || !regularMode(main.mode) || !regularMode(pr.mode) {
				return "", fmt.Errorf("%s: a union needs a regular file on both sides", e.path)
			}
			mainBody, err := raw("cat-file", "blob", main.oid)
			if err != nil {
				return "", err
			}
			prBody, err := raw("cat-file", "blob", pr.oid)
			if err != nil {
				return "", err
			}
			f := filepath.Join(tmp, fmt.Sprintf("blob-%d", i))
			if err := os.WriteFile(f, unionLines(mainBody, prBody), 0o600); err != nil {
				return "", err
			}
			oid, err := git("hash-object", "-w", "--no-filters", f)
			if err != nil {
				return "", err
			}
			if _, err := git("update-index", "--add", "--cacheinfo", main.mode+","+oid+","+e.path); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("%s: no rule", e.path)
		}
	}
	// --missing-ok: every entry names an object the remote has (merge-tree's
	// tree, main's stage, or a blob just written). Without it git checks each
	// one, and in the blobless mirror that fetches every blob of the tree
	// (gd-qDrQYJB726).
	newTree, err := git("write-tree", "--missing-ok")
	if err != nil {
		return "", err
	}
	headTree, err := git("rev-parse", head+"^{tree}")
	if err != nil {
		return "", err
	}
	if newTree == headTree {
		return "", fmt.Errorf("the merge changes nothing on the PR branch; no empty commit")
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.path)
	}
	who := []string{
		"GIT_AUTHOR_NAME=" + r.authorName, "GIT_AUTHOR_EMAIL=" + r.authorEmail,
		"GIT_COMMITTER_NAME=" + r.authorName, "GIT_COMMITTER_EMAIL=" + r.authorEmail,
	}
	out, rc, err := r.mirror.runWith(ctx, gitCommandTimeout, who, nil,
		"commit-tree", newTree, "-p", head, "-p", base, "-m", resolveSubject(paths))
	if err != nil {
		return "", err
	}
	if rc != 0 {
		return "", fmt.Errorf("git commit-tree: rc %d", rc)
	}
	return strings.TrimSpace(string(out)), nil
}

func regularMode(m string) bool { return m == "100644" || m == "100755" }

// verify checks the commit before it is pushed: no whitespace error or
// conflict marker in the resolved files, a clean merge into main, and nothing
// in the PR's diff against main that the PR did not already change (a merge
// side effect such as a renamed "path~side" file).
func (r *mechanicalResolver) verify(ctx context.Context, base, head, commit string, entries []conflictEntry) error {
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.path)
	}
	args := append([]string{"diff", "--check", base, commit, "--"}, paths...)
	if _, rc, err := r.mirror.run(ctx, gitCommandTimeout, args...); err != nil || rc != 0 {
		return fmt.Errorf("git diff --check: rc %d: %v", rc, err)
	}
	state, files, err := r.mirror.mergeTree(ctx, base, commit)
	if err != nil {
		return err
	}
	if state != mergeClean {
		return fmt.Errorf("still conflicting with main: %v", files)
	}
	after, err := r.mirror.changedFiles(ctx, base, commit)
	if err != nil {
		return err
	}
	before, err := r.mirror.changedFiles(ctx, base, head)
	if err != nil {
		return err
	}
	for _, f := range after {
		if !slices.Contains(before, f) {
			return fmt.Errorf("the merge adds %s to the PR's diff", f)
		}
	}
	return nil
}

// push sends commit to the PR branch, leased on the scanned head. A refused
// push whose branch no longer points at head lost the lease to a racing push.
func (r *mechanicalResolver) push(ctx context.Context, log *slog.Logger, branch, head, commit string) string {
	ref := "refs/heads/" + branch
	_, rc, err := r.mirror.runWith(ctx, gitFetchTimeout, nil, r.pushToken,
		"push", "--quiet", "--no-verify", "--force-with-lease="+ref+":"+head, "origin", commit+":"+ref)
	if err == nil && rc == 0 {
		return resolveOK
	}
	out, lrc, lerr := r.mirror.run(ctx, gitCommandTimeout, "ls-remote", "origin", ref)
	now, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if lerr == nil && lrc == 0 && now != head {
		log.Info("mechanical resolve: lease lost, the branch moved", "branch", branch, "now", now)
		return resolveLeaseLost
	}
	log.Warn("mechanical resolve: aborted, push refused", "reason", reasonPushRefused, "branch", branch, "rc", rc, "error", err)
	return resolveAborted
}

// resolveSubject is the commit subject, naming at most maxSubjectFiles files.
func resolveSubject(paths []string) string {
	names := paths
	more := ""
	if len(names) > maxSubjectFiles {
		names, more = names[:maxSubjectFiles], fmt.Sprintf(", +%d more", len(paths)-maxSubjectFiles)
	}
	return "merge main: resolve mechanical conflicts (" + strings.Join(names, ", ") + more + ")"
}

// resolveComment is the PR comment: the files, the rule used for each, and
// what the owner does next.
func resolveComment(base, commit, branch string, files []resolvedFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Merged `main` (%s) into this branch as %s, to resolve conflicts that are all mechanical:\n\n", short(base), short(commit))
	golden := false
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s`: %s\n", f.path, f.rule)
		golden = golden || f.rule == ruleTakeMain
	}
	b.WriteString("\nA union keeps main's lines, then this PR's lines that main does not already have, in order.")
	if golden {
		b.WriteString(" Goldens never go in a feature PR (gd-oxDhsqdFVP): main's version was kept, and bless-goldens.yml re-shoots them on main. Drop any golden you changed rather than re-blessing it here.")
	}
	fmt.Fprintf(&b, "\n\nPull before you push again: `git pull --no-rebase origin %s`. Add the label `%s` to keep the bot off this PR.\n", branch, noBotMergeLabel)
	return b.String()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// newMechanicalResolverFromConfig returns the resolver for the scanner's
// mirror, or nil when CONFLICT_RESOLVER=off. Its push token is minted apart
// from the bridge's read token, with ConflictResolverTokenPermissions.
func newMechanicalResolverFromConfig(cfg *config, mirror *gitMirror, repo string, tokens *tokenSource, gh *githubCommenter, reg prometheus.Registerer, logger *slog.Logger) *mechanicalResolver {
	if cfg.conflictResolveOff {
		logger.Info("mechanical conflict resolver disabled (CONFLICT_RESOLVER=off)")
		return nil
	}
	r, err := newMechanicalResolver(repo, mirror, gh, gh, cfg.conflictResolveAuthor, reg, logger)
	if err != nil {
		logger.Warn("mechanical conflict resolver disabled", "error", err)
		return nil
	}
	push := tokens.withPermissions(githubapp.ConflictResolverTokenPermissions)
	r.pushToken = func(ctx context.Context) (string, error) {
		tok, err := push.Token(ctx, repo)
		return tok.Reveal(), err
	}
	logger.Info("mechanical conflict resolver enabled", "repo", repo, "author", r.authorName)
	return r
}
