package main

// The PR conflict scanner (plan gd-nTCENSwMOH, task gd-RjkmIPXOgN): every open
// PR's merge state against main, computed with `git merge-tree` in a blobless
// mirror and written onto the MR bead and the task beads that carry the PR's
// mr_url. The fields and their values are documented in docs/pr-conflicts.md.
// After a full scan, the overlap pass (overlaps.go) merges the clean PRs
// against each other into mr_overlaps, and the mechanical resolver
// (resolve.go) gets each PR that conflicts on mechanical files only.
//
// A write goes out only when the computed fields differ from the bead's, and a
// change of mr_conflict_base alone is not a difference: bead history keeps a
// snapshot per write (gd-R4Rs0dJBC5), and a scan runs after every main push.
//
// Scans run on one goroutine (Run): a full scan on start, every interval, and
// conflictPushDebounce after the last push to main; a single-PR scan on a
// pull_request opened, reopened or synchronize.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
)

const (
	defaultConflictScanDir      = "/var/lib/github-bridge/mirror"
	defaultConflictScanInterval = 5 * time.Minute
	// conflictPushDebounce coalesces a burst of main pushes (a merge train)
	// into one full scan.
	conflictPushDebounce = 60 * time.Second
	// maxConflictFiles caps mr_conflict_files; the last entry then says how
	// many were left out.
	maxConflictFiles = 50
	// maxCauseFiles bounds the pathspec of the cause lookup.
	maxCauseFiles = 200

	gitFetchTimeout   = 10 * time.Minute
	gitCommandTimeout = 2 * time.Minute

	openPRsPerPage = 100
	maxOpenPRPages = 20
)

// MechanicalConflictPaths are the paths whose conflicts a machine can resolve
// without judgement: regenerated goldens and one-file-per-PR entry dirs. A
// conflict is mechanical iff every conflicting file matches one of them. The
// mechanical resolver (Task 3.2, gd-mBJXcHcBxb) reads this same list; do not
// copy it. A pattern ending in "/**" matches everything below the directory;
// any other pattern is a path.Match glob over the whole path.
var MechanicalConflictPaths = []string{
	"gardenio/controller/internal/gardenhub/testdata/golden/**",
	"gardenio/controller/internal/gardenhub/testdata/jsbudget/*.txt",
	"scripts/ratchets/line-budget.d/*.tsv",
}

// isMechanicalPath reports whether p matches MechanicalConflictPaths.
func isMechanicalPath(p string) bool {
	return mechanicalPattern(p) != ""
}

// mechanicalPattern is the MechanicalConflictPaths entry p matches, or "".
func mechanicalPattern(p string) string {
	for _, pat := range MechanicalConflictPaths {
		if dir, ok := strings.CutSuffix(pat, "/**"); ok {
			if strings.HasPrefix(p, dir+"/") {
				return pat
			}
			continue
		}
		if ok, _ := path.Match(pat, p); ok {
			return pat
		}
	}
	return ""
}

// Merge states and conflict kinds, as docs/pr-conflicts.md spells them.
const (
	mergeClean       = "clean"
	mergeConflicting = "conflicting"
	mergeUnknown     = "unknown"

	conflictMechanical = "mechanical"
	conflictHand       = "hand"
)

// The conflict fields (Task 2.1, kbeads mrFields).
const (
	fieldMergeState    = "mr_merge_state"
	fieldConflictFiles = "mr_conflict_files"
	fieldConflictKind  = "mr_conflict_kind"
	fieldConflictSince = "mr_conflict_since"
	fieldConflictBase  = "mr_conflict_base"
	fieldConflictCause = "mr_conflict_cause"
)

// conflictKind is mechanical when every file is on the mechanical list.
func conflictKind(files []string) string {
	if len(files) == 0 {
		return ""
	}
	for _, f := range files {
		if !isMechanicalPath(f) {
			return conflictHand
		}
	}
	return conflictMechanical
}

// conflictFilesJSON is the mr_conflict_files value: sorted, at most
// maxConflictFiles entries, the last one "…+N more" when capped.
func conflictFilesJSON(files []string) string {
	out := slices.Sorted(slices.Values(files))
	if len(out) > maxConflictFiles {
		keep := maxConflictFiles - 1
		out = append(out[:keep:keep], fmt.Sprintf("…+%d more", len(out)-keep))
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

// gitRunner runs git in dir with extra environment. rc is the exit code; err
// is set only when git could not be run or was killed.
type gitRunner interface {
	Run(ctx context.Context, dir string, env []string, args ...string) (stdout []byte, rc int, err error)
}

// execGit runs the git CLI with only the given environment plus PATH and HOME,
// so the bridge's own secrets are not handed to git.
type execGit struct{}

func (execGit) Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LC_ALL=C"}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), 0, nil
	case errors.As(err, &ee) && ee.ExitCode() >= 0 && ctx.Err() == nil:
		return stdout.Bytes(), ee.ExitCode(), nil
	default:
		return nil, -1, fmt.Errorf("git %s: %w: %s", firstArg(args), err, strings.TrimSpace(stderr.String()))
	}
}

// firstArg names a git invocation in errors without echoing its arguments.
func firstArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
			return a
		}
	}
	return ""
}

// The askpass handshake: git runs GIT_ASKPASS with the prompt as its only
// argument. The bridge binary is that program when askpassEnv is set, and
// answers from askpassTokenEnv, so the token travels in the environment of the
// git process tree and never in argv, a URL or a file.
const (
	askpassEnv      = "GITHUB_BRIDGE_ASKPASS"
	askpassTokenEnv = "GITHUB_BRIDGE_GIT_TOKEN"
)

// runAskpass answers a git credential prompt and reports whether this process
// was started as the askpass helper (main then exits).
func runAskpass(args []string, getenv func(string) string, w interface{ Write([]byte) (int, error) }) bool {
	if getenv(askpassEnv) != "1" {
		return false
	}
	prompt := ""
	if len(args) > 1 {
		prompt = args[1]
	}
	if strings.HasPrefix(prompt, "Username") {
		fmt.Fprintln(w, "x-access-token")
	} else {
		fmt.Fprintln(w, getenv(askpassTokenEnv))
	}
	return true
}

// gitMirror is a bare, blobless mirror of one repo's main and PR heads. Blobs
// are fetched lazily by merge-tree, only for paths both sides changed.
type gitMirror struct {
	dir    string
	remote string
	git    gitRunner
	// token returns the installation token; nil means no auth (a local
	// remote in tests).
	token func(ctx context.Context) (string, error)
	// askpass is the GIT_ASKPASS program: this binary.
	askpass string
}

// env is the environment of every git command: lazy blob fetches can happen
// in any of them, so each carries the credential. token overrides m.token
// when set (the resolver's push token).
func (m *gitMirror) env(ctx context.Context, token func(context.Context) (string, error)) ([]string, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1"}
	if token == nil {
		token = m.token
	}
	if token == nil {
		return env, nil
	}
	tok, err := token(ctx)
	if err != nil {
		return nil, fmt.Errorf("installation token: %w", err)
	}
	return append(env, "GIT_ASKPASS="+m.askpass, askpassEnv+"=1", askpassTokenEnv+"="+tok), nil
}

// run runs one git command in the mirror with a timeout.
func (m *gitMirror) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, int, error) {
	return m.runWith(ctx, timeout, nil, nil, args...)
}

// runWith is run with extra environment and, when token is set, that
// credential instead of m.token.
func (m *gitMirror) runWith(ctx context.Context, timeout time.Duration, extra []string, token func(context.Context) (string, error), args ...string) ([]byte, int, error) {
	env, err := m.env(ctx, token)
	if err != nil {
		return nil, -1, err
	}
	env = append(env, extra...)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args = append([]string{"-c", "credential.helper=", "-c", "core.quotePath=false"}, args...)
	return m.git.Run(ctx, m.dir, env, args...)
}

// ensure creates the mirror unless it already exists (an emptyDir outlives a
// container restart). It is `git init --bare` plus a promisor remote rather
// than `git clone --bare`, because a clone would also fetch every branch head.
func (m *gitMirror) ensure(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(m.dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", "."},
		{"config", "remote.origin.url", m.remote},
		{"config", "remote.origin.promisor", "true"},
		{"config", "remote.origin.partialclonefilter", "blob:none"},
	} {
		if _, rc, err := m.run(ctx, gitCommandTimeout, args...); err != nil || rc != 0 {
			return fmt.Errorf("git %s: rc %d: %v", args[0], rc, err)
		}
	}
	return nil
}

// fetch updates main and the PR heads: all of them, or only prs when given.
func (m *gitMirror) fetch(ctx context.Context, prs ...int) error {
	args := []string{"fetch", "--quiet", "--no-tags", "--filter=blob:none", "origin", "+refs/heads/main:refs/heads/main"}
	if len(prs) == 0 {
		args = append(args, "+refs/pull/*/head:refs/pull/*/head")
	}
	for _, n := range prs {
		args = append(args, fmt.Sprintf("+refs/pull/%d/head:refs/pull/%d/head", n, n))
	}
	_, rc, err := m.run(ctx, gitFetchTimeout, args...)
	if err != nil {
		return err
	}
	if rc != 0 {
		return fmt.Errorf("git fetch: rc %d", rc)
	}
	return nil
}

// revParse resolves ref to a commit sha.
func (m *gitMirror) revParse(ctx context.Context, ref string) (string, error) {
	out, rc, err := m.run(ctx, gitCommandTimeout, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	if rc != 0 {
		return "", fmt.Errorf("%s: not found", ref)
	}
	return strings.TrimSpace(string(out)), nil
}

// mergeTree merges head into base without a worktree: clean (rc 0),
// conflicting with the conflicted paths (rc 1), anything else an error.
func (m *gitMirror) mergeTree(ctx context.Context, base, head string) (string, []string, error) {
	out, rc, err := m.run(ctx, gitCommandTimeout, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", base, head)
	if err != nil {
		return "", nil, err
	}
	switch rc {
	case 0:
		return mergeClean, nil, nil
	case 1:
		// -z: the tree id, then one NUL-terminated path per conflicted file.
		// git also exits 1, with no output, on a rev it cannot resolve.
		parts := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
		if parts[0] == "" {
			return "", nil, errors.New("git merge-tree: rc 1 with no tree")
		}
		var files []string
		for _, p := range parts[1:] {
			if p != "" && !slices.Contains(files, p) {
				files = append(files, p)
			}
		}
		slices.Sort(files)
		return mergeConflicting, files, nil
	default:
		return "", nil, fmt.Errorf("git merge-tree: rc %d", rc)
	}
}

var (
	squashPRRe = regexp.MustCompile(`\(#(\d+)\)\s*$`)
	mergePRRe  = regexp.MustCompile(`^Merge pull request #(\d+) `)
)

// prOfSubject reads the PR number from a squash subject ("... (#N)") or a
// merge commit subject ("Merge pull request #N from ...").
func prOfSubject(s string) string {
	if m := squashPRRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := mergePRRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// cause is the mr_conflict_cause value: the oldest commit in from..to that
// touched one of files, with the PR that brought it to main when one can be
// read, as "<sha>[ #<pr>]". "" when none is found. A commit that came in
// through a merge commit takes the PR number of the first first-parent merge
// on main that contains it.
func (m *gitMirror) cause(ctx context.Context, from, to string, files []string) string {
	if len(files) > maxCauseFiles {
		files = files[:maxCauseFiles]
	}
	args := append([]string{"log", "--format=%H%x09%s", from + ".." + to, "--"}, files...)
	out, rc, err := m.run(ctx, gitCommandTimeout, args...)
	if err != nil || rc != 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	sha, subject, _ := strings.Cut(lines[len(lines)-1], "\t")
	if sha == "" {
		return ""
	}
	pr := prOfSubject(subject)
	if pr == "" {
		out, rc, err := m.run(ctx, gitCommandTimeout, "log", "--first-parent", "--ancestry-path", "--merges", "--reverse", "--format=%s", sha+".."+to)
		if err == nil && rc == 0 {
			first, _, _ := strings.Cut(string(out), "\n")
			pr = prOfSubject(first)
		}
	}
	if pr == "" {
		return sha
	}
	return sha + " #" + pr
}

// openPR is one open pull request the scanner checks.
type openPR struct {
	Number int
	URL    string
	Base   string
}

// openPRLister lists a repo's open pull requests.
type openPRLister interface {
	OpenPRs(ctx context.Context, repo string) ([]openPR, error)
}

// OpenPRs lists the open pull requests of repo (GET pulls?state=open, paged).
func (c *githubCommenter) OpenPRs(ctx context.Context, repo string) ([]openPR, error) {
	var out []openPR
	for page := 1; page <= maxOpenPRPages; page++ {
		var prs []struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Base    struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		p := fmt.Sprintf("/repos/%s/pulls?state=open&per_page=%d&page=%d", repo, openPRsPerPage, page)
		if err := c.do(ctx, repo, http.MethodGet, p, nil, http.StatusOK, &prs); err != nil {
			return nil, err
		}
		for _, pr := range prs {
			out = append(out, openPR{Number: pr.Number, URL: pr.HTMLURL, Base: pr.Base.Ref})
		}
		if len(prs) < openPRsPerPage {
			break
		}
	}
	return out, nil
}

// conflictBeads is what the scanner asks of the beads daemon.
type conflictBeads interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

type conflictMetrics struct {
	conflicting  *prometheus.GaugeVec
	scanSeconds  prometheus.Gauge
	errors       prometheus.Counter
	withoutBead  prometheus.Gauge
	overlapPairs prometheus.Gauge
}

func newConflictMetrics(reg prometheus.Registerer) *conflictMetrics {
	m := &conflictMetrics{
		conflicting: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "github_bridge_prs_conflicting",
			Help: "Open PRs conflicting with main at the last full scan, by conflict kind.",
		}, []string{"kind"}),
		scanSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "github_bridge_conflict_scan_seconds",
			Help: "Duration of the last full conflict scan.",
		}),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "github_bridge_conflict_scan_errors_total",
			Help: "Conflict scan failures: fetches, listings, unknown merge states and bead writes.",
		}),
		withoutBead: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "github_bridge_conflict_prs_without_bead",
			Help: "Open PRs at the last full scan with no bead carrying their mr_url.",
		}),
		overlapPairs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "github_bridge_pr_overlap_pairs",
			Help: "Pairs of open PRs, each clean against main, that conflict with each other at the last full scan.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.conflicting, m.scanSeconds, m.errors, m.withoutBead, m.overlapPairs)
	}
	return m
}

// prMemo is what the scanner remembers of a PR between scans, in memory only.
type prMemo struct {
	state     string
	cleanBase string // main sha of the last clean scan
}

// conflictResult is one PR's scan.
type conflictResult struct {
	state string
	files []string
	base  string
	head  string
	cause string // set only on a clean -> conflicting transition
	// overlaps is the mr_overlaps value, written only when overlapsKnown:
	// a full scan that computed it.
	overlaps      string
	overlapsKnown bool
}

// conflictScanner keeps the conflict fields of every open PR's beads current.
type conflictScanner struct {
	repo     string
	mirror   *gitMirror
	daemon   conflictBeads
	prs      openPRLister
	interval time.Duration
	debounce time.Duration
	now      func() time.Time
	logger   *slog.Logger
	metrics  *conflictMetrics
	// resolver, when set, is handed every PR that scans conflicting on
	// mechanical files only.
	resolver *mechanicalResolver

	pushes chan struct{}
	single chan openPR

	mu   sync.Mutex
	memo map[int]prMemo
	// overlapped is the URL of every open PR whose mr_overlaps was non-empty
	// at the last full scan, so the PR's beads are cleared once it closes.
	overlapped map[int]string
}

func newConflictScanner(repo string, mirror *gitMirror, daemon conflictBeads, prs openPRLister, interval time.Duration, metrics *conflictMetrics, logger *slog.Logger) *conflictScanner {
	if interval <= 0 {
		interval = defaultConflictScanInterval
	}
	if metrics == nil {
		metrics = newConflictMetrics(nil)
	}
	return &conflictScanner{
		repo: repo, mirror: mirror, daemon: daemon, prs: prs,
		interval: interval, debounce: conflictPushDebounce, now: time.Now,
		logger: logger, metrics: metrics,
		pushes: make(chan struct{}, 1), single: make(chan openPR, 64),
		memo: map[int]prMemo{}, overlapped: map[int]string{},
	}
}

// Handle queues scans from webhooks and never fails a delivery: a push to main
// (debounced in Run) and a pull_request whose head or openness changed.
func (s *conflictScanner) Handle(_ context.Context, ev Event) error {
	if !strings.EqualFold(ev.Repo, s.repo) {
		return nil
	}
	switch ev.Type {
	case "push":
		var p struct {
			Ref     string `json:"ref"`
			Deleted bool   `json:"deleted"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || p.Ref != "refs/heads/main" || p.Deleted {
			return nil
		}
		select {
		case s.pushes <- struct{}{}:
		default:
		}
	case "pull_request":
		switch ev.Action {
		case "opened", "reopened", "synchronize":
		default:
			return nil
		}
		var pe pullRequestEvent
		if json.Unmarshal(ev.Payload, &pe) != nil || pe.PullRequest.State != "open" {
			return nil
		}
		pr := openPR{Number: pe.PullRequest.Number, URL: pe.PullRequest.HTMLURL, Base: pe.PullRequest.Base.Ref}
		select {
		case s.single <- pr:
		default:
			s.logger.Warn("conflict scan: single-PR queue full, the next full scan covers it", "pr", pr.Number)
		}
	}
	return nil
}

// Run creates the mirror, scans everything once, then scans on the ticker, on
// the debounced main push and per PR event until ctx is done.
func (s *conflictScanner) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	var debounce *time.Timer
	var fire <-chan time.Time
	scanAll := func() {
		if err := s.ScanAll(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("conflict scan failed", "repo", s.repo, "error", err)
		}
	}
	scanAll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scanAll()
		case <-s.pushes:
			if debounce == nil {
				debounce = time.NewTimer(s.debounce)
			} else {
				debounce.Reset(s.debounce)
			}
			fire = debounce.C
		case <-fire:
			fire = nil
			scanAll()
		case pr := <-s.single:
			if err := s.ScanOne(ctx, pr); err != nil && ctx.Err() == nil {
				s.logger.Warn("conflict scan of one PR failed", "repo", s.repo, "pr", pr.Number, "error", err)
			}
		}
	}
}

// ScanAll fetches main and every PR head and scans each open PR into main. A
// failed fetch or listing aborts the scan with nothing written: it says
// nothing about any one PR.
func (s *conflictScanner) ScanAll(ctx context.Context) error {
	start := s.now()
	if err := s.mirror.ensure(ctx); err != nil {
		s.metrics.errors.Inc()
		return fmt.Errorf("mirror: %w", err)
	}
	if err := s.mirror.fetch(ctx); err != nil {
		s.metrics.errors.Inc()
		return fmt.Errorf("fetch: %w", err)
	}
	prs, err := s.prs.OpenPRs(ctx, s.repo)
	if err != nil {
		s.metrics.errors.Inc()
		return fmt.Errorf("list open PRs: %w", err)
	}
	base, err := s.mirror.revParse(ctx, "refs/heads/main")
	if err != nil {
		s.metrics.errors.Inc()
		return err
	}
	kinds := map[string]int{conflictMechanical: 0, conflictHand: 0}
	without := 0
	open := map[int]bool{}
	type scanned struct {
		pr  openPR
		res conflictResult
	}
	var all []scanned
	var clean []prHead
	for _, pr := range prs {
		if pr.Base != "main" {
			continue
		}
		open[pr.Number] = true
		res := s.check(ctx, base, pr.Number)
		switch res.state {
		case mergeConflicting:
			kinds[conflictKind(res.files)]++
		case mergeClean:
			clean = append(clean, prHead{n: pr.Number, head: res.head})
		}
		all = append(all, scanned{pr, res})
	}
	overlaps := s.overlaps(ctx, base, clean)
	overlapped := map[int]string{}
	for _, sc := range all {
		res := sc.res
		switch res.state {
		case mergeConflicting:
			// Not merged against other PRs: it needs a rebase first.
			res.overlapsKnown = true
		case mergeClean:
			res.overlaps, res.overlapsKnown = overlaps[sc.pr.Number]
		}
		s.mu.Lock()
		_, was := s.overlapped[sc.pr.Number]
		s.mu.Unlock()
		if res.overlaps != "" || (was && !res.overlapsKnown) {
			overlapped[sc.pr.Number] = sc.pr.URL
		}
		if !s.apply(ctx, sc.pr, res) {
			without++
		}
	}
	for _, sc := range all {
		s.resolve(ctx, sc.pr, sc.res)
	}
	s.mu.Lock()
	for n := range s.memo {
		if !open[n] {
			delete(s.memo, n)
		}
	}
	gone := map[int]string{}
	for n, url := range s.overlapped {
		if !open[n] {
			gone[n] = url
		}
	}
	s.overlapped = overlapped
	s.mu.Unlock()
	for n, url := range gone {
		s.clearOverlaps(ctx, n, url)
	}
	for k, n := range kinds {
		s.metrics.conflicting.WithLabelValues(k).Set(float64(n))
	}
	s.metrics.withoutBead.Set(float64(without))
	s.metrics.scanSeconds.Set(s.now().Sub(start).Seconds())
	s.logger.Info("conflict scan done", "repo", s.repo, "prs", len(open), "base", base,
		"mechanical", kinds[conflictMechanical], "hand", kinds[conflictHand], "without_bead", without)
	return nil
}

// ScanOne fetches main and one PR head and scans that PR. A head that cannot
// be fetched is unknown.
func (s *conflictScanner) ScanOne(ctx context.Context, pr openPR) error {
	if pr.Base != "main" || pr.Number <= 0 || pr.URL == "" {
		return nil
	}
	if err := s.mirror.ensure(ctx); err != nil {
		s.metrics.errors.Inc()
		return fmt.Errorf("mirror: %w", err)
	}
	var res conflictResult
	if err := s.mirror.fetch(ctx, pr.Number); err != nil {
		s.metrics.errors.Inc()
		s.logger.Warn("conflict scan: PR head not fetched", "repo", s.repo, "pr", pr.Number, "error", err)
		res = conflictResult{state: mergeUnknown}
	} else {
		base, err := s.mirror.revParse(ctx, "refs/heads/main")
		if err != nil {
			s.metrics.errors.Inc()
			return err
		}
		res = s.check(ctx, base, pr.Number)
	}
	s.apply(ctx, pr, res)
	s.resolve(ctx, pr, res)
	return nil
}

// resolve hands a PR conflicting on mechanical files only to the resolver.
func (s *conflictScanner) resolve(ctx context.Context, pr openPR, res conflictResult) {
	if s.resolver == nil || res.state != mergeConflicting || conflictKind(res.files) != conflictMechanical {
		return
	}
	s.resolver.Resolve(ctx, pr, res.base, res.head)
}

// check merges PR n's head into base and, on a clean -> conflicting
// transition, finds the cause since the last clean base.
func (s *conflictScanner) check(ctx context.Context, base string, n int) conflictResult {
	head, err := s.mirror.revParse(ctx, fmt.Sprintf("refs/pull/%d/head", n))
	var state string
	var files []string
	if err == nil {
		state, files, err = s.mirror.mergeTree(ctx, base, head)
	}
	if err != nil {
		s.metrics.errors.Inc()
		s.logger.Warn("conflict scan: merge state unknown", "repo", s.repo, "pr", n, "error", err)
		return conflictResult{state: mergeUnknown, base: base}
	}
	res := conflictResult{state: state, files: files, base: base, head: head}
	s.mu.Lock()
	prev := s.memo[n]
	switch state {
	case mergeClean:
		s.memo[n] = prMemo{state: mergeClean, cleanBase: base}
	case mergeConflicting:
		s.memo[n] = prMemo{state: mergeConflicting, cleanBase: prev.cleanBase}
	}
	s.mu.Unlock()
	if state == mergeConflicting && prev.state == mergeClean && prev.cleanBase != "" {
		res.cause = s.mirror.cause(ctx, prev.cleanBase, base, files)
	}
	return res
}

// apply writes res onto every bead carrying pr's URL, each only where its
// fields differ. It reports whether any bead was found.
func (s *conflictScanner) apply(ctx context.Context, pr openPR, res conflictResult) bool {
	beads, err := s.beadsFor(ctx, pr.URL)
	if err != nil {
		s.metrics.errors.Inc()
		s.logger.Warn("conflict scan: bead lookup failed", "pr", pr.Number, "error", err)
		return true
	}
	for _, b := range beads {
		patch := conflictPatch(b.Fields, res, s.now())
		if len(patch) == 0 {
			continue
		}
		if err := s.daemon.UpdateBeadFields(ctx, b.ID, patch); err != nil {
			s.metrics.errors.Inc()
			s.logger.Warn("conflict scan: bead write failed", "pr", pr.Number, "bead", b.ID, "error", err)
			continue
		}
		s.logger.Info("conflict state written", "pr", pr.Number, "bead", b.ID, "state", res.state)
	}
	return len(beads) > 0
}

// beadsFor returns the MR bead and the work beads whose mr_url is url.
func (s *conflictScanner) beadsFor(ctx context.Context, url string) ([]*beadsapi.BeadDetail, error) {
	types := []string{"merge-request"}
	for t := range workBeadTypes {
		types = append(types, t)
	}
	slices.Sort(types)
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    types,
		Statuses: []string{"open", "in_progress", "blocked", "deferred", "closed"},
		Fields:   map[string]string{"mr_url": url},
		Limit:    20,
	})
	if err != nil || res == nil {
		return nil, err
	}
	var out []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b.Fields["mr_url"] == url {
			out = append(out, b)
		}
	}
	return out, nil
}

// conflictPatch is the field write that brings a bead's current fields to
// res, or nil when nothing worth writing differs. mr_conflict_since and
// mr_conflict_cause are kept from the bead while it stays conflicting; an
// unknown result writes only mr_merge_state; mr_conflict_base rides along with
// another change but is never a change by itself. mr_overlaps is written only
// when res carries it.
func conflictPatch(cur map[string]string, res conflictResult, now time.Time) map[string]string {
	want := map[string]string{fieldMergeState: res.state}
	switch res.state {
	case mergeClean:
		for _, f := range []string{fieldConflictFiles, fieldConflictKind, fieldConflictSince, fieldConflictBase, fieldConflictCause} {
			want[f] = ""
		}
	case mergeConflicting:
		was := cur[fieldMergeState] == mergeConflicting
		want[fieldConflictFiles] = conflictFilesJSON(res.files)
		want[fieldConflictKind] = conflictKind(res.files)
		want[fieldConflictBase] = res.base
		want[fieldConflictSince] = now.UTC().Format(time.RFC3339)
		if was && cur[fieldConflictSince] != "" {
			want[fieldConflictSince] = cur[fieldConflictSince]
		}
		want[fieldConflictCause] = res.cause
		if was && res.cause == "" {
			want[fieldConflictCause] = cur[fieldConflictCause]
		}
	}
	if res.overlapsKnown {
		want[fieldOverlaps] = res.overlaps
	}
	patch := map[string]string{}
	for k, v := range want {
		if k != fieldConflictBase && cur[k] != v {
			patch[k] = v
		}
	}
	if len(patch) == 0 {
		return nil
	}
	if v, ok := want[fieldConflictBase]; ok && cur[fieldConflictBase] != v {
		patch[fieldConflictBase] = v
	}
	return patch
}

// newConflictScannerFromConfig returns the scanner, or nil when it is off: no
// CONFLICT_SCAN_REPO, CONFLICT_SCANNER=off, or no git binary in the image.
func newConflictScannerFromConfig(cfg *config, daemon conflictBeads, tokens *tokenSource, reg prometheus.Registerer, logger *slog.Logger) *conflictScanner {
	switch {
	case cfg.conflictScanOff:
		logger.Info("conflict scanner disabled (CONFLICT_SCANNER=off)")
		return nil
	case cfg.conflictScanRepo == "":
		logger.Info("conflict scanner disabled: CONFLICT_SCAN_REPO not set")
		return nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		logger.Warn("conflict scanner disabled: no git binary on PATH")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		logger.Warn("conflict scanner disabled: cannot resolve own path for GIT_ASKPASS", "error", err)
		return nil
	}
	repo := cfg.conflictScanRepo
	mirror := &gitMirror{
		dir:     cfg.conflictScanDir,
		remote:  "https://github.com/" + repo + ".git",
		git:     execGit{},
		askpass: exe,
		token: func(ctx context.Context) (string, error) {
			tok, err := tokens.Token(ctx, repo)
			return tok.Reveal(), err
		},
	}
	gh := newGitHubCommenter(tokens, cfg.apiURL)
	s := newConflictScanner(repo, mirror, daemon, gh, cfg.conflictScanInterval, newConflictMetrics(reg), logger)
	s.resolver = newMechanicalResolverFromConfig(cfg, mirror, repo, tokens, gh, reg, logger)
	logger.Info("conflict scanner enabled", "repo", repo, "dir", mirror.dir, "interval", s.interval)
	return s
}

// parseConflictScanConfig reads CONFLICT_SCANNER (off disables),
// CONFLICT_SCAN_REPO (owner/name), CONFLICT_SCAN_DIR and CONFLICT_SCAN_INTERVAL,
// and the resolver's CONFLICT_RESOLVER (off disables) and CONFLICT_RESOLVER_AUTHOR.
func parseConflictScanConfig(cfg *config) error {
	cfg.conflictScanOff = strings.EqualFold(strings.TrimSpace(os.Getenv("CONFLICT_SCANNER")), "off")
	cfg.conflictResolveOff = strings.EqualFold(strings.TrimSpace(os.Getenv("CONFLICT_RESOLVER")), "off")
	cfg.conflictResolveAuthor = strings.TrimSpace(os.Getenv("CONFLICT_RESOLVER_AUTHOR"))
	cfg.conflictScanRepo = strings.TrimSpace(os.Getenv("CONFLICT_SCAN_REPO"))
	if r := cfg.conflictScanRepo; r != "" {
		owner, name, ok := strings.Cut(r, "/")
		if !ok || owner == "" || name == "" || strings.ContainsAny(name, "/ ") {
			return fmt.Errorf("CONFLICT_SCAN_REPO %q is not owner/name", r)
		}
	}
	cfg.conflictScanDir = envOr("CONFLICT_SCAN_DIR", defaultConflictScanDir)
	if v := os.Getenv("CONFLICT_SCAN_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("CONFLICT_SCAN_INTERVAL %q is not a positive duration", v)
		}
		cfg.conflictScanInterval = d
	}
	return nil
}
