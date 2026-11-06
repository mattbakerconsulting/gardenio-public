package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// quietLogf discards helper diagnostics so a deliberately-failing case does not
// spam test output.
func quietLogf(string, ...any) {}

func TestPrepareMessagingTmpDir_CreatesPrivateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, ok := prepareMessagingTmpDir(quietLogf)
	if !ok {
		t.Fatalf("prepareMessagingTmpDir failed on a clean 0700 home")
	}
	if want := filepath.Join(home, messagingTmpDirName); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %04o, want 0700", perm)
	}
}

// TestPrepareMessagingTmpDir_HardensExistingLooseDir proves the chmod re-assert
// matters: MkdirAll is a no-op on an existing directory, so a dir left behind
// with loose perms (older image, wider umask) would otherwise keep failing
// Claude Code's vet for a reason no one would think to look for.
func TestPrepareMessagingTmpDir_HardensExistingLooseDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, messagingTmpDirName)
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil { // defeat umask
		t.Fatalf("seed chmod: %v", err)
	}

	if _, ok := prepareMessagingTmpDir(quietLogf); !ok {
		t.Fatalf("prepareMessagingTmpDir failed on an existing dir")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("existing dir left at %04o, want it hardened to 0700", perm)
	}
}

func TestPrepareMessagingTmpDir_Idempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	first, ok := prepareMessagingTmpDir(quietLogf)
	if !ok {
		t.Fatalf("first call failed")
	}
	second, ok := prepareMessagingTmpDir(quietLogf)
	if !ok {
		t.Fatalf("second call failed — helper must be safe to re-run per coop session")
	}
	if first != second {
		t.Errorf("path changed across calls: %q then %q", first, second)
	}
}

// TestPrepareMessagingTmpDir_RefusesUnsafeHome is the regression guard for
// ka-0000002604: a 0700 leaf under a world-writable-without-sticky parent is
// still refused by Claude Code, so the helper must not hand back such a path.
// 2777 is exactly the shape the FSGroup'd /tmp EmptyDir has in agent pods.
func TestPrepareMessagingTmpDir_RefusesUnsafeHome(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{"world-writable (the /tmp trap)", 0o777},
		{"setgid world-writable, as k8s FSGroup leaves it", os.ModeSetgid | 0o777},
		{"group-writable", 0o770},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.Chmod(home, tc.mode); err != nil {
				t.Fatalf("chmod home: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
			t.Setenv("HOME", home)

			if dir, ok := prepareMessagingTmpDir(quietLogf); ok {
				t.Errorf("expected refusal for home mode %v, got %q", tc.mode, dir)
			}
		})
	}
}

// TestPrepareMessagingTmpDir_AcceptsStickyHome mirrors a correctly-configured
// /tmp (1777): world-writable is fine once the sticky bit is set.
func TestPrepareMessagingTmpDir_AcceptsStickyHome(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, os.ModeSticky|0o777); err != nil {
		t.Fatalf("chmod home: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	t.Setenv("HOME", home)

	if _, ok := prepareMessagingTmpDir(quietLogf); !ok {
		t.Errorf("sticky world-writable home should be accepted")
	}
}

func TestPrepareMessagingTmpDir_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	if dir, ok := prepareMessagingTmpDir(quietLogf); ok {
		t.Errorf("expected refusal with no HOME, got %q", dir)
	}
}

func TestVetMessagingDirParent(t *testing.T) {
	base := t.TempDir()

	if err := vetMessagingDirParent(filepath.Join(base, "does-not-exist")); err == nil {
		t.Error("missing dir should not vet")
	}

	file := filepath.Join(base, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := vetMessagingDirParent(file); err == nil {
		t.Error("regular file should not vet as a directory")
	}

	if err := vetMessagingDirParent(base); err != nil {
		t.Errorf("clean temp dir should vet, got: %v", err)
	}
}

func TestMessagingTmpDirEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	kv := messagingTmpDirEnv(quietLogf)
	want := envClaudeCodeTmpDir + "=" + filepath.Join(home, messagingTmpDirName)
	if kv != want {
		t.Errorf("env = %q, want %q", kv, want)
	}

	t.Setenv("HOME", "")
	if kv := messagingTmpDirEnv(quietLogf); kv != "" {
		t.Errorf("unresolvable home should yield no assignment, got %q", kv)
	}
}

// TestBuildCoopEnv_SetsMessagingTmpDir wires the helper through the real env
// builder: the agent child must be pointed at the private dir, not /tmp.
func TestBuildCoopEnv_SetsMessagingTmpDir(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	env := buildCoopEnv(initConfig{role: "gardener"})
	want := envClaudeCodeTmpDir + "=" + filepath.Join(home, messagingTmpDirName)
	if !envContains(env, want) {
		t.Errorf("expected %q in coop env, got: %v", want, env)
	}
}

// TestBuildCoopEnv_MessagingTmpDirNotInheritedFromPod proves the value we set is
// authoritative: CLAUDE_CODE_TMPDIR is deliberately absent from
// claudeCodeEnvAllowlist, so a value leaked in via pod env is stripped and only
// our computed one survives — no duplicate key, no pod-env override.
func TestBuildCoopEnv_MessagingTmpDirNotInheritedFromPod(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(envClaudeCodeTmpDir, "/tmp/leaked-from-pod")

	env := buildCoopEnv(initConfig{role: "gardener"})

	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, envClaudeCodeTmpDir+"=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one %s assignment, got %v", envClaudeCodeTmpDir, got)
	}
	if want := envClaudeCodeTmpDir + "=" + filepath.Join(home, messagingTmpDirName); got[0] != want {
		t.Errorf("%s = %q, want %q (pod-env value must not win)", envClaudeCodeTmpDir, got[0], want)
	}
}
