package main

// prime_hook_trimmed_test.go pins the ka-0000001249 contract: outputPrimeForHook
// (called by SessionStart bus emit and by gb hook prime --enqueue) is the
// trimmed dynamic preview, NOT the full prime body.
//
// The full prime body (workflow context, advice, advice-role-diff, roster,
// auto-assign, user preferences) is delivered via --append-system-prompt-file
// at coop session start. Re-emitting it through the hook channel duplicates
// content that the harness's 2 KB output cap will truncate anyway.
//
// ka-0000007437 extends that contract to the three sections the hook was still
// double-emitting — identity, Slack thread context, prior thread beads — each
// of which buildSystemPromptBody already delivers at full fidelity through the
// uncapped path. On a live thread pod they inflated hook stdout to 15,475 B
// against a 2,206 B inline budget, evicting genuinely live state past the cap.
//
// What this test enforces:
//   - Trimmed output INCLUDES the live jacks block and a pointer to where the
//     full rules live.
//   - Trimmed output EXCLUDES the full-prime-only headings ("Workflow Context",
//     "Advice", "Active agents", "Auto-Assign") AND every section
//     buildSystemPromptBody already emits.
//   - The degraded-boot fallback still emits those sections when the
//     system-prompt file is absent, so a failed write can't lobotomize an
//     agent (TestOutputPrimeForHook_DegradedBoot*).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputPrimeForHook_TrimmedExcludesStaticRules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Empty list keeps every section that calls daemon (jacks, roster,
		// auto-assign, advice) cheap and predictable. The trimmed variant
		// should still be able to skip those even when data is available.
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")
	// Healthy boot: the system prompt file exists, so the hook must not
	// re-emit anything buildSystemPromptBody already carries.
	withSystemPromptFile(t)

	var buf bytes.Buffer
	outputPrimeForHook(&buf, "test-agent")
	out := buf.String()

	// Wrapper.
	if !strings.HasPrefix(out, "<system-reminder>") {
		t.Errorf("missing opening system-reminder tag: %q", out[:min(80, len(out))])
	}
	if !strings.Contains(out, "</system-reminder>") {
		t.Errorf("missing closing system-reminder tag")
	}

	// Required dynamic-preview sections.
	for _, want := range []string{
		"loaded in the system prompt",
		"`gb prime`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trimmed prime missing %q\n--- output:\n%s", want, out)
		}
	}

	// Sections that MUST NOT appear — these now live in the system prompt.
	for _, banned := range []string{
		"Workflow Context",
		"## Advice",
		"## Active agents",
		"## Auto-Assign",
		"## User Preferences",
		// ka-0000007437: buildSystemPromptBody emits these three at full
		// fidelity through the uncapped --append-system-prompt-file path.
		"## Your Identity",
		"## Slack Thread Context",
		"## Prior beads in THIS Slack thread",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("trimmed prime should not contain %q (now lives in system prompt)\n--- output:\n%s", banned, out)
		}
	}

	// Size budget: with thread context and identity gone, the healthy-boot
	// hook payload is jacks + the pointer line. That fits the harness's real
	// ~2 KB inline cap, which is the number that actually matters — anything
	// past it is spilled to a file and costs a tool call to read.
	//
	// The old 16 KB budget (gd-IVxmkxsmf2) existed only to accommodate the
	// 10 KB thread-context preview this change removes; measured hook stdout
	// on a live thread pod was 15,475 B against a 2,206 B inline budget.
	// Jacks are unbounded-ish live state (up to 50 beads), so the gate is set
	// at 4 KB: comfortably above a normal jacks block, far below the old cap,
	// and it reds if a duplicated section is ever reintroduced here.
	const hookBudgetBytes = 4096
	if got := len(out); got > hookBudgetBytes {
		t.Errorf("trimmed prime hook output is %d bytes; must stay under %d-byte hook budget\n--- output:\n%s",
			got, hookBudgetBytes, out)
	}
}

// withSystemPromptBody simulates a boot whose system prompt contains body:
// it writes the file and exports the env marker writeSystemPromptFile sets on
// success, which is what the agent's hooks inherit.
func withSystemPromptBody(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), systemPromptFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write system prompt: %v", err)
	}
	t.Setenv(envSystemPromptFile, path)
}

// withSystemPromptFile simulates a HEALTHY boot: the system prompt carries all
// three sections the hook is expected to suppress.
func withSystemPromptFile(t *testing.T) {
	t.Helper()
	withSystemPromptBody(t, strings.Join([]string{
		"## Your Identity",
		"- **Agent**: test-agent",
		"## Slack Thread Context",
		"…thread…",
		"## Prior beads in THIS Slack thread",
		"- `gd-x` [task/open] t",
		"",
	}, "\n"))
}

// TestOutputPrimeForHook_DegradedBootReemitsSections pins the safety valve on
// the ka-0000007437 de-dup: writeSystemPromptFile is best-effort, and on
// failure the agent launches WITHOUT --append-system-prompt-file. In that case
// identity/thread-context/siblings reach the agent through no other channel,
// so the hook must re-emit them rather than leave the agent contextless.
func TestOutputPrimeForHook_DegradedBootReemitsSections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")
	// Degraded boot: writeSystemPromptFile failed, so no env marker is set and
	// the agent launched without --append-system-prompt-file.
	t.Setenv(envSystemPromptFile, "")

	var buf bytes.Buffer
	outputPrimeForHook(&buf, "test-agent")
	out := buf.String()

	// Identity is the one section that renders unconditionally (thread context
	// and siblings no-op without SLACK_THREAD_* binding), so it is the
	// observable proof the fallback fired.
	for _, want := range []string{"## Your Identity", "test-agent"} {
		if !strings.Contains(out, want) {
			t.Errorf("degraded boot must re-emit %q when the system prompt file is missing\n--- output:\n%s", want, out)
		}
	}
}

// TestOutputPrimeForHook_StaleWarmPoolFileIsNotSuccess pins the warm-pool
// hazard that makes the healthy-boot signal an env var rather than a file
// stat. writeSystemPromptFile runs at pool-fill time, so a prewarmed pod
// already has a system-prompt.md on disk BEFORE it is ever assigned to a
// thread. If the assigned session's own write then fails, the agent launches
// with no appended system prompt — and a file-existence check would read the
// stale file as success, suppress the fallback, and leave the agent with
// identity in neither channel.
//
// Verified against a live prewarmed pod: /home/agent/workspace/.gb/system-prompt.md
// existed while the pod had no SLACK_THREAD_* binding and coop was not yet running.
func TestOutputPrimeForHook_StaleWarmPoolFileIsNotSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "thread")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")

	// A stale pool-fill artifact sits in the workdir...
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gb"), 0o755); err != nil {
		t.Fatalf("mkdir .gb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gb", systemPromptFileName), []byte("## Your Identity\nstale pool-fill body\n"), 0o644); err != nil {
		t.Fatalf("write stale system prompt: %v", err)
	}
	t.Chdir(dir)
	t.Setenv("WORKSPACE", dir)
	t.Setenv("KD_WORKSPACE", dir)
	// ...but THIS session's write failed, so no env marker.
	t.Setenv(envSystemPromptFile, "")

	if got := systemPromptSections(); got["## Your Identity"] {
		t.Fatal("a stale warm-pool system-prompt.md must NOT count as this session's prompt")
	}

	var buf bytes.Buffer
	outputPrimeForHook(&buf, "test-agent")
	if out := buf.String(); !strings.Contains(out, "## Your Identity") {
		t.Errorf("fallback must fire when only a stale pool-fill file exists\n--- output:\n%s", out)
	}
}

// TestOutputPrimeForHook_WarmPoolPromptLacksThreadContext pins the warm-pool
// ordering hazard that makes the de-dup PER-SECTION rather than all-or-nothing.
//
// runCoopSession writes the system prompt (init_agent_loop.go:414) BEFORE
// standbyWait (:569) blocks for an assignment, and SLACK_THREAD_* is hydrated
// only at assignment (hydrateAssignmentEnv). So a pooled pod's system prompt is
// generated while still unbound: it HAS "## Your Identity" but NOT the thread
// sections (verified against a live prewarmed pod, and by running
// `gb prime --system-prompt` with SLACK_THREAD_* unset → 0 thread sections).
//
// A boolean "was a system prompt written?" check would treat that successful
// write as healthy and suppress the thread sections — leaving a pooled thread
// agent with no conversation through EITHER channel.
func TestOutputPrimeForHook_WarmPoolPromptLacksThreadContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "thread")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")

	// The pool-fill-era prompt: identity present, thread sections absent
	// because the pod was unbound when it was generated.
	withSystemPromptBody(t, "## Your Identity\n\n- **Agent**: test-agent\n\n## Advice (0 items)\n")

	got := systemPromptSections()
	if !got["## Your Identity"] {
		t.Error("identity IS in the warm-pool prompt and must be detected as present")
	}
	if got["## Slack Thread Context"] || got["## Prior beads in THIS Slack thread"] {
		t.Error("thread sections are NOT in the warm-pool prompt; they must be reported absent so the hook re-emits them")
	}

	var buf bytes.Buffer
	outputPrimeForHook(&buf, "test-agent")
	// Identity is carried by the prompt, so the hook must not repeat it...
	if out := buf.String(); strings.Contains(out, "## Your Identity") {
		t.Errorf("identity is already in the system prompt; hook must not duplicate it\n--- output:\n%s", out)
	}
	// ...and the thread sections stay eligible for the hook channel. (They
	// still no-op here because no SLACK_THREAD_* binding exists in-test; the
	// section-presence assertions above are what pin the routing decision.)
}
