// Unit tests for session-guard.mjs — offline, no browser, no Playwright.
// Run: npm run test:unit   (node --test)
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  checkStorageState,
  assertStorageStateLive,
  loadStorageState,
  probeLiveness,
  main,
  SessionExpiredError,
  SessionConfigError,
  SESSION_EXPIRED_MESSAGE,
} from './session-guard.mjs';

const NOW = Date.parse('2026-10-14T00:00:00Z');
const day = 86_400;
const nowSec = NOW / 1000;
const cookie = (name, expires) => ({ name, value: 'x', domain: '.slack.com', path: '/', expires });
const good = { cookies: [cookie('d', nowSec + 90 * day), cookie('d-s', nowSec + 90 * day)] };

function tmpState(obj) {
  const p = join(mkdtempSync(join(tmpdir(), 'sg-')), 'storage-state.json');
  writeFileSync(p, typeof obj === 'string' ? obj : JSON.stringify(obj));
  return p;
}
const capture = () => {
  const lines = [];
  return { lines, log: (s) => lines.push(s), err: (s) => lines.push(s) };
};

test('message is the exact runbook wording', () => {
  assert.equal(SESSION_EXPIRED_MESSAGE, 'SESSION EXPIRED — refresh per slack-e2e-provisioning.md Part B');
  assert.match(new SessionExpiredError('x').message, /^SESSION EXPIRED — refresh per slack-e2e-provisioning\.md Part B \(x\)$/);
});

test('live session passes with no warnings', () => {
  const r = checkStorageState(good, { now: NOW });
  assert.equal(r.ok, true);
  assert.deepEqual(r.warnings, []);
});

test('missing cookie fails', () => {
  const r = checkStorageState({ cookies: [cookie('d', nowSec + day * 90)] }, { now: NOW });
  assert.equal(r.ok, false);
  assert.match(r.problems[0], /missing session cookie "d-s"/);
});

test('empty / malformed state fails rather than throws', () => {
  for (const s of [{}, { cookies: null }, null, undefined]) {
    assert.equal(checkStorageState(s, { now: NOW }).ok, false);
  }
});

test('expired cookie fails', () => {
  const r = checkStorageState({ cookies: [cookie('d', nowSec - day), cookie('d-s', nowSec + 90 * day)] }, { now: NOW });
  assert.equal(r.ok, false);
  assert.match(r.problems[0], /"d" expired 2026-10-13/);
});

test('session-scoped (expires -1) cookie counts as live', () => {
  assert.equal(checkStorageState({ cookies: [cookie('d', -1), cookie('d-s', -1)] }, { now: NOW }).ok, true);
});

test('any live copy across domains keeps the session live', () => {
  const s = { cookies: [cookie('d', nowSec - day), cookie('d', nowSec + 90 * day), cookie('d-s', nowSec + 90 * day)] };
  assert.equal(checkStorageState(s, { now: NOW }).ok, true);
});

test('near expiry warns but stays ok', () => {
  const r = checkStorageState({ cookies: [cookie('d', nowSec + 3 * day), cookie('d-s', nowSec + 90 * day)] }, { now: NOW });
  assert.equal(r.ok, true);
  assert.equal(r.warnings.length, 1);
  assert.match(r.warnings[0], /"d" expires in 3\.0 days/);
});

test('assertStorageStateLive throws SessionExpiredError', () => {
  assert.throws(() => assertStorageStateLive({ cookies: [] }, { now: NOW }), SessionExpiredError);
});

test('loadStorageState: no path / unreadable are config errors, bad JSON is expiry', () => {
  assert.throws(() => loadStorageState(''), SessionConfigError);
  assert.throws(() => loadStorageState('/nonexistent/ss.json'), SessionConfigError);
  assert.throws(() => loadStorageState(tmpState('{not json')), SessionExpiredError);
});

test('probeLiveness requires a storageState', async () => {
  await assert.rejects(probeLiveness({}), SessionConfigError);
});

test('CLI exit codes and output', async () => {
  let c = capture();
  assert.equal(await main(['--storage-state', tmpState(good)], c), 0);
  assert.match(c.lines.join('\n'), /Slack session live/);

  c = capture();
  assert.equal(await main(['--storage-state', tmpState({ cookies: [] })], c), 2);
  assert.match(c.lines.join('\n'), /SESSION EXPIRED — refresh per slack-e2e-provisioning\.md Part B/);

  c = capture();
  assert.equal(await main(['--storage-state', '/nonexistent.json'], c), 3);

  c = capture();
  assert.equal(await main(['--bogus'], c), 3);

  c = capture();
  assert.equal(await main(['--json', '--storage-state', tmpState({ cookies: [] })], c), 2);
  assert.equal(JSON.parse(c.lines[0]).status, 'expired');
});
