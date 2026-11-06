---
description: Show blocked issues
---

Run `kd blocked` to show issues that are blocked by unresolved dependencies.

For each blocked issue, show:
- Issue ID and title
- What it's blocked by (dependency IDs)
- Whether the blocking issues are close to completion

Suggest actions: resolve blocking issues first, or remove dependencies if they're no longer needed with `kd dep remove`.
