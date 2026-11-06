---
description: List issues with optional filters
argument-hint: [--status=open] [--type=task]
---

List kbeads issues with optional filters.

Common patterns:
- `kd list` — all open issues
- `kd list --status=open` — explicitly open
- `kd list --status=in_progress` — active work
- `kd list --status=closed` — completed
- `kd list -t epic` — only epics
- `kd list -t bug` — only bugs
- `kd list --assignee=<name>` — by assignee

Pass any flags from the arguments directly to `kd list`.
