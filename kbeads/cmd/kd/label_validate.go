package main

import (
	"fmt"
	"io"
	"strings"
)

// warnOnPipeLabels emits a stderr warning for every value passed to a label
// filter flag (-l/--label, --label-any, --exclude-label) that contains a "|".
//
// Why this exists: kd's label filters match each value LITERALLY. "|" is never
// a valid label character and is NOT an OR operator — the shipped OR mechanism
// is --label-any (ka-0000003440). But nothing validated the VALUE passed to a
// label flag, so the intuitive pipe-OR form `-l "a|b|c"` was treated as one
// literal label, matched nothing, and returned exit 0 with no signal. A
// gardener-release-owner session ran
//
//	kd list -l project:gardenio -l "preflight-broken|release-stuck|..." --status=open
//
// got [] exit 0, and read it as a clean release-gate — a FALSE all-clear that
// could greenlight cutting a tag into a broken pipeline (ka-0000002308). This is
// the same "silent empty on invalid filter input" footgun family as the
// comma-AND label trap (ka-0000003307) and the unknown-type filter
// (ka-0000006244), except the pipe form gives ZERO signal at all.
//
// The warning does NOT abort and does NOT change match semantics — it only
// writes to stderr, so scripts keep their exit codes and result stream. Pipe is
// unambiguous (never valid in a label), so the check cannot false-positive on a
// legitimate label. The suggested fix rewrites the offending value's pipes to
// commas so the reader can copy-paste a working `--label-any` form.
//
// w is the destination (os.Stderr in production; a buffer in tests) so the
// warning is observable without hijacking the process-global os.Stderr.
func warnOnPipeLabels(w io.Writer, flagName string, values []string) {
	for _, v := range values {
		if !strings.Contains(v, "|") {
			continue
		}
		fmt.Fprintf(w,
			"warning: %s value %q contains \"|\" — labels are matched literally, \"|\" is not an OR operator. For OR/any-of use: --label-any %s\n",
			flagName, v, strings.ReplaceAll(v, "|", ","))
	}
}
