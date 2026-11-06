package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetCommentBodyFlag restores commentAddCmd's shared --body flag (bound to the
// package-global commentBody) so neither the value nor its Changed state leaks
// into another test in this package.
func resetCommentBodyFlag(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		f := commentAddCmd.Flags().Lookup("body")
		_ = f.Value.Set("")
		f.Changed = false
	})
}

// TestCommentAdd_BodyAtFileExpands is the regression lock for ka-0000005879:
// `kd comment add <id> --body @file` must post the file's CONTENTS, BYTE FOR
// BYTE. Before the fix the flag value was persisted literally, so posting a
// markdown file meant routing it through the shell — and every such route
// mangles it silently at exit 0 (standing advice ka-0000004424, ka-0000002095).
//
// The fixture below is chosen to catch each shell-mangling mode at once: a
// backtick span and a $(…) span (executed by an unquoted heredoc), and a
// trailing blank line inside a closed code fence (stripped by "$(cat f.md)",
// which drops ALL trailing newlines and thereby breaks the fence). An
// assertion on the whole string is what makes the newline case bite.
func TestCommentAdd_BodyAtFileExpands(t *testing.T) {
	dir := t.TempDir()
	body := "a comment with `backtick` inline code\nand a $(echo substitution) span\n\n```\nfenced\n```\n"
	path := filepath.Join(dir, "body.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}

	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetCommentBodyFlag(t)
	if err := commentAddCmd.Flags().Set("body", "@"+path); err != nil {
		t.Fatalf("set --body: %v", err)
	}

	_ = captureStdout(t, func() {
		if err := commentAddCmd.RunE(commentAddCmd, []string{"gd-target123"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.AddCommentCalls) != 1 {
		t.Fatalf("expected 1 AddComment call, got %d", len(mc.AddCommentCalls))
	}
	if got := mc.AddCommentCalls[0].Text; got != body {
		t.Errorf("comment text = %q, want the file body %q", got, body)
	}
	// Pin the trailing newline explicitly: it is the byte "$(cat f.md)" always
	// eats, and an assertion that only checked for a substring would pass on a
	// body that had silently lost its code-fence terminator.
	if got := mc.AddCommentCalls[0].Text; !strings.HasSuffix(got, "```\n") {
		t.Errorf("trailing newline stripped: text = %q", got)
	}
}

// TestCommentAdd_BodyAtAtEscapesLiteral asserts the @@ escape survives on the
// comment path: a comment that really begins with '@' (an @mention) is written
// as --body '@@…' and must land with exactly one leading '@'.
func TestCommentAdd_BodyAtAtEscapesLiteral(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetCommentBodyFlag(t)
	if err := commentAddCmd.Flags().Set("body", "@@matt see the thread"); err != nil {
		t.Fatalf("set --body: %v", err)
	}

	_ = captureStdout(t, func() {
		if err := commentAddCmd.RunE(commentAddCmd, []string{"gd-target123"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.AddCommentCalls) != 1 {
		t.Fatalf("expected 1 AddComment call, got %d", len(mc.AddCommentCalls))
	}
	if got, want := mc.AddCommentCalls[0].Text, "@matt see the thread"; got != want {
		t.Errorf("comment text = %q, want %q", got, want)
	}
}

// TestCommentAdd_BodyMissingAtFileFailsLoud asserts a typo'd/stale path errors
// instead of durably writing the literal "@/path" pointer as the comment — the
// same fail-loud contract create's -d @file carries (ka-0000003715).
func TestCommentAdd_BodyMissingAtFileFailsLoud(t *testing.T) {
	dir := t.TempDir()

	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetCommentBodyFlag(t)
	if err := commentAddCmd.Flags().Set("body", "@"+filepath.Join(dir, "gone.md")); err != nil {
		t.Fatalf("set --body: %v", err)
	}

	err := commentAddCmd.RunE(commentAddCmd, []string{"gd-target123"})
	if err == nil {
		t.Fatal("expected error for --body @missing-file, got nil")
	}
	if len(mc.AddCommentCalls) != 0 {
		t.Errorf("AddComment must not be called when --body @file fails to read, got %d calls", len(mc.AddCommentCalls))
	}
}

// TestResolveCommentText_AtFileSourceRules pins WHICH source expands: only the
// flag. Positional text stays verbatim because a bare positional cannot
// unambiguously carry @file intent — `kd comment add <id> @home tomorrow` is
// prose, not a path.
func TestResolveCommentText_AtFileSourceRules(t *testing.T) {
	dir := t.TempDir()
	body := "read `me` from disk\n"
	path := filepath.Join(dir, "body.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}

	got, err := resolveCommentText(true, "@"+path, nil)
	if err != nil {
		t.Fatalf("flag @file: unexpected error: %v", err)
	}
	if got != body {
		t.Errorf("flag @file text = %q, want %q", got, body)
	}

	got, err = resolveCommentText(false, "", []string{"@" + path})
	if err != nil {
		t.Fatalf("positional: unexpected error: %v", err)
	}
	if want := "@" + path; got != want {
		t.Errorf("positional text = %q, want it verbatim %q", got, want)
	}
}
