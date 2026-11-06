package main

// Detectors for the problem classes the session-sweep survey found the first
// seven patterns miss (gardenio/docs/session-sweep-survey.md, task
// gd-JCoNrApHwa). Each is pinned by real-session fixtures under
// testdata/session_sweep/<class>/ (session_sweep_fixtures_test.go).

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	sweepGHTokenScope     = "gh-token-scope"
	sweepOwnership403     = "ownership-403"
	sweepInfraAccessGap   = "infra-access-gap"
	sweepBrowserDeps      = "browser-deps-eacces"
	sweepPkillSelf        = "pkill-self-match"
	sweepTmpdirUnset      = "tmpdir-unset"
	sweepWrapupQuoting    = "wrapup-quoting"
	sweepStopRefusedSched = "stop-refused-schedule"
	sweepRetriggerChurn   = "retrigger-churn"
	sweepBeadReread       = "bead-reread"
	sweepStopNotTaken     = "stop-did-not-take"
	sweepNoopFollow       = "noop-followthrough"
	sweepKilledMid        = "killed-mid-task"
	sweepInterruptCrash   = "interrupt-then-crash"

	sweepDispatchCollision = "dispatch-collision"
)

// sweepHookRule is a hook-event signature: a call matches when match returns
// kinds (nil is no match; a kindless rule returns []string{""}), and a
// session with at least min matching calls of one kind is a finding.
type sweepHookRule struct {
	pattern string
	min     int
	match   func(ev sweepHookEvent, cmd, resp string) (kinds []string)
}

// sweepKindless is a kindless rule's match result.
func sweepKindless(ok bool) []string {
	if !ok {
		return nil
	}
	return []string{""}
}

var (
	sweepPATNotAccessible = "Resource not accessible by personal access token"
	// sweepPkillF matches a pkill whose flags include -f (full command line),
	// the form that also matches the agent's own shell.
	sweepPkillF = regexp.MustCompile(`\bpkill\b(?:\s+-[A-Za-z0-9]+)*\s+-[A-Za-z]*f`)
	// sweepTmpdirPath matches a write under $TMPDIR, which is unset in agent
	// pods, so "$TMPDIR/x" expands to "/x" at the filesystem root.
	sweepTmpdirPath = regexp.MustCompile(`\$\{?TMPDIR\}?/`)
	// sweepRootDenied is a file at the filesystem root refused: "/bin/bash:
	// line 1: /esc.json: Permission denied", "mkdir: cannot create directory
	// '/commons-ux': Permission denied".
	sweepRootDenied = regexp.MustCompile(`(?:^|[\s:])'?/[^/\s:"']+'?: Permission denied`)
	// sweepPlaywrightEACCES is the browser install failing on the shared
	// browsers dir, not an agent grepping the Makefile comment that names it.
	sweepPlaywrightEACCES = regexp.MustCompile(`EACCES: permission denied, mkdir \\*'/ms-playwright`)
	sweepGBStop           = regexp.MustCompile(`\bgb\s+stop\b`)
	sweepShellQuoting     = regexp.MustCompile("syntax error near unexpected token|unexpected EOF while looking for matching")
	// sweepInfraDenied is the API server refusing the infra reader's
	// ServiceAccount; the response may be JSON-escaped when the relay cut it.
	sweepInfraDenied = regexp.MustCompile(`gardenio-infra-reader\\*" cannot (\w+) resource \\*"([\w.-]+)`)
	// sweepGitStep is one git merge, commit or push in a command line, with
	// its arguments up to the next separator (not a `git merge-base` read).
	sweepGitStep    = regexp.MustCompile(`(?m)\bgit(?:\s+-C\s+\S+)?\s+(merge|commit|push)(\s[^;&|\n]*|$)`)
	sweepOriginMain = regexp.MustCompile(`(?:^|\s)origin/main\b`)
	// sweepMergeNoCommit marks a merge that makes no commit: the merge desk's
	// trial merge (--no-commit, then --abort) never reaches a PR branch.
	sweepMergeNoCommit = regexp.MustCompile(`--no-commit\b|--squash\b|--abort\b`)
)

var sweepHookRules = []sweepHookRule{
	// The fleet GitHub token cannot read check runs or re-run jobs. Fixing it
	// needs wider access, which goes to the operator (gd-BvrgCCOXWC).
	{sweepGHTokenScope, 1, func(ev sweepHookEvent, cmd, resp string) []string {
		if sweepReadOnlyCall(ev, cmd) || !strings.Contains(resp, sweepPATNotAccessible) {
			return nil
		}
		return []string{"tool"}
	}},
	// One call can be refused on several resources (get all,ingress,...):
	// each is its own kind, counted once per call even when list and watch
	// are both refused.
	{sweepInfraAccessGap, 1, func(ev sweepHookEvent, cmd, resp string) []string {
		if sweepReadOnlyCall(ev, cmd) {
			return nil
		}
		var kinds []string
		for _, m := range sweepInfraDenied.FindAllStringSubmatch(resp, -1) {
			// A refused secret read is the reader working as intended.
			if m[2] != "secrets" && !slices.Contains(kinds, m[2]) {
				kinds = append(kinds, m[2])
			}
		}
		return kinds
	}},
	{sweepBrowserDeps, 1, func(ev sweepHookEvent, cmd, resp string) []string {
		return sweepKindless(!sweepReadOnlyCall(ev, cmd) && sweepPlaywrightEACCES.MatchString(resp))
	}},
	// pkill -f matched (and killed) the agent's own shell: bash exits 144.
	{sweepPkillSelf, 1, func(ev sweepHookEvent, cmd, _ string) []string {
		return sweepKindless(sweepPkillF.MatchString(cmd) && strings.HasPrefix(ev.Error, "Exit code 144"))
	}},
	{sweepTmpdirUnset, 1, func(_ sweepHookEvent, cmd, resp string) []string {
		return sweepKindless(sweepTmpdirPath.MatchString(cmd) && sweepRootDenied.MatchString(resp))
	}},
	// Inline --wrapup JSON broke the shell's quoting, so the stop never ran.
	{sweepWrapupQuoting, 1, func(ev sweepHookEvent, cmd, _ string) []string {
		return sweepKindless(sweepGBStop.MatchString(cmd) && strings.Contains(cmd, "--wrapup") && sweepShellQuoting.MatchString(ev.Error))
	}},
	// A scheduled run's stop refused because it holds its own schedule bead.
	{sweepStopRefusedSched, 1, func(_ sweepHookEvent, cmd, resp string) []string {
		return sweepKindless(sweepGBStop.MatchString(cmd) && strings.Contains(resp, "you have claimed in-progress work") && strings.Contains(resp, "Schedule:"))
	}},
}

// sweepReadCommands lead a pipeline segment that only reads or searches.
var sweepReadCommands = map[string]bool{
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "cat": true, "head": true, "tail": true,
	"less": true, "wc": true, "sort": true, "uniq": true, "cut": true, "nl": true, "jq": true,
	"ls": true, "cd": true, "echo": true, "printf": true, "true": true,
}

// sweepReadGit are the git subcommands that only read.
var sweepReadGit = map[string]bool{"grep": true, "log": true, "show": true, "diff": true, "blame": true, "ls-files": true}

var sweepEnvAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// sweepReadOnlyCall reports whether a tool call only read or searched: a
// Read, Grep or Glob call, or a Bash command each pipeline segment of which
// leads with a read (sed without -i, find without -exec or -delete, git grep,
// log or show), with no command substitution and no redirect to a file. The
// rules that match response text alone skip such a call, whose output may be
// a docs page or Makefile comment quoting the sample error (gd-puMsI5w1kN).
func sweepReadOnlyCall(ev sweepHookEvent, cmd string) bool {
	switch ev.ToolName {
	case "Read", "Grep", "Glob":
		return true
	case "Bash":
	default:
		return false
	}
	segs, ok := sweepSplitPipeline(cmd)
	if !ok || len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		if !sweepReadSegment(seg) {
			return false
		}
	}
	return true
}

// sweepSplitPipeline splits a command line on unquoted |, ;, & and newlines.
// ok is false for a command it cannot vouch for: one with command
// substitution, an unclosed quote or a redirect other than to /dev/null or
// another descriptor.
func sweepSplitPipeline(cmd string) (segs []string, ok bool) {
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segs = append(segs, s)
		}
		cur.Reset()
	}
	rs := []rune(cmd)
	var quote rune
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			}
		case r == '\\' && i+1 < len(rs):
			cur.WriteRune(r)
			i++
			r = rs[i]
		case r == '`' || (r == '$' && i+1 < len(rs) && rs[i+1] == '('):
			return nil, false
		case quote == '"':
			if r == '"' {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '&' && i > 0 && rs[i-1] == '>':
			// 2>&1
		case r == '|' || r == ';' || r == '&' || r == '\n':
			flush()
			continue
		case r == '>':
			rest := strings.TrimLeft(string(rs[i+1:]), " ")
			if !strings.HasPrefix(rest, "&") && !strings.HasPrefix(rest, "/dev/null") {
				return nil, false
			}
		}
		cur.WriteRune(r)
	}
	if quote != 0 {
		return nil, false
	}
	flush()
	return segs, true
}

// sweepReadSegment reports whether one pipeline segment leads with a read.
func sweepReadSegment(seg string) bool {
	words := strings.Fields(seg)
	for len(words) > 0 && sweepEnvAssign.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return false
	}
	name, args := path.Base(words[0]), words[1:]
	switch name {
	case "git":
		for len(args) >= 2 && args[0] == "-C" {
			args = args[2:]
		}
		return len(args) > 0 && sweepReadGit[args[0]]
	case "sed":
		return !slices.ContainsFunc(args, func(a string) bool {
			return strings.HasPrefix(a, "--in-place") || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'i'))
		})
	case "find":
		return !slices.ContainsFunc(args, func(a string) bool {
			return a == "-exec" || a == "-execdir" || a == "-ok" || a == "-delete"
		})
	}
	return sweepReadCommands[name]
}

// detectHookSignatures runs sweepHookRules over the bus events, one finding
// per session, pattern and kind.
func detectHookSignatures(in sweepInput) []sweepFinding {
	type acc struct {
		rule   sweepHookRule
		ev     sweepHookEvent
		kind   string
		n      int
		sample string
	}
	groups := map[string]*acc{}
	var order []string
	for _, ev := range in.Hooks {
		cmd := sweepCommand(ev)
		resp := sweepResponseText(ev)
		for _, r := range sweepHookRules {
			for _, kind := range r.match(ev, cmd, resp) {
				k := ev.SessionID + "\x00" + r.pattern + "\x00" + kind
				a := groups[k]
				if a == nil {
					sample := cmd
					if sample == "" {
						sample = ev.ToolName + " " + string(ev.ToolInput)
					}
					a = &acc{rule: r, ev: ev, kind: kind, sample: sample}
					groups[k] = a
					order = append(order, k)
				}
				a.n++
			}
		}
	}
	var out []sweepFinding
	for _, k := range order {
		a := groups[k]
		if a.n < a.rule.min {
			continue
		}
		out = append(out, sweepFinding{
			Pattern: a.rule.pattern, Kind: a.kind, Agent: a.ev.Agent, AgentBeadID: a.ev.AgentBeadID,
			Sessions: []string{a.ev.SessionID}, Count: a.n, At: a.ev.TS, Identity: a.ev.SessionID + "\x00" + a.kind,
			Detail: fmt.Sprintf("%d call(s), first: %s", a.n, truncateRunes(strings.Join(strings.Fields(a.sample), " "), 160)),
		})
	}
	return out
}

// detectRetriggerChurn flags a session that pushed a merge of origin/main or
// an empty commit 3 or more times: each push starts a PR run (owner rule
// 2026-10-31, CLAUDE.md "CI on pull requests"). Only a push counts, so a
// local trial merge never does (gd-EhoChYt9st). A desk CONFLICT MERGE counts
// once; docs/merge-desk.md caps it at two per PR.
func detectRetriggerChurn(in sweepInput) []sweepFinding {
	type acc struct {
		ev      sweepHookEvent
		n       int
		sample  string
		pending bool
	}
	sessions := map[string]*acc{}
	var order []string
	for _, ev := range in.Hooks {
		cmd := sweepCommand(ev)
		if cmd == "" {
			continue
		}
		a := sessions[ev.SessionID]
		for _, m := range sweepGitStep.FindAllStringSubmatch(cmd, -1) {
			switch verb, args := m[1], m[2]; {
			case verb == "merge" && sweepOriginMain.MatchString(args) && !sweepMergeNoCommit.MatchString(args),
				verb == "commit" && strings.Contains(args, "--allow-empty"):
				if a == nil {
					a = &acc{}
					sessions[ev.SessionID] = a
					order = append(order, ev.SessionID)
				}
				a.pending = true
			case verb == "push" && a != nil && a.pending:
				a.pending = false
				if a.n == 0 {
					a.ev, a.sample = ev, cmd
				}
				a.n++
			}
		}
	}
	var out []sweepFinding
	for _, sid := range order {
		a := sessions[sid]
		if a.n < 3 {
			continue
		}
		out = append(out, sweepFinding{
			Pattern: sweepRetriggerChurn, Agent: a.ev.Agent, AgentBeadID: a.ev.AgentBeadID,
			Sessions: []string{sid}, Count: a.n, At: a.ev.TS, Identity: sid + "\x00",
			Detail: fmt.Sprintf("%d push(es), first: %s", a.n, truncateRunes(strings.Join(strings.Fields(a.sample), " "), 160)),
		})
	}
	return out
}

// A wrap-up harness_gaps sentence is about gh-token-scope when it names the
// token, a CI read or re-run, and a refusal, in any order: "gh run rerun is
// denied to the agent PAT", "Crew cannot rerun a failed GitHub Actions job
// (PAT lacks actions:write)".
var (
	sweepGapToken   = regexp.MustCompile(`(?i)\b(?:PAT|token)\b|personal access token`)
	sweepGapCI      = regexp.MustCompile(`(?i)\bre-?run|statusCheckRollup|check[- ]runs?\b|\bpr checks\b|actions:write`)
	sweepGapRefused = regexp.MustCompile(`(?i)\b(?:cannot|can't|can not|could not|unable|denied|lacks?|403|forbidden|no way)\b|not accessible`)
)

func sweepTokenScopeSentence(s string) bool {
	return sweepGapToken.MatchString(s) && sweepGapCI.MatchString(s) && sweepGapRefused.MatchString(s)
}

// detectTokenScopeWrapups flags a wrap-up whose harness_gaps says the GitHub
// token could not read checks or re-run a job: the same gh-token-scope class,
// visible long after the bus has dropped the calls.
func detectTokenScopeWrapups(in sweepInput) []sweepFinding {
	var out []sweepFinding
	for _, a := range in.Agents {
		w, ok := parseSweepWrapup(a.Fields["wrapup"])
		if !ok || w.HarnessGaps == "" || !in.wrapupInWindow(w.Timestamp) {
			continue
		}
		for _, sentence := range splitSentences(w.HarnessGaps) {
			if !sweepTokenScopeSentence(sentence) {
				continue
			}
			out = append(out, sweepFinding{
				Pattern: sweepGHTokenScope, Kind: "wrap-up", Agent: a.Fields["agent"], AgentBeadID: a.ID, BeadID: a.Fields["task_id"],
				Count: 1, At: parseSweepTime(w.Timestamp), Identity: a.ID, Detail: truncateRunes(sentence, 220),
			})
			break
		}
	}
	return out
}

// sweepWrapup is the subset of an agent bead's wrapup field the sweep reads.
type sweepWrapup struct {
	TLDR            string `json:"tldr"`
	Accomplishments string `json:"accomplishments"`
	HarnessGaps     string `json:"harness_gaps"`
	Timestamp       string `json:"timestamp"`
}

func parseSweepWrapup(raw string) (sweepWrapup, bool) {
	var w sweepWrapup
	if raw == "" || json.Unmarshal([]byte(raw), &w) != nil {
		return w, false
	}
	return w, true
}

// wrapupInWindow reports whether a wrap-up stored at ts falls inside the
// sweep's window. Agents are fetched by updated_at, so a bulk touch of old
// agent beads brings their old wrap-ups back in (gd-KfFHevUzCi); a detector
// that reports on the wrap-up itself skips one stored before the window. A
// wrap-up with no timestamp is kept.
func (in sweepInput) wrapupInWindow(ts string) bool {
	t := parseSweepTime(ts)
	return t.IsZero() || !t.Before(in.Now.Add(-in.Since))
}

// sweepOwnershipDenied is the daemon refusing a write to a bead the caller
// does not own: "HTTP 403: ownership denied: update on bead gd-x requires ...".
var sweepOwnershipDenied = regexp.MustCompile(`HTTP 403: ownership denied`)

// sweepDeniedBead is the bead an ownership refusal names: "ownership denied:
// update on bead gd-x", or close, comment.
var sweepDeniedBead = regexp.MustCompile(`^HTTP 403: ownership denied: \w+ on bead ([a-z]{2,4}-[A-Za-z0-9]{6,})\b`)

// detectOwnership403s flags a session whose kd/gb writes to the agent's own
// task were refused for ownership: the agent was sent to work a bead its
// token may not update (inherited, pre-assigned, or a plan step). A refusal
// on some other bead is a mistaken edit, not this class.
//
// A thread agent has no task_id: its work is whatever its Slack or Commons
// thread asked for, named only in the prompt. For one, every write refused
// to the agent itself counts (gd-raXMeyjq4L): the refusal must name it ("you
// are \"<agent>\""), so a bead read or source grep that quotes another
// caller's 403 is not one. That stands in for the kd/gb check, which misses a
// refused kd call past the relay's 1024-byte cut of the command.
func detectOwnership403s(in sweepInput) []sweepFinding {
	taskOf := map[string]string{}
	threadOf := map[string]string{}
	for _, a := range in.Agents {
		taskOf[a.ID] = a.Fields["task_id"]
		if a.Fields["task_id"] == "" {
			threadOf[a.ID] = a.Fields["thread_id"]
		}
	}
	type acc struct {
		ev     sweepHookEvent
		n      int
		sample string
	}
	bySess := map[string]*acc{}
	var order []string
	for _, ev := range in.Hooks {
		task, thread := taskOf[ev.AgentBeadID], threadOf[ev.AgentBeadID]
		cmd := sweepCommand(ev)
		if (task == "" && thread == "") || cmd == "" {
			continue
		}
		resp := sweepResponseText(ev)
		loc := sweepOwnershipDenied.FindStringIndex(resp)
		if loc == nil {
			continue
		}
		if task != "" && (!sweepRunsKdGb.MatchString(cmd) || !(strings.Contains(cmd, task) || strings.Contains(resp, task))) {
			continue
		}
		if task != "" {
			if loc = sweepTaskRefusal(resp, task); loc == nil {
				continue
			}
		}
		if task == "" && !sweepRefusedCaller(resp[loc[0]:], ev.Agent) {
			continue
		}
		a := bySess[ev.SessionID]
		if a == nil {
			a = &acc{ev: ev, sample: firstLine(resp[loc[0]:])}
			bySess[ev.SessionID] = a
			order = append(order, ev.SessionID)
		}
		a.n++
	}
	var out []sweepFinding
	for _, sid := range order {
		a := bySess[sid]
		bead, what := taskOf[a.ev.AgentBeadID], "to its own task"
		if bead == "" {
			bead, what = threadOf[a.ev.AgentBeadID], "its thread asked for"
		}
		out = append(out, sweepFinding{
			Pattern: sweepOwnership403, Agent: a.ev.Agent, AgentBeadID: a.ev.AgentBeadID, Sessions: []string{sid},
			BeadID: bead, Count: a.n, At: a.ev.TS, Identity: sid,
			Detail: fmt.Sprintf("%d write(s) %s refused, first: %s", a.n, what, truncateRunes(a.sample, 160)),
		})
	}
	return out
}

// sweepTaskRefusal returns the first ownership refusal in resp that could be
// on task: one naming task as its bead, or one naming no bead. A refusal on
// another bead is out of class, however often the task id shows elsewhere in
// the call (a comment that cites it, gd-KfFHevUzCi).
func sweepTaskRefusal(resp, task string) []int {
	for _, loc := range sweepOwnershipDenied.FindAllStringIndex(resp, -1) {
		if m := sweepDeniedBead.FindStringSubmatch(resp[loc[0]:]); m == nil || m[1] == task {
			return loc
		}
	}
	return nil
}

// sweepRefusedCaller reports whether an ownership refusal names agent as the
// caller ("— you are \"<agent>\";"). A response the relay cut to a JSON
// prefix keeps the quotes escaped, so backslashes before them are skipped.
func sweepRefusedCaller(refusal, agent string) bool {
	if agent == "" {
		return false
	}
	_, rest, ok := strings.Cut(refusal, "you are ")
	if !ok {
		return false
	}
	rest = strings.TrimLeft(rest, `\"`)
	return strings.HasPrefix(rest, agent) && strings.HasPrefix(strings.TrimLeft(rest[len(agent):], `\`), `"`)
}

// sweepKdShow pulls the bead id out of each `kd show <id>` in a command.
var sweepKdShow = regexp.MustCompile(`\bkd\s+show\s+(?:-{1,2}[\w-]+(?:=\S+)?\s+)*([a-z]{2,4}-[A-Za-z0-9]{6,})\b`)

// sweepRereadMin is how many times one session may `kd show` the same bead
// before it is a re-read (survey class bead-history-reread). repeat-call
// misses these: the pipes after the id vary from call to call.
const sweepRereadMin = 4

// detectBeadRereads flags a session that read the same bead sweepRereadMin or
// more times, keyed on the bead id rather than the whole command.
func detectBeadRereads(in sweepInput) []sweepFinding {
	type acc struct {
		ev sweepHookEvent
		n  int
	}
	groups := map[string]*acc{}
	var order []string
	for _, ev := range in.Hooks {
		cmd := sweepCommand(ev)
		ids := map[string]bool{}
		for _, m := range sweepKdShow.FindAllStringSubmatch(cmd, -1) {
			ids[m[1]] = true
		}
		for id := range ids {
			k := ev.SessionID + "\x00" + id
			a := groups[k]
			if a == nil {
				a = &acc{ev: ev}
				groups[k] = a
				order = append(order, k)
			}
			a.n++
		}
	}
	sort.Strings(order)
	var out []sweepFinding
	for _, k := range order {
		a := groups[k]
		if a.n < sweepRereadMin {
			continue
		}
		_, id, _ := strings.Cut(k, "\x00")
		out = append(out, sweepFinding{
			Pattern: sweepBeadReread, Agent: a.ev.Agent, AgentBeadID: a.ev.AgentBeadID, Sessions: []string{a.ev.SessionID},
			BeadID: id, Count: a.n, At: a.ev.TS, Identity: k,
			Detail: fmt.Sprintf("kd show %s %d times in one session", id, a.n),
		})
	}
	return out
}

// sweepHookSession maps a session bead to the bus's session_id: the Claude
// session uuid, which is the basename of the bead's session_log transcript.
func sweepHookSession(b *beadsapi.BeadDetail) string {
	return strings.TrimSuffix(path.Base(b.Fields["session_log"]), ".jsonl")
}

// sweepSessionSpan is a session bead's start and end ("" end: still running).
func sweepSessionSpan(b *beadsapi.BeadDetail) (start, end time.Time) {
	return parseSweepTime(b.Fields["started_at"]), parseSweepTime(b.Fields["ended_at"])
}

// sweepHookTimes indexes the bus events' timestamps by session id, and notes
// which sessions ran gb stop.
func sweepHookTimes(in sweepInput) (times map[string][]time.Time, stopped map[string]bool) {
	times, stopped = map[string][]time.Time{}, map[string]bool{}
	for _, ev := range in.Hooks {
		times[ev.SessionID] = append(times[ev.SessionID], ev.TS)
		if sweepGBStop.MatchString(sweepCommand(ev)) {
			stopped[ev.SessionID] = true
		}
	}
	return times, stopped
}

// sweepStopIdleMin is how long a session may outlive its last tool call. A
// retired session's ended_at trails its last call by ~16 min (the retire
// sweeper's lag), so retired sessions are left out entirely.
const sweepStopIdleMin = 30 * time.Minute

// detectStopsNotTaken flags a session that ended (completed or crashed, not
// retired) sweepStopIdleMin or more after its last tool call: a gb stop that
// failed (a 403 on the agent's own bead), hung, or was never reached, so the
// pod idled until something else (often a credential switch) ended it.
//
// An own-bead 403 refuses the hook emitter too, so such a session can have no
// bus trace at all; slowest_calls is then the only record of its gb stop
// (gd-3XE1oIjRyx). A gb stop there counts as the last call only when the
// agent bead stored no wrap-up inside the session: a stop that landed later
// (a gate refused the first, work went on) stores one. stop_requested is no
// help, since a later session's stop sets it. Other slowest_calls entries
// never count: the list keeps only the slowest calls, so its latest entry is
// seldom the session's last call.
func detectStopsNotTaken(in sweepInput) []sweepFinding {
	times, _ := sweepHookTimes(in)
	agents := map[string]*beadsapi.BeadDetail{}
	for _, a := range in.Agents {
		agents[a.ID] = a
	}
	var out []sweepFinding
	for _, b := range in.Sessions {
		if b.Fields["status"] == "retired" {
			continue
		}
		start, end := sweepSessionSpan(b)
		sid := sweepHookSession(b)
		if end.IsZero() || sid == "" {
			continue
		}
		var last time.Time
		for _, t := range times[sid] {
			if !t.Before(start) && !t.After(end) && t.After(last) {
				last = t
			}
		}
		src := "bus"
		if a := agents[b.Fields["agent_bead_id"]]; a != nil && !sweepWrapupInside(a, start, end) {
			for _, c := range beadsapi.ParseSessionSlowCalls(b.Fields["slowest_calls"]) {
				if t := parseSweepTime(c.TS); sweepGBStopCall.MatchString(c.Args) && !t.Before(start) && !t.After(end) && t.After(last) {
					last, src = t, "slowest_calls gb stop, no wrap-up stored"
				}
			}
		}
		if last.IsZero() || end.Sub(last) < sweepStopIdleMin {
			continue
		}
		idle := end.Sub(last)
		out = append(out, sweepFinding{
			Pattern: sweepStopNotTaken, Agent: b.Fields["agent"], AgentBeadID: b.Fields["agent_bead_id"], Sessions: []string{b.ID},
			Count: int(idle / time.Minute), At: last, Identity: "session:" + b.ID,
			Detail: fmt.Sprintf("session %s ended %s after its last tool call (%s from %s, restart_cause=%s)",
				b.Fields["status"], idle.Round(time.Minute), last.UTC().Format("15:04Z"), src, b.Fields["restart_cause"]),
		})
	}
	return out
}

// sweepGBStopCall is a gb stop in command position, not one named inside a
// quoted argument (`gb spawn --prompt "...; then gb stop."`).
var sweepGBStopCall = regexp.MustCompile(`(?:^|[;&|(\n]\s*)gb\s+stop\b`)

// sweepWrapupInside reports whether agent bead a's wrap-up was stored between
// start and end, which a gb stop that landed does.
func sweepWrapupInside(a *beadsapi.BeadDetail, start, end time.Time) bool {
	w, ok := parseSweepWrapup(a.Fields["wrapup"])
	t := parseSweepTime(w.Timestamp)
	return ok && !t.Before(start) && !t.After(end)
}

// sweepNothingToDo matches a wrap-up that reports a wake with no work.
var sweepNothingToDo = regexp.MustCompile(`(?i)\bnothing (?:left |more |else |further )?to do\b`)

// detectNoopFollowthroughs flags an agent woken for an escalation or decision
// whose wrap-up says there was nothing left to do: its task was already done
// (PR merged, task closed) before the wake, so the spawn was wasted.
func detectNoopFollowthroughs(in sweepInput) []sweepFinding {
	var out []sweepFinding
	for _, a := range in.Agents {
		wake := a.Fields["wake_reason"]
		if wake != "escalation" && wake != "decision" {
			continue
		}
		w, ok := parseSweepWrapup(a.Fields["wrapup"])
		if !ok || !sweepNothingToDo.MatchString(w.TLDR) || !in.wrapupInWindow(w.Timestamp) {
			continue
		}
		out = append(out, sweepFinding{
			Pattern: sweepNoopFollow, Kind: "", Agent: a.Fields["agent"], AgentBeadID: a.ID, BeadID: a.Fields["task_id"],
			Count: 1, At: parseSweepTime(w.Timestamp), Identity: a.ID,
			Detail: fmt.Sprintf("%s wake, wrap-up: %s", wake, truncateRunes(w.TLDR, 180)),
		})
	}
	return out
}

// sweepKilledMinCrash is how long a crashed session must have run to count as
// killed mid-task; shorter crashes are startup crashes or restart loops.
const sweepKilledMinCrash = 5 * time.Minute

// detectKilledMidTask flags a session ended by a credential switch (or a
// crash after sweepKilledMinCrash) that was working: it made tool calls, never
// ran gb stop, and stored no wrap-up before it ended. Uncommitted work in
// such a session is lost; the fleet-wide switches at 04:21Z and 16:07Z on
// 2026-10-29 killed go test runs and golangci-lint mid-call.
//
// A pod terminated under a working session counts too. gb labels that session
// sigterm (the teardown was requested, just not by the agent), so it is told
// apart from a gb stop by the agent bead's context_cancelled_involuntary close.
func detectKilledMidTask(in sweepInput) []sweepFinding {
	times, stopped := sweepHookTimes(in)
	terminated := sweepPodTerminated(in)
	wrapAt := map[string]time.Time{}
	for _, a := range in.Agents {
		if w, ok := parseSweepWrapup(a.Fields["wrapup"]); ok {
			wrapAt[a.ID] = parseSweepTime(w.Timestamp)
		}
	}
	var out []sweepFinding
	for _, b := range in.Sessions {
		cause := b.Fields["restart_cause"]
		start, end := sweepSessionSpan(b)
		podKilled := cause == "sigterm" && strings.HasPrefix(terminated[b.ID], "context_cancelled_involuntary")
		if end.IsZero() || (cause != "credential_switch" && !podKilled && (cause != "crash" || end.Sub(start) < sweepKilledMinCrash)) {
			continue
		}
		sid := sweepHookSession(b)
		var calls int
		var last time.Time
		for _, t := range times[sid] {
			if !t.Before(start) && !t.After(end) {
				calls++
				if t.After(last) {
					last = t
				}
			}
		}
		if calls == 0 || stopped[sid] {
			continue
		}
		if w := wrapAt[b.Fields["agent_bead_id"]]; !w.IsZero() && !w.Before(start) && !w.After(end) {
			continue
		}
		out = append(out, sweepFinding{
			Pattern: sweepKilledMid, Kind: cause, Agent: b.Fields["agent"], AgentBeadID: b.Fields["agent_bead_id"], Sessions: []string{b.ID},
			Count: 1, At: end, Identity: "session:" + b.ID,
			Detail: fmt.Sprintf("ended by %s (exit %s) at %s, %d tool calls and no gb stop; last call finished %s",
				cause, b.Fields["exit_code"], end.UTC().Format("01-02 15:04Z"), calls, last.UTC().Format("15:04Z")),
		})
	}
	return out
}

// sweepInterrupted is Claude's tool error when the user rejects or interrupts
// a call; in a headless agent it is someone at the terminal.
var sweepInterrupted = regexp.MustCompile(`doesn't want to proceed with this tool use|\[Request interrupted by user`)

// sweepInterruptWindow is how soon after the interrupt the session must exit
// 143 to count as interrupt-then-crash.
const sweepInterruptWindow = 3 * time.Minute

// sweepInterruptCountedSince is when gb started counting interrupts at close.
// A missing interrupt_count cannot mark an old bead: gb writes it only when it
// is above 0. So only a bead that ended before this gets the bead-only
// fallback. It is set before this gb could ship; an old gb's bead that ends
// between it and the rollout is a miss, never a false positive.
var sweepInterruptCountedSince = time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC)

// detectInterruptCrashes flags a session that was interrupted or had a tool
// call rejected by a user and then exited 143 within sweepInterruptWindow.
// The rejected call's side effects can land anyway (gd-nt8aZJyoKQ: a merge
// shown as rejected was merged). The interrupt comes from either source:
//   - a tool error on the bus. A marker that is only a tool's output (a sed of
//     the parser that defines it) is not an interrupt, so only errors count.
//   - the session bead's last_interrupt_at, counted from the transcript at
//     close (parseSessionUsage). An interrupt between turns fires no hook, so
//     this is the only place it shows (gd-3HAERLWMLu).
//
// A requested teardown is not this class: coop's shutdown drain, and the agent
// pod's preStop hook before it, press Escape, which Claude records as a user
// interrupt (gd-Ga8iHjLaYR). So a session labelled restart_cause sigterm is
// skipped, and so is one an older gb labelled crash whose pod was terminated
// under it (sweepPodTerminated).
//
// A bead that ended before sweepInterruptCountedSince falls back to its shape:
// exit 143, restart_cause crash, under sweepInterruptWindow, and at least one
// tool call. The tool call is what keeps out the 4-8 s startup crashes, which
// make none (negative-s1em-startup-crash).
func detectInterruptCrashes(in sweepInput) []sweepFinding {
	marks := map[string]time.Time{}
	for _, ev := range in.Hooks {
		if ev.Error != "" && sweepInterrupted.MatchString(ev.Error) {
			if t, ok := marks[ev.SessionID]; !ok || ev.TS.Before(t) {
				marks[ev.SessionID] = ev.TS
			}
		}
	}
	terminated := sweepPodTerminated(in)
	var out []sweepFinding
	for _, b := range in.Sessions {
		start, end := sweepSessionSpan(b)
		if b.Fields["exit_code"] != "143" || end.IsZero() || b.Fields["restart_cause"] == "sigterm" || terminated[b.ID] != "" {
			continue
		}
		inWindow := func(t time.Time) bool { return !t.IsZero() && !end.Before(t) && end.Sub(t) <= sweepInterruptWindow }
		mark, how := marks[sweepHookSession(b)], "tool call interrupted"
		if t := parseSweepTime(b.Fields["last_interrupt_at"]); inWindow(t) && (!inWindow(mark) || t.Before(mark)) {
			mark, how = t, "user interrupt"
		}
		if !inWindow(mark) {
			// The pre-interrupt_count fallback, judged on the bead alone.
			calls, _ := strconv.Atoi(b.Fields["tool_call_count"])
			if !end.Before(sweepInterruptCountedSince) || b.Fields["restart_cause"] != "crash" || calls < 1 || !inWindow(start) {
				continue
			}
			mark, how = start, fmt.Sprintf("crashed after %d tool call(s), started", calls)
		}
		out = append(out, sweepFinding{
			Pattern: sweepInterruptCrash, Agent: b.Fields["agent"], AgentBeadID: b.Fields["agent_bead_id"], Sessions: []string{b.ID},
			Count: 1, At: mark, Identity: "session:" + b.ID,
			Detail: fmt.Sprintf("%s at %s, session exited 143 at %s (restart_cause=%s)",
				how, mark.UTC().Format("15:04:05Z"), end.UTC().Format("15:04:05Z"), b.Fields["restart_cause"]),
		})
	}
	return out
}

// sweepPodTerminateLag is how soon after a session's end its agent bead must
// close for the close to be that session's teardown. gb init closes the bead
// a few seconds after the session bead.
const sweepPodTerminateLag = time.Minute

// sweepPodTerminated maps a session bead id to its agent bead's close_reason
// when the pod was terminated under that session: the agent bead closed with a
// context_cancelled reason, which gb init writes when its own SIGTERM ends the
// restart loop, within sweepPodTerminateLag of the session's end.
func sweepPodTerminated(in sweepInput) map[string]string {
	closed := map[string]*beadsapi.BeadDetail{}
	for _, a := range in.Agents {
		if strings.HasPrefix(a.Fields["close_reason"], "context_cancelled") {
			closed[a.ID] = a
		}
	}
	out := map[string]string{}
	for _, b := range in.Sessions {
		a := closed[b.Fields["agent_bead_id"]]
		if a == nil {
			continue
		}
		_, end := sweepSessionSpan(b)
		if t := parseSweepTime(a.ClosedAt); !end.IsZero() && !t.IsZero() && !t.Before(end) && t.Sub(end) <= sweepPodTerminateLag {
			out[b.ID] = a.Fields["close_reason"]
		}
	}
	return out
}

// sweepAlreadyDonePR is a wrap-up naming a PR that had already done the work
// the agent was sent to do: "already landed on PR #1246", "already fixed on
// main by #1345", "#1204 was already merged". The PR must sit next to the
// phrase, so "the failure already on main, which #1369 fixes" is not one.
var sweepAlreadyDonePR = regexp.MustCompile(`(?i)\balready[- ](?:landed|merged|fixed|shipped|on main)\b[^.;,:()]{0,30}?(?:PR\s*)?#(\d+)\b|(?:PR\s*)?#(\d+)\s+(?:(?:was|is|had|has been)\s+)?already[- ](?:landed|merged|fixed|shipped|on main)\b`)

// sweepPullNumber is the PR number in a wrap-up pull_requests URL.
var sweepPullNumber = regexp.MustCompile(`/pull/(\d+)\b`)

// sweepWrapupPRs is the subset of a wrap-up's pull_requests the sweep reads.
type sweepWrapupPRs struct {
	PullRequests []string `json:"pull_requests"`
}

// sweepTriggeredRun is an agent a trigger or schedule fired rather than one
// sent to a piece of work: the merge desk (spawned_by trigger:<id>), a
// scheduled run (spawn_source schedule) or a fire-now gardener. Such a run
// sweeps for its own work, so finding a PR already merged is its job, not a
// collision (gd-40uyCGxXW5).
func sweepTriggeredRun(a *beadsapi.BeadDetail) bool {
	switch a.Fields["spawn_source"] {
	case "schedule", "fire-now":
		return true
	}
	return a.Fields["trigger_id"] != "" || a.Fields["schedule_id"] != "" || strings.HasPrefix(a.Fields["spawned_by"], "trigger:")
}

// detectDispatchCollisions flags an agent whose wrap-up tldr says a PR had
// already done its work, when a different agent's earlier wrap-up lists that
// PR: two agents were sent to one piece of work (a duplicate thread tend, a
// crew sent to beads a merged PR fixed). The collision is on the PR, not a shared task_id (thread agents
// carry none). Escalation and decision wakes are noop-followthrough's, a later
// wrap-up by the same agent is a respawn, not a second dispatch, and a
// triggered run is never dispatched (sweepTriggeredRun); all three still own
// the PRs their wrap-ups list.
func detectDispatchCollisions(in sweepInput) []sweepFinding {
	type owner struct {
		agent, bead string
		at          time.Time
	}
	owners := map[string][]owner{}
	for _, a := range in.Agents {
		raw := a.Fields["wrapup"]
		w, ok := parseSweepWrapup(raw)
		var prs sweepWrapupPRs
		if !ok || json.Unmarshal([]byte(raw), &prs) != nil {
			continue
		}
		for _, u := range prs.PullRequests {
			if m := sweepPullNumber.FindStringSubmatch(u); m != nil {
				owners[m[1]] = append(owners[m[1]], owner{a.Fields["agent"], a.ID, parseSweepTime(w.Timestamp)})
			}
		}
	}
	var out []sweepFinding
	for _, a := range in.Agents {
		if wake := a.Fields["wake_reason"]; wake == "escalation" || wake == "decision" || sweepTriggeredRun(a) {
			continue
		}
		w, ok := parseSweepWrapup(a.Fields["wrapup"])
		if !ok || !in.wrapupInWindow(w.Timestamp) {
			continue
		}
		at := parseSweepTime(w.Timestamp)
		seen := map[string]bool{}
		for _, m := range sweepAlreadyDonePR.FindAllStringSubmatch(w.TLDR, -1) {
			pr := m[1] + m[2]
			if seen[pr] {
				continue
			}
			var first *owner
			for i, o := range owners[pr] {
				if o.agent != a.Fields["agent"] && o.at.Before(at) && (first == nil || o.at.Before(first.at)) {
					first = &owners[pr][i]
				}
			}
			if first == nil {
				continue
			}
			seen[pr] = true
			out = append(out, sweepFinding{
				Pattern: sweepDispatchCollision, Kind: "pr", Agent: a.Fields["agent"], AgentBeadID: a.ID, BeadID: a.Fields["task_id"],
				Count: 1, At: at, Identity: a.ID + "\x00" + pr,
				Detail: fmt.Sprintf("PR #%s was %s's (%s) before this agent's wrap-up: %s", pr, first.agent, first.bead, truncateRunes(m[0], 120)),
			})
		}
	}
	return out
}
