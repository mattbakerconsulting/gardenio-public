package hooks

import (
	"os/exec"
	"strings"
)

// CheckCommitPush runs git status --porcelain in cwd.
// Returns a warning string if there are uncommitted changes, empty string otherwise.
func CheckCommitPush(cwd string) string {
	if cwd == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", cwd, "status", "--porcelain").Output()
	if err != nil {
		return ""
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return "you have uncommitted changes — run git add/commit/push"
	}
	return ""
}
