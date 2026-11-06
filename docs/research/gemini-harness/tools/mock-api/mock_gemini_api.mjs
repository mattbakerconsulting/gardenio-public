#!/usr/bin/env node
// mock_gemini_api.mjs — scripted stand-in for the Gemini generativelanguage API.
//
// Used by the Phase 0 spike (gd-llfzgEJZNN) to observe the *CLI-side* contract of
// the pinned Gemini CLI (event shapes, hooks, session files, resume, approvals)
// without a Gemini API key: point the CLI at it with
//   GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:<port>  GEMINI_API_KEY=mock-key
// It proves nothing about real model ids, real usage numbers or real quota
// errors; those stay marked "needs live key" in spike.md.
//
// Usage: node mock_gemini_api.mjs <port> <request-log.jsonl>
// Behaviour is chosen from the LAST user turn of each request:
//   contains functionResponse     -> text "Tool finished."
//   text has "MOCK:TOOL <cmd>"    -> functionCall run_shell_command {command: <cmd>}
//   text has "MOCK:WRITE <path>"  -> functionCall write_file {file_path, content}
//   text has "MOCK:429"           -> HTTP 429 RESOURCE_EXHAUSTED
//   text has "MOCK:SLOW"          -> streams a chunk, then stalls 60s (interrupt tests)
//   anything else                 -> text "pong"
// Requests that look like internal CLI calls (JSON-schema / classifier) get a
// JSON text reply; every request body is appended to the log.
import http from 'node:http';
import fs from 'node:fs';

const port = Number(process.argv[2] || 18080);
const logPath = process.argv[3] || 'requests.jsonl';

const usage = (p, c) => ({ promptTokenCount: p, candidatesTokenCount: c, totalTokenCount: p + c });

function lastUserParts(body) {
  const contents = body.contents || [];
  for (let i = contents.length - 1; i >= 0; i--) {
    if (contents[i].role === 'user') return contents[i].parts || [];
  }
  return [];
}

function plan(body) {
  const parts = lastUserParts(body);
  if (parts.some((p) => p.functionResponse)) return { text: 'Tool finished.' };
  const text = parts.map((p) => p.text || '').join('\n');
  let m;
  if (text.includes('MOCK:429')) return { status: 429 };
  if (text.includes('MOCK:SLOW')) return { text: 'working...', slow: true };
  if ((m = text.match(/MOCK:TOOL (.+)/))) {
    return { call: { name: 'run_shell_command', args: { command: m[1].trim(), description: 'mock tool call' } } };
  }
  if ((m = text.match(/MOCK:WRITE (\S+)/))) {
    return { call: { name: 'write_file', args: { file_path: m[1], content: 'hello\n' } } };
  }
  return { text: 'pong' };
}

function reply(body) {
  const cfg = body.generationConfig || {};
  const structured = cfg.responseMimeType === 'application/json' || cfg.responseJsonSchema || cfg.responseSchema;
  if (structured) {
    return { parts: [{ text: JSON.stringify({ reasoning: 'mock', complexity_reasoning: 'mock', complexity_score: 10, model_choice: 'flash', next_speaker: 'user', is_stuck: false, confidence: 0.1 }) }], usage: usage(10, 5) };
  }
  const p = plan(body);
  if (p.call) return { parts: [{ functionCall: p.call }], usage: usage(120, 20), p };
  return { parts: [{ text: p.text }], usage: usage(100, 3), p };
}

http
  .createServer((req, res) => {
    let raw = '';
    req.on('data', (d) => (raw += d));
    req.on('end', () => {
      let body = {};
      try { body = JSON.parse(raw || '{}'); } catch { /* keep {} */ }
      const url = new URL(req.url, 'http://x');
      fs.appendFileSync(logPath, JSON.stringify({ t: new Date().toISOString(), method: req.method, path: url.pathname, query: url.search, key: req.headers['x-goog-api-key'] ? '<redacted-present>' : null, ua: req.headers['user-agent'], body }) + '\n');
      const action = url.pathname.split(':').pop();
      if (action === 'countTokens') {
        res.writeHead(200, { 'content-type': 'application/json' });
        return res.end(JSON.stringify({ totalTokens: 42 }));
      }
      if (action !== 'generateContent' && action !== 'streamGenerateContent') {
        res.writeHead(404, { 'content-type': 'application/json' });
        return res.end(JSON.stringify({ error: { code: 404, message: 'mock: unknown ' + url.pathname, status: 'NOT_FOUND' } }));
      }
      const r = reply(body);
      if (r.p && r.p.status === 429) {
        res.writeHead(429, { 'content-type': 'application/json' });
        return res.end(JSON.stringify({ error: { code: 429, message: 'Resource has been exhausted (e.g. check quota).', status: 'RESOURCE_EXHAUSTED' } }));
      }
      const cand = (parts, fin) => ({ candidates: [{ content: { role: 'model', parts }, ...(fin ? { finishReason: 'STOP' } : {}), index: 0 }], ...(fin ? { usageMetadata: r.usage } : {}), modelVersion: 'mock-model' });
      if (action === 'generateContent') {
        res.writeHead(200, { 'content-type': 'application/json' });
        return res.end(JSON.stringify(cand(r.parts, true)));
      }
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      if (r.p && r.p.slow) {
        res.write('data: ' + JSON.stringify(cand(r.parts, false)) + '\n\n');
        return setTimeout(() => res.end(), 60000);
      }
      res.write('data: ' + JSON.stringify(cand(r.parts, true)) + '\n\n');
      res.end();
    });
  })
  .listen(port, '127.0.0.1', () => console.error('mock gemini api on :' + port));
