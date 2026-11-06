/* web/relface.js: the bead view's release face (bead_face_release.go; gd-j3bQqfZSt7). A type=release bead reads as its
   status and where it runs, when it was cut and built, the release before it, and its PRs by plan, folded; never the
   contents JSON as a fields row. Installed into BEAD.FACES after workbench.js; pure under node (bead_face_test.go). */
var RELFACE = (function () {
  'use strict';
  var STATUS = { cut: 'Cut, not built or deployed yet.', built: 'Built, not deployed yet.', live: 'Live.', superseded: 'Superseded.' };
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }
  function span(s) { s = Math.max(60, s); var u = s >= 86400 ? [86400, 'd'] : s >= 3600 ? [3600, 'h'] : [60, 'm']; return Math.round(s / u[0]) + u[1]; }
  function ago(now, t) { return t > 0 ? span(now - t) + ' ago' : ''; }
  function ref(r) { return r && r.id ? { bead: r.id, t: r.title || 'a bead not visible to you' } : null; }
  function out(u, t) { return /^https?:\/\//i.test(u || '') ? { url: u, t: t } : t; }
  function joined(list) { var o = []; list.filter(Boolean).forEach(function (p, i) { if (i) o.push(' · '); o.push(p); }); return o; }
  function planPart(g) { return g.plan ? ref(g.plan) : g.id ? { bead: g.id } : { b: 'Outside any plan' }; }
  /* view(f) with `this` the face context (workbench.js faceCtx): {h, state, tone, rows, items, note, links}. */
  function view(f) {
    var now = this.now, deps = f.deployments || [], on = deps.filter(function (d) { return !d.until; }), was = deps.filter(function (d) { return d.until; });
    var where = on.map(function (d) { return d.target + (d.since ? ' since ' + ago(now, d.since) : ''); }).join(', ');
    var state = (STATUS[f.status] || STATUS.cut) + (where ? ' Runs on ' + where + '.' : '');
    var rows = [], plans = f.plans || [], named = plans.filter(function (g) { return g.plan || g.id; }), items = [];
    rows.push(['Cut', [f.cut_at ? ago(now, f.cut_at) : 'unknown', f.cut_by ? ' by ' + f.cut_by : '', f.source ? { sm: 'via ' + f.source } : '']]);
    if (f.built_at) rows.push(['Built', [ago(now, f.built_at)]]);
    if (f.sha) rows.push(['Commit', [{ sm: f.sha.slice(0, 10) }]]);
    if (f.previous || f.previous_tag) rows.push(['Previous', [ref(f.previous) || f.previous_tag]]);
    if (was.length) rows.push(['Ran on', [was.map(function (d) { return d.target + (d.since ? ' for ' + span(d.until - d.since) : '') + ' until ' + ago(now, d.until); }).join(', ')]]);
    rows.push(['Contents', [plural(f.count || 0, 'PR', 'PRs') + (named.length ? ' from ' + plural(named.length, 'plan', 'plans') : ''), f.incomplete ? { sm: 'the compare was cut short: some are missing' } : '']]);
    if (named.length) rows.push(['By plan', joined(plans.map(function (g) { return [planPart(g), ' (' + g.rows.length + ')']; })).reduce(function (a, p) { return a.concat(p); }, [])]);
    plans.forEach(function (g) {
      g.rows.forEach(function (r) {
        items.push({ at: r.merged_at || 0, parts: [out(r.url, r.pr ? '#' + r.pr : 'PR'), ' ', r.title || '', plans.length > 1 ? { sm: g.plan ? g.plan.title || g.plan.id : g.id || 'no plan' } : ''] });
      });
    });
    items.sort(function (a, b) { return b.at - a.at; });
    var links = [];
    if (this.has('release')) links.push({ t: 'Open the changelog', go: this.base + '/garden?release=' + encodeURIComponent(this.id), aria: 'The changelog by plan and step, the truck’s view' });
    return { h: 'Release ' + (f.tag || ''), state: state, tone: f.status === 'live' ? 'ok' : f.status === 'superseded' ? 'dim' : '', rows: rows,
      items: items, itemsH: items.length ? 'Pull requests, newest merge first' : '', note: f.more ? '+' + plural(f.more, 'more PR', 'more PRs') + ' not listed.' : '', links: links.length ? links : undefined };
  }
  function install(B) { if (B && B.FACES) B.FACES.release = view; return B; }
  if (typeof BEAD !== 'undefined') install(BEAD);
  return { view: view, install: install, ago: ago };
})();
if (typeof module !== 'undefined') module.exports = RELFACE;
