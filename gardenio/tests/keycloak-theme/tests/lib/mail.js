// A tiny SMTP sink so the screens that need an email can be reached: Keycloak
// (the container) sends to host.docker.internal:2525, this process receives,
// and every message is written to KC_MAIL_DIR as <n>-<recipient>.eml. Nothing
// leaves the machine. The test realm points its smtpServer here.
const net = require('net');
const fs = require('fs');
const path = require('path');

function startMailSink(dir, port) {
  let n = 0;
  const server = net.createServer((sock) => {
    let buf = '';
    let rcpt = 'unknown';
    let inData = false;
    let data = '';
    sock.write('220 sink ESMTP\r\n');
    sock.on('data', (chunk) => {
      buf += chunk.toString('latin1');
      for (;;) {
        if (inData) {
          const end = buf.indexOf('\r\n.\r\n');
          if (end < 0) {
            data += buf;
            buf = '';
            // keep a 4-byte tail so a terminator split across chunks is still seen
            if (data.length > 4) {
              buf = data.slice(-4);
              data = data.slice(0, -4);
            }
            return;
          }
          data += buf.slice(0, end);
          buf = buf.slice(end + 5);
          inData = false;
          const file = path.join(dir, `${String(++n).padStart(4, '0')}-${rcpt.replace(/[^a-z0-9@._-]/gi, '_')}.eml`);
          fs.writeFileSync(file, data);
          data = '';
          sock.write('250 queued\r\n');
          continue;
        }
        const eol = buf.indexOf('\r\n');
        if (eol < 0) return;
        const line = buf.slice(0, eol);
        buf = buf.slice(eol + 2);
        const cmd = line.slice(0, 4).toUpperCase();
        if (cmd === 'RCPT') {
          rcpt = (/<([^>]+)>/.exec(line) || [, 'unknown'])[1];
          sock.write('250 ok\r\n');
        } else if (cmd === 'DATA') {
          inData = true;
          sock.write('354 go ahead\r\n');
        } else if (cmd === 'QUIT') {
          sock.end('221 bye\r\n');
          return;
        } else {
          sock.write('250 ok\r\n');
        }
      }
    });
    sock.on('error', () => {});
  });
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, '0.0.0.0', () => resolve(server));
  });
}

// Quoted-printable -> text (enough for the links in Keycloak's mails).
const qp = (s) =>
  s.replace(/=\r?\n/g, '').replace(/=([0-9A-F]{2})/gi, (_, h) => String.fromCharCode(parseInt(h, 16)));

// The newest mail to `recipient`, polled for up to `timeoutMs`. Returns the
// first action-token link in it, or throws.
async function latestActionLink(dir, recipient, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const files = fs
      .readdirSync(dir)
      .filter((f) => f.endsWith(`-${recipient}.eml`))
      .sort();
    if (files.length) {
      const text = qp(fs.readFileSync(path.join(dir, files[files.length - 1]), 'latin1'));
      const m = /https?:\/\/[^\s"<>]+action-token\?[^\s"<>]+/.exec(text);
      if (m) return m[0].replace(/&amp;/g, '&');
    }
    if (Date.now() > deadline) throw new Error(`no mail with an action link for ${recipient} in ${dir}`);
    await new Promise((r) => setTimeout(r, 500));
  }
}

// ---- S1e: read a whole message (subject, text part, html part) ---------------

function decodeBody(raw, headers) {
  const enc = (headers['content-transfer-encoding'] || '7bit').trim().toLowerCase();
  let bytes;
  if (enc === 'base64') bytes = Buffer.from(raw.replace(/\s+/g, ''), 'base64');
  else if (enc === 'quoted-printable') {
    const s = raw.replace(/=\r?\n/g, '');
    const out = [];
    for (let i = 0; i < s.length; i++) {
      if (s[i] === '=' && /^[0-9A-F]{2}$/i.test(s.slice(i + 1, i + 3))) {
        out.push(parseInt(s.slice(i + 1, i + 3), 16));
        i += 2;
      } else out.push(s.charCodeAt(i) & 0xff);
    }
    bytes = Buffer.from(out);
  } else bytes = Buffer.from(raw, 'latin1');
  return bytes.toString('utf8');
}

function parseHeaders(block) {
  const headers = {};
  // Unfold continuation lines, then split name: value.
  for (const line of block.replace(/\r?\n[ \t]+/g, ' ').split(/\r?\n/)) {
    const i = line.indexOf(':');
    if (i > 0) headers[line.slice(0, i).trim().toLowerCase()] = line.slice(i + 1).trim();
  }
  return headers;
}

// Returns { subject, parts: [{ type, text }] } with multipart bodies flattened.
function parseMessage(source) {
  const split = (s) => {
    const m = /\r?\n\r?\n/.exec(s);
    return m ? [s.slice(0, m.index), s.slice(m.index + m[0].length)] : [s, ''];
  };
  const parts = [];
  const walk = (src) => {
    const [head, body] = split(src);
    const headers = parseHeaders(head);
    const type = (headers['content-type'] || 'text/plain').toLowerCase();
    const boundary = /boundary="?([^";\s]+)"?/i.exec(headers['content-type'] || '');
    if (type.startsWith('multipart/') && boundary) {
      for (const chunk of body.split('--' + boundary[1])) {
        const c = chunk.replace(/^\r?\n/, '');
        if (!c.trim() || c.startsWith('--')) continue;
        walk(c);
      }
    } else {
      parts.push({ type: type.split(';')[0].trim(), text: decodeBody(body, headers) });
    }
    return headers;
  };
  const headers = walk(source);
  return { subject: headers.subject || '', parts };
}

// The newest message to `recipient` whose subject matches `subject`, polled for
// up to `timeoutMs`. Returns { subject, text, html }.
async function latestMail(dir, recipient, subject, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const files = fs.readdirSync(dir).filter((f) => f.endsWith(`-${recipient}.eml`)).sort().reverse();
    for (const f of files) {
      const msg = parseMessage(fs.readFileSync(path.join(dir, f), 'latin1'));
      if (subject.test(msg.subject)) {
        return {
          subject: msg.subject,
          text: msg.parts.find((p) => p.type === 'text/plain')?.text,
          html: msg.parts.find((p) => p.type === 'text/html')?.text,
        };
      }
    }
    if (Date.now() > deadline) throw new Error(`no mail to ${recipient} with subject ${subject} in ${dir}`);
    await new Promise((r) => setTimeout(r, 500));
  }
}

module.exports = { startMailSink, latestActionLink, latestMail, parseMessage };
