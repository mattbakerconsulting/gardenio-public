package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// envClaudeCodeTmpDir is Claude Code's override for the directory it uses for
// its cross-session messaging sockets (and screenshot scratch files). It is the
// sanctioned lever for relocating those off $TMPDIR — Claude Code's own error
// text names it: "Point XDG_RUNTIME_DIR or CLAUDE_CODE_TMPDIR at a private
// (0700) directory you own to use a different location."
const envClaudeCodeTmpDir = "CLAUDE_CODE_TMPDIR"

// messagingTmpDirName is the private scratch dir created under $HOME.
//
// Why not /tmp (bug ka-0000002604): agent pods mount VolumeTmp — a plain
// EmptyDir — at /tmp, and the pod SecurityContext sets FSGroup=AgentGID. The
// kubelet therefore chgrps the volume root to gid 1000 and chmods it g+rwxs,
// leaving /tmp as owner 0:1000 mode 2777 (drwxrwsrwx). Kubernetes never sets the
// sticky bit on an EmptyDir root, and the mount masks the image's own 1777 /tmp.
//
// Claude Code walks EVERY ancestor of its messaging socket path and refuses any
// directory that is world- or group-writable without the sticky bit, so /tmp
// fails the vet and cross-session messaging is silently disabled with:
//
//	Cross-session messaging is off: its socket directory could not be set up:
//	'/tmp' is world-writable without the sticky bit (owner 0:1000, mode 2777)
//
// Adding the sticky bit is not available to us: that needs a root init
// container, and agent pods run under PSA "restricted" with RunAsNonRoot=true
// (see podmanager.buildPodSpec). The one root init container in the fleet
// (buildThawStatePermsInit) is explicitly gated to runsc pods in the
// PSA-privileged gVisor namespace.
//
// $HOME is the container's overlay filesystem — NOT one of the FSGroup'd
// EmptyDirs (/tmp, /home/agent/bot, /home/agent/.claude are all 2777) — so
// /home/agent is 0755 and owned by the agent uid, and every ancestor (/, /home)
// is root- or self-owned and not group/world-writable. A 0700 dir there passes.
const messagingTmpDirName = ".claude-tmp"

// prepareMessagingTmpDir creates (and hardens) the private scratch directory
// Claude Code should use for its messaging sockets, returning its path.
//
// Returns ("", false) if the directory cannot be made safe — callers must treat
// this as best-effort and simply not set CLAUDE_CODE_TMPDIR, leaving Claude Code
// on its default path. Failing open costs cross-session messaging; it never
// blocks the agent from launching.
//
// Idempotent: re-run on every coop session start in the same pod.
func prepareMessagingTmpDir(logf func(string, ...any)) (string, bool) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		logf("warning: %s not set: cannot resolve home dir: %v", envClaudeCodeTmpDir, err)
		return "", false
	}

	// The parent must itself pass Claude Code's ancestor vet — a 0700 leaf under
	// a world-writable parent is still refused, which is exactly the /tmp trap
	// this helper exists to avoid. Bail rather than hand back a path that will
	// fail the same check for a less obvious reason.
	if err := vetMessagingDirParent(home); err != nil {
		logf("warning: %s not set: home dir %s unsuitable: %v", envClaudeCodeTmpDir, home, err)
		return "", false
	}

	dir := filepath.Join(home, messagingTmpDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logf("warning: %s not set: mkdir %s: %v", envClaudeCodeTmpDir, dir, err)
		return "", false
	}
	// MkdirAll is a no-op on an existing dir, so re-assert the mode: a dir left
	// by an older image (or a looser umask) would otherwise silently keep perms
	// that fail the vet.
	if err := os.Chmod(dir, 0o700); err != nil {
		logf("warning: %s not set: chmod 0700 %s: %v", envClaudeCodeTmpDir, dir, err)
		return "", false
	}
	return dir, true
}

// vetMessagingDirParent mirrors the directory rule Claude Code applies to every
// ancestor of its socket path: the directory must exist, be a directory, be
// owned by us or root, and must not be group- or world-writable unless the
// sticky bit is set.
func vetMessagingDirParent(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("not a directory")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Non-POSIX stat: cannot verify, so do not vouch for it.
		return fmt.Errorf("cannot stat ownership")
	}
	if uid := os.Getuid(); st.Uid != uint32(uid) && st.Uid != 0 {
		return fmt.Errorf("owned by uid %d, not %d or root", st.Uid, uid)
	}
	mode := fi.Mode()
	if mode&os.ModeSticky == 0 && mode.Perm()&0o022 != 0 {
		return fmt.Errorf("group- or world-writable without the sticky bit (mode %04o)", mode.Perm())
	}
	return nil
}

// messagingTmpDirEnv returns the CLAUDE_CODE_TMPDIR assignment to append to the
// coop child env, or "" when the directory could not be prepared.
//
// Appended AFTER the inherited-env strip in buildCoopEnv/runCoopOnce, matching
// how CLAUDE_CODE_DISABLE_AUTO_MEMORY is layered on: the allowlist governs what
// may be INHERITED from pod env, while values we set ourselves are authoritative.
func messagingTmpDirEnv(logf func(string, ...any)) string {
	dir, ok := prepareMessagingTmpDir(logf)
	if !ok {
		return ""
	}
	return envClaudeCodeTmpDir + "=" + dir
}
