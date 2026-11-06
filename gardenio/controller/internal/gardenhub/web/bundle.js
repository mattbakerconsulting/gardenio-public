/* web/bundle.js: the bundle panel (Lands workbench-glass spec WB-9, step 5.10, gd-UFvVkR7Mwx), the Workbench's bundle kind,
   /garden?bundle=<id>. It reads GET /garden/bundle/<id> (bundle.go: the viewer's own bundle page's documents, raw, in their
   order, the pull request that produced them and the bead the bundle was filed against) and shows the documents as one,
   each by its format, built node by node (never HTML from the document):
   - md (and a report's markdown): the report face's reader (BEAD.markdown and mdInto), its screenshots through the viewer's
     image proxy;
   - image (a screenshot): the picture at the panel's width when the proxy serves it, else a link to it;
   - diff: its lines, each marked added, removed or a hunk's header in a word as well as a colour;
   - csv: a table (the first row its header), up to CSV_ROWS rows; code: its text, in a block that scrolls;
   - link: a link out; html: the page in a frame that runs no script and reaches nothing of this page;
   - any other format: its text.
   On a desktop the panel reads wide and the documents' headlines stand in a contents rail beside them (each a button that
   scrolls to its document, so the address keeps no #fragment); on a phone they read one after another. Opened from a
   bundle's bead face ("Read as one document") and the viewer's retired /bundle/<id> page
   (R6). The bundle bead's own moves refresh it (live.go; a document added or removed is a dependency write, which moves
   both ends). Text goes in through textContent. */
var BUNDLE = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  function url(id) { return '/garden/bundle/' + encodeURIComponent(id); }
  var CSV_ROWS = 500;   // rows drawn as a table; the rest are counted
  var FORMATS = { md: 'md', markdown: 'md', report: 'md', diff: 'diff', patch: 'diff', csv: 'csv', code: 'code', image: 'image', screenshot: 'image', png: 'image', link: 'link', url: 'link', html: 'html' };
  /* How a document reads: one of md, diff, csv, code, image, link, html, or '' for its text as it is. */
  function kindOf(p) { return FORMATS[String((p && p.format) || '').trim().toLowerCase()] || ''; }
  var WORDS = { md: 'document', diff: 'diff', csv: 'table', code: 'code', image: 'screenshot', link: 'link', html: 'web page', '': 'text' };
  /* The word under a document's headline for what it is. */
  function formatWord(p) {
    var k = kindOf(p), w = WORDS[k];
    if ((k === 'code' || k === 'diff') && p.language) w += ' · ' + p.language;
    return w;
  }
  /* A diff's lines, each {k, s}: k is file (a file's header: diff --git, index, ---, +++), hunk (@@), add, del or ctx. */
  function diffLines(text) {
    var inHunk = false;
    return String(text || '').replace(/\r\n?/g, '\n').replace(/\n$/, '').split('\n').map(function (s) {
      if (/^diff --git /.test(s)) { inHunk = false; return { k: 'file', s: s }; }
      if (/^@@/.test(s)) { inHunk = true; return { k: 'hunk', s: s }; }
      if (!inHunk && /^(index |--- |\+\+\+ |new file|deleted file|similarity |rename |old mode|new mode|Binary files)/.test(s)) return { k: 'file', s: s };
      var c = s.charAt(0);
      return { k: c === '+' ? 'add' : c === '-' ? 'del' : 'ctx', s: s };
    });
  }
  /* A diff's counts: {files, add, del}; its files by their diff --git lines, else by their +++ ones (a plain unified diff). */
  function diffCounts(lines) {
    var n = { files: 0, add: 0, del: 0 }, git = 0, plus = 0;
    lines.forEach(function (l) {
      if (l.k === 'add') n.add++;
      else if (l.k === 'del') n.del++;
      else if (l.k === 'file') { if (/^diff --git /.test(l.s)) git++; else if (/^\+\+\+ /.test(l.s)) plus++; }
    });
    n.files = git || plus;
    return n;
  }
  /* A csv's rows (RFC 4180: quoted fields, "" an escaped quote, line breaks inside quotes), ragged rows as they are, or null
     when a quote never closes. */
  function csvRows(text) {
    var s = String(text || '').replace(/\r\n?/g, '\n'), rows = [], row = [], f = '', q = false, i = 0;
    if (!s.trim()) return [];
    for (; i < s.length; i++) {
      var c = s.charAt(i);
      if (q) {
        if (c === '"' && s.charAt(i + 1) === '"') { f += '"'; i++; }
        else if (c === '"') q = false;
        else f += c;
      } else if (c === '"' && f === '') q = true;
      else if (c === ',') { row.push(f); f = ''; }
      else if (c === '\n') { row.push(f); rows.push(row); row = []; f = ''; }
      else f += c;
    }
    if (q) return null;
    if (f !== '' || row.length) { row.push(f); rows.push(row); }
    return rows;
  }
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }
  /* The line under the title: how many documents, the pull request's state, the bundle's kind. */
  function summary(d) {
    var docs = (d.docs || []).length + (d.more || 0), out = [plural(docs, 'document', 'documents')];
    if (d.kind) out.push(String(d.kind).replace(/_/g, ' '));
    if (d.status === 'closed') out.push('closed');
    return out.join(' · ');
  }
  /* A pull request's words: !12 · merged, its title in the tooltip. */
  function mrWord(m) {
    if (!m) return '';
    var n = m.number ? (/^[!#]/.test(m.number) ? m.number : '!' + m.number) : 'the pull request';
    return n + (m.state ? ' · ' + String(m.state).replace(/_/g, ' ') : '');
  }
  var api = { url: url, kindOf: kindOf, formatWord: formatWord, diffLines: diffLines, diffCounts: diffCounts, csvRows: csvRows, summary: summary, mrWord: mrWord, CSV_ROWS: CSV_ROWS };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined' || typeof BEAD === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = B.dataset.base || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  var lastAt = 0, titles = {};   // titles: the bundle's mentions, for the bead ids its documents name (bundle.go)

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function href(id) { return base + '/garden?bead=' + encodeURIComponent(id); }
  /* A bead named by its title (TITLES learns it), and one named by what it is to this bundle (its bead, the bundle bead). */
  function beadLink(id, title, cls) {
    if (typeof TITLES !== 'undefined') return TITLES.link(id, { cls: cls || 'bl', text: title && title !== id ? title : '' });
    return labelLink(id, title || id, cls);
  }
  function labelLink(id, label, cls) { var a = el('a', cls || 'bl', label); a.href = href(id); a.title = id; return a; }
  function outLink(u, label, cls) {
    var to = BEAD.safeHref(u || '');
    if (!to) return el('span', cls || '', label || u || '');
    var a = el('a', cls || 'bl bv-url', label || u); a.href = to; a.title = u; a.target = '_blank'; a.rel = 'noopener noreferrer';
    return a;
  }
  function retryButton(id) { var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button'; b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; W.reload('bundle', id); }; return b; }

  /* ---------- one document's body, by its format ---------- */
  /* A block of text in the markdown's pre, its font the page's own (the <code> inside never the browser's monospace). */
  function pre(cls, text) { var b = el('div', 'bv-md'), p = el('pre', 'bv-md-pre ' + cls); p.tabIndex = 0; p.appendChild(el('code', '', text)); b.appendChild(p); return b; }
  function mdBody(p) {
    var blocks = BEAD.markdown(p.content);
    if (blocks.length && blocks[0].t === 'h' && BEAD.mdPlain(blocks[0].c).trim() === String(p.title || '').trim()) blocks.shift();   // the headline says it already
    return BEAD.mdInto(el('div', 'bv-md'), blocks, titles);
  }
  function imageBody(p) {
    var u = p.url || p.content || '', src = BEAD.mdPicSrc(base, u);
    if (!src) {
      var box = el('p', 'bd-out');
      box.appendChild(document.createTextNode('This screenshot is not served by the garden: '));
      box.appendChild(outLink(u, 'open it where it is'));
      return box;
    }
    var f = el('figure', 'bv-md-fig'), a = el('a', 'bv-md-pic'), img = el('img');
    a.href = src; a.target = '_blank'; a.rel = 'noopener noreferrer'; a.title = 'Open the screenshot whole';
    img.src = src; img.alt = p.title || 'screenshot'; img.loading = 'lazy'; img.decoding = 'async'; img.referrerPolicy = 'no-referrer';
    img.onerror = function () { a.textContent = 'The screenshot did not load' + (p.title ? ': ' + p.title : ''); a.className = 'bv-md-pic gone'; };
    a.appendChild(img); f.appendChild(a);
    return f;
  }
  var DIFF_SAY = { add: 'added', del: 'removed' };
  function diffBody(p) {
    var lines = diffLines(p.content), n = diffCounts(lines), box = el('div', 'bv-md bd-diff');
    var c = [];
    if (n.files) c.push(plural(n.files, 'file', 'files'));
    c.push(n.add + ' added', n.del + ' removed');
    box.appendChild(el('p', 'bd-sub', c.join(' · ')));
    var code = el('code');
    lines.forEach(function (l) {
      var s = el('span', 'bd-l bd-' + l.k);
      if (DIFF_SAY[l.k]) s.setAttribute('aria-label', DIFF_SAY[l.k] + ': ' + l.s.slice(1));
      s.textContent = l.s;
      code.appendChild(s);
      code.appendChild(document.createTextNode('\n'));
    });
    var pr = el('pre', 'bv-md-pre bd-diff-pre'); pr.tabIndex = 0; pr.setAttribute('aria-label', 'Diff'); pr.appendChild(code);
    box.appendChild(pr);
    return box;
  }
  function csvBody(p) {
    var rows = csvRows(p.content);
    if (!rows || !rows.length) return pre('bd-text', p.content || '');
    var t = el('table'), tr = el('tr'), body = el('tbody'), head = rows[0], width = rows.reduce(function (m, r) { return Math.max(m, r.length); }, 0);
    for (var k = 0; k < width; k++) { var th = el('th', '', head[k] || ''); th.scope = 'col'; tr.appendChild(th); }
    t.appendChild(el('thead')).appendChild(tr);
    rows.slice(1, CSV_ROWS + 1).forEach(function (r) { var row = el('tr'); for (var j = 0; j < width; j++) row.appendChild(el('td', '', r[j] || '')); body.appendChild(row); });
    t.appendChild(body);
    var box = el('div', 'bv-md'), wrap = el('div', 'bv-md-table');
    wrap.tabIndex = 0; wrap.setAttribute('role', 'region'); wrap.setAttribute('aria-label', 'Table: ' + (p.title || 'data')); wrap.appendChild(t);
    box.appendChild(wrap);
    if (rows.length - 1 > CSV_ROWS) box.appendChild(el('p', 'bv-note', 'The first ' + CSV_ROWS + ' of ' + (rows.length - 1) + ' rows. Its bead has them all.'));
    return box;
  }
  function linkBody(p) {
    var box = el('p', 'bd-out');
    if (!p.url) { box.textContent = 'This link names no address.'; return box; }
    box.appendChild(outLink(p.url, p.title || p.url, 'bl bv-url bd-link'));
    if (p.title) box.appendChild(el('span', 'bd-sub bd-url', ' ' + p.url));
    return box;
  }
  /* An html document in a frame with no scripts, no forms and no access to this page; its links open a new tab. */
  function htmlBody(p) {
    var f = el('iframe', 'bd-frame');
    f.setAttribute('sandbox', 'allow-popups allow-popups-to-escape-sandbox');
    f.setAttribute('referrerpolicy', 'no-referrer');
    f.setAttribute('loading', 'lazy');
    f.title = p.title || 'Web page';
    f.srcdoc = '<base target="_blank">' + (p.content || '');
    return f;
  }
  function body(p) {
    var k = kindOf(p);
    if (k === 'image') return imageBody(p);
    if (k === 'link') return linkBody(p);
    if (!p.content) {
      var e = el('p', 'bv-empty');
      if (!p.url) e.textContent = 'This document is empty.';
      else { e.appendChild(document.createTextNode('Its content is at ')); e.appendChild(outLink(p.url, p.url)); }
      return e;
    }
    if (k === 'md') return mdBody(p);
    if (k === 'diff') return diffBody(p);
    if (k === 'csv') return csvBody(p);
    if (k === 'html') return htmlBody(p);
    return pre(k === 'code' ? 'bd-code' : 'bd-text', p.content);
  }
  function docEl(p, i) {
    var a = el('article', 'bd-doc');
    a.id = 'bd-' + i; a.tabIndex = -1; a.dataset.id = p.id; a.dataset.format = kindOf(p) || 'text';
    var h = el('h3', 'bd-h', p.title || p.id);
    a.appendChild(h);
    var sub = el('p', 'bd-sub');
    sub.appendChild(document.createTextNode(formatWord(p) + ' · '));
    sub.appendChild(labelLink(p.id, 'its bead', 'bl bd-bead'));
    a.appendChild(sub);
    a.appendChild(body(p));
    if (p.cut) { var n = el('p', 'bv-note', 'Only the start is shown: the rest is too long for the panel. '); n.appendChild(labelLink(p.id, 'Its bead has it all.')); a.appendChild(n); }
    return a;
  }
  /* The contents rail (desktop): each headline a button that brings its document to the top of the panel and focuses it.
     It follows the reader: the document read is aria-current, its entry kept in the rail's view. That is the first still 48px
     under the body's top, or the last at the body's end; a click's stands while the body stays where the click put it. */
  var rail;
  function mark(i) {
    var b = rail.querySelectorAll('.bd-toc-b'), r, n;
    if (i === rail.cur || !b[i]) return;
    if (b[rail.cur]) b[rail.cur].removeAttribute('aria-current');
    b[rail.cur = i].setAttribute('aria-current', 'location');
    r = b[i].parentNode.getBoundingClientRect(); n = rail.getBoundingClientRect();
    rail.scrollTop += r.top < n.top ? r.top - n.top : r.bottom > n.bottom ? r.bottom - n.bottom : 0;
  }
  function follow() {
    if (!rail || !rail.isConnected || !rail.offsetHeight || rail.hold === bodyEl.scrollTop) return;
    var y = bodyEl.getBoundingClientRect().top + 48, i = 0, d;
    while ((d = document.getElementById('bd-' + i)) && d.getBoundingClientRect().bottom <= y) i++;
    mark(bodyEl.scrollTop && bodyEl.scrollTop + bodyEl.clientHeight >= bodyEl.scrollHeight - 2 ? rail.querySelectorAll('li').length - 1 : i);
  }
  bodyEl.addEventListener('scroll', follow, { passive: true });
  function toc(docs) {
    var nav = rail = el('nav', 'bd-toc'), ol = el('ol');
    nav.setAttribute('aria-label', 'Contents');
    nav.appendChild(el('h3', '', 'Contents'));
    docs.forEach(function (p, i) {
      var li = el('li'), b = el('button', 'bd-toc-b', p.title || p.id);
      b.type = 'button';
      b.onclick = function () {
        var t = document.getElementById('bd-' + i);
        if (!t) return;
        bodyEl.scrollTop += t.getBoundingClientRect().top - bodyEl.getBoundingClientRect().top - 8;
        try { t.focus({ preventScroll: true }); } catch (e) { t.focus(); }
        nav.hold = bodyEl.scrollTop; mark(i);
      };
      li.appendChild(b); li.appendChild(el('span', 'bd-sub', formatWord(p)));
      ol.appendChild(li);
    });
    nav.appendChild(ol);
    return nav;
  }

  function paint(id, d, note, isErr, retry) {
    kindEl.textContent = 'bundle';
    titleEl.textContent = (d && d.title) || (typeof TITLES !== 'undefined' && TITLES.title(id)) || 'This bundle';   // gd-dv5wiNnKuh
    bodyEl.textContent = '';
    if (d) lastAt = Math.floor(Date.now() / 1000);
    titles = d && BEAD.titles ? BEAD.titles(d) : {};
    if (note) {
      var n = el('p', 'bv-note' + (isErr ? ' err' : ''), note);
      n.setAttribute('role', 'status');
      if (isErr && retry) { n.appendChild(document.createTextNode(' ')); n.appendChild(retryButton(id)); }
      bodyEl.appendChild(n);
    }
    if (!d) return;
    var meta = el('div', 'bv-meta bd-meta');
    meta.appendChild(el('span', '', summary(d)));
    if (d.parent && d.parent.id) { var f = el('span', '', 'for '), pa = f.appendChild(beadLink(d.parent.id, d.parent.title)); if (typeof TITLES !== 'undefined') TITLES.unseen(pa, d.parent.id, d.parent.title); meta.appendChild(f); }
    if (d.mr && d.mr.url) { var m = el('span', '', 'from '); m.appendChild(outLink(d.mr.url, mrWord(d.mr))); if (d.mr.title) m.lastChild.title = d.mr.title; meta.appendChild(m); }
    meta.appendChild(labelLink(d.id, 'the bundle bead', 'bl bd-bead'));
    bodyEl.appendChild(meta);
    if (d.description) bodyEl.appendChild(BEAD.mdInto(el('div', 'bv-md bd-desc'), BEAD.markdown(d.description), titles));
    if ((d.partial || []).length) {
      var pn = el('p', 'bv-note err', 'Could not read its documents: the beads daemon did not answer. ');
      pn.setAttribute('role', 'status'); pn.appendChild(retryButton(id)); bodyEl.appendChild(pn);
    }
    var docs = d.docs || [];
    if (!docs.length) { if (!(d.partial || []).length) bodyEl.appendChild(el('p', 'bv-empty', 'No documents in this bundle yet.')); return; }
    var lay = el('div', 'bd-lay' + (docs.length > 1 ? ' toc' : '')), list = el('div', 'bd-docs');
    if (docs.length > 1) lay.appendChild(toc(docs));
    docs.forEach(function (p, i) { list.appendChild(docEl(p, i)); });
    if (d.more) { var m = el('p', 'bv-note', plural(d.more, 'more document is', 'more documents are') + ' not shown here. '); m.appendChild(labelLink(d.id, 'The bundle bead has them all.')); list.appendChild(m); }
    lay.appendChild(list);
    bodyEl.appendChild(lay);
    follow();
  }

  W.register('bundle', {
    noun: 'bundle',
    url: url,
    paint: paint,
    live: function (id) { return '/garden/bead/' + encodeURIComponent(id) + '/events'; },   // live.go: the bundle bead moved
    failed: function (id, text, retry) {   // the documents stay: say they could not refresh
      var n = el('p', 'bv-note err bv-livenote', W.refreshFailure(text, 'bundle') + ' It shows the bundle as it was ' + (Date.now() / 1000 - lastAt >= 60 ? Math.round((Date.now() / 1000 - lastAt) / 60) + 'm ago' : 'a moment ago') + '. ');
      n.setAttribute('role', 'status');
      if (retry) n.appendChild(retryButton(id));
      var old = bodyEl.querySelector('.bv-livenote');
      if (old) old.remove();
      bodyEl.insertBefore(n, bodyEl.firstChild);
    },
    closed: function () { lastAt = 0; }
  });
  return api;
})();
if (typeof module !== 'undefined') module.exports = BUNDLE;
