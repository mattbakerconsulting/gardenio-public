package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// stopGateProcRoot is the /proc mount stopGateLiveChildProcess scans.
// Variable (not const) so tests can point it at a synthetic tree instead of
// the real, host-specific /proc.
var stopGateProcRoot = "/proc"

// stopGateInfraProcessNames are the process command (comm) names present in
// an agent pod regardless of whether real work is running. Getting this set
// wrong in the permissive direction is the expensive failure: a name missing
// here that is in fact always running makes EVERY agent immortal on the
// cooldown path, fleet-wide. The set was taken from a live pod's table at
// rest, which is exactly:
//
//	1 S gb        <- PID 1, `gb init`; every gb subcommand, including this
//	                 stop-gate invocation, shares the name
//	  S coop      <- the coop session manager that supervises the CLI
//	  S claude    <- the Claude Code CLI itself (see below)
//	  Z tee, ...  <- coop's per-command pipes, reaped; excluded by state
//
// plus the shell hosting each Bash-tool invocation (stopGateShellProcessNames,
// checked alongside this set -- kept separate because the ancestry walk below
// needs the shell names on their own, without the rest of the infra list).
// The language servers are here for the same reason: setup_config.go enables
// the gopls and rust-analyzer LSP plugins whenever those binaries are on PATH
// (they are, in the agent image), and a language server is a long-lived
// daemon that idles for the whole session -- live, but never evidence that
// work is in flight. rust-analyzer is matched by prefix rather than added
// here (see stopGateLiveChildProcess): /proc/<pid>/comm truncates to 15
// bytes, and rust-analyzer spawns a proc-macro-srv helper whose full name,
// "rust-analyzer-proc-macro-srv", is 29 bytes and so reports as
// "rust-analyzer-p" -- an exact-match entry here would miss it.
var stopGateInfraProcessNames = map[string]bool{
	"gb":      true,
	"coop":    true,
	"coopmux": true,
	"claude":  true,
	"gopls":   true,
}

// stopGateShellProcessNames are the shells that host Bash-tool invocations.
// Kept separate from stopGateInfraProcessNames because the ancestry walk in
// stopGateAncestorIsCLIOwned needs to recognise a shell ancestor on its own:
// finding one partway up a chain means "this is Bash-tool work", full stop,
// regardless of how many hops separate it from the candidate.
var stopGateShellProcessNames = map[string]bool{
	"bash": true,
	"sh":   true,
	"dash": true,
	"zsh":  true,
}

// stopGateProcEntry is one process's /proc/<pid>/{stat,cmdline} snapshot,
// read once per scan into stopGateBuildProcTable rather than re-read per
// candidate per ancestry hop.
type stopGateProcEntry struct {
	comm    string
	state   string
	ppid    int
	cmdline string
}

// stopGateBuildProcTable reads every process's stat + cmdline in one pass,
// keyed by pid, so both the CLI-pid lookup and the ancestry walk work off one
// consistent snapshot of /proc instead of re-reading files per hop.
// Unparseable entries (a non-numeric directory name, a pid that exited
// mid-scan, a malformed stat) are omitted rather than aborting the scan.
func stopGateBuildProcTable(entries []os.DirEntry) map[int]stopGateProcEntry {
	table := make(map[int]stopGateProcEntry, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, state, ppid, ok := stopGateReadProcStat(pid)
		if !ok {
			continue
		}
		table[pid] = stopGateProcEntry{
			comm:    comm,
			state:   state,
			ppid:    ppid,
			cmdline: stopGateReadProcCmdline(pid),
		}
	}
	return table
}

// stopGateIsClaudeCLIEntry reports whether a process table entry is the
// Claude Code CLI itself. The agent image ships it three ways:
// @anthropic-ai/claude-code now installs a native binary whose comm is
// `claude`; older installs exec its cli.js under node (images/agent/install/
// node.sh, which still falls back to symlinking cli.js), matched by cmdline
// since bare comm=="node" is deliberately not treated as infra -- that would
// swallow a backgrounded `node build.js` or `npm test`; and a version-manager
// install execs a path containing "/claude/versions/" regardless of comm.
func stopGateIsClaudeCLIEntry(e stopGateProcEntry) bool {
	if e.comm == "claude" {
		return true
	}
	if e.comm == "node" && strings.Contains(e.cmdline, "claude-code") {
		return true
	}
	return strings.Contains(e.cmdline, "/claude/versions/")
}

// stopGateFindClaudePids returns the pid SET of every Claude Code CLI
// process in the table -- the parent (direct or transitive) every stdio MCP
// server and the gopls/rust-analyzer LSP plugins hang off of for the life of
// the session (see stopGateAncestorIsCLIOwned). A set, not a single pid,
// because a pod can legitimately run more than one CLI-shaped process (a
// session process and a daemon, for instance); matching only the
// lexicographically-first one would miss MCP servers parented under the
// others. An empty set means no CLI process was found at all -- the caller
// logs that loudly, since it silently disables the whole ownership filter.
func stopGateFindClaudePids(table map[int]stopGateProcEntry) map[int]bool {
	pids := make(map[int]bool)
	for pid, e := range table {
		if stopGateIsClaudeCLIEntry(e) {
			pids[pid] = true
		}
	}
	return pids
}

// stopGateAncestryMaxHops bounds the walk in stopGateAncestorIsCLIOwned so a
// cyclic or malformed /proc snapshot (a race during the scan, a corrupt test
// fixture) cannot spin forever.
const stopGateAncestryMaxHops = 64

// stopGateAncestorIsCLIOwned walks a candidate process's ancestry, starting
// at its immediate parent pid and following ppid toward PID 1, to classify it
// as CLI-owned (an MCP server, an LSP plugin, or any process transitively
// spawned by one -- e.g. an npx-launched MCP server's own child, which has
// the server, not the CLI, as its direct parent) versus real work.
//
// The first ancestor that resolves the question wins:
//   - a shell (stopGateShellProcessNames) means the candidate is Bash-tool
//     work, however many hops separate it from the shell -- not CLI-owned;
//   - membership in claudePids means the candidate hangs off the CLI,
//     directly or transitively -- CLI-owned, not work in flight;
//   - reaching PID 1 with neither means an orphaned background job,
//     reparented once the shell that launched it exited -- not CLI-owned.
//
// Bounded hops, a visited set (cycle guard), and a missing table entry
// (an ancestor that exited mid-scan) all end the walk the same way: return
// false. That is the fail-open-toward-counting-as-work direction -- an
// ancestry too confused to resolve is not confident evidence that the
// candidate should be excluded, matching stopGateLiveChildProcess's
// documented failure mode (a wrongly-spared agent is bounded by the 90m-2h
// idle/stale reaper, not permanent; a wrongly-closed one is not recoverable).
func stopGateAncestorIsCLIOwned(table map[int]stopGateProcEntry, claudePids map[int]bool, startPPID int) bool {
	visited := make(map[int]bool)
	pid := startPPID
	for hops := 0; hops < stopGateAncestryMaxHops; hops++ {
		if pid == 0 || pid == 1 || visited[pid] {
			return false
		}
		if claudePids[pid] {
			return true
		}
		visited[pid] = true
		entry, ok := table[pid]
		if !ok {
			return false
		}
		if stopGateShellProcessNames[entry.comm] {
			return false
		}
		pid = entry.ppid
	}
	return false
}

// stopGateLiveChildProcess scans this pod's process table for a live,
// non-shell, non-infrastructure process that is not owned by the Claude Code
// CLI. One agent runs per pod, so the pod's own PID namespace IS the session
// -- there is no separate "coop session pid" to walk down from, and a
// background job that outlives the shell that launched it gets reparented to
// PID 1 (gb init) rather than staying a child of a shell that may have
// already exited, so a scan scoped to literal children of a shell process
// would miss exactly the case this guard exists for.
//
// Returns the pid and command line of the first qualifying process found (in
// pid order), or (0, "") if none.
//
// Claude Code keeps every configured MCP server, and the gopls/rust-analyzer
// LSP plugins, running for the whole session as a child of the CLI process --
// not always a DIRECT child: an npx-launched MCP server's own child process
// has the server, not the CLI, as its parent. None of them match an infra
// name or the CLI's own cmdline signature, so without the ancestry walk in
// stopGateAncestorIsCLIOwned, a single configured MCP server (or a grandchild
// of one) makes EVERY agent in the fleet immortal on the cooldown path, not
// just the pod running it. Bash-tool work is unaffected: it runs under a
// shell (recognised at any depth by the same walk) or, once backgrounded and
// the shell exits, is reparented to PID 1 -- never to the CLI -- so it is
// still detected.
//
// A name that belongs in the infra list but is missing from it, or a
// misidentified CLI ancestor, keeps a pod alive past cooldown. That is
// bounded, not permanent, but by a wider window than "within the hour": the
// live-child path has no counter of its own (only recordStopGateBlock, on
// the block path, advances the escape-hatch state this branch reads), so a
// wrongly-spared agent rides out every subsequent quiet turn-end until
// either the 90m warm-keepalive idle-TTL (idleTTLDefault,
// init_agent_idle_ttl.go) despawns it or the 2h stale reaper
// (STALE_AGENT_TIMEOUT, config.go) reaps it on updated_at -- call it a 90m-2h
// bound, not "within the hour".
//
// Best-effort and Linux-specific (/proc): a pid that exits between listing
// and reading, a permission error, or a completely absent /proc (non-Linux,
// or a test that has not pointed stopGateProcRoot at a fixture) all fail
// CLOSED -- treated as "no live child found", matching stopGateClaimedTask's
// fail-closed contract, never as evidence either way.
func stopGateLiveChildProcess() (int, string) {
	entries, err := os.ReadDir(stopGateProcRoot)
	if err != nil {
		return 0, ""
	}
	table := stopGateBuildProcTable(entries)
	claudePids := stopGateFindClaudePids(table)
	if len(claudePids) == 0 {
		fmt.Fprintln(os.Stderr, "[stop-gate] warning: no Claude Code CLI process found in /proc; the MCP/LSP ownership filter cannot exclude any process this scan")
	}

	selfPid := os.Getpid()
	pids := make([]int, 0, len(table))
	for pid := range table {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	for _, pid := range pids {
		if pid == selfPid {
			continue
		}
		entry := table[pid]
		if !stopGateProcStateIsLive(entry.state) {
			continue
		}
		if stopGateInfraProcessNames[entry.comm] || stopGateShellProcessNames[entry.comm] {
			continue
		}
		if strings.HasPrefix(entry.comm, "rust-analyzer") {
			continue
		}
		if stopGateIsClaudeCLIEntry(entry) {
			continue
		}
		if stopGateAncestorIsCLIOwned(table, claudePids, entry.ppid) {
			continue
		}
		cmdline := entry.cmdline
		if cmdline == "" {
			cmdline = entry.comm
		}
		return pid, cmdline
	}
	return 0, ""
}

// stopGateProcStateIsLive reports whether a /proc/<pid>/stat state letter is
// a live process: Running, Sleeping (interruptible -- e.g. a `sleep` call or
// any process blocked on I/O, the common case for a backgrounded build), or
// Disk-sleep (uninterruptible I/O wait). Zombie (Z) and stopped/traced (T)
// are not live work.
func stopGateProcStateIsLive(state string) bool {
	return state == "R" || state == "S" || state == "D"
}

// stopGateReadProcStat reads the comm + state + ppid fields from
// /proc/<pid>/stat. The comm field is parenthesized and may itself contain
// spaces or unbalanced parens, so it is extracted between the first "(" and
// the LAST ")" rather than by naive whitespace splitting. state and ppid are
// the first two whitespace-separated fields after the closing paren (kernel
// order: pid (comm) state ppid ...).
func stopGateReadProcStat(pid int) (comm, state string, ppid int, ok bool) {
	data, err := os.ReadFile(filepath.Join(stopGateProcRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", "", 0, false
	}
	line := string(data)
	open := strings.IndexByte(line, '(')
	closeParen := strings.LastIndexByte(line, ')')
	if open < 0 || closeParen < 0 || closeParen <= open || closeParen+1 >= len(line) {
		return "", "", 0, false
	}
	comm = line[open+1 : closeParen]
	rest := strings.Fields(line[closeParen+1:])
	if len(rest) < 2 {
		return "", "", 0, false
	}
	ppid, err = strconv.Atoi(rest[1])
	if err != nil {
		return "", "", 0, false
	}
	return comm, rest[0], ppid, true
}

// stopGateReadProcCmdline reads /proc/<pid>/cmdline, joining its
// NUL-separated argv into a single space-separated string for the
// spare-reason log line.
func stopGateReadProcCmdline(pid int) string {
	data, err := os.ReadFile(filepath.Join(stopGateProcRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}
