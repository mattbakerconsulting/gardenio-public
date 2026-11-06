package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"gardenio/kbeads/internal/ui"
	"github.com/spf13/cobra"
)

// Patterns used to colorize Cobra's default help output.
var (
	// Section headers: unindented line ending with ":" (e.g. "Beads:", "Flags:").
	// Excludes "Usage:" which we leave unstyled.
	reGroupHeader = regexp.MustCompile(`(?m)^([A-Z][^\n]*:)\s*$`)

	// Command names: two-space indent, then a word, then two-or-more spaces
	// before the description.
	reCommand = regexp.MustCompile(`(?m)^(  )(\S+)(  )`)

	// Flag type annotations: e.g. "--server string", "--limit int32".
	reFlagType = regexp.MustCompile(`(--?\S+\s+)(string|int|int32|duration|stringSlice|stringArray)`)

	// Default values in square brackets that look like defaults, e.g.
	// (default "foo"). Only match brackets containing "default" or
	// starting with a quote to avoid matching [command], [flags], etc.
	reDefault = regexp.MustCompile(`\(default "[^"]*"\)`)
)

// colorizedHelpFunc returns a Cobra help function that post-processes the
// default help text with ANSI colors when the terminal supports it.
func colorizedHelpFunc() func(*cobra.Command, []string) {
	return func(cmd *cobra.Command, args []string) {
		out := helpBody(cmd)
		if ui.ShouldUseColor() {
			out = colorizeHelpOutput(out)
		}
		fmt.Fprint(cmd.OutOrStdout(), out)
	}
}

// helpBody renders a command's full help text: the Long (or Short) description
// preamble followed by the usage string. This mirrors Cobra's default
// HelpTemplate — `{{or .Long .Short}}` then the usage — which the previous
// implementation dropped by calling only cmd.Usage(), so no command's
// descriptive Long/Short help ever surfaced under `--help` (ka-0000004855 Fix B
// depends on the children schema doc actually rendering).
func helpBody(cmd *cobra.Command) string {
	var b bytes.Buffer
	if long := strings.TrimRight(cmd.Long, " \t\n"); strings.TrimSpace(long) != "" {
		b.WriteString(long)
		b.WriteString("\n\n")
	} else if short := strings.TrimSpace(cmd.Short); short != "" {
		b.WriteString(short)
		b.WriteString("\n\n")
	}
	b.WriteString(cmd.UsageString())
	return b.String()
}

// colorizeHelpOutput applies ANSI styling to Cobra's plain-text help.
func colorizeHelpOutput(s string) string {
	// 1. Color section headers.
	s = reGroupHeader.ReplaceAllStringFunc(s, func(match string) string {
		return ui.RenderAccent(strings.TrimSpace(match))
	})

	// 2. Color command names.
	s = reCommand.ReplaceAllStringFunc(s, func(match string) string {
		parts := reCommand.FindStringSubmatch(match)
		if len(parts) == 4 {
			return parts[1] + ui.RenderCommand(parts[2]) + parts[3]
		}
		return match
	})

	// 3. Color flag type annotations.
	s = reFlagType.ReplaceAllStringFunc(s, func(match string) string {
		parts := reFlagType.FindStringSubmatch(match)
		if len(parts) == 3 {
			return parts[1] + ui.RenderMuted(parts[2])
		}
		return match
	})

	// 4. Color default values.
	s = reDefault.ReplaceAllStringFunc(s, func(match string) string {
		return ui.RenderMuted(match)
	})

	return s
}
