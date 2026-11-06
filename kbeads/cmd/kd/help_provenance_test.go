package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestHelpTextHasNoBareProvenanceIDs is the durable regression guard for
// finding F5 of the Opus/Fable-5 tool-description audit (bundle gd-FZZF0RWbw5,
// remediated under task gd-Ec0hDzQbDw). Agent-facing help text — the cobra
// Short/Long/Example an agent reads via `kd <cmd> --help` — must not carry a
// BARE maintainer-provenance parenthetical like "(ka-0000007224)". Those are
// changelog / "why we built it this way" references: noise to an agent reading
// --help (they name nothing the agent should DO and invite a pointless
// `kd show` on an unrelated bug-fix bead). Per Anthropic's Rule 6 that
// provenance belongs in the linked design doc / commit, not the tool surface.
//
// What is ALLOWED, and why the guard only flags the *bare* parenthetical form:
//   - Placeholder ids in worked examples: gd-abc123, gd-def456, gd-scanStep …
//     — these teach the argument shape, not provenance.
//   - QUALIFIED references where a noun tells the agent what the id is and
//     often what to do with it: "(advice gd-…)", "(epic gd-…)", "(bug gd-…)",
//     "see gd-…", "design doc gd-…". A bare "(gd-…)" with no such cue is the
//     shape this guard forbids.
//
// A regex over the source string literals (mirrors the audit's own extractor)
// is deliberately chosen over rendering each command's help: it needs no
// command execution, catches the id in Short/Long/Example uniformly, and is
// the same mechanical check a future author would want CI to run.
func TestHelpTextHasNoBareProvenanceIDs(t *testing.T) {
	bareProv := regexp.MustCompile(`\((?:gd|ka)-[A-Za-z0-9]{6,}\)`)

	placeholders := map[string]bool{
		"gd-abc123": true,
		"gd-def456": true,
		"gd-ghi789": true,
		"gd-xyz789": true,
	}

	var violations []string
	var walk func(prefix string, c *cobra.Command)
	walk = func(prefix string, c *cobra.Command) {
		path := strings.TrimSpace(prefix + " " + c.Name())
		for _, field := range []struct{ name, text string }{
			{"Short", c.Short},
			{"Long", c.Long},
			{"Example", c.Example},
		} {
			for _, m := range bareProv.FindAllString(field.text, -1) {
				id := strings.TrimSuffix(strings.TrimPrefix(m, "("), ")")
				if placeholders[id] {
					continue
				}
				violations = append(violations,
					"`"+path+"` "+field.name+" contains bare provenance "+m)
			}
		}
		for _, sub := range c.Commands() {
			walk(path, sub)
		}
	}
	walk("", rootCmd)

	if len(violations) > 0 {
		t.Errorf("agent-facing help text must not embed bare maintainer-provenance (gd-…) parentheticals "+
			"(finding F5, bundle gd-FZZF0RWbw5) — move the id into the linked doc/commit, or qualify it "+
			"as 'advice/epic/bug/see gd-…' when the agent should act on it:\n  %s",
			strings.Join(violations, "\n  "))
	}
}
