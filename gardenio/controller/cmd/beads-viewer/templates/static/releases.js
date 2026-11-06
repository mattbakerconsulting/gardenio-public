// The Release pages (plan gd-UZKZ9H25UD step 8, release_page.go).
//
// /releases (#rel-list-page): each section[data-project] is filled from
// GET /api/releases?project=<p>&limit=<n>, answered {items: [Release...],
// next_cursor?}, newest cut first; "Older releases" follows next_cursor.
//
// /release/{id} (#rel-page): the page is filled from GET /api/releases/{id}
// (plus ?project= when {id} is a tag): the header facts, Contents grouped by
// plan and step, the Deployments timeline, and Previous / Next.
//
// The JSON routes are step 6's (gd-DiI90CU9Fy) and carry the Lands contract
// Release with the viewer's extras; every extra is optional here and an
// unknown status reads as cut. The words come from the page (#rel-words and
// the <template> rows), never from this file, so a /garden release view can
// read the same JSON with the same words.
(function () {
  'use strict';

  var PAGE = 20;
  var STATUSES = { cut: 1, built: 1, live: 1, superseded: 1 };

  function word(key, vars) {
    var el = document.querySelector('#rel-words [data-word="' + key + '"]');
    var s = el ? el.textContent : key;
    if (vars) {
      Object.keys(vars).forEach(function (k) { s = s.split('{' + k + '}').join(String(vars[k])); });
    }
    return s;
  }

  function count(n, key) {
    return n === 1 ? word(key + '-1') : word(key, { n: n });
  }

  function statusWord(s) {
    return STATUSES[s] ? s : 'cut';
  }

  function slot(root, name) {
    return root.querySelector('[data-slot="' + name + '"]');
  }

  function clone(id) {
    var tpl = document.getElementById(id);
    return tpl.content.firstElementChild.cloneNode(true);
  }

  function parseTime(s) {
    if (!s) return null;
    var d = new Date(s);
    return isNaN(d.getTime()) ? null : d;
  }

  function pad(n) { return n < 10 ? '0' + n : String(n); }

  // "2026-10-31 12:19Z" in UTC: one form for every reader, as the bead pages.
  function stamp(d) {
    return d.getUTCFullYear() + '-' + pad(d.getUTCMonth() + 1) + '-' + pad(d.getUTCDate()) +
      ' ' + pad(d.getUTCHours()) + ':' + pad(d.getUTCMinutes()) + 'Z';
  }

  function ago(d) {
    var sec = Math.max(0, Math.round((Date.now() - d.getTime()) / 1000));
    var v;
    if (sec < 90) v = sec + 's';
    else if (sec < 5400) v = Math.round(sec / 60) + 'm';
    else if (sec < 172800) v = Math.round(sec / 3600) + 'h';
    else v = Math.round(sec / 86400) + 'd';
    return word('ago', { d: v });
  }

  // setTime fills a <time>: the stamp as text, the relative age as its title.
  function setTime(el, s) {
    var d = parseTime(s);
    if (!el) return false;
    if (!d) { el.textContent = s || '—'; return !!s; }
    el.dateTime = d.toISOString();
    el.textContent = stamp(d);
    el.title = ago(d);
    return true;
  }

  function setStatus(el, status, skipped) {
    var w = skipped ? 'skipped' : statusWord(status);
    el.className = 'release-status release-status-' + w;
    el.textContent = skipped ? word('skipped') : w;
    if (skipped) el.title = word('skipped-title');
    el.hidden = false;
  }

  function getJSON(url) {
    return fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/json' } }).then(function (r) {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    });
  }

  function releaseHref(base, rel) {
    return base + '/release/' + encodeURIComponent(rel.id);
  }

  function tagHref(base, tag, project) {
    return base + '/release/' + encodeURIComponent(tag) + '?project=' + encodeURIComponent(project);
  }

  function arr(v) { return Array.isArray(v) ? v : []; }

  function plansOf(rel) {
    var seen = {}, n = 0;
    arr(rel.contents).forEach(function (c) { if (c.plan && !seen[c.plan]) { seen[c.plan] = 1; n++; } });
    return n;
  }

  // live deployment rows: those with no until.
  function runsOn(rel) {
    return arr(rel.deployments).filter(function (d) { return !d.until; });
  }

  // ---- /releases ----------------------------------------------------------

  function listPage(page) {
    var base = page.getAttribute('data-base') || '';
    page.querySelectorAll('section[data-project]').forEach(function (sec) {
      var project = sec.getAttribute('data-project');
      var list = slot(sec, 'list'), state = slot(sec, 'state'), more = slot(sec, 'more');
      // seenOut: a newer release already went out, so an older cut/built one
      // that never ran anywhere was skipped (releases-design.md §3.2).
      var seenOut = false, cursor = '';

      function load() {
        var url = base + '/api/releases?project=' + encodeURIComponent(project) + '&limit=' + PAGE;
        if (cursor) url += '&cursor=' + encodeURIComponent(cursor);
        more.disabled = true;
        getJSON(url).then(function (body) {
          var items = arr(body.items || body.releases);
          items.forEach(function (rel) {
            var st = statusWord(rel.status);
            var skipped = seenOut && (st === 'cut' || st === 'built');
            if (st === 'live' || st === 'superseded') seenOut = true;
            list.appendChild(row(base, rel, skipped));
          });
          if (!list.children.length) {
            state.textContent = word('empty');
          } else {
            state.hidden = true;
            list.hidden = false;
          }
          cursor = body.next_cursor || '';
          more.hidden = !cursor;
          more.disabled = false;
        }).catch(function () {
          state.hidden = false;
          state.textContent = word('error');
          state.classList.add('rel-state-error');
          more.disabled = false;
        });
      }
      more.addEventListener('click', load);
      load();
    });
  }

  function row(base, rel, skipped) {
    var li = clone('rel-row-tpl');
    var tag = slot(li, 'tag');
    tag.textContent = rel.tag;
    tag.href = releaseHref(base, rel);
    setStatus(slot(li, 'status'), rel.status, skipped);
    if (!setTime(slot(li, 'cut_at'), rel.cut_at)) slot(li, 'cut_at').textContent = '—';
    if (rel.cut_by) slot(li, 'cut_by').textContent = rel.cut_by;
    else slot(li, 'cut_by_wrap').hidden = true;
    var prs = arr(rel.contents).length, plans = plansOf(rel);
    slot(li, 'counts').textContent = count(prs, 'prs') + ' · ' + count(plans, 'plans');
    var runs = slot(li, 'runs'), live = runsOn(rel);
    if (!live.length) {
      runs.textContent = arr(rel.deployments).length ? '' : word('not-deployed');
      runs.classList.add('rel-muted');
    }
    live.forEach(function (d) {
      var r = clone('rel-run-tpl');
      slot(r, 'target').textContent = d.target;
      setTime(slot(r, 'since'), d.since);
      runs.appendChild(r);
    });
    return li;
  }

  // ---- /release/{id} ------------------------------------------------------

  function releasePage(page) {
    var base = page.getAttribute('data-base') || '';
    var id = page.getAttribute('data-release');
    var project = page.getAttribute('data-project') || '';
    var state = slot(page, 'state');
    getJSON(base + '/api/releases/' + encodeURIComponent(id)).then(function (rel) {
      fillRelease(page, base, project || rel.project || '', rel);
      state.hidden = true;
    }).catch(function () {
      state.textContent = word('error');
      state.classList.add('rel-state-error');
    });
  }

  function fillRelease(page, base, project, rel) {
    setStatus(slot(page, 'status'), rel.status, false);

    var sha = slot(page, 'sha'), repo = page.getAttribute('data-repo') || repoOf(rel);
    if (rel.sha) {
      sha.textContent = '';
      var short = rel.sha.slice(0, 12);
      if (repo) {
        var a = document.createElement('a');
        a.href = repo + '/commit/' + rel.sha;
        a.textContent = short;
        a.title = rel.sha;
        a.rel = 'noopener';
        sha.appendChild(a);
      } else {
        sha.textContent = short;
        sha.title = rel.sha;
      }
    }
    setTime(slot(page, 'cut_at'), rel.cut_at);
    if (rel.cut_by) {
      slot(page, 'cut_by').textContent = rel.cut_by;
      slot(page, 'cut_by_wrap').hidden = false;
    }
    if (rel.built_at) {
      setTime(slot(page, 'built_at'), rel.built_at);
      slot(page, 'built_label').hidden = false;
      slot(page, 'built_wrap').hidden = false;
    }
    var live = runsOn(rel), runs = slot(page, 'runs_on');
    runs.textContent = '';
    if (!live.length) runs.textContent = word('not-deployed');
    live.forEach(function (d, i) {
      if (i) runs.appendChild(document.createTextNode(', '));
      var r = clone('rel-run-tpl');
      slot(r, 'target').textContent = d.target;
      setTime(slot(r, 'since'), d.since);
      runs.appendChild(r);
    });

    var prev = slot(page, 'previous');
    if (rel.previous_tag || rel.previous) {
      prev.href = rel.previous ? base + '/release/' + encodeURIComponent(rel.previous) : tagHref(base, rel.previous_tag, project);
      slot(page, 'previous_tag').textContent = rel.previous_tag || rel.previous;
      prev.hidden = false;
    }
    var next = slot(page, 'next');
    if (rel.next_tag || rel.next) {
      next.href = rel.next ? base + '/release/' + encodeURIComponent(rel.next) : tagHref(base, rel.next_tag, project);
      slot(page, 'next_tag').textContent = rel.next_tag || rel.next;
      next.hidden = false;
    }

    fillContents(page, base, rel);
    fillDeployments(page, rel);
  }

  // repoOf reads the repository from a PR url when the page has none.
  function repoOf(rel) {
    var c = arr(rel.contents).find(function (r) { return /^https:\/\/github\.com\/[^/]+\/[^/]+\/pull\//.test(r.url || ''); });
    return c ? c.url.replace(/\/pull\/.*$/, '') : '';
  }

  // groupContents groups the rows by plan, then step. Plans this release
  // finished (plan_status closed) come first, then by PR count; unplanned last.
  function groupContents(rows) {
    var plans = {}, order = [];
    rows.forEach(function (r) {
      var key = r.plan || '';
      var p = plans[key];
      if (!p) {
        p = plans[key] = { id: key, title: r.plan_title || '', status: r.plan_status || '', steps: {}, stepOrder: [], n: 0 };
        order.push(p);
      }
      if (!p.title && r.plan_title) p.title = r.plan_title;
      if (!p.status && r.plan_status) p.status = r.plan_status;
      var skey = r.step || r.task || '';
      var s = p.steps[skey];
      if (!s) {
        s = p.steps[skey] = { id: skey, title: r.step_title || '', rows: [] };
        p.stepOrder.push(s);
      }
      if (!s.title && r.step_title) s.title = r.step_title;
      s.rows.push(r);
      p.n++;
    });
    order.sort(function (a, b) {
      if (!a.id !== !b.id) return a.id ? -1 : 1;
      var fa = a.status === 'closed', fb = b.status === 'closed';
      if (fa !== fb) return fa ? -1 : 1;
      if (a.n !== b.n) return b.n - a.n;
      return (a.title || a.id).localeCompare(b.title || b.id);
    });
    return order;
  }

  function fillContents(page, base, rel) {
    var rows = arr(rel.contents), box = slot(page, 'contents'), summary = slot(page, 'contents_summary');
    if (!rows.length) {
      summary.textContent = word('no-contents');
      return;
    }
    var groups = groupContents(rows);
    var planned = groups.filter(function (g) { return g.id; }).length;
    summary.textContent = count(rows.length, 'prs') + ' · ' + count(planned, 'plans') +
      (rel.contents_complete === false ? ' · ' + word('contents-partial') : '');
    groups.forEach(function (g) {
      var sec = clone('rel-plan-tpl'), head = slot(sec, 'plan');
      if (g.id) {
        head.href = base + '/plans/' + encodeURIComponent(g.id);
        head.textContent = g.title || word('untitled-plan');
        if (!g.title) head.title = g.id;
        if (g.status === 'closed') {
          var fin = slot(sec, 'finished');
          fin.textContent = word('finished');
          fin.hidden = false;
        }
      } else {
        head.removeAttribute('href');
        head.textContent = word('unplanned');
        head.title = word('unplanned-note');
      }
      slot(sec, 'count').textContent = count(g.n, 'prs');
      var steps = slot(sec, 'steps');
      g.stepOrder.forEach(function (s) {
        var st = clone('rel-step-tpl'), sh = slot(st, 'step');
        if (s.id) {
          sh.href = base + '/' + encodeURIComponent(s.id);
          sh.textContent = s.title || word('untitled-step');
          if (!s.title) sh.title = s.id;
        } else if (!g.id) {
          st.querySelector('h4').remove();
        } else {
          sh.removeAttribute('href');
          sh.textContent = word('no-step');
        }
        var list = slot(st, 'prs');
        s.rows.forEach(function (r) { list.appendChild(prRow(r)); });
        steps.appendChild(st);
      });
      box.appendChild(sec);
    });
  }

  function prRow(r) {
    var li = clone('rel-pr-tpl'), num = slot(li, 'pr');
    num.textContent = '#' + r.pr;
    if (r.url) { num.href = r.url; num.rel = 'noopener'; } else num.removeAttribute('href');
    slot(li, 'title').textContent = r.title || '';
    if (r.train && r.train !== r.pr) {
      var t = slot(li, 'train');
      t.textContent = word('train', { n: r.train });
      t.hidden = false;
    }
    var m = slot(li, 'merged_at');
    if (!setTime(m, r.merged_at)) m.textContent = '';
    else m.textContent = word('merged', { t: m.textContent });
    return li;
  }

  function fillDeployments(page, rel) {
    var rows = arr(rel.deployments), list = slot(page, 'deployments');
    if (!rows.length) {
      var none = slot(page, 'no_deployments');
      none.textContent = word('no-deployments');
      none.hidden = false;
      return;
    }
    rows.forEach(function (d) {
      var li = clone('rel-deploy-tpl');
      slot(li, 'target').textContent = d.target;
      slot(li, 'kind').textContent = d.kind || '';
      setTime(slot(li, 'since'), d.since);
      var st = slot(li, 'state');
      if (d.until) {
        var t = document.createElement('time');
        setTime(t, d.until);
        st.textContent = word('superseded-at', { t: t.textContent });
        st.className = 'release-status release-status-superseded';
        st.title = t.title;
      } else {
        li.classList.add('rel-deploy-live');
        st.textContent = word('running');
        st.className = 'release-status release-status-live';
      }
      var meta = [];
      if (d.revision) meta.push(word('revision', { r: d.revision }));
      if (d.by) meta.push(word('by', { b: d.by }));
      if (d.via) meta.push(d.via);
      if (d.image_tag && d.image_tag !== rel.tag) meta.push(d.image_tag);
      slot(li, 'meta').textContent = meta.join(' · ');
      list.appendChild(li);
    });
    list.hidden = false;
  }

  function boot() {
    var list = document.getElementById('rel-list-page');
    if (list) listPage(list);
    var one = document.getElementById('rel-page');
    if (one) releasePage(one);
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
