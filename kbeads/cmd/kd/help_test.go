package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// helpBody must lead with the command's Long description (falling back to Short)
// before the usage block. The prior implementation rendered only cmd.Usage(),
// so every command's descriptive help was invisible under `--help` — which
// silently made the children --json schema doc (ka-0000004855 Fix B) dead text.

func TestHelpBody_IncludesLongThenUsage(t *testing.T) {
	cmd := &cobra.Command{
		Use:   "widget <id>",
		Short: "short desc",
		Long:  "LONG DESCRIPTION with a schema table",
	}
	out := helpBody(cmd)
	if !strings.Contains(out, "LONG DESCRIPTION with a schema table") {
		t.Errorf("helpBody must include Long; got:\n%s", out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("helpBody must include the usage block; got:\n%s", out)
	}
	// Long must precede Usage.
	if strings.Index(out, "LONG DESCRIPTION") > strings.Index(out, "Usage:") {
		t.Errorf("Long preamble must come before Usage; got:\n%s", out)
	}
}

func TestHelpBody_FallsBackToShort(t *testing.T) {
	cmd := &cobra.Command{
		Use:   "widget <id>",
		Short: "just a short desc",
	}
	out := helpBody(cmd)
	if !strings.Contains(out, "just a short desc") {
		t.Errorf("helpBody must fall back to Short when Long is empty; got:\n%s", out)
	}
}

func TestHelpBody_NoDescription_UsageOnly(t *testing.T) {
	cmd := &cobra.Command{Use: "widget <id>"}
	out := helpBody(cmd)
	if !strings.Contains(out, "Usage:") {
		t.Errorf("helpBody must still render usage with no description; got:\n%s", out)
	}
	// No stray leading blank lines when there is no preamble.
	if strings.HasPrefix(out, "\n") {
		t.Errorf("helpBody must not start with a blank line when there is no description; got:\n%q", out)
	}
}

// The real children command's Long must actually flow through helpBody — this is
// the end-to-end guard that the schema doc reaches `kd children --help`.
func TestHelpBody_ChildrenCmd_RendersSchemaDoc(t *testing.T) {
	out := helpBody(childrenCmd)
	for _, want := range []string{"dep_type", "kd list --parent", "Usage:"} {
		if !strings.Contains(out, want) {
			t.Errorf("children help must contain %q; got:\n%s", want, out)
		}
	}
}
