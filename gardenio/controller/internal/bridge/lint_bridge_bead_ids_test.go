package bridge

// Tests for the bridge bead-ID lint guard at
// gardenio/controller/scripts/lint-bridge-bead-ids.sh. The guard forbids
// bare bead-ID interpolation in Slack-bound format strings — patterns like
// fmt.Sprintf("`%s`", bead.ID) — and steers callers to b.beadLink /
// b.beadTitleLink instead. See ka-0000006452 (epic ka-0000003402).
//
// One test runs the script against a deliberately violating fixture under
// testdata/lint_bridge_bead_ids/ and asserts the script flags it. A second
// test runs the script against the live bridge package and asserts no
// regressions slip in.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scriptPath is relative to gardenio/controller/. Tests cd to that dir
// before invoking so the script's default scan path resolves.
const lintScriptPath = "scripts/lint-bridge-bead-ids.sh"

// runLintScript invokes the lint script from gardenio/controller/ with the
// given file args. Returns combined stdout+stderr and the exit error.
func runLintScript(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{lintScriptPath}, args...)...)
	cmd.Dir = filepath.Join("..", "..") // gardenio/controller from internal/bridge
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestLintScriptFlagsViolationFixture asserts the lint guard flags a known
// violating Go file under testdata/. If this test fails, the lint regex
// has regressed and would silently miss real Slack-bound bead-ID leaks.
func TestLintScriptFlagsViolationFixture(t *testing.T) {
	out, err := runLintScript(t, "internal/bridge/testdata/lint_bridge_bead_ids/violation.go")
	if err == nil {
		t.Fatalf("expected lint script to fail on fixture; got nil error.\noutput:\n%s", out)
	}
	if !strings.Contains(out, "violation.go") {
		t.Fatalf("expected fixture filename in output, got:\n%s", out)
	}
	if !strings.Contains(out, "bare bead-ID interpolation") {
		t.Fatalf("expected guard message in output, got:\n%s", out)
	}
}

// TestLintScriptPassesOnBridgePackage asserts the lint guard does not flag
// the current bridge package. This is the regression net for the
// "after mechanical conversion" success criterion in the parent epic.
func TestLintScriptPassesOnBridgePackage(t *testing.T) {
	out, err := runLintScript(t)
	if err != nil {
		t.Fatalf("lint script failed on live bridge package: %v\noutput:\n%s", err, out)
	}
}
