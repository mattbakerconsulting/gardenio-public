package harness

import (
	"gardenio/controller/internal/beadsapi"
	"os"
	"path/filepath"
)

// StateDir is the CLI's per-pod state directory: ~/.claude for claude,
// $CODEX_HOME (default ~/.codex) for codex, and $GEMINI_CLI_HOME/.gemini
// (default ~/.gemini) for gemini. claudeStateDir is the already resolved
// PVC-backed Claude dir, returned unchanged for claude.
func StateDir(h, claudeStateDir string) string {
	switch h {
	case beadsapi.HarnessCodex:
		if dir := os.Getenv("CODEX_HOME"); dir != "" {
			return dir
		}
		return filepath.Join(os.Getenv("HOME"), ".codex")
	case beadsapi.HarnessGemini:
		return GeminiHome(os.Getenv("HOME"))
	}
	return claudeStateDir
}
