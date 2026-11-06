---
description: Find ready-to-work tasks with no blockers
---

Run `gb ready` to show tasks that are ready to work on (no blocking dependencies).

Present results showing:
- Issue ID
- Title
- Priority
- Issue type

If there are ready tasks, ask the user which one they'd like to work on. If they choose one, run `kd claim <id>` to assign it.

If there are no ready tasks, suggest checking `kd blocked` or creating a new issue with `kd create`.
