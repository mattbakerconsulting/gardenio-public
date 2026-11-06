---
description: Manage dependencies between issues
argument-hint: add|remove <issue> <depends-on>
---

Manage dependencies between kbeads issues.

Commands:
- `kd dep add <issue> <depends-on>` — issue depends on depends-on (depends-on blocks issue)
- `kd dep remove <issue> <depends-on>` — remove dependency
- `kd show <id>` — see current dependencies

If no arguments, ask what dependency operation to perform and which issues to link.
