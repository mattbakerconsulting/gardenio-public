// Hermetic tests: drive slack-view.mjs against local fixture pages, no Slack needed.
// Run: node --test tools/slack-view/test/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { parseArgs, resolveTarget, UsageError } from '../slack-view.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const script = path.join(here, '..', 'slack-view.mjs');
const fixtureUrl = (name) => pathToFileURL(path.join(here, 'fixture', name)).href;

function tmp() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'slack-view-'));
  const state = path.join(dir, 'state.json');
  fs.writeFileSync(state, JSON.stringify({ cookies: [], origins: [] }));
  return { dir, state };
}

const run = (args) => spawnSync(process.execPath, [script, ...args], { encoding: 'utf8' });

test('resolveTarget: shorthand, permalink and passthrough forms', () => {
  assert.deepEqual(resolveTarget('T0ABCDEF1/C0123456789'), {
    url: 'https://app.slack.com/client/T0ABCDEF1/C0123456789', thread: false,
  });
  assert.equal(resolveTarget('C0123456789', 'T0ABCDEF1').url, 'https://app.slack.com/client/T0ABCDEF1/C0123456789');
  assert.throws(() => resolveTarget('C0123456789'), UsageError);
  assert.deepEqual(
    resolveTarget('https://ws.slack.com/archives/C0123456789/p1791936599928719?thread_ts=1791936599.928719', 'T0ABCDEF1'),
    { url: 'https://app.slack.com/client/T0ABCDEF1/C0123456789/thread/C0123456789-1791936599.928719', thread: true },
  );
  // No team id: the permalink is opened as given.
  const bare = resolveTarget('https://ws.slack.com/archives/C0123456789/p1791936599928719?thread_ts=1791936599.928719');
  assert.equal(bare.url, 'https://ws.slack.com/archives/C0123456789/p1791936599928719?thread_ts=1791936599.928719');
  assert.equal(bare.thread, true);
  assert.throws(() => resolveTarget('not a target'), UsageError);
  assert.throws(() => resolveTarget('ftp://x/y'), UsageError);
});

test('parseArgs: requires target and storage state, rejects unknown options', () => {
  assert.throws(() => parseArgs(['--storage-state', 's.json'], {}), UsageError);
  assert.throws(() => parseArgs(['T0ABCDEF1/C0123456789'], {}), UsageError);
  assert.throws(() => parseArgs(['--bogus', 'x', 'y'], {}), UsageError);
  const o = parseArgs(['--storage-state=s.json', '--timeout', '500', '--full-page', 'x'], {});
  assert.equal(o.storageState, 's.json');
  assert.equal(o.timeout, 500);
  assert.equal(o.fullPage, true);
  assert.equal(parseArgs(['x'], { SLACK_STORAGE_STATE: 'e.json' }).storageState, 'e.json');
});

test('captures four screenshots and message JSON from a fixture page', () => {
  const { dir, state } = tmp();
  const out = path.join(dir, 'out');
  const r = run(['--storage-state', state, '--out', out, '--settle', '0', fixtureUrl('channel.html')]);
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout.trim(), path.join(out, 'messages.json'));

  for (const f of ['desktop-light', 'desktop-dark', 'mobile-light', 'mobile-dark']) {
    const png = fs.readFileSync(path.join(out, `${f}.png`));
    assert.equal(png.subarray(1, 4).toString(), 'PNG', f);
  }
  const dims = (f) => {
    const b = fs.readFileSync(path.join(out, f));
    return [b.readUInt32BE(16), b.readUInt32BE(20)];
  };
  assert.deepEqual(dims('desktop-light.png'), [1280, 800]);
  assert.deepEqual(dims('mobile-dark.png'), [390, 844]);
  // Dark mode must actually change pixels, or the emulation is not applied.
  assert.notDeepEqual(
    fs.readFileSync(path.join(out, 'desktop-light.png')),
    fs.readFileSync(path.join(out, 'desktop-dark.png')),
  );

  const j = JSON.parse(fs.readFileSync(path.join(out, 'messages.json'), 'utf8'));
  assert.equal(j.screenshots.length, 4);
  assert.deepEqual(j.messages.map((m) => m.author), ['gardenio', 'gardenio', 'Test User']);
  assert.equal(j.messages[0].text, 'Plan is built and its captain is running.');
  assert.equal(j.messages[0].ts, '1791936600.000100');
  assert.deepEqual(j.messages[0].reactions, ['eyes, 2 reactions']);
  assert.equal(j.messages[2].text, 'Looks good on mobile?');
  assert.ok(j.messages.every((m) => m.inViewport));
  assert.match(j.visibleText, /Second line from the same author\./);
  assert.ok(!fs.readdirSync(out).includes('state.json'), 'storageState must not be copied to --out');
});

test('exits 3 on a sign-in page', () => {
  const { dir, state } = tmp();
  const r = run(['--storage-state', state, '--out', path.join(dir, 'o'), '--timeout', '3000', fixtureUrl('signin.html')]);
  assert.equal(r.status, 3, r.stderr);
  assert.match(r.stderr, /not signed in/);
});

test('exits 4 when no messages appear', () => {
  const { dir, state } = tmp();
  const r = run([
    '--storage-state', state, '--out', path.join(dir, 'o'), '--timeout', '1000',
    '--message-selector', '.nothing-here', fixtureUrl('channel.html'),
  ]);
  assert.equal(r.status, 4, r.stderr);
});

test('exits 2 on usage errors and an unreadable storageState', () => {
  const { dir } = tmp();
  assert.equal(run([]).status, 2);
  const r = run(['--storage-state', path.join(dir, 'missing.json'), fixtureUrl('channel.html')]);
  assert.equal(r.status, 2, r.stderr);
});
