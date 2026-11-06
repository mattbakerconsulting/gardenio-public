/* ARMS: the bead view's Heraldry section and its Change arms control (heraldry plan gd-N6I3Oa7uDg steps 3.2 and 5.5b; spec
   gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md §5, §6.3, §7, §8). An epic, plan or project shows its arms
   first: the renderer's picture, a row of its charges 64 px high (SVG, GET /garden/arms.svg, bead_arms.go), with the
   plain-words name beside it. Under them, for a viewer who may write (body data-edit), Change arms opens the correction tool:
   the arms as one row of three to five emoji in order, each with its Remove, the library as a grid searchable by name (a
   charge picked goes on the end of the row; picked again, it comes off), and the project's register read once
   (GET /garden/arms/register) so a clash or a look-alike is named before Save. Agents pick arms (R7); this is for an operator
   who dislikes what an agent chose, not the way arms are meant to be made. Save posts the run to the viewer's
   POST /b/<id>/heraldry, which the daemon checks again; Remove arms posts "none". The daemon's refusal is said in words.

   No text emoji anywhere in /garden (spec §5): a charge is drawn from its artwork (GET /garden/arms/charge/<codepoints>),
   every name is words, and the emoji travel only as the values the control compares and posts. Text goes in through
   textContent only. */
var ARMS = (function () {
  'use strict';
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  /* How many emoji arms hold (heraldry.MinRun, MaxRun), and the words for each count. */
  var MIN = 3, MAX = 5, WORDS = ['no', 'one', 'two', 'three', 'four', 'five'];
  /* An emoji's lookup form: presentation selectors dropped, as the renderer's and the daemon's lookups do. */
  function key(e) { return String(e || '').replace(/[\uFE0E\uFE0F]/g, ''); }
  /* The library's charge for an emoji, or null. lib is the indexed library (index()). */
  function charge(lib, e) { return (lib && lib.byKey && lib.byKey[key(e)]) || null; }
  function index(j) {
    j = j || {};
    var byKey = {}, longest = 0;
    (j.charges || []).forEach(function (c) { var k = key(c.e); byKey[k] = c; longest = Math.max(longest, k.length); });
    return { charges: j.charges || [], byKey: byKey, longest: longest };
  }
  /* A run ("🔥🧭📦", spaces allowed) as its charges' emoji in order, each spelled as the library spells it, or null when part
     of it is no library emoji. The longest library emoji at each point wins, so a sequence is never read as its parts. */
  function split(lib, run) {
    var s = key(run).replace(/\s+/g, ''), out = [];
    while (s) {
      var hit = null;
      for (var n = Math.min(lib.longest, s.length); n > 0 && !hit; n--) hit = lib.byKey[s.slice(0, n)] || null;
      if (!hit) return null;
      out.push(hit.e);
      s = s.slice(key(hit.e).length);
    }
    return out;
  }
  /* The register's keys (kbeads heraldry.Run and EmojiSet): the run itself, and its emoji with their order forgotten. The
     same run: taken. The same emoji in another order: a look-alike. */
  function run(list) { return list.join(''); }
  function set(list) { return list.map(key).sort().join(' '); }
  /* The plain-words name (§8, heraldry.PlainName): "arms: fire, compass, package". */
  function plainName(lib, list) {
    return 'arms: ' + list.map(function (e) { var c = charge(lib, e); return c ? c.n : 'an emoji'; }).join(', ');
  }
  /* What the register says about a draft row on bead self: the bead bearing the same run (taken) and those bearing the same
     emoji in another order, as kd heraldry check and the daemon read it. The bead's own row is never a clash. */
  function collisions(lib, register, self, list) {
    var out = { taken: null, otherOrder: [] };
    if (!list.length) return out;
    var ru = key(run(list)), st = set(list);
    (register || []).forEach(function (b) {
      var o = b && b.id !== self ? split(lib, b.arms) : null;
      if (!o) return;
      if (key(run(o)) === ru) out.taken = b;
      else if (set(o) === st) out.otherOrder.push(b);
    });
    return out;
  }
  /* What the row's count says, in words: how many more to pick, how many more it may take, or that it is full. */
  function tally(n) {
    if (!n) return 'No emoji yet: pick three to five below, in order.';
    if (n < MIN) return 'Pick ' + WORDS[MIN - n] + ' more: arms are three to five emoji.';
    if (n < MAX) return sentence(WORDS[n]) + ' emoji. You may add ' + (MAX - n === 1 ? 'one more' : 'up to ' + WORDS[MAX - n] + ' more') + '.';
    return 'Five emoji, the most arms hold. Remove one to add another.';
  }
  /* The daemon's refusal in plain words (kbeads heraldry.Parse, the register's 409): its help line dropped, which spells the
     example in emoji, and each run of library emoji in it said as their names; an emoji the library lacks is "an emoji". */
  function words(lib, msg) {
    var s = String(msg || '').replace(/\s+/g, ' ').trim(), h = s.indexOf(' heraldry is a bead\'s arms');
    if (h >= 0) s = s.slice(0, h);
    s = s.replace(/^invalid arms: /, '');
    var out = '', group = [];
    var flush = function () { if (group.length) { out += group.join(', '); group = []; } };
    while (s) {
      var hit = null, k = key(s.slice(0, 2 * lib.longest));
      for (var n = Math.min(lib.longest, k.length); n > 0 && !hit; n--) hit = lib.byKey[k.slice(0, n)] || null;
      if (hit) {
        group.push(hit.n);
        var m = 0, want = key(hit.e).length;   // step over the emoji and any selector it was spelled with
        for (var got = 0; got < want; m++) if (!/[\uFE0E\uFE0F]/.test(s.charAt(m))) got++;
        while (/[\uFE0E\uFE0F]/.test(s.charAt(m))) m++;
        s = s.slice(m);
        continue;
      }
      var pic = /^(?:\p{Extended_Pictographic}|\p{Regional_Indicator})[\uFE0E\uFE0F\u200D\p{Extended_Pictographic}\p{Emoji_Modifier}\p{Regional_Indicator}]*/u.exec(s);
      if (pic) { group.push('an emoji'); s = s.slice(pic[0].length); continue; }
      flush();
      out += s.charAt(0); s = s.slice(1);
    }
    flush();
    out = sentence(out.trim());
    return out && !/[.!?]$/.test(out) ? out + '.' : out;
  }
  /* The charges whose name has a word starting with each word of q: {list: the first max, total}. "key" finds key and
     keyboard, not monkey. The name itself comes first, then names holding every word whole ("old key" before "musical
     keyboard"), each in library order. */
  function search(charges, q, max) {
    var words = String(q || '').toLowerCase().split(/\s+/).filter(Boolean), exact = [], whole = [], part = [];
    (charges || []).forEach(function (c) {
      var name = String(c.n || '').toLowerCase(), ns = name.split(/[\s\-:,.()]+/);
      if (!words.every(function (w) { return ns.some(function (n) { return n.indexOf(w) === 0; }); })) return;
      (name === words.join(' ') ? exact : words.every(function (w) { return ns.indexOf(w) >= 0; }) ? whole : part).push(c);
    });
    var all = exact.concat(whole, part);
    return { list: max ? all.slice(0, max) : all, total: all.length };
  }
  /* The picture's width for n emoji at height h (heraldry.Width): a square per charge and an eighth of one between them. */
  function width(n, h) { return n > 0 ? n * h + (n - 1) * Math.round(h / 8) : 0; }
  /* The picture's and a charge's URLs under base (the page's data-base). */
  function svgSrc(base, arms, night) { return (base || '') + '/garden/arms.svg?a=' + encodeURIComponent(arms) + '&theme=' + (night ? 'night' : 'day'); }
  function chargeSrc(base, c) { return (base || '') + '/garden/arms/charge/' + encodeURIComponent(c.c); }
  /* A name as a line of words: its first letter capitalised. */
  function sentence(s) { s = String(s || ''); return s.charAt(0).toUpperCase() + s.slice(1); }
  /* Arms a as day and night imgs h px high in span.ar-pic, alt its name (§8); png: lazy renderer bitmaps (5.9). */
  function pic(base, a, h, cls, title, png) {
    if (!a || !a.canonical || typeof document === 'undefined') return null;
    var box = document.createElement('span'); box.className = 'ar-pic' + (cls ? ' ' + cls : '');
    [false, true].forEach(function (night) {
      var img = document.createElement('img'); img.className = night ? 'ar-night' : 'ar-day';
      img.width = width(a.n || 1, h); img.height = h; img.decoding = 'async'; img.alt = a.name || ''; if (png) img.loading = 'lazy';
      img.src = png ? HERALDRY.src(base, a.canonical, HERALDRY.deviceHeight(h, window.devicePixelRatio), night) : svgSrc(base, a.canonical, night);
      if (title) img.title = title;
      box.appendChild(img);
    });
    return box;
  }

  var api = { MIN: MIN, MAX: MAX, key: key, charge: charge, index: index, split: split, run: run, set: set, plainName: plainName,
    collisions: collisions, tally: tally, words: words, search: search, width: width, svgSrc: svgSrc, chargeSrc: chargeSrc, sentence: sentence, pic: pic,
    section: function () { return null; }, lineArms: function () { return null; } };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var B = document.body, base = B.dataset.base || '', told = {}, libP = null;
  var GRID_MAX = 120;   // grid buttons drawn before "Show all"
  var PIC_H = 64;       // the Heraldry section's picture, CSS px high (spec §6.3)
  var LINE_H = 24;      // garden.css .bv-title .ar-line
  function canEdit() { return B.dataset.edit === '1'; }
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function button(text, cls, fn) { var b = el('button', cls || 'btn small', text); b.type = 'button'; if (fn) b.onclick = fn; return b; }
  function bw() { return typeof BEADWRITE !== 'undefined' ? BEADWRITE : null; }
  function getJSON(path) {
    return fetch(base + path, { credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { Accept: 'application/json' } }).then(function (r) {
      if (r.type === 'opaqueredirect') throw new Error('Your session ended. Reload the page to sign in again.');
      if (!r.ok) throw new Error('status ' + r.status);
      return r.json();
    });
  }
  /* The library, read once a page (the browser keeps it a day); a failed read is tried again on the next open. */
  function library() {
    if (!libP) libP = getJSON('/garden/arms/library').then(index, function (e) { libP = null; throw e; });
    return libP;
  }
  function post(id, value) {
    return fetch(base + '/b/' + encodeURIComponent(id) + '/heraldry', { method: 'POST', credentials: 'same-origin', redirect: 'manual', cache: 'no-store',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: new URLSearchParams({ heraldry: value }).toString() })
      .then(function (r) {
        if (r.type === 'opaqueredirect') throw new Error('Your session ended. Reload the page to sign in again.');
        return r.json().catch(function () { return null; }).then(function (j) {
          var W = bw(), e = W ? W.failure(r.status, j) : (r.ok ? '' : 'That did not go through (' + r.status + ').');
          if (e) throw new Error(e);
        });
      }, function () { throw new Error('The garden did not answer. Try again.'); });
  }
  function reload(id, text) { told[id] = text; if (typeof WORKBENCH !== 'undefined') WORKBENCH.reload('bead', id); }
  function say(note, text, err) { note.textContent = text; note.className = 'bv-note' + (err ? ' err' : ''); }
  function beadLink(b) { var a = el('a', 'bl', b.title || b.id); a.href = base + '/garden?bead=' + encodeURIComponent(b.id); a.title = b.id; return a; }

  /* The picture of arms holding n emoji, PIC_H high, one <img> per theme (garden.css shows the one for the sky), alt in plain
     words. Its size is set before it loads, so the name beside it does not move. */
  function picture(arms, name, n) {
    var box = el('span', 'ar-pic');
    [false, true].forEach(function (night) {
      var img = el('img', night ? 'ar-night' : 'ar-day');
      img.width = width(n, PIC_H); img.height = PIC_H; img.decoding = 'async';
      img.alt = name; img.src = svgSrc(base, arms, night);
      box.appendChild(img);
    });
    return box;
  }

  /* The arms before a bead view's title: x's own, else its line's (bead.go line_arms), else the row's (k.arms); null when none. */
  function lineArms(x, k) {
    x = x || {}; k = k || {};
    var a = x.arms || x.line_arms || k.arms, from = x.arms ? null : x.line_from;
    return pic(base, a, LINE_H, 'ar-line', from && from.id ? 'Arms of ' + (from.title || from.id) : '');
  }

  /* The Heraldry section of bead document d, or null: a bead without arms shows none (arms are optional, R1) unless the
     viewer may give it some. */
  function section(d) {
    if (!d || !d.id || !(d.arms || (d.arms_ok && canEdit()))) return null;
    var s = el('section', 'bv-sec bv-arms'), row = el('div', 'ar-row'), note = el('p', 'bv-note');
    note.setAttribute('role', 'status');
    s.appendChild(el('h3', '', 'Heraldry'));
    if (d.arms) {
      row.appendChild(picture(d.arms.canonical, d.arms.name, d.arms.n || 1));
      row.appendChild(el('p', 'ar-name', sentence(d.arms.name)));
    } else row.appendChild(el('p', 'bv-empty', 'No arms yet. The agent that holds an epic gives it arms; you may give it some here.'));
    s.appendChild(row);
    if (told[d.id]) { say(note, told[d.id], false); delete told[d.id]; }
    if (canEdit() && d.arms_ok) {
      var bar = el('div', 'bv-acts'), slot = el('div', 'ar-slot-host');
      var open = button(d.arms ? 'Change arms' : 'Give arms', 'btn small ghost', function () {
        bar.hidden = true;
        editor(slot, d, note, function () { slot.textContent = ''; bar.hidden = false; open.focus(); });
      });
      bar.appendChild(open);
      s.appendChild(bar); s.appendChild(slot);
    }
    s.appendChild(note);
    return s;
  }

  /* Change arms: the row with each emoji's Remove, its count in words, the register's word on it, the library grid, and Save. */
  function editor(host, d, note, done) {
    var form = el('form', 'ar-edit');
    form.setAttribute('aria-label', 'Change the arms of ' + d.id);
    form.appendChild(el('p', 'bv-note', 'Loading the emoji…'));
    host.appendChild(form);
    library().then(function (lib) { build(form, lib, d, note, done); }, function (e) {
      form.remove(); say(note, 'Could not load the emoji (' + e.message + '). Try again.', true); done();
    });
  }

  function build(form, lib, d, note, done) {
    form.textContent = '';
    var st = { list: (d.arms && split(lib, d.arms.canonical)) || [], register: null, regErr: false };
    if (st.list.length > MAX) st.list = st.list.slice(0, MAX);

    /* the row: each emoji in order with its Remove, a dashed hole for each still wanted up to three, then its count and name */
    var fs = el('fieldset', 'ar-runbox'), rowEl = el('ol', 'ar-run'), name = el('p', 'ar-name'), tal = el('p', 'ar-tally');
    fs.appendChild(el('legend', '', 'The arms, in order'));
    name.setAttribute('aria-live', 'polite');
    fs.appendChild(rowEl); fs.appendChild(name); fs.appendChild(tal);
    form.appendChild(fs);

    /* the register's word on the draft, said as it changes */
    var col = el('div', 'ar-col');
    col.setAttribute('role', 'status');
    form.appendChild(col);

    /* the library: a search box and the grid of charges matching it */
    var find = el('input', 'bw-in ar-find'), count0 = el('p', 'bw-h ar-count'), grid = el('div', 'ar-grid'), all = null, max = GRID_MAX;
    find.type = 'search'; find.placeholder = 'Find an emoji by name: fire, compass, key…'; find.maxLength = 80; find.autocomplete = 'off'; find.spellcheck = false;
    find.setAttribute('aria-label', 'Find an emoji by name');
    find.oninput = function () { max = GRID_MAX; fill(); };
    find.onkeydown = function (e) { if (e.key === 'Enter') { e.preventDefault(); var f = grid.querySelector('button'); if (f) f.click(); } };
    form.appendChild(find); form.appendChild(count0); form.appendChild(grid);

    /* Save, Remove arms (on its second tap, rubric C8) and Cancel */
    var acts = el('div', 'bv-acts'), save = el('button', 'btn small', 'Save arms'), cancel = button('Cancel', 'btn small ghost', function () { form.remove(); done(); });
    save.type = 'submit';
    acts.appendChild(save);
    if (d.arms) {
      var rm = button('Remove arms', 'btn small ghost danger', function () {
        if (!rm.dataset.armed) { rm.dataset.armed = '1'; rm.textContent = 'Tap again to remove the arms'; setTimeout(function () { if (rm.isConnected) { delete rm.dataset.armed; rm.textContent = 'Remove arms'; } }, 4000); return; }
        send('none', 'Removed the arms.');
      });
      acts.appendChild(rm);
    }
    acts.appendChild(cancel);
    form.appendChild(acts);
    var fnote = el('p', 'bv-note'); fnote.setAttribute('role', 'status');
    form.appendChild(fnote);

    /* A charge from the grid: off the row when it is on it (an emoji is in the arms once, R11), else on the end of it. */
    function pick(c) {
      var j = st.list.map(key).indexOf(key(c.e));
      if (j >= 0) st.list.splice(j, 1);
      else if (st.list.length >= MAX) { say(fnote, 'Arms hold five emoji at most. Remove one from the row to add ' + c.n + '.', true); return; }
      else st.list.push(c.e);
      say(fnote, '', false);
      paint();
    }
    function drawRow() {
      rowEl.textContent = '';
      st.list.forEach(function (e, i) {
        var c = charge(lib, e), li = el('li', 'ar-pick'), img = el('img');
        if (c) img.src = chargeSrc(base, c); img.alt = ''; img.width = 40; img.height = 40; img.decoding = 'async';
        var x = button('Remove', 'btn small ghost ar-x', function () {
          st.list.splice(i, 1); say(fnote, '', false); paint();
          var next = rowEl.querySelectorAll('.ar-x')[Math.min(i, st.list.length - 1)];
          (next || find).focus();
        });
        x.setAttribute('aria-label', 'Remove ' + (c ? c.n : 'an emoji') + ', ' + (i + 1) + ' of ' + st.list.length);
        li.appendChild(img); li.appendChild(x);
        rowEl.appendChild(li);
      });
      for (var h = st.list.length; h < MIN; h++) { var hole = el('li', 'ar-hole'); hole.setAttribute('aria-hidden', 'true'); rowEl.appendChild(hole); }
    }
    function fill() {
      var res = search(lib.charges, find.value, max), on = {}, full = st.list.length >= MAX;
      st.list.forEach(function (e) { on[key(e)] = true; });
      grid.textContent = '';
      res.list.forEach(function (c) {
        var is = !!on[key(c.e)], b = button('', 'ar-charge' + (is ? ' on' : full ? ' full' : ''), function () { pick(c); b.focus(); });
        var img = el('img'); img.src = chargeSrc(base, c); img.alt = ''; img.width = 32; img.height = 32; img.loading = 'lazy'; img.decoding = 'async';
        b.appendChild(img); b.title = c.n; b.setAttribute('aria-label', c.n + (is ? ', in the arms' : ''));
        b.setAttribute('aria-pressed', is ? 'true' : 'false');
        grid.appendChild(b);
      });
      var q = find.value.trim();
      count0.textContent = !res.total ? 'No emoji has "' + q + '" in its name.' : (q ? res.total + ' with "' + q + '" in the name' : res.total + ' emoji') + (res.total > res.list.length ? ', the first ' + res.list.length + ' shown' : '') + '.';
      if (all) { all.remove(); all = null; }
      if (res.total > res.list.length) {
        all = button('Show all ' + res.total, 'btn small ghost bv-more', function () { max = 0; fill(); });
        grid.parentNode.insertBefore(all, grid.nextSibling);
      }
    }
    function sayCollisions() {
      col.textContent = '';
      var line = function (cls, parts) { var p = el('p', 'bv-note' + (cls ? ' ' + cls : '')); parts.forEach(function (x) { p.appendChild(typeof x === 'string' ? document.createTextNode(x) : x); }); col.appendChild(p); };
      save.disabled = false;
      if (st.list.length < MIN) return;
      if (st.regErr) { line('', ['Could not read the arms already given in ' + (d.project || 'this project') + '. Save still checks them.']); return; }
      if (!st.register) { line('', ['Checking the arms already given in ' + (d.project || 'this project') + '…']); return; }
      var c = collisions(lib, st.register, d.id, st.list);
      if (c.taken) line('err', ['Taken: ', beadLink(c.taken), ' bears these arms. Arms are unique in a project, so choose other emoji or another order.']);
      c.otherOrder.forEach(function (b) { line('', ['Looks alike: ', beadLink(b), ' has these emoji in another order.']); });
      if (!c.taken && !c.otherOrder.length) line('', ['No other bead in ' + (d.project || 'this project') + ' has these arms.']);
      save.disabled = !!c.taken;
    }
    function paint() {
      var n = st.list.length;
      drawRow();
      name.textContent = n ? sentence(plainName(lib, st.list)) : '';
      name.hidden = !n;
      tal.textContent = tally(n);
      tal.className = 'ar-tally' + (n && n < MIN ? ' short' : '');
      sayCollisions();
      fill();
    }
    function send(value, done2) {
      Array.prototype.forEach.call(form.querySelectorAll('button,input'), function (x) { x.disabled = true; });
      say(fnote, value === 'none' ? 'Removing…' : 'Saving…', false);
      post(d.id, value).then(function () { reload(d.id, done2); }, function (err) {
        Array.prototype.forEach.call(form.querySelectorAll('button,input'), function (x) { x.disabled = false; });
        if (rm) { delete rm.dataset.armed; rm.textContent = 'Remove arms'; }
        paint();
        say(fnote, words(lib, err.message), true);
      });
    }
    form.onsubmit = function (e) {
      e.preventDefault();
      var n = st.list.length;
      if (n < MIN) { say(fnote, 'Arms are three to five emoji: pick ' + WORDS[MIN - n] + ' more, or Cancel.', true); find.focus(); return; }
      var arms = run(st.list);
      if (d.arms && arms === d.arms.canonical) { say(fnote, 'Nothing changed.', false); return; }
      send(arms, 'Saved the arms.');
    };

    paint();
    find.focus();
    if (d.project) getJSON('/garden/arms/register?project=' + encodeURIComponent(d.project)).then(function (j) { st.register = j.bearers || []; sayCollisions(); },
      function () { st.regErr = true; sayCollisions(); });
    else { st.regErr = true; sayCollisions(); }
  }

  api.section = section;
  api.lineArms = lineArms;
  return api;
})();
if (typeof module !== 'undefined') module.exports = ARMS;
