// Playwright globalSetup for the beads-viewer behavioral suite.
//
// Phase T2 (ka-0000006317): seed deterministic fixtures into the beads
// daemon before any test runs and emit `fixtures/ids.json` so the test
// files can resolve `fixtures.<key>` → `kd-...` ids.
//
// Skip the seed via BEADS_VIEWER_E2E_SKIP_SEED=1 — useful when you have
// already seeded in this shell and want a tight inner loop. The seed
// itself is idempotent against the same daemon (label-based upsert), so
// the skip is purely about cutting ~1s of overhead on each run.

import { spawnSync } from 'node:child_process';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const seedScript = resolve(__dirname, 'seed', 'seed.mjs');

export default function globalSetup() {
  if (process.env.BEADS_VIEWER_E2E_SKIP_SEED === '1') {
    console.log('[globalSetup] BEADS_VIEWER_E2E_SKIP_SEED=1, skipping seed');
    return;
  }
  const r = spawnSync('node', [seedScript], { stdio: 'inherit' });
  if (r.status !== 0) {
    throw new Error(`seed.mjs exited with status ${r.status}`);
  }
}
