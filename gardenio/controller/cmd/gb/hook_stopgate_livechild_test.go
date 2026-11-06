package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Layer-1 coverage for the stop-gate LIVE-CHILD GUARD (gd-C2JJqX31UI).
//
// The claim guard (hook_stopgate_claimguard_test.go) spares an agent that still
// holds an in-progress task, but it cannot see the population that killed
// gd-o0dnX8BWXV: an agent that backgrounded `make test-k8s-native-live`, ended
// its turn, held no claim, and had its pod deleted mid-run with edits
// uncommitted. A live child process is stronger evidence of work in flight than
// a claim, so it spares too.
//
// Both directions are load-bearing in opposite ways. A false negative resurrects
// that death; a false positive keeps a finished pod alive burning spend — which
// is why every infrastructure process present in EVERY pod (gb, coop, the Bash
// tool's shells, the Claude Code CLI itself) must NOT spare on its own.

// useProcFixture points stopGateLiveChildProcess at a synthetic /proc tree for
// the duration of one test, and returns its root.
func useProcFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	old := stopGateProcRoot
	stopGateProcRoot = root
	t.Cleanup(func() { stopGateProcRoot = old })
	return root
}

// writeProcEntry materialises one <root>/<pid>/{stat,cmdline} pair in the
// fixture, in the real kernel's formats: stat is "pid (comm) state ppid …" and
// cmdline is NUL-separated argv with a trailing NUL. Passing an empty argv
// writes no cmdline file at all — the shape of a kernel thread, and the case
// where the reason line has to fall back to comm. ppid defaults to 1 (as if
// reparented to PID 1, the common shape for a backgrounded job); use
// writeProcEntryPPID directly to model a process parented under another pid
// in the fixture (e.g. an MCP server parented under the claude process).
func writeProcEntry(t *testing.T, root string, pid int, comm, state string, argv ...string) {
	t.Helper()
	writeProcEntryPPID(t, root, pid, 1, comm, state, argv...)
}

// writeProcEntryPPID is writeProcEntry with an explicit parent pid.
func writeProcEntryPPID(t *testing.T, root string, pid, ppid int, comm, state string, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	stat := fmt.Sprintf("%d (%s) %s %d 1 0 0 -1 4194304 100 0 0 0 1 2 0 0 20 0 1 0 999\n", pid, comm, state, ppid)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	if len(argv) > 0 {
		blob := strings.Join(argv, "\x00") + "\x00"
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(blob), 0o644); err != nil {
			t.Fatalf("write cmdline: %v", err)
		}
	}
}

// writeInfraProcs lays down the processes that exist in every agent pod whether
// or not any work is running: PID 1 is `gb init`, gb's own stop-gate invocation,
// the coop session manager, and the shell hosting the Bash tool.
func writeInfraProcs(t *testing.T, root string) {
	t.Helper()
	writeProcEntry(t, root, 1, "gb", "S", "gb", "init")
	writeProcEntry(t, root, 12, "coopmux", "S", "coopmux", "serve")
	writeProcEntry(t, root, 30, "bash", "S", "bash", "-lc", "gb hook stop-gate")
	writeProcEntry(t, root, 31, "gb", "R", "gb", "hook", "stop-gate")
	writeProcEntry(t, root, 40, "node", "S", "node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js")
}

// TestStopGateLiveChildProcess_BackgroundedTest is the exact regression:
// `make test-k8s-native-live` still running when cooldown hits must be found,
// and reported with the pid and full command so the log line says why.
func TestStopGateLiveChildProcess_BackgroundedTest(t *testing.T) {
	root := useProcFixture(t)
	writeInfraProcs(t, root)
	writeProcEntry(t, root, 77, "make", "S", "make", "test-k8s-native-live")

	pid, cmd := stopGateLiveChildProcess()
	if pid != 77 {
		t.Fatalf("stopGateLiveChildProcess pid = %d, want 77 — a backgrounded test must spare the agent", pid)
	}
	if cmd != "make test-k8s-native-live" {
		t.Errorf("cmd = %q, want %q — the spare reason must name the command", cmd, "make test-k8s-native-live")
	}
}

// TestStopGateLiveChildProcess_InfraOnly is the shutdown-safe direction: a pod
// running nothing but its own scaffolding has no work in flight, so the guard
// must fall through and let the cooldown close happen. If this ever returns a
// pid, every agent in the fleet becomes immortal.
func TestStopGateLiveChildProcess_InfraOnly(t *testing.T) {
	root := useProcFixture(t)
	writeInfraProcs(t, root)

	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		t.Fatalf("stopGateLiveChildProcess = (%d, %q), want (0, \"\") — pod scaffolding alone must not spare an agent", pid, cmd)
	}
}

// TestStopGateLiveChildProcess_SkipsDeadStates pins that a process which is no
// longer doing work does not spare: a zombie awaiting reap (Z) and a
// stopped/traced process (T, t) are corpses, not builds.
func TestStopGateLiveChildProcess_SkipsDeadStates(t *testing.T) {
	for _, state := range []string{"Z", "T", "t", "X"} {
		t.Run(state, func(t *testing.T) {
			root := useProcFixture(t)
			writeInfraProcs(t, root)
			writeProcEntry(t, root, 77, "make", state, "make", "test-k8s-native-live")

			if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
				t.Fatalf("state %q: got (%d, %q), want (0, \"\") — a non-live process is not work in flight", state, pid, cmd)
			}
		})
	}
}

// TestStopGateLiveChildProcess_LiveStates is the converse: the three states a
// real backgrounded job is actually observed in. Sleeping (S) is the common
// case — a compile blocked on I/O, or a literal `sleep` — and dropping it would
// make the guard nearly useless.
func TestStopGateLiveChildProcess_LiveStates(t *testing.T) {
	for _, state := range []string{"R", "S", "D"} {
		t.Run(state, func(t *testing.T) {
			root := useProcFixture(t)
			writeInfraProcs(t, root)
			writeProcEntry(t, root, 77, "sleep", state, "sleep", "300")

			if pid, _ := stopGateLiveChildProcess(); pid != 77 {
				t.Fatalf("state %q: pid = %d, want 77", state, pid)
			}
		})
	}
}

// TestStopGateLiveChildProcess_ClaudeCodeCLINotWork guards the subtlest false
// positive. The CLI itself runs as `node …/claude-code/cli.js`, so a bare
// comm=="node" skip would be wrong (it would swallow a backgrounded `npm test`)
// and no skip at all would be worse (every agent, always spared). The match is
// on cmdline, so only the CLI is excused.
func TestStopGateLiveChildProcess_ClaudeCodeCLINotWork(t *testing.T) {
	root := useProcFixture(t)
	writeInfraProcs(t, root) // includes the claude-code node process at pid 40

	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		t.Fatalf("got (%d, %q), want (0, \"\") — the Claude Code CLI is not work in flight", pid, cmd)
	}

	// …but another node process is.
	writeProcEntry(t, root, 88, "node", "R", "node", "build.js")
	if pid, cmd := stopGateLiveChildProcess(); pid != 88 {
		t.Fatalf("got (%d, %q), want pid 88 — a backgrounded node job must still spare the agent", pid, cmd)
	}
}

// TestStopGateLiveChildProcess_SkipsSelf pins that the stop-gate process
// reading /proc never counts itself. It is spelled `gb` in practice and so is
// already skipped by name, but the pid skip is the guarantee that survives a
// rename of the binary.
func TestStopGateLiveChildProcess_SkipsSelf(t *testing.T) {
	root := useProcFixture(t)
	writeProcEntry(t, root, os.Getpid(), "some-renamed-gb", "R", "some-renamed-gb", "hook", "stop-gate")

	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		t.Fatalf("got (%d, %q), want (0, \"\") — the stop-gate must not spare itself", pid, cmd)
	}
}

// TestStopGateLiveChildProcess_CmdlineFallback covers the kernel-thread shape:
// an empty /proc/<pid>/cmdline. The reason line still has to name something, so
// it falls back to comm rather than logging "spared by pid 77 ()".
func TestStopGateLiveChildProcess_CmdlineFallback(t *testing.T) {
	root := useProcFixture(t)
	writeProcEntry(t, root, 77, "kworker/u8:3", "S") // no argv → no cmdline file

	pid, cmd := stopGateLiveChildProcess()
	if pid != 77 {
		t.Fatalf("pid = %d, want 77", pid)
	}
	if cmd != "kworker/u8:3" {
		t.Errorf("cmd = %q, want the comm fallback %q", cmd, "kworker/u8:3")
	}
}

// TestStopGateLiveChildProcess_NoProcFailsClosed is the fail-CLOSED contract,
// matching stopGateClaimedTask: an unreadable or absent /proc (a non-Linux
// host, a locked-down sandbox) is "no evidence", never "assume work". A
// fail-open guard here would wedge every pod alive on any platform without
// /proc.
func TestStopGateLiveChildProcess_NoProcFailsClosed(t *testing.T) {
	old := stopGateProcRoot
	stopGateProcRoot = filepath.Join(t.TempDir(), "definitely-absent")
	t.Cleanup(func() { stopGateProcRoot = old })

	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		t.Fatalf("got (%d, %q), want (0, \"\") — an unreadable /proc must fail closed", pid, cmd)
	}
}

// TestStopGateLiveChildProcess_SkipsUnparseableEntries pins that junk in the
// scan (a non-numeric directory like /proc/self, a pid whose stat is truncated,
// a pid that exited between ReadDir and the read) is stepped over rather than
// aborting the scan — the real live child sits behind them.
func TestStopGateLiveChildProcess_SkipsUnparseableEntries(t *testing.T) {
	root := useProcFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatalf("mkdir self: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "uptime"), []byte("1 1\n"), 0o644); err != nil {
		t.Fatalf("write uptime: %v", err)
	}
	// pid 2: directory with no stat file at all (exited mid-scan).
	if err := os.MkdirAll(filepath.Join(root, "2"), 0o755); err != nil {
		t.Fatalf("mkdir 2: %v", err)
	}
	// pid 3: stat present but malformed.
	if err := os.MkdirAll(filepath.Join(root, "3"), 0o755); err != nil {
		t.Fatalf("mkdir 3: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "3", "stat"), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	writeProcEntry(t, root, 77, "make", "S", "make", "test")

	if pid, _ := stopGateLiveChildProcess(); pid != 77 {
		t.Fatalf("pid = %d, want 77 — unparseable /proc entries must be skipped, not abort the scan", pid)
	}
}

// TestStopGateReadProcStat_CommWithSpacesAndParens pins the parse that a naive
// whitespace split gets wrong. A process is free to name itself
// "(my weird) proc", and the kernel does not escape it; splitting on spaces
// would read the state letter out of the middle of the name and mistake a live
// process for a dead one.
func TestStopGateReadProcStat_CommWithSpacesAndParens(t *testing.T) {
	root := useProcFixture(t)
	writeProcEntry(t, root, 77, "(my weird) proc", "R", "whatever")

	comm, state, ppid, ok := stopGateReadProcStat(77)
	if !ok {
		t.Fatal("stopGateReadProcStat ok = false, want true")
	}
	if comm != "(my weird) proc" {
		t.Errorf("comm = %q, want %q — comm runs to the LAST paren, not the first", comm, "(my weird) proc")
	}
	if state != "R" {
		t.Errorf("state = %q, want R", state)
	}
	if ppid != 1 {
		t.Errorf("ppid = %d, want 1", ppid)
	}
}

// TestStopGateLiveChildProcess_McpServerNotWork guards the expensive failure
// the coordinator review caught on PR #2 for this task: Claude Code keeps
// every configured MCP server, and the gopls/rust-analyzer LSP plugins, as a
// live, non-shell, direct child of the claude/cli.js process for the whole
// session. None of them match an infra name or the node+claude-code cmdline
// exclusion, so without a parent check a single configured MCP server makes
// EVERY agent in the fleet immortal on the cooldown path -- not just the pod
// that happens to have one configured.
func TestStopGateLiveChildProcess_McpServerNotWork(t *testing.T) {
	root := useProcFixture(t)
	writeInfraProcs(t, root) // pid 40: the node-hosted claude-code CLI
	writeProcEntryPPID(t, root, 90, 40, "bun", "S", "bun", "/root/.mcp-servers/gardenio-channel.ts")

	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		t.Fatalf("got (%d, %q), want (0, \"\") — a direct child of the claude process is an MCP/LSP server, not work in flight", pid, cmd)
	}

	// A genuine backgrounded job -- reparented to PID 1, as an orphaned
	// background job actually is once its shell exits -- must still spare.
	writeProcEntry(t, root, 77, "make", "S", "make", "test")
	if pid, _ := stopGateLiveChildProcess(); pid != 77 {
		t.Fatalf("pid = %d, want 77 — a genuine background job must still spare the agent even with an MCP server present", pid)
	}
}

// TestStopGateLiveChildProcess_McpServerGrandchildNotWork pins the
// coordinator's follow-up fix on PR #2: a single-hop parent check only
// catches an MCP server parented DIRECTLY under the CLI. An npx-launched
// server's own child process (e.g. mcp-server-playwright spawning a browser)
// has the SERVER, not the CLI, as its immediate parent -- reproduced live in
// a crew pod as claude -> gopls -> a gopls worker, and by the coordinator's
// fixture as claude -> npx -> mcp-server-playwright -> chrome. The ancestry
// walk has to climb the whole chain toward PID 1, not stop after one hop.
func TestStopGateLiveChildProcess_McpServerGrandchildNotWork(t *testing.T) {
	root := useProcFixture(t)
	writeInfraProcs(t, root) // pid 40: the node-hosted claude-code CLI
	writeProcEntryPPID(t, root, 90, 40, "npx", "S", "npx", "@playwright/mcp")
	writeProcEntryPPID(t, root, 91, 90, "node", "S", "node", "mcp-server-playwright")
	writeProcEntryPPID(t, root, 92, 91, "chrome", "S", "chrome", "--headless")

	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		t.Fatalf("got (%d, %q), want (0, \"\") — a grandchild of the claude process, reached via an MCP server, is not work in flight", pid, cmd)
	}

	// A genuine backgrounded job -- reparented to PID 1 -- must still spare
	// even with a multi-hop MCP server chain present.
	writeProcEntry(t, root, 77, "make", "S", "make", "test")
	if pid, _ := stopGateLiveChildProcess(); pid != 77 {
		t.Fatalf("pid = %d, want 77 — a genuine background job must still spare the agent", pid)
	}
}

// TestStopGateLiveChildProcess_GoplsWorkerGrandchildNotWork is the exact
// live shape the coordinator reproduced in a crew pod: claude(35) ->
// gopls(2154) -> a gopls worker process(2169). gopls itself is excluded by
// name at the top level regardless of ancestry, but its own child is not --
// and that child's immediate parent is gopls, not claude, so the walk must
// continue past gopls to find the CLI.
func TestStopGateLiveChildProcess_GoplsWorkerGrandchildNotWork(t *testing.T) {
	root := useProcFixture(t)
	writeInfraProcs(t, root) // pid 40: the node-hosted claude-code CLI
	writeProcEntryPPID(t, root, 50, 40, "gopls", "S", "gopls", "serve")
	writeProcEntryPPID(t, root, 51, 50, "telemetry", "S", "gopls-telemetry-reporter")

	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		t.Fatalf("got (%d, %q), want (0, \"\") — a gopls worker process, parented under gopls rather than claude directly, is not work in flight", pid, cmd)
	}
}

// TestStopGateFindClaudePids_MultipleCLIProcesses pins the should-fix item
// from the coordinator's review: a pod can run more than one CLI-shaped
// process (a session process and a daemon), and the ancestry walk must
// recognise an MCP server parented under EITHER one, not just the
// lexicographically-first pid a single-value lookup would have kept.
func TestStopGateFindClaudePids_MultipleCLIProcesses(t *testing.T) {
	root := useProcFixture(t)
	writeInfraProcs(t, root)                                         // pid 40: node-hosted CLI
	writeProcEntry(t, root, 41, "claude", "S", "claude", "--daemon") // a second, native-binary CLI process

	pids := stopGateFindClaudePids(stopGateBuildProcTable(mustReadDir(t, root)))
	if !pids[40] || !pids[41] {
		t.Fatalf("stopGateFindClaudePids = %v, want both pid 40 and pid 41", pids)
	}

	// An MCP server parented under the SECOND CLI process (41, not 40) must
	// still be excluded.
	writeProcEntryPPID(t, root, 90, 41, "bun", "S", "bun", "mcp-server.js")
	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		t.Fatalf("got (%d, %q), want (0, \"\") — an MCP server parented under the second CLI process is still CLI-owned", pid, cmd)
	}
}

// TestStopGateLiveChildProcess_NoCLIWarnsLoudly pins the should-fix item that
// zero CLI candidates must not silently disable the ownership filter: the
// scan still runs (a live non-infra process is still found), but a warning
// names the degraded state so it is visible in pod logs rather than
// discovered later as an unexplained immortal agent.
func TestStopGateLiveChildProcess_NoCLIWarnsLoudly(t *testing.T) {
	root := useProcFixture(t)
	writeProcEntry(t, root, 1, "gb", "S", "gb", "init") // no CLI process in this fixture at all
	writeProcEntry(t, root, 77, "make", "S", "make", "test")

	out := captureStderr(t, func() {
		pid, _ := stopGateLiveChildProcess()
		if pid != 77 {
			t.Fatalf("pid = %d, want 77 — the scan must still find real work with no CLI present", pid)
		}
	})
	if !strings.Contains(out, "no Claude Code CLI process found") {
		t.Fatalf("stderr = %q, want a loud warning that the CLI ownership filter is disabled", out)
	}
}

// mustReadDir is os.ReadDir with test-friendly error handling, for tests that
// need the raw entries to call stopGateBuildProcTable directly.
func mustReadDir(t *testing.T, root string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", root, err)
	}
	return entries
}

// TestStopGateLiveChildProcess_RustAnalyzerProcMacroTruncated pins the second
// coordinator-review fix: /proc/<pid>/comm truncates to 15 bytes, so
// rust-analyzer's own proc-macro-srv helper -- "rust-analyzer-proc-macro-srv"
// (29 bytes), spawned as rust-analyzer's OWN child rather than the claude
// process's, so the parent check above does not reach it -- reports as
// "rust-analyzer-p". An exact-name match on "rust-analyzer" misses that
// truncated name entirely; the exclusion has to be a prefix match.
func TestStopGateLiveChildProcess_RustAnalyzerProcMacroTruncated(t *testing.T) {
	root := useProcFixture(t)
	writeInfraProcs(t, root)
	writeProcEntry(t, root, 95, "rust-analyzer-p", "S", "rust-analyzer-proc-macro-srv")

	if pid, cmd := stopGateLiveChildProcess(); pid != 0 {
		t.Fatalf("got (%d, %q), want (0, \"\") — the truncated proc-macro-srv comm must still be excluded as infra", pid, cmd)
	}
}

// TestStopGateCooldownSparesAgent_ClaimWins pins the ordering contract the
// pre-existing claim-guard tests depend on: when a claim is held the reason is
// exactly the task ID, unchanged from before this guard existed, with no pid
// descriptor wrapped around it.
func TestStopGateCooldownSparesAgent_ClaimWins(t *testing.T) {
	withActor(t, "monorepo-crew-abc")
	setupClaimedTaskDaemon(t, "gd-task1", "in_progress")
	root := useProcFixture(t)
	writeInfraProcs(t, root)
	writeProcEntry(t, root, 77, "make", "S", "make", "test")

	reason, spare := stopGateCooldownSparesAgent(context.Background())
	if !spare {
		t.Fatal("spare = false, want true — a claim-holding agent must still be spared")
	}
	if reason != "gd-task1" {
		t.Errorf("reason = %q, want the bare task ID gd-task1", reason)
	}
}

// TestStopGateCooldownSparesAgent_LiveChildNoClaim is the new branch end to
// end: no claim at all — the gd-o0dnX8BWXV shape — but a backgrounded test
// running, so the agent survives cooldown and the reason names the pid.
func TestStopGateCooldownSparesAgent_LiveChildNoClaim(t *testing.T) {
	withActor(t, "monorepo-crew-abc")
	setupClaimedTaskDaemon(t, "", "")
	root := useProcFixture(t)
	writeInfraProcs(t, root)
	writeProcEntry(t, root, 77, "make", "S", "make", "test-k8s-native-live")

	reason, spare := stopGateCooldownSparesAgent(context.Background())
	if !spare {
		t.Fatal("spare = false, want true — a backgrounded test must survive cooldown")
	}
	if want := "pid 77 (make test-k8s-native-live)"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}

// TestStopGateCooldownSparesAgent_IdleCloses is the whole-guard shutdown-safe
// case: no claim, nothing running but pod scaffolding. This agent is genuinely
// done and must still be closed, or the cooldown path stops reclaiming pods.
func TestStopGateCooldownSparesAgent_IdleCloses(t *testing.T) {
	withActor(t, "monorepo-crew-abc")
	setupClaimedTaskDaemon(t, "", "")
	root := useProcFixture(t)
	writeInfraProcs(t, root)

	reason, spare := stopGateCooldownSparesAgent(context.Background())
	if spare {
		t.Fatalf("spare = true (reason %q), want false — an idle agent with no claim must still close", reason)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}
