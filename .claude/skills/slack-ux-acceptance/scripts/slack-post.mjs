#!/usr/bin/env node
// Post or edit a Block Kit message in the Slack TEST workspace, for the
// slack-ux-acceptance skill. Lints the payload first (lint-blocks.mjs).
//
// Usage:
//   node slack-post.mjs post   <payload.json> [--channel C0…] [--thread-ts TS] [--no-lint]
//   node slack-post.mjs update <payload.json> --ts TS [--channel C0…] [--no-lint]
//   node slack-post.mjs lint   <payload.json>
//
// Environment:
//   SLACK_UX_BOT_TOKEN  xoxb- token of the TEST app (the `bot-token` property of SM
//                       gardenio/prod/slack-e2e/app-credentials). Deliberately not
//                       SLACK_BOT_TOKEN, so the production token is never picked up.
//   SLACK_UX_CHANNEL    default channel (the test channel's C0… id)
//   SLACK_TEAM_ID       if set, auth.test must report this team or nothing is sent
//   SLACK_API_BASE      override for hermetic tests (default https://slack.com/api)
//
// Prints one JSON line: { ok, channel, ts, permalink, lint } on success, or
// { ok:false, error, lint } on failure. Exit 0 ok, 1 Slack/lint error, 2 usage.
// `--no-lint` sends a payload that fails lint anyway, to capture Slack's real
// invalid_blocks answer as U1/U12 evidence.

import fs from 'node:fs';
import { lintPayload } from './lint-blocks.mjs';

const fail = (code, obj) => {
  console.log(JSON.stringify({ ok: false, ...obj }));
  process.exit(code);
};

export function parse(argv) {
  const [cmd, file, ...rest] = argv;
  const opts = { cmd, file, lint: true };
  for (let i = 0; i < rest.length; i++) {
    const a = rest[i];
    if (a === '--no-lint') opts.lint = false;
    else if (['--channel', '--ts', '--thread-ts'].includes(a)) opts[a.slice(2).replace('-t', 'T')] = rest[++i];
    else throw new Error(`unknown argument ${a}`);
  }
  return opts;
}

async function api(base, token, method, body) {
  const res = await fetch(`${base}/${method}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json; charset=utf-8', authorization: `Bearer ${token}` },
    body: JSON.stringify(body),
  });
  return res.json();
}

async function main() {
  let o;
  try {
    o = parse(process.argv.slice(2));
  } catch (e) {
    console.error(e.message);
    process.exit(2);
  }
  if (!['post', 'update', 'lint'].includes(o.cmd) || !o.file) {
    console.error('usage: slack-post.mjs post|update|lint <payload.json> [--channel C] [--ts TS] [--thread-ts TS] [--no-lint]');
    process.exit(2);
  }
  const payload = JSON.parse(fs.readFileSync(o.file, 'utf8'));
  const lint = lintPayload(payload);
  if (o.cmd === 'lint') {
    console.log(JSON.stringify({ ok: lint.ok, lint }));
    process.exit(lint.ok ? 0 : 1);
  }
  if (o.lint && !lint.ok) fail(1, { error: 'payload fails lint; fix it or pass --no-lint to capture Slack\'s answer', lint });

  const token = process.env.SLACK_UX_BOT_TOKEN;
  const channel = o.channel || process.env.SLACK_UX_CHANNEL;
  const base = process.env.SLACK_API_BASE || 'https://slack.com/api';
  if (!token) fail(2, { error: 'SLACK_UX_BOT_TOKEN not set (the TEST app bot token; see SKILL.md prerequisites)' });
  if (!channel) fail(2, { error: 'no channel: pass --channel or set SLACK_UX_CHANNEL' });
  if (o.cmd === 'update' && !o.ts) fail(2, { error: 'update needs --ts' });

  const who = await api(base, token, 'auth.test', {});
  if (!who.ok) fail(1, { error: `auth.test: ${who.error}`, lint });
  if (process.env.SLACK_TEAM_ID && who.team_id !== process.env.SLACK_TEAM_ID) {
    fail(1, { error: `token belongs to team ${who.team_id}, expected ${process.env.SLACK_TEAM_ID}; refusing to post outside the test workspace`, lint });
  }

  const body = { ...payload, channel, unfurl_links: false, unfurl_media: false };
  if (o.cmd === 'post' && o.threadTs) body.thread_ts = o.threadTs;
  if (o.cmd === 'update') body.ts = o.ts;
  const r = await api(base, token, o.cmd === 'post' ? 'chat.postMessage' : 'chat.update', body);
  if (!r.ok) {
    fail(1, { error: r.error, errors: r.response_metadata?.messages, lint });
  }
  const ts = r.ts ?? o.ts;
  const link = await api(base, token, 'chat.getPermalink', { channel: r.channel ?? channel, message_ts: ts });
  console.log(JSON.stringify({ ok: true, channel: r.channel ?? channel, ts, permalink: link.permalink ?? null, lint }));
}

main().catch((e) => fail(1, { error: e.message }));
