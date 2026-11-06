package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// flag_hint.go — turn a bare "unknown flag" into a corrective hint (bug
// ka-0000006140).
//
// WHY. Many `gb` subcommands take their subject POSITIONALLY
// (`gb mr status <bead-id-or-mr-url>`) while their siblings take named flags
// (`gb mr list --owner/--spawned-by`). So `--bead <id>` is a reasonable
// affordance guess, and cobra answers it with a bare `unknown flag: --bead`
// plus a flag dump that never mentions the positional contract. The caller
// burns a round-trip discovering a shape the error could just have told them.
//
// This wraps flag-parse failures at the ROOT: cobra's FlagErrorFunc is
// inherited by every descendant (Command.FlagErrorFunc walks to the parent), so
// one hook covers the whole command tree rather than per-leaf patching.
//
// It only ADDS a line to the error text — exit codes, the usage block, and the
// original pflag message are all preserved, so nothing that greps for
// "unknown flag" regresses.

// usagePositionalRe matches a positional placeholder in a cobra Use line, i.e.
// `<bead-id-or-mr-url>` in "status <bead-id-or-mr-url>". Optional-wrapped
// placeholders ("status [<bead-id-or-mr-url>]") match too — the brackets sit
// outside the capture.
var usagePositionalRe = regexp.MustCompile(`<[^<>]+>`)

// positionalHintForCommand returns a one-line corrective hint for a command that
// takes positional arguments, or "" when it declares none or is a container.
//
// The hint is derived from the command's own Use line, so it stays correct as
// commands are added or their placeholders renamed — there is no hand-maintained
// list of positional commands to drift out of sync.
//
// flagDisplay is the flag as the caller typed it ("--bead", "-b"); an empty
// string falls back to a generic subject.
func positionalHintForCommand(cmd *cobra.Command, flagDisplay string) string {
	// Container commands (root `gb <command>`, group `gb mr`) take a SUBCOMMAND,
	// not a positional value — "gb takes --x positionally: gb <command>" would be
	// nonsense, and cobra's usage block already lists the available subcommands.
	if cmd.HasSubCommands() {
		return ""
	}
	placeholders := usagePositionalRe.FindAllString(cmd.Use, -1)
	if len(placeholders) == 0 {
		return ""
	}
	// cmd.Use is "status <bead-id-or-mr-url>"; CommandPath() is "gb mr status".
	// Rebuild the invocation from the path + placeholders so the hint shows the
	// full command the caller should type.
	invocation := cmd.CommandPath() + " " + strings.Join(placeholders, " ")

	flagDesc := "that value"
	if flagDisplay != "" {
		flagDesc = flagDisplay
	}
	return fmt.Sprintf("hint: %s takes %s positionally: %s",
		cmd.CommandPath(), flagDesc, invocation)
}

// flagErrorWithPositionalHint is the root FlagErrorFunc. For an unknown-flag
// error on a command that accepts positional args, it appends a hint naming the
// positional form; every other flag error passes through untouched.
func flagErrorWithPositionalHint(cmd *cobra.Command, err error) error {
	var notExist *pflag.NotExistError
	if !errors.As(err, &notExist) {
		return err
	}
	// Echo the flag the way the caller typed it: `-b` for a shorthand (pflag
	// reports the offending char via the shorthand group), `--bead` for a long
	// flag. Rendering a shorthand as "--b" would suggest a flag that never
	// existed.
	flagDisplay := ""
	if name := notExist.GetSpecifiedName(); name != "" {
		if notExist.GetSpecifiedShortnames() != "" {
			// Decode the first RUNE, not the first byte: slicing name[:1] would
			// split a multibyte shorthand into a broken partial rune.
			r, _ := utf8.DecodeRuneInString(name)
			flagDisplay = "-" + string(r)
		} else {
			flagDisplay = "--" + name
		}
	}
	hint := positionalHintForCommand(cmd, flagDisplay)
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w\n%s", err, hint)
}
