---
description: Close a completed issue
argument-hint: [issue-id] [reason]
---

Close a kbeads issue that's been completed.

If arguments are provided:
- $1: Issue ID
- $2+: Completion reason (optional)

If the issue ID is missing, ask for it.

Run: `kd close <id> --reason="<reason>"`

After closing, suggest checking:
- `gb ready` for newly unblocked work
- Creating follow-up issues with `kd create` if needed
