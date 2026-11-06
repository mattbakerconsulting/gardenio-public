package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// This file is the ka-0000003818 audit's behavioural contract: for every gb
// prose-taking string flag that persists agent-authored text into a bead, we
// assert EITHER (1) @file/@-/@@ expansion is wired in — the file BODY is stored
// and a missing path fails LOUD — OR (2) the value is stored LITERALLY by
// explicit design (squawk / dm --body). The trap the audit closes is the
// "accidental middle": a value that LOOKS like a file pointer silently stored
// as a dangling literal with a success exit. Each test below pins one side of
// that contract so the choice can never silently regress.
//
// The shared resolveTextValue helper (atfile.go) and the --content path
// (decision_report_atfile_test.go) are covered by MR !6315; these tests cover
// the flags this follow-up bead audited.

// captureFields spins up an httptest daemon that records the fields JSON of the
// first POST /v1/beads and returns a sane default for every other request
// (dep walks, closes, GETs) so create paths that do project resolution / dedup
// / gate satisfaction run to completion without a live daemon.
func captureFields(t *testing.T, createID string, out *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var req beadsapi.CreateBeadRequest
			_ = json.Unmarshal(body, &req)
			if *out == nil { // capture the first create only
				var f map[string]any
				_ = json.Unmarshal(req.Fields, &f)
				*out = map[string]any{
					"__title":       req.Title,
					"__description": req.Description,
				}
				for k, v := range f {
					(*out)[k] = v
				}
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": createID})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads"):
			// Dedup / project-resolution / gate reads: an empty list and a
			// benign bead keep the create path out of the dup/gate branches.
			if strings.HasSuffix(r.URL.Path, "/dependencies") {
				w.WriteHeader(200)
				_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": []any{}})
				return
			}
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = oldDaemon; srv.Close() })
	return srv
}

// createRejects wires a daemon that FAILS the test if any bead is ever created,
// used by the fail-loud cases: an unreadable @file must abort before the create.
func createRejects(t *testing.T, created *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			*created = true
		}
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-x", "dependencies": []any{}})
	}))
	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = oldDaemon; srv.Close() })
	return srv
}

func writeBody(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// ── EXPAND: gb decision create --context ────────────────────────────────

func TestDecisionCreate_ContextAtFileStoresBody(t *testing.T) {
	body := "## Background\n\nSourced from a file, not a dangling @pointer.\n"
	path := writeBody(t, "ctx.md", body)

	var fields map[string]any
	captureFields(t, "gd-dec-ctx", &fields)

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("prompt", "Ship it?")
	_ = cmd.Flags().Set("options", `[{"id":"yes","short":"Ship","artifact_type":"report"}]`)
	_ = cmd.Flags().Set("context", "@"+path)
	_ = cmd.Flags().Set("no-wait", "true")
	t.Cleanup(func() {
		_ = cmd.Flags().Set("prompt", "")
		_ = cmd.Flags().Set("options", "")
		_ = cmd.Flags().Set("context", "")
		_ = cmd.Flags().Set("no-wait", "false")
	})

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	got, _ := fields["context"].(string)
	if got != body {
		t.Errorf("context = %q, want file body %q", got, body)
	}
	if strings.HasPrefix(got, "@") {
		t.Errorf("context is a literal pointer %q — @file expansion did not run", got)
	}
}

func TestDecisionCreate_ContextMissingAtFileFailsLoud(t *testing.T) {
	var created bool
	createRejects(t, &created)

	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("prompt", "Ship it?")
	_ = cmd.Flags().Set("options", `[{"id":"yes","short":"Ship","artifact_type":"report"}]`)
	_ = cmd.Flags().Set("context", "@"+filepath.Join(t.TempDir(), "gone.md"))
	_ = cmd.Flags().Set("no-wait", "true")
	t.Cleanup(func() {
		_ = cmd.Flags().Set("prompt", "")
		_ = cmd.Flags().Set("options", "")
		_ = cmd.Flags().Set("context", "")
		_ = cmd.Flags().Set("no-wait", "false")
	})

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected an error for --context @missing-file, got nil")
	}
	if !strings.Contains(err.Error(), "--context") {
		t.Errorf("error %q should name --context", err.Error())
	}
	if created {
		t.Error("decision must not be created when --context @file fails to read")
	}
}

// ── EXPAND: gb escalation create --context ──────────────────────────────

func TestEscalationCreate_ContextAtFileStoresBody(t *testing.T) {
	body := "Operator: this grants read-only IAM to the audit role for 24h.\n"
	path := writeBody(t, "escctx.md", body)

	var fields map[string]any
	captureFields(t, "gd-esc-ctx", &fields)

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("title", "Grant audit IAM")
	_ = cmd.Flags().Set("operation-type", "aws_iam")
	_ = cmd.Flags().Set("context", "@"+path)
	t.Cleanup(func() {
		_ = cmd.Flags().Set("title", "")
		_ = cmd.Flags().Set("operation-type", "")
		_ = cmd.Flags().Set("context", "")
	})

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	got, _ := fields["context"].(string)
	if got != body {
		t.Errorf("context = %q, want file body %q", got, body)
	}
	if strings.HasPrefix(got, "@") {
		t.Errorf("context is a literal pointer %q — @file expansion did not run", got)
	}
}

func TestEscalationCreate_ContextMissingAtFileFailsLoud(t *testing.T) {
	var created bool
	createRejects(t, &created)

	cmd := escalationCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("title", "Grant audit IAM")
	_ = cmd.Flags().Set("operation-type", "aws_iam")
	_ = cmd.Flags().Set("context", "@"+filepath.Join(t.TempDir(), "gone.md"))
	t.Cleanup(func() {
		_ = cmd.Flags().Set("title", "")
		_ = cmd.Flags().Set("operation-type", "")
		_ = cmd.Flags().Set("context", "")
	})

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected an error for --context @missing-file, got nil")
	}
	if !strings.Contains(err.Error(), "--context") {
		t.Errorf("error %q should name --context", err.Error())
	}
	if created {
		t.Error("escalation must not be created when --context @file fails to read")
	}
}

// ── EXPAND: gb advice add --description ─────────────────────────────────

func TestAdviceAdd_DescriptionAtFileStoresBody(t *testing.T) {
	body := "# Advice body\n\nThe long-form guidance, drafted in a file.\n"
	path := writeBody(t, "advice.md", body)

	var fields map[string]any
	captureFields(t, "gd-adv-1", &fields)

	cmd := adviceAddCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("description", "@"+path)
	_ = cmd.Flags().Set("project", "gardenio")
	_ = cmd.Flags().Set("role", "crew")  // narrow the target so the bare-project broadcast guard passes
	_ = cmd.Flags().Set("force", "true") // skip the FindSimilar dup check
	t.Cleanup(func() {
		_ = cmd.Flags().Set("description", "")
		_ = cmd.Flags().Set("project", "")
		_ = cmd.Flags().Set("role", "")
		_ = cmd.Flags().Set("force", "false")
	})

	if err := cmd.RunE(cmd, []string{"short advice title"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	got, _ := fields["__description"].(string)
	if got != body {
		t.Errorf("description = %q, want file body %q", got, body)
	}
	if strings.HasPrefix(got, "@") {
		t.Errorf("description is a literal pointer %q — @file expansion did not run", got)
	}
	// The positional <text> title stays literal (not @-expanded).
	if title, _ := fields["__title"].(string); title != "short advice title" {
		t.Errorf("title = %q, want the positional text unchanged", title)
	}
}

func TestAdviceAdd_DescriptionMissingAtFileFailsLoud(t *testing.T) {
	var created bool
	createRejects(t, &created)

	cmd := adviceAddCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("description", "@"+filepath.Join(t.TempDir(), "gone.md"))
	_ = cmd.Flags().Set("project", "gardenio")
	_ = cmd.Flags().Set("force", "true")
	t.Cleanup(func() {
		_ = cmd.Flags().Set("description", "")
		_ = cmd.Flags().Set("project", "")
		_ = cmd.Flags().Set("force", "false")
	})

	err := cmd.RunE(cmd, []string{"short advice title"})
	if err == nil {
		t.Fatal("expected an error for --description @missing-file, got nil")
	}
	if !strings.Contains(err.Error(), "--description") {
		t.Errorf("error %q should name --description", err.Error())
	}
	if created {
		t.Error("advice must not be created when --description @file fails to read")
	}
}

// ── EXPAND: gb mail send --body ─────────────────────────────────────────

func TestMailSend_BodyAtFileStoresBody(t *testing.T) {
	body := "Hey — here is the full write-up you asked for.\n\n(multiple paragraphs)\n"
	path := writeBody(t, "mail.md", body)

	var fields map[string]any
	captureFields(t, "gd-mail-1", &fields)

	cmd := mailSendCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("subject", "Write-up")
	_ = cmd.Flags().Set("body", "@"+path)
	t.Cleanup(func() {
		_ = cmd.Flags().Set("subject", "")
		_ = cmd.Flags().Set("body", "")
	})

	if err := cmd.RunE(cmd, []string{"gardenio-someone"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	// mail stores the body in the bead Description.
	got, _ := fields["__description"].(string)
	if got != body {
		t.Errorf("body(Description) = %q, want file body %q", got, body)
	}
	if strings.HasPrefix(got, "@") {
		t.Errorf("body is a literal pointer %q — @file expansion did not run", got)
	}
}

func TestMailSend_BodyMissingAtFileFailsLoud(t *testing.T) {
	var created bool
	createRejects(t, &created)

	cmd := mailSendCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("subject", "Write-up")
	_ = cmd.Flags().Set("body", "@"+filepath.Join(t.TempDir(), "gone.md"))
	t.Cleanup(func() {
		_ = cmd.Flags().Set("subject", "")
		_ = cmd.Flags().Set("body", "")
	})

	err := cmd.RunE(cmd, []string{"gardenio-someone"})
	if err == nil {
		t.Fatal("expected an error for --body @missing-file, got nil")
	}
	if !strings.Contains(err.Error(), "--body") {
		t.Errorf("error %q should name --body", err.Error())
	}
	if created {
		t.Error("mail must not be created when --body @file fails to read")
	}
}

// ── LITERAL by design: gb squawk ────────────────────────────────────────

// TestSquawk_LeadingAtStoredLiteral pins the deliberate decision that squawk
// does NOT expand @file: a message starting with "@" (a Slack mention, or a
// path-shaped string) is stored verbatim and the command succeeds. This is the
// documented "literal by explicit design" side of the audit contract, NOT the
// accidental middle — if someone later wires resolveTextValue into squawk this
// test fails and forces the decision back into the open.
func TestSquawk_LeadingAtStoredLiteral(t *testing.T) {
	// squawk creates then immediately closes the message bead; a benign daemon
	// that 200s everything lets runSquawk complete. We assert the value that
	// would be stored is the raw arg, i.e. runSquawk never rewrites args[0].
	var fields map[string]any
	captureFields(t, "gd-sq-1", &fields)
	// squawk auto-comments on the claimed bead; disable to keep the daemon
	// interaction minimal and deterministic.
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	oldNoComment := squawkNoComment
	squawkNoComment = true
	t.Cleanup(func() { squawkNoComment = oldNoComment })

	squawkCmd.SetContext(context.Background())
	const msg = "@matt heads up — /tmp/looks/like/a/path deployed"
	if err := runSquawk(squawkCmd, []string{msg}); err != nil {
		t.Fatalf("runSquawk: %v", err)
	}
	got, _ := fields["text"].(string)
	if got != msg {
		t.Errorf("squawk text = %q, want the message stored LITERALLY %q (no @file expansion by design)", got, msg)
	}
}

// ── LITERAL by design: gb dm send --body ────────────────────────────────

// TestDMSend_LeadingAtStoredLiteral pins that `gb dm send --body` stores a
// leading-"@" body verbatim (Slack mention syntax), the documented literal side
// of the audit contract. Long file-sourced prose belongs in `gb mail send`.
func TestDMSend_LeadingAtStoredLiteral(t *testing.T) {
	var fields map[string]any
	captureFields(t, "gd-dm-1", &fields)

	cmd := dmSendCmd
	cmd.SetContext(context.Background())
	const body = "@here quick sync in 5?"
	_ = cmd.Flags().Set("body", body)
	t.Cleanup(func() { _ = cmd.Flags().Set("body", "") })

	if err := cmd.RunE(cmd, []string{"U123"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	got, _ := fields["body"].(string)
	if got != body {
		t.Errorf("dm body = %q, want the body stored LITERALLY %q (no @file expansion by design)", got, body)
	}
}
