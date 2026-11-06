// Package toollatency implements the P2 HOOK_EVENTS consumer (plan
// gardenio/plans/agent-tool-latency-attribution.md, bead gd-sWwPnwkZtw): a
// durable JetStream consumer that turns every PostToolUse hook event into a
// bounded-cardinality Prometheus sample (the metric plane) and, for calls
// over a configurable threshold, a full-argv redacted row in the
// slow_tool_calls ledger (the record plane).
package toollatency

import (
	"path/filepath"
	"regexp"
	"strings"
)

// ClassCmdOther is the fallback cmd_class for any Bash command whose
// top-level binary is not in cmdClassAllowlist, or that fails to tokenize at
// all. It is DELIBERATELY the single largest bucket — the allowlist exists to
// break out the handful of binaries operators actually care about, not to
// enumerate every binary an agent might invoke.
const ClassCmdOther = "other"

// cmdClassSubcommands maps each allowlisted binary (plan §4.1) to the small,
// fixed set of subcommands that get their own cmd_class value
// ("<bin>_<subcommand>"); any other subcommand of that binary collapses to
// "<bin>_other". This keeps the total cmd_class domain hard-capped — 12
// binaries × (≤3 named subcommands + 1 "_other") + "other" itself — rather
// than growing without bound from argv content (argv must NEVER become a
// metric label).
//
// make has no entry: Makefile target names are project-defined and
// unbounded, so make commands classify as the bare "make" value regardless
// of target.
var cmdClassSubcommands = map[string][]string{
	"git":     {"push", "pull", "commit"},
	"go":      {"build", "test", "run"},
	"kd":      {"create", "show", "close"},
	"gb":      {"ready", "squawk", "decision"},
	"cargo":   {"build", "test", "run"},
	"npm":     {"install", "run", "test"},
	"helm":    {"install", "upgrade", "lint"},
	"kubectl": {"get", "apply", "logs"},
	"aws":     {"s3", "ec2", "sts"},
	"docker":  {"build", "run", "push"},
	"gh":      {"pr", "issue", "release"},
}

// cmdClassBinaries is the full allowlist of binaries (plan §4.1) that get a
// dedicated cmd_class rather than falling into ClassCmdOther. Includes "make"
// (handled specially — see cmdClassSubcommands) alongside the binaries with a
// subcommand breakout.
var cmdClassBinaries = func() map[string]bool {
	m := map[string]bool{"make": true}
	for bin := range cmdClassSubcommands {
		m[bin] = true
	}
	return m
}()

// cdPrefixRe matches one leading "cd <dir> &&" segment (optionally
// semicolon-separated: "cd <dir>;"), so a command like
// "cd /work/repo && make build" or "cd /work/repo; make build" classifies on
// the real command, not on "cd".
var cdPrefixRe = regexp.MustCompile(`^cd\s+\S+\s*(&&|;)\s*`)

// envAssignmentPrefixRe matches one leading "VAR=value" shell assignment
// token (and any trailing whitespace), so "FOO=bar git push" classifies as
// "git_push" rather than failing to find a binary at all.
var envAssignmentPrefixRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*\s*`)

// stripLeadingPrefixes repeatedly strips leading "cd ... &&"/"cd ...;" and
// "VAR=value" prefixes (in any order/repetition — a command can chain
// several, e.g. "cd a && FOO=bar cd b && git push") until neither matches,
// returning what's left to classify.
func stripLeadingPrefixes(command string) string {
	s := strings.TrimSpace(command)
	for {
		if m := cdPrefixRe.FindString(s); m != "" {
			s = strings.TrimSpace(s[len(m):])
			continue
		}
		if m := envAssignmentPrefixRe.FindString(s); m != "" {
			s = strings.TrimSpace(s[len(m):])
			continue
		}
		break
	}
	return s
}

// ClassifyCmd normalizes a Bash command line into a bounded cmd_class value
// per plan §4.1: strip leading "cd ... &&"/env-assignment prefixes, take the
// binary basename, and for the fixed allowlist append the subcommand (itself
// bounded to a small per-binary set, "<bin>_other" otherwise); any
// non-allowlisted binary — or an empty/unparseable command — collapses to
// ClassCmdOther ("other").
func ClassifyCmd(command string) string {
	rest := stripLeadingPrefixes(command)
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ClassCmdOther
	}
	bin := filepath.Base(fields[0])
	if !cmdClassBinaries[bin] {
		return ClassCmdOther
	}
	subs, hasSubcommands := cmdClassSubcommands[bin]
	if !hasSubcommands {
		// make: bounded to the bare binary name, target is unbounded.
		return bin
	}
	if len(fields) < 2 {
		return bin + "_other"
	}
	sub := fields[1]
	for _, s := range subs {
		if s == sub {
			return bin + "_" + sub
		}
	}
	return bin + "_other"
}
