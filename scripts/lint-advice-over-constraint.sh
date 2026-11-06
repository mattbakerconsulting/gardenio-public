#!/usr/bin/env bash
# lint-advice-over-constraint.sh
#
# The `make lint` gate for the QUALITATIVE advice over-constraint guard
# (task ka-0000004072, P4 of the Opus/Fable-5 context-audit epic ka-0000006604).
#
# It runs the offline frozen-baseline test TestRealCatalogOverConstraintBudget,
# which audits the committed advice-catalog snapshot
# (gardenio/controller/cmd/gb/testdata/advice_catalog_snapshot.json) through the
# exact production checks `gb advice lint --over-constraint` runs:
#
#   over-constraint:rules-over-judgment  no-discretion absolutes that replace
#                                        judgment with a rigid rule
#   over-constraint:directive-density    too many emphatic imperatives
#                                        (MUST/NEVER/DO NOT/…) per bead length
#   over-constraint:redundancy           restates the injected workflow-context
#                                        every agent already reads
#
# The test needs NO beads daemon — it reads the committed snapshot and builds
# the reference "system prompt" from the in-memory EnsureConfigs seed — so it is
# safe to run in the offline lint pipeline. It fails when the count of any
# check's findings climbs past its frozen baseline + headroom, i.e. when a
# re-leak of over-constrained advice framing lands.
#
# To refresh the snapshot (against a live daemon) and re-baseline after a
# DELIBERATE change, see the header of
# gardenio/controller/cmd/gb/advice_over_constraint_budget_test.go and
# real_catalog_budget_test.go (`-update-advice-snapshot`).
#
# Usage:
#   ./scripts/lint-advice-over-constraint.sh
#
# Companion test:     scripts/lint-advice-over-constraint_test.sh
# Companion Go guard: gardenio/controller/cmd/gb/advice_over_constraint_budget_test.go

set -euo pipefail

PROG="$(basename "$0")"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Allow a caller (the test harness) to point at a different module dir.
CONTROLLER_DIR="${CONTROLLER_DIR:-${REPO_ROOT}/gardenio/controller}"

if ! command -v go >/dev/null 2>&1; then
  echo "${PROG}: go toolchain not found on PATH" >&2
  exit 2
fi

echo "${PROG}: auditing committed advice snapshot for over-constraint regressions…"

# GOWORK=off mirrors the golangci-lint invocation in `make lint` so the module
# builds standalone. -count=1 disables the test cache so the guard always
# re-runs against the current snapshot.
#
# Two offline, daemon-free P4 over-constraint guards run together:
#   TestRealCatalogOverConstraintBudget       — the ADVICE corpus (frozen
#                                                snapshot) never over-stacks
#                                                directives / no-discretion
#                                                absolutes / system-prompt echoes.
#   TestGardenerTemplateOverConstraintBudget    — the gardener trigger-prompt
#                                                AUTHORING template (`gb gardener
#                                                start` playbook) — the surface
#                                                the P2 re-audit found became the
#                                                hottest injected surface once
#                                                init_prompts.go was de-constrained
#                                                (finding N1, task ka-0000004887) —
#                                                stays at its 0-directive baseline.
if ! ( cd "$CONTROLLER_DIR" && GOWORK=off go test ./cmd/gb/ \
        -run '^(TestRealCatalogOverConstraintBudget|TestGardenerTemplateOverConstraintBudget)$' -count=1 ); then
  echo "${PROG}: over-constraint budget guard FAILED." >&2
  echo "${PROG}: a bead in the advice corpus (or the gardener authoring template)" >&2
  echo "${PROG}: over-stacks directives, frames a rule as a no-discretion absolute," >&2
  echo "${PROG}: or restates the injected system prompt. Audit advice with" >&2
  echo "${PROG}: 'gb advice lint --over-constraint' and gardener prompts with" >&2
  echo "${PROG}: 'gb lint trigger-prompts --over-constraint', de-constrain the" >&2
  echo "${PROG}: offending surface, or re-baseline in-MR with justification." >&2
  exit 1
fi

echo "${PROG}: over-constraint budget guard passed."
