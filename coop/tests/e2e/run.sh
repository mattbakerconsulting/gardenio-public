#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
#
# Run Playwright mux E2E tests. Uses bun if available, falls back to npm.

set -euo pipefail
cd "$(dirname "$0")"

if command -v bun >/dev/null 2>&1; then
  bun install --frozen-lockfile
  bunx playwright install chromium
  bunx playwright test "$@"
else
  npm ci
  npx playwright install chromium
  npx playwright test "$@"
fi
