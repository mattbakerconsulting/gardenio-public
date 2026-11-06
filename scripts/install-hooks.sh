#!/usr/bin/env bash
# Install repo-managed git hooks by pointing core.hooksPath at scripts/hooks.
#
# Run once per clone: `make hooks` (or `./scripts/install-hooks.sh`).
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

chmod +x scripts/hooks/*
git config core.hooksPath scripts/hooks

echo "Installed: core.hooksPath -> scripts/hooks"
echo "Hooks active:"
ls -1 scripts/hooks/ | sed 's/^/  /'
