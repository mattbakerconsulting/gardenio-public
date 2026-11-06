/* web/creds.js: the credential pool (Lands workbench-glass spec GL-15, step 6.9, gd-zM1oXXxlYp), the Workbench's creds
   kind, /garden?creds=pool: coopmux's credential pool, read only, for an admin. It reads GET /garden/glass/credentials
   (glass_pool.go: the snapshot mux.html's CredentialPanel.tsx polls, through the Glass relay) and shows:
   - the line under the title: how many accounts, how many healthy, how many sessions they carry, and how the broker picks
     an account for a new session (least loaded, or round robin and which healthy account is next);
   - each account in the broker's order: its name and health (Healthy, Warming, Throttled, No key) as the Glass's state
     chip, its 5 h and 7 d windows as meters with their percent in words (from the controller's rate-limit probe; "Not
     probed yet" before it), and a line: provider, sessions now and in all, when its throttle resets, when its key expires;
   - when auto-assign is off (the default in prod) the broker counts no sessions, so the counts are left out and the panel
     says why instead of showing zeros (ka-0000005557);
   - the footer: read only, when it was read; it reads again every REFRESH_S seconds while open.
   It changes nothing: adding, pushing and rebalancing accounts stay on mux.html (spec 3.2 LATER). Opened from the operator
   land's header, the project panel of a land that is not a tenant's (web/project.js, credential_pool). Degraded: a
   refusal says the pool is for admins, coopmux not wired here says so, no answer offers Try again; a refresh that fails
   keeps the accounts shown and says so over them. Text goes in through textContent. */
var CREDS = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var ID = 'pool', URL = '/garden/glass/credentials', REFRESH_S = 30;
  var STATUS = { healthy: ['Healthy', 'ok'], warming: ['Warming', 'warn'], throttled: ['Throttled', 'bad'], missing: ['No key', 'warn'] };
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }
  function num(v) { return typeof v === 'number' && isFinite(v); }
  /* An account's health in words and its chip's tone (the Glass's .gl-state ok / warn / bad); an unknown one as written. */
  function statusWord(s) { var x = STATUS[s]; return x ? x[0] : s ? String(s).charAt(0).toUpperCase() + String(s).slice(1) : 'Unknown'; }
  function statusTone(s) { var x = STATUS[s]; return x ? x[1] : ''; }
  /* A window used, as a whole percent ("42%"), '' when not known. */
  function pct(v) { return num(v) ? Math.round(v * 100) + '%' : ''; }
  /* A meter's tone: 90% and over is bad, 70% and over a warning (CredentialPanel.tsx's UtilBar thresholds). */
  function meterTone(v) { return !num(v) ? '' : v >= 0.9 ? 'bad' : v >= 0.7 ? 'warn' : 'ok'; }
  function hhmm(sec) { return new Date(sec * 1000).toISOString().slice(11, 16) + ' UTC'; }
  /* A span of seconds as the shortest words that say it: "40 min", "3 h", "2 days". */
  function span(s) {
    s = Math.max(0, Math.round(s));
    if (s < 3600) return Math.max(1, Math.round(s / 60)) + ' min';
    if (s < 2 * 86400) return Math.round(s / 3600) + ' h';
    return Math.round(s / 86400) + ' days';
  }
  /* How the broker picks an account for a new session. next is the round robin's index among the healthy accounts. */
  function strategyWord(s, next) {
    if (s === 'least_loaded') return 'New sessions go to the least loaded healthy account.';
    if (s === 'round_robin') return 'New sessions take the healthy accounts in turn' + (num(next) ? '; the next is healthy account ' + (next + 1) + '.' : '.');
    return s ? 'Strategy: ' + s + '.' : '';
  }
  /* The line under the title. */
  function summary(d) {
    var out = [plural(d.total || 0, 'account', 'accounts') + ', ' + (d.healthy || 0) + ' healthy'];
    if (!d.uncounted) out.push(plural(d.sessions || 0, 'session', 'sessions'));
    return out.join(' · ');
  }
  /* An account's line, at now (unix seconds): provider, sessions (when counted), the throttle's reset, the key's expiry. */
  function accountLine(a, d, now) {
    var out = [];
    if (a.provider) out.push(a.provider);
    if (!d.uncounted) out.push(plural(a.sessions || 0, 'session', 'sessions') + ' now, ' + (a.assigned || 0) + ' in all');
    if (!a.has_key) out.push('no key set');
    var reset = a.reset_at ? Date.parse(a.reset_at) / 1000 : NaN;
    if (isFinite(reset) && reset > now && (a.status === 'throttled' || a.status === 'warming' || (a.throttle && a.throttle !== 'allowed'))) out.push('resets at ' + hhmm(reset));
    if (a.expires_at) out.push(a.expires_at > now ? 'key expires in ' + span(a.expires_at - now) : 'key expired');
    return out;
  }
  /* A failed load in the panel's own words, {text, retry}, or null for the host's. */
  function failure(status) {
    if (status === 403) return { text: 'The credential pool is for gardenio admins.', retry: false };
    if (status === 503) return { text: 'The credential pool is not available here: coopmux is not wired to this garden.', retry: false };
    if (status === 502) return { text: 'Could not read the credential pool: coopmux did not answer.', retry: true };
    return null;
  }

  var api = { ID: ID, URL: URL, REFRESH_S: REFRESH_S, statusWord: statusWord, statusTone: statusTone, pct: pct, meterTone: meterTone, span: span,
    strategyWord: strategyWord, summary: summary, accountLine: accountLine, failure: failure };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH;
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  var timer = 0, noteEl = null;

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function cur() { var c = W.current(); return c && c.kind === 'creds' ? c.id : ''; }
  function retryButton(id) {
    var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button';
    b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; W.refresh('creds', id); };
    return b;
  }
  function note(text, isErr, retry, id) {
    var n = el('p', 'bv-note' + (isErr ? ' err' : ''), text || '');
    n.setAttribute('role', 'status');
    if (isErr && retry) { n.appendChild(document.createTextNode(' ')); n.appendChild(retryButton(id)); }
    return n;
  }
  function chip(status) {
    var c = el('span', 'gl-state ' + statusTone(status));
    c.appendChild(el('span', 'gl-dot')).setAttribute('aria-hidden', 'true');
    c.appendChild(document.createTextNode(statusWord(status)));
    return c;
  }
  /* The 5 h and 7 d windows: a label, a meter and its percent, or one line when the account is not probed yet. */
  function bars(a) {
    if (!num(a.util_5h) && !num(a.util_7d)) return el('p', 'qv-line', 'Not probed yet.');
    var g = el('div', 'gc-bars');
    [['5 h', a.util_5h, '5-hour'], ['7 d', a.util_7d, '7-day']].forEach(function (w) {
      g.appendChild(el('span', 'gc-l', w[0]));
      var bar = el('div', 'gc-bar'), fill = el('div', 'gc-fill ' + meterTone(w[1]));
      bar.setAttribute('role', 'meter'); bar.setAttribute('aria-label', a.name + ', ' + w[2] + ' window used');
      bar.setAttribute('aria-valuemin', '0'); bar.setAttribute('aria-valuemax', '100');
      if (num(w[1])) { bar.setAttribute('aria-valuenow', String(Math.round(w[1] * 100))); fill.style.width = Math.min(100, Math.max(0, w[1] * 100)) + '%'; }
      else bar.setAttribute('aria-valuetext', 'not probed');
      bar.appendChild(fill); g.appendChild(bar);
      g.appendChild(el('span', 'gc-p', pct(w[1]) || '—'));
    });
    return g;
  }
  function account(a, d, now) {
    var li = el('li', 'qv-row' + (a.status === 'throttled' || a.status === 'missing' ? ' needs' : '')), head = el('div', 'gc-head');
    head.appendChild(el('span', 'gc-name', a.name)); head.appendChild(chip(a.status));
    li.appendChild(head);
    li.appendChild(bars(a));
    var line = accountLine(a, d, now);
    if (line.length) li.appendChild(el('p', 'qv-line', line.join(' · ')));
    return li;
  }
  function stop() { if (timer) clearTimeout(timer); timer = 0; }
  function again(id) { stop(); timer = setTimeout(function () { timer = 0; if (cur() === id) W.refresh('creds', id); }, REFRESH_S * 1000); }

  function paint(id, d, text, isErr, retry) {
    kindEl.textContent = 'credentials';
    titleEl.textContent = 'Credential pool';
    bodyEl.textContent = '';
    noteEl = note(text, isErr, retry, id);
    if (!d) { bodyEl.appendChild(noteEl); return; }
    var now = d.read_at || Math.floor(Date.now() / 1000);
    var meta = el('div', 'bv-meta'); meta.appendChild(el('span', '', summary(d)));
    var how = strategyWord(d.strategy, d.next); if (how) meta.appendChild(el('p', 'qv-line', how));
    bodyEl.appendChild(meta);
    bodyEl.appendChild(noteEl);
    if (d.uncounted) bodyEl.appendChild(el('p', 'bv-note', 'Auto-assign is off here, so coopmux counts no sessions per account.'));
    var s = el('section', 'bv-sec'); s.appendChild(el('h3', '', 'Accounts'));
    if (!(d.accounts || []).length) s.appendChild(el('p', 'bv-empty', 'Coopmux holds no account.'));
    else {
      var ul = el('ul', 'qv-list'); ul.setAttribute('aria-label', 'Accounts');
      d.accounts.forEach(function (a) { ul.appendChild(account(a, d, now)); });
      s.appendChild(ul);
    }
    bodyEl.appendChild(s);
    bodyEl.appendChild(el('p', 'gc-foot', 'Read only. Read at ' + hhmm(now) + '; it reads again every ' + REFRESH_S + ' s.'));
  }

  W.register('creds', {
    noun: 'credential pool',
    url: function () { return URL; },
    paint: paint,
    failure: failure,
    done: function (id, d) { if (d) again(id); else stop(); },   // a first load that failed reads again only by Try again
    /* A refresh that failed keeps the accounts shown and says so over them, and tries again on the next beat. */
    failed: function (id, text, retry) {
      if (noteEl) { var n = note(W.refreshFailure(text, '', 'Could not read the pool again'), true, retry, id); noteEl.replaceWith(n); noteEl = n; }
      again(id);
    },
    closed: function () { stop(); noteEl = null; }
  });
  return api;
})();
if (typeof module !== 'undefined') module.exports = CREDS;
