// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

package podmanager

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The in-pod agent's uid/gid. The thaw/refresh scripts hardcode this because it
// is a property of the POD, not of whoever runs the tests.
const (
	agentUID = 1000
	agentGID = 1000
)

// Executable occurrences of the agent uid/gid in the scripts the behavioral
// tests actually EXECUTE. Comments and log strings also say "1000" and are
// deliberately not matched -- rewriting prose would be noise, and the
// structural guards (e.g. chownCallRe) assert against the UNrewritten script.
var (
	// _os.chown(_rp,1000,1000) / os.chown(rp,1000,1000), any spacing.
	runnerChownPyRe = regexp.MustCompile(`(_?os\.chown\(\s*_?rp\s*,\s*)1000(\s*,\s*)1000(\s*\))`)
	// HMNT chown 1000:1000 "$THAWDIR"
	runnerChownShRe = regexp.MustCompile(`(chown )1000:1000`)
	// st.st_uid==1000 / st.st_gid==1000 in the readability and traversal classifiers.
	runnerUIDCmpRe = regexp.MustCompile(`(st\.st_uid\s*==\s*)1000`)
	runnerGIDCmpRe = regexp.MustCompile(`(st\.st_gid\s*==\s*)1000`)
)

// rewriteAgentIDsForRunner retargets a script fragment's hardcoded agent
// uid/gid onto the uid/gid actually running the tests, and is a no-op when they
// already match (the uid-1000 developer/agent lane, where it changes nothing).
//
// WHY. These suites execute real fragments of the prod thaw scripts against
// tmpdir fixtures. Two things in those fragments are hard-bound to uid 1000:
//
//   - `_os.chown(_rp,1000,1000)` — a no-op for a uid-1000 runner that already
//     owns the fixture, but EPERM for any other uid. The script swallows the
//     OSError, which also skips the `_os.chmod(...|0o640)` on the very next
//     line — so the group-read bit the test asserts is silently never applied.
//   - `st.st_uid==1000` / `st.st_gid==1000` — the readability and traversal
//     classifiers. A fixture owned by uid 1001 fails all three POSIX classes
//     (not owner, not group, and 0640 has no other-read), so a perfectly
//     readable token is judged UNREADABLE and the probe fail-closes rc=128.
//
// Both are artifacts of WHO RUNS THE TEST, not of what is being tested. The
// question "can uid N read a file owned N:N at mode M" is isomorphic across N,
// so retargeting preserves each assertion's meaning exactly rather than
// relaxing it: the 0640-passes / 0000-fails contract still has to hold, and a
// missing chmod still reds the bar.
//
// The prod scripts keep their 1000 literals; only the copy handed to the test's
// interpreter is rewritten.
func rewriteAgentIDsForRunner(script string) string {
	return rewriteAgentIDsTo(script, os.Getuid(), os.Getgid())
}

// rewriteAgentIDsTo is the pure form, split out so the substitution is testable
// on a uid-1000 box where rewriteAgentIDsForRunner is by definition a no-op.
func rewriteAgentIDsTo(script string, uid, gid int) string {
	if uid == agentUID && gid == agentGID {
		return script
	}
	u, g := strconv.Itoa(uid), strconv.Itoa(gid)
	script = runnerChownPyRe.ReplaceAllString(script, "${1}"+u+"${2}"+g+"${3}")
	script = runnerChownShRe.ReplaceAllString(script, "${1}"+u+":"+g)
	script = runnerUIDCmpRe.ReplaceAllString(script, "${1}"+u)
	script = runnerGIDCmpRe.ReplaceAllString(script, "${1}"+g)
	return script
}

// TestRewriteAgentIDsTo_HitsEveryExecutableForm pins the substitution against
// the exact spellings that appear in the executed script fragments. On a
// uid-1000 box rewriteAgentIDsForRunner is a no-op, so without this the regexes
// would be unverifiable locally and could silently stop matching after a
// reformat of freezethaw_node.go.
func TestRewriteAgentIDsTo_HitsEveryExecutableForm(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"pyagent cred-overlay chown", `_os.chown(_rp,1000,1000)`, `_os.chown(_rp,1001,1002)`},
		{"token-refresh chown", `os.chown(rp,1000,1000)`, `os.chown(rp,1001,1002)`},
		{"chown spaced", `_os.chown(_rp, 1000, 1000)`, `_os.chown(_rp, 1001, 1002)`},
		{"thawdir shell chown", `HMNT chown 1000:1000 "$THAWDIR"`, `HMNT chown 1001:1002 "$THAWDIR"`},
		{"read classifier uid", `owner_ok=st.st_uid==1000 and (md&stat.S_IRUSR)`, `owner_ok=st.st_uid==1001 and (md&stat.S_IRUSR)`},
		{"read classifier gid", `group_ok=st.st_gid==1000 and (md&stat.S_IRGRP)`, `group_ok=st.st_gid==1002 and (md&stat.S_IRGRP)`},
		{"traversal classifier", `    if st.st_uid==1000: return bool(md&stat.S_IXUSR)`, `    if st.st_uid==1001: return bool(md&stat.S_IXUSR)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteAgentIDsTo(tc.in, 1001, 1002); got != tc.want {
				t.Errorf("rewriteAgentIDsTo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRewriteAgentIDsTo_NoOpAtAgentUID guarantees the uid-1000 lane executes the
// prod script byte-for-byte, so this helper cannot change what that lane tests.
func TestRewriteAgentIDsTo_NoOpAtAgentUID(t *testing.T) {
	src := "_os.chown(_rp,1000,1000)\nst.st_uid==1000\nHMNT chown 1000:1000 \"$THAWDIR\"\n"
	if got := rewriteAgentIDsTo(src, agentUID, agentGID); got != src {
		t.Errorf("must be a byte-identical no-op at uid/gid 1000, got:\n%s", got)
	}
}

// TestRewriteAgentIDsTo_LeavesProseAlone keeps the rewrite off comments and log
// strings: the structural guards assert against the UNrewritten script, and a
// rewritten diagnostic would misreport the prod contract.
func TestRewriteAgentIDsTo_LeavesProseAlone(t *testing.T) {
	prose := `# the restored UID/GID-1000 coop EACCESes; chown to 1000:1000 is a no-op`
	if got := rewriteAgentIDsTo(prose, 1001, 1002); got != prose {
		t.Errorf("prose must be untouched, got: %s", got)
	}
}
