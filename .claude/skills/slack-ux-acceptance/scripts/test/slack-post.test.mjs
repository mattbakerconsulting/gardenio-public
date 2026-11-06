// Hermetic tests: no Slack. Run: node --test .claude/skills/slack-ux-acceptance/scripts/test/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { lintPayload } from '../lint-blocks.mjs';

const script = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'slack-post.mjs');
const good = {
  text: 'Agent done: fixed the thing',
  blocks: [
    { type: 'header', text: { type: 'plain_text', text: 'Done: fixed the thing' } },
    { type: 'actions', elements: [
      { type: 'button', action_id: 'a1', text: { type: 'plain_text', text: 'Approve' }, style: 'primary' },
      { type: 'button', action_id: 'a2', text: { type: 'plain_text', text: 'Reject' }, style: 'danger',
        confirm: { title: { type: 'plain_text', text: 'Reject?' }, text: { type: 'mrkdwn', text: 'Closes it.' },
          confirm: { type: 'plain_text', text: 'Reject' }, deny: { type: 'plain_text', text: 'Cancel' } } },
    ] },
  ],
};

test('lint: a clean card passes', () => {
  assert.deepEqual(lintPayload(good).errors, []);
});

test('lint: catches the rubric section 2 limits', () => {
  const bad = {
    text: '',
    blocks: [
      { type: 'header', text: { type: 'plain_text', text: 'x'.repeat(151) } },
      { type: 'header', text: { type: 'plain_text', text: 'second' } },
      { type: 'section', text: { type: 'mrkdwn', text: 'y'.repeat(3001) } },
      { type: 'image', image_url: 'https://x/y.png' },
      { type: 'actions', elements: [
        { type: 'button', action_id: 'd', text: { type: 'plain_text', text: 'Kill' }, style: 'danger' },
        { type: 'button', action_id: 'd', text: { type: 'plain_text', text: 'Go' }, style: 'primary' },
        { type: 'button', action_id: 'e', text: { type: 'plain_text', text: 'Also' }, style: 'primary' },
      ] },
    ],
  };
  const gates = lintPayload(bad).errors.map((e) => `${e.gate}:${e.where}`);
  for (const want of ['U12:text', 'U1:blocks[0].text', 'U9:blocks', 'U1:blocks[2].text', 'U12:blocks[3]', 'U7:blocks[4].elements[0]', 'U7:blocks[4].elements[1].action_id', 'U7:blocks[4]']) {
    assert.ok(gates.includes(want), `missing ${want} in ${gates}`);
  }
});

test('lint: counts characters, not bytes', () => {
  const p = { text: 't', blocks: [{ type: 'header', text: { type: 'plain_text', text: '🌱'.repeat(150) } }] };
  assert.deepEqual(lintPayload(p).errors, []);
});

function fakeSlack(team) {
  const calls = [];
  const server = http.createServer((req, res) => {
    let b = '';
    req.on('data', (c) => (b += c));
    req.on('end', () => {
      const method = req.url.slice(1);
      const body = JSON.parse(b || '{}');
      calls.push({ method, body, auth: req.headers.authorization });
      const out = {
        'auth.test': { ok: true, team_id: team },
        'chat.postMessage': { ok: true, channel: body.channel, ts: '1791950000.000100' },
        'chat.update': { ok: true, channel: body.channel, ts: body.ts },
        'chat.getPermalink': { ok: true, permalink: `https://t.slack.com/archives/${body.channel}/p1` },
      }[method] ?? { ok: false, error: 'unknown_method' };
      res.end(JSON.stringify(out));
    });
  });
  return new Promise((resolve) => server.listen(0, () => resolve({ server, calls, base: `http://127.0.0.1:${server.address().port}` })));
}

function run(args, env) {
  return new Promise((resolve) => {
    const p = spawn(process.execPath, [script, ...args], { env: { PATH: process.env.PATH, ...env } });
    let out = '';
    p.stdout.on('data', (c) => (out += c));
    p.on('close', (code) => resolve({ code, json: JSON.parse(out.trim().split('\n').pop()) }));
  });
}

test('post then update against a fake Slack API; unfurls off; team guard', async () => {
  const f = await fakeSlack('T0TEST');
  const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'slack-post-')), 'p.json');
  fs.writeFileSync(file, JSON.stringify(good));
  const env = { SLACK_API_BASE: f.base, SLACK_UX_BOT_TOKEN: 'xoxb-test', SLACK_UX_CHANNEL: 'C0TEST', SLACK_TEAM_ID: 'T0TEST' };
  try {
    const posted = await run(['post', file], env);
    assert.equal(posted.code, 0);
    assert.equal(posted.json.ts, '1791950000.000100');
    assert.match(posted.json.permalink, /C0TEST/);
    const post = f.calls.find((c) => c.method === 'chat.postMessage');
    assert.equal(post.body.unfurl_links, false);
    assert.equal(post.auth, 'Bearer xoxb-test');

    const updated = await run(['update', file, '--ts', posted.json.ts], env);
    assert.equal(updated.code, 0);
    assert.equal(f.calls.find((c) => c.method === 'chat.update').body.ts, posted.json.ts);

    const wrongTeam = await run(['post', file], { ...env, SLACK_TEAM_ID: 'T0PROD' });
    assert.equal(wrongTeam.code, 1);
    assert.match(wrongTeam.json.error, /refusing to post outside/);

    const noToken = await run(['post', file], { ...env, SLACK_UX_BOT_TOKEN: '' });
    assert.equal(noToken.code, 2);
  } finally {
    f.server.close();
  }
});

test('post refuses a lint failure unless --no-lint', async () => {
  const f = await fakeSlack('T0TEST');
  const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'slack-post-')), 'p.json');
  fs.writeFileSync(file, JSON.stringify({ blocks: good.blocks }));
  const env = { SLACK_API_BASE: f.base, SLACK_UX_BOT_TOKEN: 'xoxb-test', SLACK_UX_CHANNEL: 'C0TEST' };
  try {
    const r = await run(['post', file], env);
    assert.equal(r.code, 1);
    assert.ok(r.json.lint.errors.some((e) => e.gate === 'U12'));
    assert.equal(f.calls.length, 0);
    const forced = await run(['post', file, '--no-lint'], env);
    assert.equal(forced.code, 0);
  } finally {
    f.server.close();
  }
});
