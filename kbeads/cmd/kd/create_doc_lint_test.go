package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestLintOrphanDoc is the regression for ka-0000000755: `kd create
// --type=doc` with no stated destination silently auto-parents the doc to the
// agent's current task instead of the bundle it was written for, so the
// bundle renders empty with no error on any surface.
func TestLintOrphanDoc(t *testing.T) {
	tests := []struct {
		name           string
		beadType       string
		parentExplicit bool
		noParent       bool
		wantWarn       bool
	}{
		{
			name:     "doc with no stated destination warns",
			beadType: "doc",
			wantWarn: true,
		},
		{
			name:           "explicit --parent-task is the author choosing; stays quiet",
			beadType:       "doc",
			parentExplicit: true,
		},
		{
			name:     "--no-parent is a deliberate standalone doc; stays quiet",
			beadType: "doc",
			noParent: true,
		},
		{
			name:     "non-doc types are unaffected",
			beadType: "task",
		},
		{
			name:     "bundle parent bead itself is unaffected",
			beadType: "bundle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			n := lintOrphanDoc(&buf, tt.beadType, tt.parentExplicit, tt.noParent)
			if tt.wantWarn && n != 1 {
				t.Fatalf("expected 1 warning, got %d (output %q)", n, buf.String())
			}
			if !tt.wantWarn {
				if n != 0 {
					t.Fatalf("expected no warning, got %d", n)
				}
				if buf.Len() != 0 {
					t.Fatalf("expected no output, got %q", buf.String())
				}
			}
		})
	}
}

// TestLintOrphanDoc_WarningRoutesToBundleAdd asserts the warning names the
// command that actually works. A warning that only says "this may be wrong"
// re-runs the same mistake; the fix is only discoverable if `gb bundle add`
// is in the text (ka-0000000755).
func TestLintOrphanDoc_WarningRoutesToBundleAdd(t *testing.T) {
	var buf bytes.Buffer
	if n := lintOrphanDoc(&buf, "doc", false, false); n != 1 {
		t.Fatalf("expected the warning to fire, got %d", n)
	}
	msg := buf.String()
	for _, want := range []string{
		"gb bundle add", // the command that creates + links + closes
		"--parent-task", // the in-place escape hatch
		"--no-parent",   // how to silence it deliberately
		"CURRENT TASK",  // names what actually goes wrong
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning should mention %q, got:\n%s", want, msg)
		}
	}
}
