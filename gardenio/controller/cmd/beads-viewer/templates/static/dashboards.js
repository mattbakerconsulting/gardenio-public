// dashboards.js — the client-side driver for the operator-engagement + security
// dashboards (/dashboards, task ka-0000004892; customer-1 telemetry plan §1/§5).
//
// Fetches every panel from the daemon's engagement/security/uniqueness chart
// endpoints through the read-only viewer proxy (/api/v1/charts/...), and draws
// them with the vendored ECharts. These panels are CATEGORICAL / INSTANT
// (bars, KPI tiles, tables) — not the long-format time-series the shared
// echarts_catalog.js renderer consumes — so this module builds its own bar
// option; it reuses only BeadsCharts.isDark() for skin-aware theming.
//
// PRIVACY (plan §4): every operator identity is resolved SERVER-SIDE. Each row
// arrives with a pre-resolved `display` name; this file NEVER resolves an
// operator_id and never receives an email / slack_user_id in a metric label.
// A row with `forgotten:true` is a right-to-be-forgotten redaction — we render
// a fixed "redacted" label + badge and NEVER surface any residual identity.

(function () {
  'use strict';

  var BASE = (window.BEADS_VIEWER_BASE_PATH || '').replace(/\/$/, '');

  // Categorical palette — shared with echarts_catalog.js so the two chart
  // surfaces read as one system. Assigned in fixed order, never cycled; every
  // bar carries a direct category-axis label so adjacent-hue separation is
  // backed by text, not color alone.
  var COLORS = ['#5b9bd5', '#e07b00', '#7fa869', '#b07cc6', '#d96c6c', '#3f9c8a', '#a07f4f', '#888888'];

  // The reserved sentinel the backend uses for a right-to-be-forgotten operator
  // display. We render our own badge, so treat any forgotten row uniformly
  // regardless of the residual display string.
  var REDACTED_LABEL = 'redacted';

  function isDark() {
    return window.BeadsCharts ? window.BeadsCharts.isDark() : false;
  }

  // axisPalette mirrors echarts_catalog.js: ECharts can't inherit CSS, so read
  // the ambient text colour off the container and pick translucent neutrals for
  // the axis/grid that read on both light and dark skins.
  function axisPalette(node) {
    var label = '#444';
    try {
      var c = getComputedStyle(node).color;
      if (c) label = c;
    } catch (e) { /* detached node */ }
    var dark = isDark();
    return {
      label: label,
      axisLine: dark ? 'rgba(200,200,200,0.35)' : 'rgba(60,60,60,0.35)',
      splitLine: dark ? 'rgba(200,200,200,0.12)' : 'rgba(60,60,60,0.10)',
      tooltipBg: dark ? 'rgba(30,30,30,0.95)' : 'rgba(255,255,255,0.96)',
      tooltipText: dark ? '#eee' : '#222',
    };
  }

  function compactNumber(v) {
    if (v == null || isNaN(v)) return '–';
    var abs = Math.abs(v);
    if (abs >= 1e9) return (v / 1e9).toFixed(2) + 'B';
    if (abs >= 1e6) return (v / 1e6).toFixed(2) + 'M';
    if (abs >= 1e3) return (v / 1e3).toFixed(2) + 'k';
    if (abs >= 100) return v.toFixed(0);
    if (abs >= 1) return v.toFixed(1);
    if (abs === 0) return '0';
    return v.toFixed(3);
  }

  function usd(v) {
    if (v == null || isNaN(v)) return '–';
    if (Math.abs(v) >= 1000) return '$' + compactNumber(v);
    return '$' + v.toFixed(2);
  }

  // formatDuration renders a seconds value as a short human string (s / m / h)
  // for the session-duration table.
  function formatDuration(secs) {
    if (secs == null || isNaN(secs) || secs <= 0) return '–';
    if (secs < 90) return secs.toFixed(0) + 's';
    if (secs < 3600) return (secs / 60).toFixed(1) + 'm';
    return (secs / 3600).toFixed(1) + 'h';
  }

  // pct renders a [0,1] fraction as a percentage string.
  function pct(frac) {
    if (frac == null || isNaN(frac)) return '–';
    return (frac * 100).toFixed(1) + '%';
  }

  // opLabel resolves the display text for an operator row WITHOUT ever touching
  // an id: a forgotten row is redacted, otherwise the server-resolved display
  // is used (with a defensive fallback that is still never an id/email — the
  // backend already guarantees a non-PII fallback).
  function opLabel(row) {
    if (row && row.forgotten) return REDACTED_LABEL;
    if (row && row.display) return row.display;
    return 'unknown';
  }

  // ── state rendering ────────────────────────────────────────────────────────

  function clearChart(node) {
    if (node.__ec) {
      try { node.__ec.dispose(); } catch (e) { /* already gone */ }
      node.__ec = null;
    }
  }

  function showState(node, cls, text) {
    clearChart(node);
    node.innerHTML = '';
    var span = document.createElement('span');
    span.className = 'meta dash-state ' + cls;
    span.textContent = text;
    node.appendChild(span);
  }
  function showLoading(node) { showState(node, 'dash-chart-loading', 'loading…'); }
  function showEmpty(node) { showState(node, 'dash-chart-empty', 'no data yet'); }
  function showError(node, msg) { showState(node, 'dash-chart-error', msg || 'failed to load'); }

  // buildBarOption assembles a horizontal categorical bar chart. `rows` is
  // [{label, value, forgotten?}], already sorted by the caller (the backend
  // returns descending). Horizontal bars keep long category labels (surface
  // names, operator display names, denial reasons) readable without rotation.
  function buildBarOption(node, rows, opts) {
    opts = opts || {};
    var pal = axisPalette(node);
    // ECharts category axis draws bottom→top, so reverse to show the largest
    // (first) row at the top.
    var cats = rows.map(function (r) { return r.label; }).reverse();
    var data = rows.map(function (r, i) {
      // Colour by ENTITY position in the original (sorted) order so a redacted
      // row still gets a stable hue; index into COLORS by the pre-reverse rank.
      var color = COLORS[(rows.length - 1 - i) % COLORS.length];
      return { value: r.value, itemStyle: { color: color, borderRadius: [0, 3, 3, 0] } };
    }).reverse();
    var fmt = opts.format || compactNumber;
    return {
      animation: false,
      grid: { left: 4, right: 12, top: 8, bottom: 4, containLabel: true },
      tooltip: {
        trigger: 'axis',
        axisPointer: { type: 'shadow' },
        backgroundColor: pal.tooltipBg,
        borderWidth: 0,
        textStyle: { color: pal.tooltipText, fontSize: 12 },
        formatter: function (items) {
          if (!items || !items.length) return '';
          var it = items[0];
          var v = it.value;
          return it.axisValueLabel + ': ' + fmt(v);
        },
      },
      xAxis: {
        type: 'value',
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel: { color: pal.label, fontSize: 10, formatter: function (v) { return fmt(v); } },
        splitLine: { lineStyle: { color: pal.splitLine } },
      },
      yAxis: {
        type: 'category',
        data: cats,
        axisLine: { lineStyle: { color: pal.axisLine } },
        axisTick: { show: false },
        axisLabel: { color: pal.label, fontSize: 11, width: 140, overflow: 'truncate' },
      },
      series: [{
        type: 'bar',
        data: data,
        barMaxWidth: 22,
        label: {
          show: true,
          position: 'right',
          color: pal.label,
          fontSize: 10,
          formatter: function (p) { return fmt(p.value); },
        },
      }],
    };
  }

  // drawBar (re)creates the ECharts instance for a bar panel. Empty rows show
  // the empty state instead of an axis with no bars.
  function drawBar(node, rows, opts) {
    if (typeof window.echarts === 'undefined') { showError(node, 'chart engine unavailable'); return; }
    if (!rows || !rows.length) { showEmpty(node); return; }
    if (!node.__ec) {
      node.innerHTML = '';
      node.__ec = window.echarts.init(node, null, { renderer: 'canvas' });
    }
    node.__ec.setOption(buildBarOption(node, rows, opts), { notMerge: true });
    node.__ec.resize();
  }

  // ── fetch ──────────────────────────────────────────────────────────────────

  // fetchJSON GETs a chart endpoint through the proxy. A 503 (backend
  // unconfigured) resolves to a sentinel so callers render "no data yet" rather
  // than an error — an empty backend is the common v1 case, not a fault.
  function fetchJSON(path, params) {
    var qs = [];
    params = params || {};
    Object.keys(params).forEach(function (k) {
      var v = params[k];
      if (v == null || v === '') return;
      qs.push(encodeURIComponent(k) + '=' + encodeURIComponent(v));
    });
    var url = BASE + '/api/v1/charts/' + path + (qs.length ? '?' + qs.join('&') : '');
    return fetch(url, { headers: { Accept: 'application/json' }, credentials: 'same-origin' })
      .then(function (resp) {
        if (resp.status === 503) return { __unavailable: true };
        if (!resp.ok) return { __error: 'error ' + resp.status };
        return resp.json();
      })
      .catch(function () { return { __error: 'failed to load' }; });
  }

  // ── panels ───────────────────────────────────────────────────────────────

  function loadUniqueness() {
    var tiles = document.querySelectorAll('#dash-uniqueness .dash-kpi');
    fetchJSON('uniqueness').then(function (data) {
      var byKey = {};
      if (data && data.panels) data.panels.forEach(function (p) { byKey[p.key] = p.count; });
      tiles.forEach(function (tile) {
        var key = tile.getAttribute('data-uniq-key');
        var el = tile.querySelector('.dash-kpi-value');
        if (data && data.__unavailable) { el.textContent = '–'; return; }
        var n = byKey[key];
        el.textContent = (n == null) ? '–' : String(n);
      });
    });
  }

  function loadActivity(window_) {
    var surfaceNode = document.querySelector('[data-dash-chart="activity-surface"]');
    var opsNode = document.querySelector('[data-dash-chart="activity-operators"]');
    showLoading(surfaceNode); showLoading(opsNode);
    fetchJSON('engagement/activity', { window: window_ }).then(function (data) {
      if (!data || data.__error) { showError(surfaceNode, data && data.__error); showError(opsNode, data && data.__error); return; }
      if (data.__unavailable) { showEmpty(surfaceNode); showEmpty(opsNode); return; }
      var surfaces = (data.by_surface || []).map(function (s) { return { label: s.surface, value: s.count }; });
      drawBar(surfaceNode, surfaces, {});
      var ops = (data.top_operators || []).map(function (o) { return { label: opLabel(o), value: o.value, forgotten: o.forgotten }; });
      drawBar(opsNode, ops, {});
    });
  }

  function loadSpend() {
    var node = document.querySelector('[data-dash-chart="spend-operators"]');
    var othersEl = document.querySelector('[data-dash-spend-others]');
    showLoading(node);
    fetchJSON('engagement/spend').then(function (data) {
      if (othersEl) othersEl.textContent = '';
      if (!data || data.__error) { showError(node, data && data.__error); return; }
      if (data.__unavailable) { showEmpty(node); return; }
      var ops = (data.top_operators || []).map(function (o) { return { label: opLabel(o), value: o.value, forgotten: o.forgotten }; });
      drawBar(node, ops, { format: usd });
      if (othersEl && data.others_usd > 0) othersEl.textContent = 'Others: ' + usd(data.others_usd) + '.';
    });
  }

  function loadSessions(window_) {
    var table = document.querySelector('[data-dash-table="sessions"] tbody');
    if (!table) return;
    table.innerHTML = '<tr><td colspan="5" class="meta dash-state">loading…</td></tr>';
    fetchJSON('engagement/sessions', { window: window_ }).then(function (data) {
      if (!data || data.__error) { stateRow(table, 5, data && data.__error || 'failed to load'); return; }
      if (data.__unavailable) { stateRow(table, 5, 'no data yet'); return; }
      var rows = data.top_operators || [];
      if (!rows.length) { stateRow(table, 5, 'no data yet'); return; }
      table.innerHTML = '';
      rows.forEach(function (o) {
        var q = o.quantiles || {};
        var tr = document.createElement('tr');
        appendOpCell(tr, o);
        appendNumCell(tr, formatDuration(q.p50));
        appendNumCell(tr, formatDuration(q.p90));
        appendNumCell(tr, formatDuration(q.p99));
        appendNumCell(tr, o.session_count != null ? compactNumber(o.session_count) : '–');
        table.appendChild(tr);
      });
    });
  }

  function loadAuthz(window_) {
    var node = document.querySelector('[data-dash-chart="authz"]');
    var headline = document.querySelector('[data-dash-headline="authz-total"]');
    showLoading(node);
    if (headline) headline.textContent = '';
    fetchJSON('security/authz', { window: window_ }).then(function (data) {
      if (!data || data.__error) { showError(node, data && data.__error); return; }
      if (data.__unavailable) { showEmpty(node); return; }
      // Aggregate by reason so the bar chart is a compact, stable category set;
      // the (action, reason, bead_type) tuples are summarized to their reason.
      var byReason = {};
      (data.denials || []).forEach(function (d) { byReason[d.reason] = (byReason[d.reason] || 0) + d.count; });
      var rows = Object.keys(byReason).map(function (k) { return { label: k, value: byReason[k] }; });
      rows.sort(function (a, b) { return b.value - a.value; });
      drawBar(node, rows, {});
      if (headline) headline.textContent = data.total != null ? compactNumber(data.total) + ' total' : '';
    });
  }

  function loadDecisions(window_) {
    var lifeNode = document.querySelector('[data-dash-chart="decisions-lifecycle"]');
    var escNode = document.querySelector('[data-dash-chart="escalations"]');
    showLoading(lifeNode); showLoading(escNode);
    fetchJSON('security/decisions', { window: window_ }).then(function (data) {
      if (!data || data.__error) { showError(lifeNode, data && data.__error); showError(escNode, data && data.__error); return; }
      if (data.__unavailable) { showEmpty(lifeNode); showEmpty(escNode); return; }
      // Lifecycle: aggregate by event across urgencies for a compact panel.
      var byEvent = {};
      (data.lifecycle || []).forEach(function (r) { byEvent[r.event] = (byEvent[r.event] || 0) + r.count; });
      var life = Object.keys(byEvent).map(function (k) { return { label: k, value: byEvent[k] }; });
      life.sort(function (a, b) { return b.value - a.value; });
      drawBar(lifeNode, life, {});
      // Escalations: aggregate by outcome across roles.
      var byOutcome = {};
      (data.escalations || []).forEach(function (r) { byOutcome[r.outcome] = (byOutcome[r.outcome] || 0) + r.count; });
      var esc = Object.keys(byOutcome).map(function (k) { return { label: k, value: byOutcome[k] }; });
      esc.sort(function (a, b) { return b.value - a.value; });
      drawBar(escNode, esc, {});
    });
  }

  function loadTokenSLO(window_) {
    var table = document.querySelector('[data-dash-table="token-slo"] tbody');
    var badge = document.querySelector('[data-dash-slo-badge]');
    var sub = document.querySelector('[data-dash-slo-sub]');
    if (!table) return;
    table.innerHTML = '<tr><td colspan="5" class="meta dash-state">loading…</td></tr>';
    if (badge) badge.hidden = true;
    fetchJSON('security/token-slo', { window: window_ }).then(function (data) {
      if (!data || data.__error) { stateRow(table, 5, data && data.__error || 'failed to load'); return; }
      if (data.__unavailable) { stateRow(table, 5, 'no data yet'); return; }
      var objectives = data.objectives || [];
      // v1-conditional: the histogram may not be emitted yet. Echo the target
      // objectives but badge the panel "awaiting data" rather than fabricating a
      // verdict from an empty histogram.
      if (badge) {
        if (!data.available) { badge.hidden = false; badge.textContent = 'awaiting data'; }
        else { badge.hidden = true; }
      }
      if (sub) {
        var src = data.target_source === 'config' ? 'operator-approved' : 'default';
        sub.textContent = 'Agent token-revocation latency against the ' + src + ' SLO target.';
      }
      if (!objectives.length) { stateRow(table, 5, 'no objectives configured'); return; }
      table.innerHTML = '';
      objectives.forEach(function (o) {
        var tr = document.createElement('tr');
        appendTextCell(tr, 'p' + Math.round(o.percentile * 100));
        appendNumCell(tr, formatDuration(o.budget_seconds));
        appendNumCell(tr, o.observed_quantile_seconds != null ? formatDuration(o.observed_quantile_seconds) : '–');
        appendNumCell(tr, o.fraction_within_budget != null ? pct(o.fraction_within_budget) : '–');
        appendStatusCell(tr, data.available, o.met);
        table.appendChild(tr);
      });
    });
  }

  // ── table cell helpers ─────────────────────────────────────────────────────

  function stateRow(tbody, cols, text) {
    tbody.innerHTML = '';
    var tr = document.createElement('tr');
    var td = document.createElement('td');
    td.colSpan = cols;
    td.className = 'meta dash-state';
    td.textContent = text;
    tr.appendChild(td);
    tbody.appendChild(tr);
  }

  function appendTextCell(tr, text) {
    var td = document.createElement('td');
    td.textContent = text;
    tr.appendChild(td);
  }

  function appendNumCell(tr, text) {
    var td = document.createElement('td');
    td.className = 'dash-num';
    td.textContent = text;
    tr.appendChild(td);
  }

  // appendOpCell writes the operator name cell. A forgotten operator gets the
  // redacted label + an inline badge and NEVER any residual identity text.
  function appendOpCell(tr, row) {
    var td = document.createElement('td');
    var name = document.createElement('span');
    name.className = 'dash-op-name';
    name.textContent = opLabel(row);
    td.appendChild(name);
    if (row && row.forgotten) {
      var badge = document.createElement('span');
      badge.className = 'dash-forgotten-inline';
      badge.textContent = 'redacted';
      badge.style.marginLeft = '0.4rem';
      td.appendChild(badge);
    }
    tr.appendChild(td);
  }

  function appendStatusCell(tr, available, met) {
    var td = document.createElement('td');
    var pill = document.createElement('span');
    pill.className = 'dash-slo-pill';
    if (!available || met == null) {
      pill.className += ' dash-slo-pill--na';
      pill.textContent = available ? 'no data' : 'awaiting data';
    } else if (met) {
      pill.className += ' dash-slo-pill--met';
      pill.textContent = '✓ met';
    } else {
      pill.className += ' dash-slo-pill--miss';
      pill.textContent = '✗ missed';
    }
    td.appendChild(pill);
    tr.appendChild(td);
  }

  // ── orchestration ──────────────────────────────────────────────────────────

  var state = { window: '7d', tab: 'engagement', engagementLoaded: false, securityLoaded: false };

  function loadEngagement() {
    loadUniqueness();
    loadActivity(state.window);
    loadSpend();
    loadSessions(state.window);
    state.engagementLoaded = true;
  }

  function loadSecurity() {
    loadAuthz(state.window);
    loadDecisions(state.window);
    loadTokenSLO(state.window);
    state.securityLoaded = true;
  }

  // reloadWindowed re-fetches only the panels that depend on the window; spend +
  // uniqueness are window-independent and are left as-is.
  function reloadActive() {
    if (state.tab === 'engagement') {
      loadActivity(state.window);
      loadSessions(state.window);
    } else {
      loadSecurity();
    }
  }

  function showTab(tab) {
    state.tab = tab;
    document.querySelectorAll('[data-dash-panel]').forEach(function (sec) {
      sec.hidden = sec.getAttribute('data-dash-panel') !== tab;
    });
    document.querySelectorAll('[data-dash-tab]').forEach(function (btn) {
      var on = btn.getAttribute('data-dash-tab') === tab;
      btn.classList.toggle('is-active', on);
      if (on) { btn.setAttribute('aria-current', 'page'); } else { btn.removeAttribute('aria-current'); }
    });
    // Lazy-load a section the first time it is shown; charts also need a resize
    // because ECharts can't measure a hidden (display:none) container.
    if (tab === 'engagement') {
      if (!state.engagementLoaded) loadEngagement();
    } else {
      if (!state.securityLoaded) loadSecurity();
    }
    resizeAll();
  }

  function resizeAll() {
    document.querySelectorAll('[data-dash-chart]').forEach(function (n) {
      if (n.__ec) { try { n.__ec.resize(); } catch (e) { /* disposed */ } }
    });
  }

  function wire() {
    document.querySelectorAll('[data-dash-tab]').forEach(function (btn) {
      btn.addEventListener('click', function () { showTab(btn.getAttribute('data-dash-tab')); });
    });
    document.querySelectorAll('.dash-window-chip').forEach(function (chip) {
      chip.addEventListener('click', function () {
        state.window = chip.getAttribute('data-window') || '7d';
        document.querySelectorAll('.dash-window-chip').forEach(function (c) {
          var on = c === chip;
          c.classList.toggle('is-active', on);
          c.setAttribute('aria-pressed', on ? 'true' : 'false');
        });
        reloadActive();
      });
    });
    var resizeTimer = null;
    window.addEventListener('resize', function () {
      if (resizeTimer) clearTimeout(resizeTimer);
      resizeTimer = setTimeout(resizeAll, 150);
    });
  }

  function start() {
    // echarts.min.js + echarts_catalog.js load with `defer`, so they are ready
    // by the time this deferred script runs; guard anyway.
    if (typeof window.echarts === 'undefined') {
      window.addEventListener('load', start, { once: true });
      return;
    }
    wire();
    loadEngagement(); // engagement is the default visible tab
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
