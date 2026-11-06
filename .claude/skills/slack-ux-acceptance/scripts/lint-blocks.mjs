#!/usr/bin/env node
// Payload lint for Slack Block Kit messages: the rubric's section 2 hard limits
// (gardenio/docs/slack-ux-rubric.md) plus the cheap objective halves of U7, U9
// and U12. It runs before a message is posted, because chat.postMessage and
// chat.update answer a violation with invalid_blocks and a failed update leaves
// the old card in place. slackmock does not enforce these limits.
//
// Usage: node lint-blocks.mjs <payload.json>   ('-' reads stdin)
// <payload.json> is a chat.postMessage body: { text, blocks, attachments[].blocks }.
// A bare array is treated as the blocks array. Prints a JSON report; exit 0 when
// there are no errors, 1 when there are, 2 on usage.

import fs from 'node:fs';
import { pathToFileURL } from 'node:url';

const chars = (s) => [...String(s ?? '')].length; // Slack counts characters, not bytes

export function lintPayload(payload) {
  const errors = [];
  const warnings = [];
  const err = (gate, where, msg) => errors.push({ gate, where, msg });
  const warn = (gate, where, msg) => warnings.push({ gate, where, msg });

  if (Array.isArray(payload)) payload = { blocks: payload };
  const text = payload.text ?? '';
  if (!String(text).trim()) err('U12', 'text', 'empty fallback text (notifications show "This content can\'t be displayed")');
  else if (chars(text) > 300) warn('U12', 'text', `fallback text is ${chars(text)} chars; keep it near 300 or less`);
  if (chars(text) > 40000) err('U1', 'text', 'message text over 40000 chars');

  const lists = [{ name: 'blocks', blocks: payload.blocks ?? [] }];
  (payload.attachments ?? []).forEach((a, i) => lists.push({ name: `attachments[${i}].blocks`, blocks: a.blocks ?? [] }));

  let total = 0;
  for (const { name, blocks } of lists) {
    total += blocks.length;
    const ids = new Set();
    const actionIds = new Set();
    let headers = 0;
    blocks.forEach((b, i) => {
      const at = `${name}[${i}]`;
      if (b.block_id) {
        if (chars(b.block_id) > 255) err('U1', `${at}.block_id`, 'block_id over 255 chars');
        if (ids.has(b.block_id)) err('U7', `${at}.block_id`, `duplicate block_id ${b.block_id}`);
        ids.add(b.block_id);
      }
      switch (b.type) {
        case 'header':
          headers++;
          if (b.text?.type !== 'plain_text') err('U1', `${at}.text`, 'header text must be plain_text');
          if (chars(b.text?.text) > 150) err('U1', `${at}.text`, `header is ${chars(b.text?.text)} chars, limit 150`);
          if (/&(lt|gt|amp);|<[^>]+>|\*\*/.test(b.text?.text ?? '')) err('U1', `${at}.text`, 'markup or entity in header shows literally');
          break;
        case 'section':
          if (chars(b.text?.text) > 3000) err('U1', `${at}.text`, `section text is ${chars(b.text.text)} chars, limit 3000`);
          else if (chars(b.text?.text) > 600) warn('U10', `${at}.text`, `section text is ${chars(b.text.text)} chars; fold past ~600 into a link or canvas`);
          if ((b.fields ?? []).length > 10) err('U1', `${at}.fields`, `${b.fields.length} fields, limit 10`);
          (b.fields ?? []).forEach((f, j) => chars(f.text) > 2000 && err('U1', `${at}.fields[${j}]`, 'field over 2000 chars'));
          if (b.accessory) checkElement(b.accessory, `${at}.accessory`);
          break;
        case 'context':
          if ((b.elements ?? []).length > 10) err('U1', `${at}.elements`, `${b.elements.length} context elements, limit 10`);
          (b.elements ?? []).forEach((e, j) => checkElement(e, `${at}.elements[${j}]`));
          break;
        case 'actions': {
          const els = b.elements ?? [];
          if (els.length > 25) err('U1', `${at}.elements`, `${els.length} elements, limit 25`);
          if (els.length > 5) warn('U3', `${at}.elements`, `${els.length} buttons in one row wrap raggedly at 430px`);
          const primaries = els.filter((e) => e.style === 'primary').length;
          if (primaries > 1) err('U7', at, `${primaries} primary buttons in one actions block, at most 1`);
          els.forEach((e, j) => checkElement(e, `${at}.elements[${j}]`));
          break;
        }
        case 'image':
          if (!b.alt_text) err('U12', at, 'image block without alt_text');
          break;
        default:
          break;
      }
    });
    if (headers > 1) err('U9', name, `${headers} header blocks, expected exactly 1 per card`);
    if (blocks.length > 12) warn('U10', name, `${blocks.length} blocks; a normal card stays near 12 or fewer`);

    function checkElement(e, at) {
      if (e.type === 'image' && !e.alt_text) err('U12', at, 'image element without alt_text');
      if (e.action_id) {
        if (chars(e.action_id) > 255) err('U1', `${at}.action_id`, 'action_id over 255 chars');
        if (actionIds.has(e.action_id)) err('U7', `${at}.action_id`, `duplicate action_id ${e.action_id} (clicks route to the wrong handler)`);
        actionIds.add(e.action_id);
      }
      if (e.type === 'button') {
        const t = e.text?.text ?? '';
        if (chars(t) > 75) err('U1', `${at}.text`, `button label ${chars(t)} chars, limit 75`);
        else if (chars(t) > 20) warn('U3', `${at}.text`, `button label ${chars(t)} chars; over ~20 truncates on a phone`);
        if (chars(e.value) > 2000) err('U1', `${at}.value`, 'button value over 2000 chars');
        if (e.style === 'danger' && !e.confirm) err('U7', at, 'danger button without a confirm dialog');
        if (e.confirm) {
          if (chars(e.confirm.title?.text) > 100) err('U1', `${at}.confirm.title`, 'confirm title over 100 chars');
          if (chars(e.confirm.text?.text) > 300) err('U1', `${at}.confirm.text`, 'confirm text over 300 chars');
          if (chars(e.confirm.confirm?.text) > 30) err('U1', `${at}.confirm.confirm`, 'confirm button over 30 chars');
        }
      }
    }
  }
  if (total > 50) err('U1', 'blocks', `${total} blocks, limit 50`);
  return { ok: errors.length === 0, errors, warnings };
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  const arg = process.argv[2];
  if (!arg) {
    console.error('usage: lint-blocks.mjs <payload.json|->');
    process.exit(2);
  }
  let payload;
  try {
    payload = JSON.parse(fs.readFileSync(arg === '-' ? 0 : arg, 'utf8'));
  } catch (e) {
    console.error(`cannot read payload: ${e.message}`);
    process.exit(2);
  }
  const report = lintPayload(payload);
  console.log(JSON.stringify(report, null, 2));
  process.exit(report.ok ? 0 : 1);
}
