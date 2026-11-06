/* web/wishface.js: the bead view's wish face (bead_face.go wishFace, gd-zn6iUnPd9Q). A thread head reads as its ask, then the
   thread's counts as one line, each a link ({sec}) to the section below that lists it, then who asked, where and when. The
   conversation is its own section (web/beadtalk.js), never repeated here. Installed into BEAD.FACES after workbench.js; pure
   under node (bead_kin_test.go). */
var WISHFACE = (function () {
  'use strict';
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }
  function span(s) { s = Math.max(60, s); var u = s >= 86400 ? [86400, 'd'] : s >= 3600 ? [3600, 'h'] : [60, 'm']; return Math.round(s / u[0]) + u[1]; }
  function who(p) { p = String(p || ''); return /^oidc:/.test(p) ? p.slice(5).split('@')[0] : p.replace(/^(agent|slack):/, '') || 'someone'; }   // as beadtalk.js who
  function joined(list) { var o = []; list.forEach(function (p, i) { if (i) o.push(' · '); o.push(p); }); return o; }
  /* view(f, d) with `this` the face context (workbench.js faceCtx): {h, said, lead, state, sum, rows}; said, as the kind word
     over the panel is "wish" too. */
  function view(f, d) {
    var x = d || {}, sum = [], where = '', asked = [who(f.asker)];
    var to = function (key, t) { return key ? { sec: key, t: t } : t; };
    if (f.counted) {
      sum.push(to(x.kin && x.kin.conversation ? 'conversation' : '', f.replies ? plural(f.replies, 'reply', 'replies') : 'no replies yet'));
      if (f.agents_live) sum.push(plural(f.agents_live, 'agent', 'agents') + ' at work' + (f.agents_left ? ', ' + f.agents_left + ' left' : ''));
      else if (f.agents_left) sum.push(plural(f.agents_left, 'agent', 'agents') + ' left');
      if (f.work_open || f.work_closed) sum.push(to((x.children || []).length ? 'children' : '', 'work: ' + [f.work_open ? f.work_open + ' open' : '', f.work_closed ? f.work_closed + ' closed' : ''].filter(Boolean).join(', ')));
      if (f.prs_merged) sum.push(plural(f.prs_merged, 'PR merged', 'PRs merged'));
    }
    if (f.where === 'slack') where = /^https?:\/\//i.test(f.permalink || '') ? { url: f.permalink, t: 'in Slack' } : 'in Slack';
    else if (f.where === 'commons') where = 'in the Commons, #general';
    if (where) asked.push(' ', where);   // the Slack channel id stays in the document: it names nothing to a reader
    if (f.at) asked.push(' · ' + span(this.now - f.at) + ' ago');
    return { h: 'Wish', said: true, lead: f.ask || '', state: f.ask ? '' : 'No words came with this wish.', sum: sum.length ? joined(sum) : null, rows: [['Asked by', asked]] };
  }
  function install(B) { if (B && B.FACES) B.FACES.wish = view; return B; }
  if (typeof BEAD !== 'undefined') install(BEAD);
  return { view: view, install: install };
})();
if (typeof module !== 'undefined') module.exports = WISHFACE;
