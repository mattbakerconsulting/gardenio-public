package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Codex skills parity (ADR 0004 §E, task gd-akab5xu9QQ).
//
// Claude pods get the primary + sibling repos' .claude/skills entries linked
// into ~/.claude/skills, filtered by the role bead's enabled_skills /
// disabled_skills (agent_k8s_workspace.go, docs/agent-skill-toggle.md). A Codex
// pod gets the same set under $CODEX_HOME/skills, which Codex discovers as user
// scope (verified on codex-cli 0.162.1: `skills/list` finds
// <CODEX_HOME>/skills/<dir>/SKILL.md through a symlinked dir). Codex reads the
// same SKILL.md front matter (name, description) and ignores Claude-only keys
// such as allowed-tools, so the repo's .claude/skills tree is the single source
// and nothing is converted or copied.
//
// The link step IS linkExtensionSubdir, so primary-bare / sibling-prefixed
// naming, primary-wins collisions and the deny-wins filter are identical by
// construction. Agents and slash commands have no Codex counterpart and are not
// linked (ADR §11 keeps them a non-goal).

const codexSkillsDir = "skills"

// symlinkCodexSkills links the project's repo skills into codexHome/skills,
// gated by the role skill filter. Mirrors symlinkClaudeExtensions: a no-op
// without BOAT_PROJECT or a cloned primary repo.
func symlinkCodexSkills(codexHome string, filter extensionFilter) {
	project := os.Getenv("BOAT_PROJECT")
	if project == "" {
		return
	}
	botRoot := filepath.Join("/home/agent/bot", project)
	primary := filepath.Join(botRoot, "work")
	if _, err := os.Stat(primary); os.IsNotExist(err) {
		return
	}
	linkCodexSkills(codexHome, primary, siblingRepoDirs(botRoot), filter)
}

// linkCodexSkills is symlinkCodexSkills with explicit paths, so tests need not
// fake /home/agent/bot.
func linkCodexSkills(codexHome, primary string, siblings []string, filter extensionFilter) {
	pruneRepoSkillLinks(filepath.Join(codexHome, codexSkillsDir))
	linkExtensionSubdir(codexHome, codexSkillsDir, primary, siblings, filter)
}

// pruneRepoSkillLinks removes symlinks into a repo's .claude/skills left by an
// earlier setup. $CODEX_HOME can outlive one pod start, and linkExtensionSubdir
// never overwrites, so without this a skill the role has since disabled would
// stay linked. Real directories (Codex's .system skills, anything installed with
// the skill installer) and symlinks to elsewhere are left alone.
func pruneRepoSkillLinks(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		path := filepath.Join(dir, e.Name())
		target, err := os.Readlink(path)
		if err != nil || !strings.Contains(filepath.ToSlash(target), "/.claude/skills/") {
			continue
		}
		if err := os.Remove(path); err != nil {
			fmt.Printf("[gb agent start] warning: prune stale skill link %s: %v\n", path, err)
		}
	}
}
