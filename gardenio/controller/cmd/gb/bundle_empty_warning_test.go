package main

import (
	"context"
	"strings"
	"testing"
)

// TestBundleCreate_WarnsBundleIsEmpty is the gb-side half of ka-0000000755.
//
// `gb bundle create` prints a viewer URL for a bundle that, by construction,
// has zero docs — that page renders "No documents attached to this bundle
// yet". An agent that shares the link at this moment shares an empty page,
// and nothing on any surface says so: the bundle page has no error, and the
// `kd create --type=doc` an agent reaches for next silently auto-parents the
// doc to the agent's task instead of this bundle.
//
// Measured 2026-08-15: a finished blog draft was posted to Slack as a bundle
// link that read as empty to the human who opened it. The URL is the moment
// to say it, because it is the artifact that gets copied.
func TestBundleCreate_WarnsBundleIsEmpty(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Origin", map[string]any{
		"title": "origin task",
		"type":  "task",
		"kind":  "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("title", "Two Impedance Levels")
	_ = cmd.Flags().Set("kind", "report")

	out := string(captureStdout(t, func() error {
		return cmd.RunE(cmd, []string{"gd-Origin"})
	}))

	// It must say the bundle is empty RIGHT NOW — not merely describe how
	// bundles work in general.
	if !strings.Contains(out, "EMPTY") {
		t.Errorf("output should state the bundle is currently empty, got:\n%s", out)
	}
	// It must name the command that actually attaches a doc. A warning with
	// no remedy just re-runs the mistake.
	if !strings.Contains(out, "gb bundle add") {
		t.Errorf("output should route to `gb bundle add`, got:\n%s", out)
	}
	// It must name the specific wrong turn: kd create --type=doc looks like
	// it attaches and does not.
	if !strings.Contains(out, "kd create --type=doc") {
		t.Errorf("output should warn that `kd create --type=doc` does not attach, got:\n%s", out)
	}
	// It must offer the pre-share check.
	if !strings.Contains(out, "gb bundle show") {
		t.Errorf("output should suggest verifying with `gb bundle show`, got:\n%s", out)
	}
	// The bundle id must appear in the suggested commands so they are
	// copy-pasteable rather than a template the reader has to fill in.
	if !strings.Contains(out, "gb bundle add gd-") {
		t.Errorf("suggested commands should carry the concrete bundle id, got:\n%s", out)
	}
}

// TestBundleCreate_JSONOutputStaysClean guards the machine-readable path: the
// advisory text is for humans and must never leak into --json, which callers
// parse.
func TestBundleCreate_JSONOutputStaysClean(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Origin", map[string]any{
		"title": "origin task",
		"type":  "task",
		"kind":  "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	origJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = origJSON })

	cmd := bundleCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("title", "Two Impedance Levels")

	out := string(captureStdout(t, func() error {
		return cmd.RunE(cmd, []string{"gd-Origin"})
	}))

	if strings.Contains(out, "EMPTY") || strings.Contains(out, "gb bundle add") {
		t.Errorf("--json output must stay parseable, got:\n%s", out)
	}
}
