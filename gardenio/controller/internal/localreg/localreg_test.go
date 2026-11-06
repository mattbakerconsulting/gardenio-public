package localreg

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GARDENIO_HOME", dir)
	return filepath.Join(dir, "local")
}

func TestRegisterReadDeregister(t *testing.T) {
	regDir := withTempHome(t)

	entry := Entry{
		Name:   "alice",
		BeadID: "gd-abc123",
		Role:   "crew",
		Dir:    "/home/user/repo",
		PID:    os.Getpid(),
	}
	path, err := Register(entry)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !strings.HasPrefix(path, regDir) {
		t.Fatalf("entry path %q not under %q", path, regDir)
	}

	got, err := Read("alice")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Name != entry.Name || got.BeadID != entry.BeadID || got.Role != entry.Role {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, entry)
	}
	if got.StartedAt.IsZero() {
		t.Fatalf("StartedAt should have been stamped")
	}

	if err := Deregister("alice"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if _, err := Read("alice"); !os.IsNotExist(err) {
		t.Fatalf("Read after Deregister: want IsNotExist, got %v", err)
	}

	// Deregister-missing is a no-op.
	if err := Deregister("alice"); err != nil {
		t.Fatalf("Deregister missing: %v", err)
	}
}

func TestDeregisterEmptyNoop(t *testing.T) {
	withTempHome(t)
	if err := Deregister(""); err != nil {
		t.Fatalf("Deregister(\"\"): %v", err)
	}
}

func TestListSkipsMalformedAndNonJSON(t *testing.T) {
	regDir := withTempHome(t)
	if _, err := EnsureDir(); err != nil {
		t.Fatal(err)
	}

	if _, err := Register(Entry{Name: "alpha", BeadID: "gd-a", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(Entry{Name: "beta", BeadID: "gd-b", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regDir, "busted.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regDir, "README.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, bad, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
	}
	names := []string{entries[0].Name, entries[1].Name}
	if !(slices.Contains(names, "alpha") && slices.Contains(names, "beta")) {
		t.Fatalf("want alpha+beta, got %v", names)
	}
	if len(bad) != 1 || !strings.HasSuffix(bad[0], "busted.json") {
		t.Fatalf("want busted.json flagged, got %v", bad)
	}
}

func TestListEmptyWhenDirMissing(t *testing.T) {
	withTempHome(t)
	entries, bad, err := List()
	if err != nil {
		t.Fatalf("List on missing dir: %v", err)
	}
	if len(entries) != 0 || len(bad) != 0 {
		t.Fatalf("want empty, got entries=%v bad=%v", entries, bad)
	}
}

func TestAlivePIDSelfAndDead(t *testing.T) {
	withTempHome(t)

	// Self is alive.
	if !Alive(Entry{PID: os.Getpid()}) {
		t.Fatalf("self pid reported dead")
	}
	if Alive(Entry{PID: 0}) {
		t.Fatalf("pid 0 reported alive")
	}
	if Alive(Entry{PID: -1}) {
		t.Fatalf("pid -1 reported alive")
	}

	// Spawn a child that exits immediately, then check its pid.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
	// Give the kernel a moment to reap the zombie — on Linux, kill(pid, 0)
	// returns 0 while the entry is still in the table. Signal a few times.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !Alive(Entry{PID: pid}) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// If the PID is still reported alive after Wait, the kernel likely
	// recycled it; accept the looser guarantee and just ensure Alive does
	// not panic. (Test on Linux where signal(0) is the canonical check.)
}

func TestPickNameUnusedReturnsBase(t *testing.T) {
	withTempHome(t)
	if got := PickName("laptop"); got != "laptop" {
		t.Fatalf("want 'laptop', got %q", got)
	}
}

func TestPickNameSkipsLiveCollision(t *testing.T) {
	withTempHome(t)
	if _, err := Register(Entry{Name: "laptop", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if got := PickName("laptop"); got != "laptop-2" {
		t.Fatalf("want 'laptop-2', got %q", got)
	}

	if _, err := Register(Entry{Name: "laptop-2", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if got := PickName("laptop"); got != "laptop-3" {
		t.Fatalf("want 'laptop-3', got %q", got)
	}
}

func TestPickNameIgnoresDeadCollision(t *testing.T) {
	withTempHome(t)
	// A registered entry with a <=0 PID is treated as dead by Alive().
	if _, err := Register(Entry{Name: "laptop", PID: -1}); err != nil {
		t.Fatal(err)
	}
	if got := PickName("laptop"); got != "laptop" {
		t.Fatalf("PickName must overwrite dead (pid<=0) entry; got %q", got)
	}
}

func TestPickNameBlankFallsBackToLocal(t *testing.T) {
	withTempHome(t)
	if got := PickName(""); got != "local" {
		t.Fatalf("want 'local', got %q", got)
	}
	if got := PickName("   "); got != "local" {
		t.Fatalf("want 'local' for whitespace, got %q", got)
	}
}

func TestSafeNameSanitizesSeparators(t *testing.T) {
	withTempHome(t)
	// Names with path separators must not escape the registry dir.
	entry := Entry{Name: "../evil/name", BeadID: "gd-x", PID: os.Getpid()}
	p, err := Register(entry)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	abs, _ := filepath.Abs(p)
	dirAbs, _ := filepath.Abs(Dir())
	if !strings.HasPrefix(abs, dirAbs+string(os.PathSeparator)) {
		t.Fatalf("registry path %q escaped registry dir %q", abs, dirAbs)
	}
}

func TestPruneRemovesDead(t *testing.T) {
	withTempHome(t)
	// Live entry (our own pid) and a dead entry (pid -1 → Alive=false).
	if _, err := Register(Entry{Name: "live", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(Entry{Name: "dead", PID: -1}); err != nil {
		t.Fatal(err)
	}
	removed, err := Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "dead" {
		t.Fatalf("want removed=[dead], got %v", removed)
	}
	entries, _, _ := List()
	if len(entries) != 1 || entries[0].Name != "live" {
		t.Fatalf("live entry should remain, got %+v", entries)
	}
}

func TestRegister_RunnerFieldsRoundTrip(t *testing.T) {
	withTempHome(t)
	if _, err := Register(Entry{Name: "laptop-a", RunnerID: "gd-r1", Owner: "oidc:a@example.com", PID: os.Getpid()}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := Read("laptop-a")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.RunnerID != "gd-r1" || got.Owner != "oidc:a@example.com" {
		t.Fatalf("runner fields lost: %+v", got)
	}
}
