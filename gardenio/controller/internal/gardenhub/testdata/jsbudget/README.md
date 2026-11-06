# web/*.js budget entries

`TestEmbeddedJSBudget` (`../../costs_frames_test.go`) caps all of `web/*.js`, raw and
gzipped at -9. The caps are a base in `jsBudgetCaps` plus the sum of every `*.txt` here
(gd-2ZqzHAxHIt).

When your PR trips the budget and the growth is worth it:

1. Trim first (comments, dead code). The budget is a decision, not a number to bump in passing.
2. Add one file, `<your-bead>.txt`, with one line:

   ```
   +<raw KiB> +<gz KiB> <reason>
   ```

   For example `+4 +2 gd-XXXXXXXXXX relface.js, the release face in the bead view`.
   Whole KiB, sized to **your PR's own growth** rounded up, not to main's headroom.
   A negative entry (`-90 -30 ...`) pays budget back when code leaves.
3. Say in the PR what grew and why.

Never edit the base or another PR's entry to make room. Each PR touches only its own
file, so two budget PRs never conflict, and because entries add, two PRs that each count
their own growth cannot together push main over.

Folding entries into the base is a separate, single PR, when this directory gets long.
